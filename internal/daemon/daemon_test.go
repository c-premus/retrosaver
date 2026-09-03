package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/c-premus/retrosaver/internal/config"
	"github.com/c-premus/retrosaver/internal/idle"
	"github.com/c-premus/retrosaver/internal/modules"
)

// The tests drive the state machine through fakes and synchronise on the
// trace channel rather than on wall-clock time. The daemon owns no timers --
// every deadline comes from the idle monitor -- which is what makes that
// possible. Do not add a time.After to the daemon.

const traceTimeout = 2 * time.Second

// ---------------------------------------------------------------- fakes

type fakeMonitor struct {
	mu      sync.Mutex
	fired   chan idle.WatchID
	next    idle.WatchID
	added   []time.Duration
	actives []idle.WatchID
	removed []idle.WatchID
	closed  bool
	addErr  error

	// idletime is what Idletime reports, modelling a session that was already
	// idle when the daemon armed. idletimeErr models the D-Bus read failing.
	idletime    time.Duration
	idletimeErr error

	// activeErr makes AddUserActiveWatch fail, which is a different path from
	// addErr: the user-active watch is armed per stage, not by arm().
	activeErr error
}

func newFakeMonitor() *fakeMonitor {
	return &fakeMonitor{fired: make(chan idle.WatchID, 8)}
}

// Idletime reports how long the session has been idle. Zero unless a test
// sets it, which keeps every existing test on the "session just went idle"
// path where no swap threshold has been missed.
func (f *fakeMonitor) Idletime() (time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.idletime, f.idletimeErr
}

func (f *fakeMonitor) setIdletime(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idletime = d
}

func (f *fakeMonitor) AddIdleWatch(d time.Duration) (idle.WatchID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return 0, f.addErr
	}
	f.next++
	f.added = append(f.added, d)
	return f.next, nil
}

func (f *fakeMonitor) AddUserActiveWatch() (idle.WatchID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.activeErr != nil {
		return 0, f.activeErr
	}
	f.next++
	f.actives = append(f.actives, f.next)
	return f.next, nil
}

func (f *fakeMonitor) RemoveWatch(id idle.WatchID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
	return nil
}

func (f *fakeMonitor) Fired() <-chan idle.WatchID { return f.fired }

func (f *fakeMonitor) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// isClosed reads the flag under the fake's own mutex. Reading f.closed
// directly races Run's deferred Close on its own goroutine.
func (f *fakeMonitor) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeMonitor) intervals() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.added)
}

func (f *fakeMonitor) activeWatches() []idle.WatchID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.actives)
}

func (f *fakeMonitor) removedWatches() []idle.WatchID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.removed)
}

// fakeSaver records whether it was stopped, and how often.
type fakeSaver struct {
	mu    sync.Mutex
	name  string
	stops int
}

func (s *fakeSaver) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops++
	return nil
}

func (s *fakeSaver) stopCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stops
}

// fakeLauncher hands out module names in order and can hold a launch open so
// a test can inject user activity while a module is mid-launch.
type fakeLauncher struct {
	mu       sync.Mutex
	names    []string // handed out in order; the last repeats
	failures map[string]error
	release  chan struct{} // when non-nil, Launch waits on it
	// ignoreCancel models a launch that has already got far enough that
	// cancelling it cannot un-map the window: it returns a live saver even
	// though the context is done. That is the case the generation counter
	// exists for. Without it the fake simply returns ctx.Err() and there is
	// nothing to discard.
	ignoreCancel bool
	// honourAvoid makes Pick behave like realLauncher: it returns the first
	// name not in the avoid list and errors when every name is avoided. The
	// default is index-based and ignores avoid entirely, which is what most
	// tests want; the cycling tests need real exhaustion semantics, because
	// telling exhaustion apart from an empty selection is the interesting
	// part of machine.pick.
	honourAvoid bool
	asked       []string
	avoided     [][]string
	savers      []*fakeSaver
	pickErr     error
	// filters records SetFilters calls, so a reload test can prove the
	// launcher was actually re-pointed rather than left on its stale copy.
	filters [][2][]string
}

func (l *fakeLauncher) SetFilters(include, exclude []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.filters = append(l.filters, [2][]string{slices.Clone(include), slices.Clone(exclude)})
	// Model the real launcher: what Pick hands out follows the include list.
	if len(include) > 0 {
		l.names = slices.Clone(include)
	}
}

// lastFilters returns the include/exclude pair from the most recent
// SetFilters call, or nil if it was never called.
// saverAt returns the nth saver the launcher produced, under the fake's own
// mutex: Launch appends from the daemon's goroutine while the test reads.
func (l *fakeLauncher) saverAt(t *testing.T, n int) *fakeSaver {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if n >= len(l.savers) {
		t.Fatalf("launcher produced %d savers, want more than %d", len(l.savers), n)
	}
	return l.savers[n]
}

// saverCount reports how many savers the launcher produced.
func (l *fakeLauncher) saverCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.savers)
}

func (l *fakeLauncher) lastFilters() ([]string, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.filters) == 0 {
		return nil, nil
	}
	f := l.filters[len(l.filters)-1]
	return f[0], f[1]
}

func (l *fakeLauncher) Pick(avoid ...string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pickErr != nil {
		return "", l.pickErr
	}
	l.avoided = append(l.avoided, slices.Clone(avoid))
	if l.honourAvoid {
		for _, n := range l.names {
			if !slices.Contains(avoid, n) {
				return n, nil
			}
		}
		// The wording modules.Available uses, so a test reads like the real
		// failure the daemon has to disambiguate.
		return "", errors.New("no modules available: none survived INCLUDE/EXCLUDE")
	}
	i := min(len(l.avoided)-1, len(l.names)-1)
	return l.names[i], nil
}

func (l *fakeLauncher) Launch(ctx context.Context, name string) (saver, error) {
	l.mu.Lock()
	l.asked = append(l.asked, name)
	release := l.release
	ignoreCancel := l.ignoreCancel
	err := l.failures[name]
	l.mu.Unlock()

	if release != nil {
		if ignoreCancel {
			<-release
		} else {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	if err != nil {
		return nil, err
	}

	s := &fakeSaver{name: name}
	l.mu.Lock()
	l.savers = append(l.savers, s)
	l.mu.Unlock()
	return s, nil
}

func (l *fakeLauncher) askedFor() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.asked)
}

func (l *fakeLauncher) avoidedOn(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n >= len(l.avoided) {
		return nil
	}
	return l.avoided[n]
}

type fakeSession struct {
	mu           sync.Mutex
	locks        int
	idleDelays   []int
	restores     int
	lockErr      error
	setDelayErr  error
	restoreDelay error
}

