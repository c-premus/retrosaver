package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/c-premus/retrosaver/internal/config"
	"github.com/c-premus/retrosaver/internal/idle"
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

	// activeErr makes AddUserActiveWatch fail, which is a different path from
	// addErr: the user-active watch is armed per stage, not by arm().
	activeErr error
}

func newFakeMonitor() *fakeMonitor {
	return &fakeMonitor{fired: make(chan idle.WatchID, 8)}
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
	asked        []string
	avoided      [][]string
	savers       []*fakeSaver
	pickErr      error
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
}

func defaultConfig() config.Config {
	return config.Config{
		SaverDelay: 300 * time.Second,
		LockAfter:  900 * time.Second,
		BlankAfter: 120 * time.Second,
	}
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
	for _, fn := range tweak {
		fn(h)
	}

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

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.errc <- h.d.Run(ctx) }()

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

// stop cancels Run and returns its error.
func (h *harness) stop() error {
	h.t.Helper()
	h.cancel()
	select {
	case err := <-h.errc:
		return err
	case <-time.After(traceTimeout):
		h.t.Fatal("Run did not return after ctx cancel")
		return nil
	}
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
)

// ---------------------------------------------------------------- tests

func TestArmsAllFourWatchesAtTheRightThresholds(t *testing.T) {
	h := start(t, defaultConfig())
	defer h.stop() //nolint:errcheck // asserted elsewhere

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
	defer h.stop() //nolint:errcheck

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
	defer h.stop() //nolint:errcheck

	if got := h.mon.intervals(); len(got) != 1 {
		t.Errorf("idle watch thresholds = %v, want only the saver watch", got)
	}
}

func TestBlankAfterZeroArmsSaverAndLockOnly(t *testing.T) {
	cfg := defaultConfig()
	cfg.BlankAfter = 0
	h := start(t, cfg)
	defer h.stop() //nolint:errcheck

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
	if got := h.lau.savers[0].stopCount(); got != 1 {
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
	defer h.stop() //nolint:errcheck

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	h.fire(wLock, "watch:lock")

	if h.lau.savers[0].stopCount() == 0 {
		t.Error("the lock stage locked without stopping the module")
	}
}

func TestUserActiveTearsDownAndReArms(t *testing.T) {
	h := start(t, defaultConfig())
	defer h.stop() //nolint:errcheck

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	h.fire(wActive, "watch:active")

	if got := h.lau.savers[0].stopCount(); got != 1 {
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
	defer h.stop() //nolint:errcheck

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
	defer h.stop() //nolint:errcheck

	h.fire(wLock, "watch:lock")

	if got := h.mon.activeWatches(); len(got) != 1 {
		t.Errorf("user-active watches = %v, want one armed by the lock stage", got)
	}
}

// After a reset the machine must run a whole second cycle.
func TestSecondCycleAfterReset(t *testing.T) {
	h := start(t, defaultConfig())
	defer h.stop() //nolint:errcheck

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
	defer h.stop() //nolint:errcheck

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
	defer h.stop() //nolint:errcheck

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
	defer h.stop() //nolint:errcheck

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
	defer h.stop() //nolint:errcheck

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
	defer h.stop() //nolint:errcheck

	h.fire(wSaver, "watch:saver")
	// The launch is now blocked inside fakeLauncher.Launch.

	h.fire(wActive, "watch:active")

	// Let the launch finish, after the reset.
	close(release)
	h.want("launch:discarded")

	if len(h.lau.savers) != 1 {
		t.Fatalf("launcher produced %d savers, want 1", len(h.lau.savers))
	}
	if got := h.lau.savers[0].stopCount(); got != 1 {
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
	defer h.stop() //nolint:errcheck

	h.fire(wSaver, "watch:saver")
	h.fire(wLock, "watch:lock")

	close(release)
	h.want("launch:discarded")

	if got := h.lau.savers[0].stopCount(); got != 1 {
		t.Errorf("the late saver was stopped %d times, want 1", got)
	}
	if got := h.sess.lockCount(); got != 1 {
		t.Errorf("Lock called %d times, want 1", got)
	}
}

// Stages only advance, so a repeat of the same watch is a no-op.
func TestDuplicateSaverFireLaunchesOnce(t *testing.T) {
	h := start(t, defaultConfig())
	defer h.stop() //nolint:errcheck

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
	defer h.stop() //nolint:errcheck

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
	if got := h.lau.savers[0].stopCount(); got == 0 {
		t.Error("shutdown left the module running")
	}
	if got := h.sess.restoreCount(); got != 1 {
		t.Errorf("RestoreIdleDelay called %d times on shutdown, want 1", got)
	}
	if !h.mon.closed {
		t.Error("shutdown did not close the idle monitor")
	}
}

// Losing the bus must be reported, so Restart=always brings the daemon back
// with fresh watches rather than leaving it running and deaf.
func TestClosedFiredChannelIsAnError(t *testing.T) {
	h := start(t, defaultConfig())

	close(h.mon.fired)

	select {
	case err := <-h.errc:
		if err == nil {
			t.Fatal("Run() = nil when the bus connection dropped, want an error")
		}
	case <-time.After(traceTimeout):
		t.Fatal("Run did not return after the Fired channel closed")
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
	h := start(t, defaultConfig())
	h.d.backstop = func() error { called++; return nil }

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
	defer h.stop() //nolint:errcheck

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
	defer h.stop() //nolint:errcheck

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
	defer h.stop() //nolint:errcheck

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	cfg := defaultConfig()
	cfg.SaverDelay = 60 * time.Second
	h.reload(cfg, "reload:ok")

	if got := h.lau.savers[0].stopCount(); got != 1 {
		t.Errorf("stopCount = %d, want 1: a reload must tear the module down", got)
	}
}

// A reload while blanked must hand idle-delay back to 0, or the display would
// stay off with the daemon believing it is idle.
func TestReloadRestoresIdleDelayWhenBlanked(t *testing.T) {
	h := start(t, defaultConfig())
	defer h.stop() //nolint:errcheck

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
	defer h.stop() //nolint:errcheck

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	before := h.mon.intervals()
	h.reloadFailing(errors.New("parsing retrosaver.conf: bad value"))

	if got := h.mon.intervals(); !slices.Equal(got, before) {
		t.Errorf("intervals = %v, want them unchanged at %v", got, before)
	}
	if got := h.lau.savers[0].stopCount(); got != 0 {
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
	defer h.stop() //nolint:errcheck

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	before := h.mon.intervals()
	h.reload(defaultConfig(), "reload:unchanged")

	if got := h.mon.intervals(); !slices.Equal(got, before) {
		t.Errorf("intervals = %v, want them unchanged at %v", got, before)
	}
	if got := h.lau.savers[0].stopCount(); got != 0 {
		t.Errorf("stopCount = %d, want 0: an unchanged reload must not tear down", got)
	}
}

// Disabling a stage through a reload must drop that watch entirely.
func TestReloadCanDisableTheLockAndBlankStages(t *testing.T) {
	h := start(t, defaultConfig())
	defer h.stop() //nolint:errcheck

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
	select {
	case err := <-h.errc:
		return err
	case <-time.After(traceTimeout):
		h.t.Fatal("Run did not return")
		return nil
	}
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