func (s *fakeSession) Lock() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.locks++
	return s.lockErr
}

func (s *fakeSession) SetIdleDelay(n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.setDelayErr != nil {
		return s.setDelayErr
	}
	s.idleDelays = append(s.idleDelays, n)
	return nil
}

func (s *fakeSession) RestoreIdleDelay() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restores++
	return s.restoreDelay
}

func (s *fakeSession) lockCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.locks
}

func (s *fakeSession) delays() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.idleDelays)
}

func (s *fakeSession) restoreCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restores
}

// ---------------------------------------------------------------- harness

type harness struct {
	t     *testing.T
	d     *Daemon
	mon   *fakeMonitor
	lau   *fakeLauncher
	sess  *fakeSession
	trace chan string

	// reloadC drives the config-reload path; nextCfg is what the injected
	// loader hands back, and loadErr makes it fail instead.
	reloadC chan struct{}
	cfgMu   sync.Mutex
	nextCfg config.Config
	loadErr error

	cancel context.CancelFunc
	errc   chan error

	// finished and runErr memoise Run's return, so result is idempotent.
	finished bool
	runErr   error
}

// defaultConfig deliberately leaves CycleAfter zero, so every test that does
// not care about cycling exercises the single-module-per-idle-period path and
// keeps the watch ids below stable. It is therefore NOT a mirror of
// config.Defaults(), which does enable cycling.
func defaultConfig() config.Config {
	return config.Config{
		SaverDelay: 300 * time.Second,
		LockAfter:  900 * time.Second,
		BlankAfter: 120 * time.Second,
	}
}

// scale turns a list of plain second counts into durations, so a table of
// expected thresholds reads as 300/1200/1320 rather than as six repetitions
// of time.Second.
func scale(secs []time.Duration) []time.Duration {
	out := make([]time.Duration, len(secs))
	for i, s := range secs {
		out[i] = s * time.Second
	}
	return out
}

// cyclingConfig turns cycling on, at an interval that fits several swaps in
// before the lock threshold at 1200s: watches land at 400, 500, 600 ...
func cyclingConfig() config.Config {
	cfg := defaultConfig()
	cfg.CycleAfter = 100 * time.Second
	return cfg
}

func start(t *testing.T, cfg config.Config, tweak ...func(*harness)) *harness {
	t.Helper()

	h := &harness{
		t:     t,
		mon:   newFakeMonitor(),
		lau:   &fakeLauncher{names: []string{"atlantis"}},
		sess:  &fakeSession{},
		trace: make(chan string, 64),
		errc:  make(chan error, 1),

		reloadC: make(chan struct{}, 1),
		nextCfg: cfg,
	}
	h.d = New(cfg)

	h.d.connect = func() (idleMonitor, error) { return h.mon, nil }
	h.d.modules = h.lau
	h.d.session = h.sess
	h.d.backstop = func() error { return nil }
	h.d.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	h.d.traceC = h.trace
	h.d.Reload(h.reloadC)
	h.d.loadCfg = func() (config.Config, error) {
		h.cfgMu.Lock()
		defer h.cfgMu.Unlock()
		return h.nextCfg, h.loadErr
	}

	// After the defaults, not before: a tweak that replaces one of the fields
	// set above -- d.backstop, say -- would otherwise be silently overwritten.
	// Still before Run starts, so nothing here races the daemon's goroutine.
	for _, fn := range tweak {
		fn(h)
	}

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.errc <- h.d.Run(ctx) }()

	// Unconditional teardown. Without it a test that never calls stop leaves
	// Run and its goroutines running past the end of the test -- which one
	// test genuinely did -- and every other test needs a
	// `defer h.stop() //nolint:errcheck` purely to avoid that.
	t.Cleanup(func() {
		cancel()
		_ = h.result()
	})

	h.want("armed")
	return h
}

// want asserts the next trace tag.
func (h *harness) want(tag string) {
	h.t.Helper()
	select {
	case got := <-h.trace:
		if got != tag {
			h.t.Fatalf("trace = %q, want %q", got, tag)
		}
	case <-time.After(traceTimeout):
		h.t.Fatalf("timed out waiting for trace %q", tag)
	}
}

// reload makes the injected loader return cfg, then triggers a reload and
// waits for the daemon to finish handling it.
func (h *harness) reload(cfg config.Config, tag string) {
	h.t.Helper()
	h.cfgMu.Lock()
	h.nextCfg, h.loadErr = cfg, nil
	h.cfgMu.Unlock()
	h.reloadC <- struct{}{}
	h.want(tag)
}

// reloadFailing triggers a reload whose config cannot be read.
func (h *harness) reloadFailing(err error) {
	h.t.Helper()
	h.cfgMu.Lock()
	h.loadErr = err
	h.cfgMu.Unlock()
	h.reloadC <- struct{}{}
	h.want("reload:failed")
}

// fire delivers a watch and waits for the daemon to finish handling it.
func (h *harness) fire(id idle.WatchID, tag string) {
	h.t.Helper()
	h.mon.fired <- id
	h.want(tag)
}

// result waits for Run to return, at most once.
//
// Repeat calls hand back the same error, which is what lets start register an
// unconditional cleanup while a test still asserts the error itself.
func (h *harness) result() error {
	h.t.Helper()
	if h.finished {
		return h.runErr
	}
	select {
	case err := <-h.errc:
		h.runErr, h.finished = err, true
	case <-time.After(traceTimeout):
		h.t.Fatal("Run did not return")
	}
	return h.runErr
}

// stop cancels Run and returns its error.
func (h *harness) stop() error {
	h.t.Helper()
	h.cancel()
	return h.result()
}

// Watch IDs are handed out in arming order by fakeMonitor:
// 1 saver, 2 lock, 3 blank, 4 user-active (with the full default config).
const (
	wSaver  idle.WatchID = 1
	wLock   idle.WatchID = 2
	wBlank  idle.WatchID = 3
	wActive idle.WatchID = 4

	// arm() registers three idle watches; the user-active watch is added when
	// a stage begins, so with the default config the ids run
	// 1 saver, 2 lock, 3 blank, then 4 user-active once the saver fires.
	// A re-arm registers the next three idle watches.
	wSaver2 idle.WatchID = 5
	wLock2  idle.WatchID = 6

	// With cycling on, onSaver arms the user-active watch and then the first
	// cycle watch, so the cycle ids follow the user-active one. Each fire
	// retires its watch and arms the next, hence a fresh id every time.
	wCycle  idle.WatchID = 5
	wCycle2 idle.WatchID = 6
)

// ---------------------------------------------------------------- tests

func TestArmsAllFourWatchesAtTheRightThresholds(t *testing.T) {
	h := start(t, defaultConfig())

	want := []time.Duration{
		300 * time.Second,  // saver
		1200 * time.Second, // lock: saver + 900
		1320 * time.Second, // blank: lock + 120
	}
	if got := h.mon.intervals(); !slices.Equal(got, want) {
		t.Errorf("idle watch thresholds = %v, want %v", got, want)
	}
	// No user-active watch until a stage actually starts; see
	// TestUserActiveWatchIsNotReArmedImmediately.
	if got := h.mon.activeWatches(); len(got) != 0 {
		t.Errorf("user-active watches = %v, want none before any stage runs", got)
	}
	// The daemon takes ownership of the idle policy on entry.
	if got := h.sess.delays(); len(got) == 0 || got[0] != 0 {
		t.Errorf("idle-delay writes = %v, want it to start with 0", got)
	}
}

func TestLockDisabledArmsOnlySaverAndActive(t *testing.T) {
	cfg := defaultConfig()
	cfg.LockAfter = 0
	h := start(t, cfg)

	want := []time.Duration{300 * time.Second}
	if got := h.mon.intervals(); !slices.Equal(got, want) {
		t.Errorf("idle watch thresholds = %v, want %v", got, want)
	}
}

// Blanking without locking is meaningless, so BLANK_AFTER alone does nothing.
func TestBlankDisabledWhenLockDisabled(t *testing.T) {
	cfg := defaultConfig()
	cfg.LockAfter = 0
	cfg.BlankAfter = 120 * time.Second
	h := start(t, cfg)

	if got := h.mon.intervals(); len(got) != 1 {
		t.Errorf("idle watch thresholds = %v, want only the saver watch", got)
	}
}

func TestBlankAfterZeroArmsSaverAndLockOnly(t *testing.T) {
	cfg := defaultConfig()
	cfg.BlankAfter = 0
	h := start(t, cfg)

	want := []time.Duration{300 * time.Second, 1200 * time.Second}
	if got := h.mon.intervals(); !slices.Equal(got, want) {
		t.Errorf("idle watch thresholds = %v, want %v", got, want)
	}
}

func TestFullSequenceSaverThenLockThenBlank(t *testing.T) {
	h := start(t, defaultConfig())

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	h.fire(wLock, "watch:lock")
	if got := h.lau.saverAt(t, 0).stopCount(); got != 1 {
		t.Errorf("module stopped %d times at the lock stage, want 1", got)
	}
	if got := h.sess.lockCount(); got != 1 {
		t.Errorf("Lock called %d times, want 1", got)
	}

	h.fire(wBlank, "watch:blank")
	delays := h.sess.delays()
	if len(delays) < 2 || delays[len(delays)-1] != blankIdleDelay {
		t.Errorf("idle-delay writes = %v, want the blank stage to set %d", delays, blankIdleDelay)
	}

	if err := h.stop(); err != nil {
		t.Errorf("Run() = %v, want nil on ctx cancel", err)
	}
}

// The module must be stopped before the lock screen appears, not after.
func TestLockStopsTheModuleBeforeLocking(t *testing.T) {
	h := start(t, defaultConfig())

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	h.fire(wLock, "watch:lock")

	if h.lau.saverAt(t, 0).stopCount() == 0 {
		t.Error("the lock stage locked without stopping the module")
	}
}

func TestUserActiveTearsDownAndReArms(t *testing.T) {
	h := start(t, defaultConfig())

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	h.fire(wActive, "watch:active")

	if got := h.lau.saverAt(t, 0).stopCount(); got != 1 {
		t.Errorf("module stopped %d times on user activity, want 1", got)
	}
	// Re-arming drops every watch and registers a fresh set, which is correct
	// whether or not Mutter's idle watches survive a reset -- a question that
	// cannot be settled without a human at the keyboard, since XTEST input
	// does not move Mutter's idle clock.
	if got := h.mon.removedWatches(); len(got) != 4 {
		t.Errorf("RemoveWatch called with %v, want all four watches dropped", got)
	}
	if got := h.mon.intervals(); len(got) != 6 {
		t.Errorf("idle watches added = %v, want six (three per arming, twice)", got)
	}
	// Exactly one, armed when the saver stage began. Re-arming a second one
	// here is the bug TestUserActiveWatchIsNotReArmedImmediately guards.
	if got := h.mon.activeWatches(); len(got) != 1 {
		t.Errorf("user-active watches = %v, want one", got)
	}
}

// Regression test for a live-observed storm: roughly 200 teardowns in 20
// seconds while the user was simply using the machine.
//
// A user-active watch added while the user is ALREADY active fires
// immediately, so re-arming one from the user-active handler loops: fire,
// tear down, re-arm, fire again, for as long as the user keeps typing. The
// watch must therefore be armed when a stage begins -- when the session is
// idle by definition -- and never from the handler.
func TestUserActiveWatchIsNotReArmedImmediately(t *testing.T) {
	h := start(t, defaultConfig())

	if got := h.mon.activeWatches(); len(got) != 0 {
		t.Fatalf("user-active watches = %v, want none before a stage runs", got)
	}

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")
	if got := h.mon.activeWatches(); len(got) != 1 {
		t.Fatalf("user-active watches = %v, want one once the saver stage began", got)
	}

	h.fire(wActive, "watch:active")
	if got := h.mon.activeWatches(); len(got) != 1 {
		t.Errorf("user-active watches = %v after teardown; re-arming one here is "+
			"exactly the loop that caused ~200 teardowns in 20s on a real session", got)
	}

	// It comes back only when the next cycle's saver stage starts.
	h.fire(wSaver2, "watch:saver")
	h.want("launch:ok:atlantis")
	if got := h.mon.activeWatches(); len(got) != 2 {
		t.Errorf("user-active watches = %v, want a second one for the second cycle", got)
	}
}

// On a cold start past the lock threshold the lock watch can fire without the
// saver handler ever running, so the user-active watch must be armed there too
// or the daemon would never notice the user coming back.
func TestLockStageArmsTheUserActiveWatchToo(t *testing.T) {
	h := start(t, defaultConfig())

	h.fire(wLock, "watch:lock")

	if got := h.mon.activeWatches(); len(got) != 1 {
		t.Errorf("user-active watches = %v, want one armed by the lock stage", got)
	}
}

// After a reset the machine must run a whole second cycle.
func TestSecondCycleAfterReset(t *testing.T) {
	h := start(t, defaultConfig())

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")
	h.fire(wActive, "watch:active")

	// Re-arming registers a fresh set, so the second cycle's watches carry
	// new ids: 5 saver, 6 lock, 7 blank, 8 user-active.
	h.fire(wSaver2, "watch:saver")
	h.want("launch:ok:atlantis")

	if got := len(h.lau.askedFor()); got != 2 {
		t.Errorf("Launch called %d times across two cycles, want 2", got)
	}
	// The first cycle's ids are stale now and must be ignored, not
	// dispatched to whatever stage happens to sit at that index.
	h.mon.fired <- wSaver
	h.fire(wLock2, "watch:lock")
	if got := h.sess.lockCount(); got != 1 {
		t.Errorf("Lock called %d times; a stale watch id was dispatched", got)
	}
}

// Restoring idle-delay to 0 after blanking is what re-arms GNOME's own
// blanking for the next cycle.
func TestUserActiveAfterBlankRestoresIdleDelayToZero(t *testing.T) {
	h := start(t, defaultConfig())

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")
	h.fire(wLock, "watch:lock")
	h.fire(wBlank, "watch:blank")
	h.fire(wActive, "watch:active")

	delays := h.sess.delays()
	if len(delays) == 0 || delays[len(delays)-1] != 0 {
		t.Errorf("idle-delay writes = %v, want the last to be 0 after user activity", delays)
	}
}

func TestRetriesOnceWithADifferentModule(t *testing.T) {
	h := start(t, defaultConfig(), func(h *harness) {
		h.lau.names = []string{"atlantis", "flame"}
		h.lau.failures = map[string]error{"atlantis": errors.New("no GL context")}
	})

	h.fire(wSaver, "watch:saver")
	h.want("launch:failed:atlantis")
	h.want("launch:ok:flame")

	if got, want := h.lau.askedFor(), []string{"atlantis", "flame"}; !slices.Equal(got, want) {
		t.Errorf("modules launched = %v, want %v", got, want)
	}
	// The retry must tell Pick what already failed, or it can pick it again.
	if got := h.lau.avoidedOn(1); !slices.Contains(got, "atlantis") {
		t.Errorf("retry avoided %v, want it to exclude atlantis", got)
	}
}

func TestRetriesOnlyOnce(t *testing.T) {
	h := start(t, defaultConfig(), func(h *harness) {
		h.lau.names = []string{"atlantis", "flame"}
		h.lau.failures = map[string]error{
			"atlantis": errors.New("boom"),
			"flame":    errors.New("boom"),
		}
	})

	h.fire(wSaver, "watch:saver")
	h.want("launch:failed:atlantis")
	h.want("launch:failed:flame")

	// A third attempt would show up as another trace tag. Prove the lock
	// stage still works after both launches failed.
	h.fire(wLock, "watch:lock")
	if got := h.sess.lockCount(); got != 1 {
		t.Errorf("Lock called %d times after both launches failed, want 1", got)
	}
	if got := len(h.lau.askedFor()); got != 2 {
		t.Errorf("Launch called %d times, want exactly 2 (one retry)", got)
	}
}

// idle-delay is 0 while the daemon runs, so a cycle that cannot start a
// module must still lock and blank. Otherwise the session would be left with
// no auto-lock at all.
func TestLockStillFiresWhenNoModuleIsAvailable(t *testing.T) {
	h := start(t, defaultConfig(), func(h *harness) {
		h.lau.pickErr = errors.New("no modules available")
	})

	// startLaunch traces from inside the saver handler, and handleWatch
	// traces only once the handler has returned, so the inner tag arrives
	// first. That ordering is what makes h.fire's synchronisation meaningful.
	h.mon.fired <- wSaver
	h.want("launch:unavailable")
	h.want("watch:saver")

	h.fire(wLock, "watch:lock")
	if got := h.sess.lockCount(); got != 1 {
		t.Errorf("Lock called %d times, want 1", got)
	}
}

// The race the generation counter exists for: the user comes back while a
// module is still starting. The late saver must be stopped, never adopted.
func TestUserActiveDuringLaunchDiscardsTheLateSaver(t *testing.T) {
	release := make(chan struct{})
	h := start(t, defaultConfig(), func(h *harness) {
		h.lau.release = release
		h.lau.ignoreCancel = true
	})

	h.fire(wSaver, "watch:saver")
	// The launch is now blocked inside fakeLauncher.Launch.

	h.fire(wActive, "watch:active")

	// Let the launch finish, after the reset.
	close(release)
	h.want("launch:discarded")

	if h.lau.saverCount() != 1 {
		t.Fatalf("launcher produced %d savers, want 1", h.lau.saverCount())
	}
	if got := h.lau.saverAt(t, 0).stopCount(); got != 1 {
		t.Errorf("the late saver was stopped %d times, want 1", got)
	}
}

// Same race, but the lock stage rather than user activity wins.
func TestLockDuringLaunchDiscardsTheLateSaver(t *testing.T) {
	release := make(chan struct{})
	h := start(t, defaultConfig(), func(h *harness) {
		h.lau.release = release
		h.lau.ignoreCancel = true
	})

	h.fire(wSaver, "watch:saver")
	h.fire(wLock, "watch:lock")

	close(release)
	h.want("launch:discarded")

	if got := h.lau.saverAt(t, 0).stopCount(); got != 1 {
		t.Errorf("the late saver was stopped %d times, want 1", got)
	}
	if got := h.sess.lockCount(); got != 1 {
		t.Errorf("Lock called %d times, want 1", got)
	}
}

// Stages only advance, so a repeat of the same watch is a no-op.
func TestDuplicateSaverFireLaunchesOnce(t *testing.T) {
	h := start(t, defaultConfig())

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")
	h.fire(wSaver, "watch:saver")

	if got := len(h.lau.askedFor()); got != 1 {
		t.Errorf("Launch called %d times for a duplicate watch, want 1", got)
	}
}

// A watch ID from a previous arming, or one Mutter reused, must be ignored
// rather than dispatched to an arbitrary stage.
func TestUnknownWatchIDIsIgnored(t *testing.T) {
	h := start(t, defaultConfig())

	h.mon.fired <- idle.WatchID(999)

	// Nothing should happen; a real watch afterwards must still work.
	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")
}

func TestCtxCancelStopsTheSaverAndRestoresIdleDelay(t *testing.T) {
	h := start(t, defaultConfig())

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	if err := h.stop(); err != nil {
		t.Errorf("Run() = %v, want nil on ctx cancel", err)
	}
	if got := h.lau.saverAt(t, 0).stopCount(); got == 0 {
		t.Error("shutdown left the module running")
	}
	if got := h.sess.restoreCount(); got != 1 {
		t.Errorf("RestoreIdleDelay called %d times on shutdown, want 1", got)
	}
	if !h.mon.isClosed() {
		t.Error("shutdown did not close the idle monitor")
	}
}

// Losing the bus must be reported, so Restart=always brings the daemon back
// with fresh watches rather than leaving it running and deaf.
func TestClosedFiredChannelIsAnError(t *testing.T) {
	h := start(t, defaultConfig())

	close(h.mon.fired)

	// result, not a bare receive on h.errc: draining the channel behind the
	// harness leaves the cleanup waiting on a value that will never come.
	if err := h.waitErr(); err == nil {
		t.Fatal("Run() = nil when the bus connection dropped, want an error")
	}
	if got := h.sess.restoreCount(); got != 1 {
		t.Errorf("RestoreIdleDelay called %d times, want 1 even on the error path", got)
	}
}

func TestConnectFailureIsReturned(t *testing.T) {
	want := errors.New("no idle monitor")
	d := New(defaultConfig())
	d.connect = func() (idleMonitor, error) { return nil, want }
	d.log = slog.New(slog.NewTextHandler(io.Discard, nil))

	if err := d.Run(context.Background()); !errors.Is(err, want) {
		t.Errorf("Run() = %v, want %v", err, want)
	}
}

// If idle-delay cannot be taken over, gnome-shell will blank over the
// screensaver, so starting up is pointless.
func TestIdleDelayTakeoverFailureAborts(t *testing.T) {
	mon := newFakeMonitor()
	d := New(defaultConfig())
	d.connect = func() (idleMonitor, error) { return mon, nil }
	d.session = &fakeSession{setDelayErr: errors.New("gsettings missing")}
	d.backstop = func() error { return nil }
	d.log = slog.New(slog.NewTextHandler(io.Discard, nil))

	err := d.Run(context.Background())
	if err == nil {
		t.Fatal("Run() = nil when idle-delay could not be set, want an error")
	}
	if !mon.closed {
		t.Error("Run did not close the idle monitor on the error path")
	}
}

func TestShutdownRunsTheBackstop(t *testing.T) {
	var called int
	// Installed through the tweak hook, which runs before Run starts. Writing
	// d.backstop afterwards races the daemon's own goroutine reading it.
	h := start(t, defaultConfig(), func(h *harness) {
		h.d.backstop = func() error { called++; return nil }
	})

	if err := h.stop(); err != nil {
		t.Fatalf("Run() = %v", err)
	}
	if called != 1 {
		t.Errorf("backstop called %d times on shutdown, want 1", called)
	}
}

// ---------------------------------------------------------------- reload

func TestReloadReArmsAtTheNewThresholds(t *testing.T) {
	h := start(t, defaultConfig())

	cfg := config.Config{
		SaverDelay: 120 * time.Second,
		LockAfter:  600 * time.Second,
		BlankAfter: 120 * time.Second,
	}
	h.reload(cfg, "reload:ok")

	// The first three intervals are the original arming; the next three are
	// the reload, and they are cumulative.
	got := h.mon.intervals()
	want := []time.Duration{
		300 * time.Second, 1200 * time.Second, 1320 * time.Second,
		120 * time.Second, 720 * time.Second, 840 * time.Second,
	}
	if !slices.Equal(got, want) {
		t.Errorf("intervals = %v, want %v", got, want)
	}
}

// TestReloadRepointsTheLauncher is the test that matters most. The launcher
// keeps its own copy of include/exclude, taken in New, so a reload that
// updated only Daemon.cfg would change the timings and silently leave module
// selection on the stale lists.
func TestReloadRepointsTheLauncher(t *testing.T) {
	h := start(t, defaultConfig())

	cfg := defaultConfig()
	cfg.Include = []string{"flame", "ifs"}
	cfg.Exclude = []string{"webcollage"}
	h.reload(cfg, "reload:ok")

	include, exclude := h.lau.lastFilters()
	if !slices.Equal(include, []string{"flame", "ifs"}) {
		t.Errorf("include = %v, want [flame ifs]", include)
	}
	if !slices.Equal(exclude, []string{"webcollage"}) {
		t.Errorf("exclude = %v, want [webcollage]", exclude)
	}

	// And it must actually affect selection, not just be recorded. No stage
	// ran before the reload, so no user-active watch was armed and the fresh
	// saver watch is 4, not wSaver2.
	h.fire(idle.WatchID(4), "watch:saver")
	h.want("launch:ok:flame")
	if got := h.lau.askedFor(); !slices.Contains(got, "flame") {
		t.Errorf("launched %v, want the reloaded include list to be used", got)
	}
}

func TestReloadStopsARunningModule(t *testing.T) {
	h := start(t, defaultConfig())

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	cfg := defaultConfig()
	cfg.SaverDelay = 60 * time.Second
	h.reload(cfg, "reload:ok")

	if got := h.lau.saverAt(t, 0).stopCount(); got != 1 {
		t.Errorf("stopCount = %d, want 1: a reload must tear the module down", got)
	}
}

// A reload while blanked must hand idle-delay back to 0, or the display would
// stay off with the daemon believing it is idle.
func TestReloadRestoresIdleDelayWhenBlanked(t *testing.T) {
	h := start(t, defaultConfig())

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")
	h.fire(wLock, "watch:lock")
	h.fire(wBlank, "watch:blank")

	cfg := defaultConfig()
	cfg.SaverDelay = 60 * time.Second
	h.reload(cfg, "reload:ok")

	// 0 at startup, blankIdleDelay at the blank stage, 0 again on reload.
	want := []int{0, blankIdleDelay, 0}
	if got := h.sess.delays(); !slices.Equal(got, want) {
		t.Errorf("idle-delay writes = %v, want %v", got, want)
	}
}

// A typo in the config must never cost the user their screensaver AND their
// auto-lock, so a failed reload keeps the previous config and keeps running.
func TestReloadKeepsThePreviousConfigOnError(t *testing.T) {
	h := start(t, defaultConfig())

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	before := h.mon.intervals()
	h.reloadFailing(errors.New("parsing retrosaver.conf: bad value"))

	if got := h.mon.intervals(); !slices.Equal(got, before) {
		t.Errorf("intervals = %v, want them unchanged at %v", got, before)
	}
	if got := h.lau.saverAt(t, 0).stopCount(); got != 0 {
		t.Errorf("stopCount = %d, want 0: a failed reload must not tear down", got)
	}
	if h.d.cfg.SaverDelay != 300*time.Second {
		t.Errorf("SaverDelay = %v, want the previous 5m0s", h.d.cfg.SaverDelay)
	}
}

// inotify fires on every save, including one that changed nothing. Tearing
// down a running module for that would be visible to the user.
func TestReloadWithAnIdenticalConfigIsANoOp(t *testing.T) {
	h := start(t, defaultConfig())

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	before := h.mon.intervals()
	h.reload(defaultConfig(), "reload:unchanged")

	if got := h.mon.intervals(); !slices.Equal(got, before) {
		t.Errorf("intervals = %v, want them unchanged at %v", got, before)
	}
	if got := h.lau.saverAt(t, 0).stopCount(); got != 0 {
		t.Errorf("stopCount = %d, want 0: an unchanged reload must not tear down", got)
	}
}

// Disabling a stage through a reload must drop that watch entirely.
func TestReloadCanDisableTheLockAndBlankStages(t *testing.T) {
	h := start(t, defaultConfig())

	cfg := defaultConfig()
	cfg.LockAfter = 0
	cfg.BlankAfter = 0
	h.reload(cfg, "reload:ok")

	// Three watches at startup, then only the saver watch.
	want := []time.Duration{
		300 * time.Second, 1200 * time.Second, 1320 * time.Second,
		300 * time.Second,
	}
	if got := h.mon.intervals(); !slices.Equal(got, want) {
		t.Errorf("intervals = %v, want %v", got, want)
	}
}

// setAddErr makes every subsequent AddIdleWatch fail, through the fake's own
// mutex rather than by writing the field while Run is live.
func (f *fakeMonitor) setAddErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addErr = err
}

// waitErr waits for Run to return on its own, without cancelling the context.
func (h *harness) waitErr() error {
	h.t.Helper()
	return h.result()
}

// A re-arm that fails must end Run so systemd's Restart=always produces a
// clean process.
//
// arm() replaces the watch map before it adds anything, so a failure on the
// first AddIdleWatch leaves the machine with zero watches and nothing that
// ever retries: no screensaver and no stage-2 lock for the life of the
// process. Logging the error and carrying on -- which is what this used to do
// -- makes the daemon permanently deaf while still looking healthy.
func TestAFailedReArmEndsRun(t *testing.T) {
	h := start(t, defaultConfig())

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	sentinel := errors.New("mutter went away")
	h.mon.setAddErr(sentinel)
	h.mon.fired <- wActive

	err := h.waitErr()
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run() = %v, want it to wrap %v", err, sentinel)
	}
}

// The same contract on the reload path: a reload that cannot re-arm leaves the
// daemon just as deaf as a failed user-active re-arm does.
func TestAFailedReArmAfterAReloadEndsRun(t *testing.T) {
	h := start(t, defaultConfig())

	sentinel := errors.New("mutter went away")
	h.mon.setAddErr(sentinel)

	cfg := defaultConfig()
	cfg.SaverDelay = 42 * time.Second
	h.cfgMu.Lock()
	h.nextCfg, h.loadErr = cfg, nil
	h.cfgMu.Unlock()
	h.reloadC <- struct{}{}
	h.want("reload:failed")

	if err := h.waitErr(); !errors.Is(err, sentinel) {
		t.Fatalf("Run() = %v, want it to wrap %v", err, sentinel)
	}
}

// With LOCK_AFTER=0 the saver is the only stage, so a user-active watch that
// cannot be armed has no later stage to retry from: the module would stay
// fullscreen over the user's session forever. That must end Run too.
func TestAFailedUserActiveWatchWithNoLockStageEndsRun(t *testing.T) {
	cfg := defaultConfig()
	cfg.LockAfter = 0
	cfg.BlankAfter = 0

	sentinel := errors.New("no more watches for you")
	h := start(t, cfg, func(h *harness) { h.mon.activeErr = sentinel })

	h.mon.fired <- wSaver
	if err := h.waitErr(); !errors.Is(err, sentinel) {
		t.Fatalf("Run() = %v, want it to wrap %v", err, sentinel)
	}
}

// A failed lock must not stop the sequence advancing.
//
// loginctl can fail transiently, and the blank stage is what still gets the
// display off. Treating a lock error as terminal would leave a lit screen for
// the rest of the idle period.
func TestAFailedLockStillAdvancesToBlank(t *testing.T) {
	h := start(t, defaultConfig(), func(h *harness) {
		h.sess.lockErr = errors.New("loginctl said no")
	})

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")
	h.fire(wLock, "watch:lock")

	if got := h.sess.lockCount(); got != 1 {
		t.Fatalf("Lock called %d times, want 1", got)
	}

	// The blank stage must still run despite the lock failing.
	h.fire(wBlank, "watch:blank")
	if got := h.sess.delays(); !slices.Contains(got, blankIdleDelay) {
		t.Errorf("idle-delay writes = %v, want one of %d for the blank stage",
			got, blankIdleDelay)
	}
}

// A failed RestoreIdleDelay must not stop a clean shutdown.
//
// Run still returns nil on SIGTERM: the unit's ExecStopPost runs `retrosaver
// stop`, which restores idle-delay too, and a non-zero exit here would make
// systemd log a failure for something already handled.
func TestShutdownSurvivesAFailedRestore(t *testing.T) {
	h := start(t, defaultConfig(), func(h *harness) {
		h.sess.restoreDelay = errors.New("gsettings went away")
	})

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	if err := h.stop(); err != nil {
		t.Fatalf("Run() = %v, want nil: a failed restore must not fail the shutdown", err)
	}
	if got := h.sess.restoreCount(); got != 1 {
		t.Errorf("RestoreIdleDelay called %d times, want 1", got)
	}
}

// Arming fails at startup: Run must report it AND still hand the idle policy
// back, or the session is left at idle-delay 0 with no auto-lock at all.
func TestAFailedArmAtStartupStillRestoresIdleDelay(t *testing.T) {
	sentinel := errors.New("no watches today")

	h := &harness{
		t:       t,
		mon:     newFakeMonitor(),
		lau:     &fakeLauncher{names: []string{"atlantis"}},
		sess:    &fakeSession{},
		trace:   make(chan string, 64),
		errc:    make(chan error, 1),
		reloadC: make(chan struct{}, 1),
		nextCfg: defaultConfig(),
	}
	h.mon.setAddErr(sentinel)
	h.d = New(defaultConfig())
	h.d.connect = func() (idleMonitor, error) { return h.mon, nil }
	h.d.modules = h.lau
	h.d.session = h.sess
	h.d.backstop = func() error { return nil }
	h.d.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	h.d.traceC = h.trace

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	err := h.d.Run(ctx)

	if !errors.Is(err, sentinel) {
		t.Fatalf("Run() = %v, want it to wrap %v", err, sentinel)
	}
	if got := h.sess.restoreCount(); got != 1 {
		t.Errorf("RestoreIdleDelay called %d times, want 1: the session was left at idle-delay 0", got)
	}
}

// ------------------------------------------------- realLauncher (not a fake)

// fakeModuleTree builds a directory pair that modules.Finder will discover:
// an XML config and a matching executable for each name.
func fakeModuleTree(t *testing.T, names ...string) *modules.Finder {
	t.Helper()
	root := t.TempDir()
	cfgDir := filepath.Join(root, "config")
	binDir := filepath.Join(root, "bin")
	for _, d := range []string{cfgDir, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(cfgDir, n+".xml"), []byte("<screensaver/>"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(binDir, n), []byte("#!/bin/true\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &modules.Finder{ConfigDir: cfgDir, BinDir: binDir}
}

// Pick must honour include, exclude and the per-cycle avoid list.
func TestRealLauncherPickHonoursItsFilters(t *testing.T) {
	f := fakeModuleTree(t, "alpha", "beta", "gamma", "delta")

	l := &realLauncher{finder: f, exclude: []string{"delta"}}
	for range 40 {
		got, err := l.Pick("gamma")
		if err != nil {
			t.Fatalf("Pick() = %v", err)
		}
		if got == "delta" {
			t.Fatal("Pick returned an excluded module")
		}
		if got == "gamma" {
			t.Fatal("Pick returned a module it was told to avoid")
		}
	}

	// include narrows to exactly one candidate, so the result is determinate.
	l.SetFilters([]string{"beta"}, nil)
	got, err := l.Pick()
	if err != nil {
		t.Fatalf("Pick() = %v", err)
	}
	if got != "beta" {
		t.Errorf("Pick() = %q, want \"beta\": include was not applied", got)
	}
}

// SetFilters must re-point what Pick selects from, not just record the values.
// This is the launcher half of the reload contract; TestReloadRepointsTheLauncher
// only proves SetFilters is called.
func TestRealLauncherSetFiltersTakesEffect(t *testing.T) {
	f := fakeModuleTree(t, "alpha", "beta")

	l := &realLauncher{finder: f, include: []string{"alpha"}}
	if got, err := l.Pick(); err != nil || got != "alpha" {
		t.Fatalf("Pick() = %q, %v; want \"alpha\", nil", got, err)
	}

	l.SetFilters(nil, []string{"alpha"})
	if got, err := l.Pick(); err != nil || got != "beta" {
		t.Errorf("Pick() after SetFilters = %q, %v; want \"beta\", nil", got, err)
	}
}

// Pick must not write through its exclude slice into the caller's array.
//
// Pick appends the avoid list to l.exclude. Without the slices.Clone it would
// append in place whenever the slice has spare capacity, scribbling on the
// backing array that config.Config.Exclude still refers to -- and nothing in
// the daemon would fail visibly, which is exactly what makes it worth pinning.
func TestRealLauncherPickDoesNotWriteThroughExclude(t *testing.T) {
	f := fakeModuleTree(t, "alpha", "beta", "gamma")

	// Spare capacity is what makes an in-place append possible at all.
	backing := make([]string, 1, 4)
	backing[0] = "gamma"
	l := &realLauncher{finder: f, exclude: backing}

	if _, err := l.Pick("beta"); err != nil {
		t.Fatalf("Pick() = %v", err)
	}

	// A view of the whole array. Without the clone, "beta" now sits at index 1.
	full := backing[:cap(backing)]
	for i, got := range full[1:] {
		if got != "" {
			t.Errorf("Pick wrote %q into the caller's backing array at index %d",
				got, i+1)
		}
	}
}

// ---------------------------------------------------------------- cycling

func TestCycleSwapsToAnotherModule(t *testing.T) {
	h := start(t, cyclingConfig(), func(h *harness) {
		h.lau.names = []string{"atlantis", "flame"}
		h.lau.honourAvoid = true
	})

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	h.fire(wCycle, "watch:cycle")
	h.want("launch:ok:flame")

	if got, want := h.lau.askedFor(), []string{"atlantis", "flame"}; !slices.Equal(got, want) {
		t.Errorf("modules launched = %v, want %v", got, want)
	}
	// The swap must tell Pick what has already been shown, or it can pick the
	// module that is already on screen.
	if got := h.lau.avoidedOn(1); !slices.Contains(got, "atlantis") {
		t.Errorf("the swap avoided %v, want it to exclude atlantis", got)
	}
	if got := h.lau.saverAt(t, 0).stopCount(); got != 1 {
		t.Errorf("outgoing module stopped %d times, want 1", got)
	}
}

// The outgoing module must survive until its replacement is actually on
// screen. Window discovery blocks for up to five seconds, so stopping it when
// the cycle watch fired would leave the desktop bare for that long.
func TestCycleStopsTheOldModuleOnlyOnceTheNewOneIsUp(t *testing.T) {
	release := make(chan struct{})
	h := start(t, cyclingConfig(), func(h *harness) {
		h.lau.names = []string{"atlantis", "flame"}
		h.lau.honourAvoid = true
	})

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	// Hold the replacement's launch open, the way real window discovery does.
	h.lau.mu.Lock()
	h.lau.release = release
	h.lau.mu.Unlock()

	h.fire(wCycle, "watch:cycle")
	if got := h.lau.saverAt(t, 0).stopCount(); got != 0 {
		t.Fatalf("outgoing module stopped %d times mid-launch, want 0", got)
	}

	close(release)
	h.want("launch:ok:flame")
	if got := h.lau.saverAt(t, 0).stopCount(); got != 1 {
		t.Errorf("outgoing module stopped %d times after the swap landed, want 1", got)
	}
}

func TestCycleWatchesArmOneAtATime(t *testing.T) {
	h := start(t, cyclingConfig(), func(h *harness) {
		h.lau.names = []string{"atlantis", "flame", "ifs"}
		h.lau.honourAvoid = true
	})

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	// arm() registered 300/1200/1320; onSaver added the first swap at 400.
	want := []time.Duration{300, 1200, 1320, 400}
	if got := h.mon.intervals(); !slices.Equal(got, scale(want)) {
		t.Errorf("idle watch thresholds = %v, want %v", got, scale(want))
	}

	h.fire(wCycle, "watch:cycle")
	h.want("launch:ok:flame")

	want = append(want, 500)
	if got := h.mon.intervals(); !slices.Equal(got, scale(want)) {
		t.Errorf("after one swap, thresholds = %v, want %v", got, scale(want))
	}
	// The watch that fired must be retired, or a night of swaps leaks one
	// map entry per CYCLE_AFTER.
	if got := h.mon.removedWatches(); !slices.Contains(got, wCycle) {
		t.Errorf("removed watches = %v, want the fired cycle watch %d among them", got, wCycle)
	}
}

// A swap at or past the lock threshold would either be invisible or flash a
// fresh window up behind the lock screen.
func TestNoCycleWatchAtOrPastTheLockThreshold(t *testing.T) {
	cfg := cyclingConfig()
	cfg.CycleAfter = 900 * time.Second // 300 + 900 == the lock threshold
	h := start(t, cfg)

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	want := []time.Duration{300, 1200, 1320}
	if got := h.mon.intervals(); !slices.Equal(got, scale(want)) {
		t.Errorf("idle watch thresholds = %v, want %v (no cycle watch)", got, scale(want))
	}
}

func TestCycleAfterZeroArmsNoCycleWatch(t *testing.T) {
	h := start(t, defaultConfig()) // CycleAfter is zero there

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	want := []time.Duration{300, 1200, 1320}
	if got := h.mon.intervals(); !slices.Equal(got, scale(want)) {
		t.Errorf("idle watch thresholds = %v, want %v (cycling disabled)", got, scale(want))
	}
}

// With one selectable module a swap would tear down a perfectly good window
// and put an identical one back, every CYCLE_AFTER. It has to be a no-op.
func TestCycleWithOneModuleKeepsItRunning(t *testing.T) {
	h := start(t, cyclingConfig(), func(h *harness) {
		h.lau.names = []string{"atlantis"}
		h.lau.honourAvoid = true
	})

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	// startLaunch traces from inside the cycle handler, so the inner tag
	// arrives before handleWatch's.
	h.mon.fired <- wCycle
	h.want("cycle:skipped")
	h.want("watch:cycle")

	if got := len(h.lau.askedFor()); got != 1 {
		t.Errorf("Launch called %d times, want 1: the module must not be restarted", got)
	}
	if got := h.lau.saverAt(t, 0).stopCount(); got != 0 {
		t.Errorf("the running module was stopped %d times, want it left alone", got)
	}
}

// Pick reports an exhausted pool and a genuinely empty selection identically,
// so running out of unseen modules must not read as "no module available".
func TestCycleStartsTheSetOverWhenEveryModuleHasBeenShown(t *testing.T) {
	h := start(t, cyclingConfig(), func(h *harness) {
		h.lau.names = []string{"atlantis", "flame"}
		h.lau.honourAvoid = true
	})

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")
	h.fire(wCycle, "watch:cycle")
	h.want("launch:ok:flame")

	// Both have now been shown. The set restarts, and the one on screen is
	// still held back so the swap is a real change.
	h.fire(wCycle2, "watch:cycle")
	h.want("launch:ok:atlantis")
}

// The retry-once cap used to read len(m.tried) < 2, where tried doubled as
// the no-repeat pool. Once the pool grew past two entries -- the second swap
// -- that test was permanently false and a broken module was never retried.
func TestRetriesOnASwapAsWellAsTheFirstLaunch(t *testing.T) {
	h := start(t, cyclingConfig(), func(h *harness) {
		h.lau.names = []string{"atlantis", "flame", "ifs"}
		h.lau.honourAvoid = true
		h.lau.failures = map[string]error{"flame": errors.New("no GL context")}
	})

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	h.fire(wCycle, "watch:cycle")
	h.want("launch:failed:flame")
	h.want("launch:ok:ifs")
}

func TestFailedSwapKeepsTheRunningModule(t *testing.T) {
	h := start(t, cyclingConfig(), func(h *harness) {
		h.lau.names = []string{"atlantis", "flame", "ifs"}
		h.lau.honourAvoid = true
		h.lau.failures = map[string]error{
			"flame": errors.New("boom"),
			"ifs":   errors.New("boom"),
		}
	})

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	h.fire(wCycle, "watch:cycle")
	h.want("launch:failed:flame")
	h.want("launch:failed:ifs")

	if got := h.lau.saverAt(t, 0).stopCount(); got != 0 {
		t.Errorf("running module stopped %d times after a failed swap, want it kept", got)
	}
	// And the session must still lock on schedule.
	h.fire(wLock, "watch:lock")
	if got := h.sess.lockCount(); got != 1 {
		t.Errorf("Lock called %d times, want 1", got)
	}
}

// sameConfig enumerates every field by hand, so a new key that is not added
// there makes a reload of it a silent no-op.
func TestReloadOfCycleAfterIsNotSwallowed(t *testing.T) {
	h := start(t, cyclingConfig())

	cfg := cyclingConfig()
	cfg.CycleAfter = 200 * time.Second
	h.reload(cfg, "reload:ok") // "reload:unchanged" means sameConfig missed it
}

// The bug a real session found and the fakes could not: an overdue idle watch
// fires as soon as it is added, so a daemon arming against an already-idle
// session used to walk the whole swap series back-to-back, launching a module
// per missed interval. Observed as six modules in about a second.
func TestCycleSkipsThresholdsAlreadyIdledThrough(t *testing.T) {
	cfg := cyclingConfig() // saver 300, cycle 100, lock at 1200
	h := start(t, cfg, func(h *harness) {
		h.lau.names = []string{"atlantis", "flame", "ifs"}
		h.lau.honourAvoid = true
		// Idle for 640s already: swaps at 400, 500 and 600 have all gone by.
		h.mon.setIdletime(640 * time.Second)
	})

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	// One cycle watch, at the next threshold past 640 -- not one per missed
	// interval, and not 400.
	want := []time.Duration{300, 1200, 1320, 700}
	if got := h.mon.intervals(); !slices.Equal(got, scale(want)) {
		t.Errorf("idle watch thresholds = %v, want %v", got, scale(want))
	}
	if got := len(h.lau.askedFor()); got != 1 {
		t.Errorf("Launch called %d times on a cold start, want exactly 1", got)
	}
}

// The same skip must not arm anything once the lock threshold has gone by too.
func TestNoCycleWatchWhenIdlePastTheLockThreshold(t *testing.T) {
	h := start(t, cyclingConfig(), func(h *harness) {
		h.mon.setIdletime(5000 * time.Second) // far past the lock at 1200
	})

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	want := []time.Duration{300, 1200, 1320}
	if got := h.mon.intervals(); !slices.Equal(got, scale(want)) {
		t.Errorf("idle watch thresholds = %v, want %v (no cycle watch)", got, scale(want))
	}
}

// A failed Idletime read must cost the skip, not the feature.
func TestCycleStillArmsWhenIdletimeReadFails(t *testing.T) {
	h := start(t, cyclingConfig(), func(h *harness) {
		h.mon.idletimeErr = errors.New("dbus: no reply")
	})

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	want := []time.Duration{300, 1200, 1320, 400}
	if got := h.mon.intervals(); !slices.Equal(got, scale(want)) {
		t.Errorf("idle watch thresholds = %v, want %v (fallback to counting)", got, scale(want))
	}
}
