// Package daemon implements the three-stage idle state machine.
//
// Stages, all timed from when the session goes idle. Any user activity at any
// stage tears everything down and re-arms from zero. See docs/spec.md
// sections 3 and 6.3.
//
//	saver   SAVER_DELAY                             launch a random module
//	lock    SAVER_DELAY + LOCK_AFTER                stop the module, lock the session
//	blank   SAVER_DELAY + LOCK_AFTER + BLANK_AFTER  power the display off
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/c-premus/retrosaver/internal/config"
	"github.com/c-premus/retrosaver/internal/idle"
	"github.com/c-premus/retrosaver/internal/modules"
	"github.com/c-premus/retrosaver/internal/session"
	"github.com/c-premus/retrosaver/internal/window"
)

// blankIdleDelay is the idle-delay, in seconds, used to implement the blank
// stage. It sits far below the session's current idle time, so gnome-shell
// blanks on its next tick. retrosaver never touches DPMS itself.
const blankIdleDelay = 10

// The collaborators the state machine needs, named as interfaces so the
// machine can be exercised headlessly. The real packages satisfy them
// structurally and know nothing about these declarations.
type (
	idleMonitor interface {
		AddIdleWatch(d time.Duration) (idle.WatchID, error)
		AddUserActiveWatch() (idle.WatchID, error)
		RemoveWatch(id idle.WatchID) error
		Fired() <-chan idle.WatchID
		Close() error
	}

	// saver is a module that has been launched and can be torn down.
	saver interface{ Stop() error }

	// launcher picks and launches display modules.
	launcher interface {
		Pick(avoid ...string) (string, error)
		Launch(ctx context.Context, name string) (saver, error)
	}

	// controller drives the GNOME session.
	controller interface {
		Lock() error
		SetIdleDelay(seconds int) error
		RestoreIdleDelay() error
	}
)

// Daemon owns the idle watches and the currently running saver.
type Daemon struct {
	cfg config.Config

	// Collaborators. New installs the real implementations; tests in this
	// package overwrite the fields before calling Run. Keeping them as
	// fields rather than adding a second constructor is what lets New and
	// Run keep the signatures cmd/retrosaver already depends on.
	connect  func() (idleMonitor, error)
	modules  launcher
	session  controller
	backstop func() error
	log      *slog.Logger

	// traceC, when non-nil, receives one tag per completed event. Only tests
	// set it; it is how they synchronise without sleeping.
	traceC chan<- string
}

// New returns a Daemon configured from cfg.
func New(cfg config.Config) *Daemon {
	return &Daemon{
		cfg: cfg,
		connect: func() (idleMonitor, error) {
			m, err := idle.Connect()
			if err != nil {
				// Returning idle.Connect() directly would put a typed nil
				// into the interface, making every `mon == nil` check
				// downstream silently false.
				return nil, err
			}
			return m, nil
		},
		modules: &realLauncher{
			finder:  modules.NewFinder(),
			include: cfg.Include,
			exclude: cfg.Exclude,
		},
		session:  sessionController{},
		backstop: window.StopRunning,
		log:      slog.Default(),
	}
}

// realLauncher adapts internal/modules and internal/window to launcher.
type realLauncher struct {
	finder           *modules.Finder
	include, exclude []string
}

// Pick chooses a module, avoiding any already tried this cycle. The "retry
// with a different module" rule falls out of Finder.Pick's existing exclude
// handling rather than needing new selection logic.
func (l *realLauncher) Pick(avoid ...string) (string, error) {
	return l.finder.Pick(l.include, append(slices.Clone(l.exclude), avoid...))
}

func (l *realLauncher) Launch(ctx context.Context, name string) (saver, error) {
	s, err := window.LaunchContext(ctx, l.finder.Path(name))
	if err != nil {
		return nil, err // avoid a typed-nil *window.Saver in the interface
	}
	return s, nil
}

type sessionController struct{}

func (sessionController) Lock() error              { return session.Lock() }
func (sessionController) SetIdleDelay(n int) error { return session.SetIdleDelay(n) }
func (sessionController) RestoreIdleDelay() error  { return session.RestoreIdleDelay() }

func (d *Daemon) trace(tag string) {
	if d.traceC != nil {
		d.traceC <- tag
	}
}

// Run arms the watches and blocks until ctx is cancelled.
//
// On shutdown it must tear down the saver and restore idle-delay, including on
// an unclean exit, which is why the systemd unit also carries ExecStopPost.
func (d *Daemon) Run(ctx context.Context) error {
	mon, err := d.connect()
	if err != nil {
		return err // idle.Connect's message is already actionable
	}
	defer mon.Close()

	// retrosaver owns the entire idle policy while it runs. This doubles as
	// the repair path: a previous run that died after setting idle-delay to
	// blankIdleDelay gets corrected here.
	if err := d.session.SetIdleDelay(0); err != nil {
		return fmt.Errorf("daemon: taking ownership of idle-delay: %w", err)
	}

	m := &machine{d: d, mon: mon, launched: make(chan launchResult, 1)}
	defer m.shutdown()

	if err := m.arm(); err != nil {
		return err
	}
	d.log.Info("armed",
		"saver", d.cfg.SaverDelay,
		"lock", stageDesc(d.cfg.LockEnabled(), d.cfg.SaverDelay+d.cfg.LockAfter),
		"blank", stageDesc(d.cfg.BlankEnabled(), d.cfg.SaverDelay+d.cfg.LockAfter+d.cfg.BlankAfter))
	d.trace("armed")

	fired := mon.Fired()
	for {
		select {
		case <-ctx.Done():
			d.log.Info("shutting down")
			return nil // SIGTERM and SIGINT must exit 0

		case id, ok := <-fired:
			if !ok {
				// The bus connection dropped: gnome-shell restarted, or the
				// session went away. Report it so Restart=always brings us
				// back with fresh watches; ExecStopPost restores idle-delay
				// in the meantime.
				return errors.New(
					"daemon: lost the session bus connection to org.gnome.Mutter.IdleMonitor")
			}
			m.handleWatch(id)

		case r := <-m.launched:
			m.handleLaunch(r)
		}
	}
}

func stageDesc(enabled bool, at time.Duration) string {
	if !enabled {
		return "disabled"
	}
	return at.String()
}

// stage is how far through the idle sequence we have got. Stages only ever
// advance until a reset, which is what makes a duplicate or out-of-order
// WatchFired harmless.
type stage int

const (
	stageIdle stage = iota // nothing running; the reset state
	stageSaver
	stageLock
	stageBlank
)

type watchKind int

const (
	kindSaver watchKind = iota
	kindLock
	kindBlank
	kindActive
)

func (k watchKind) String() string {
	switch k {
	case kindSaver:
		return "saver"
	case kindLock:
		return "lock"
	case kindBlank:
		return "blank"
	case kindActive:
		return "user-active"
	}
	return "unknown"
}

type launchResult struct {
	gen   uint64
	name  string
	saver saver
	err   error
}

// machine holds the state. Every field is touched only by Run's goroutine, so
// there is no mutex anywhere: the generation counter, not locking, is what
// makes the concurrent launch safe.
type machine struct {
	d   *Daemon
	mon idleMonitor

	watches map[idle.WatchID]watchKind
	stage   stage

	gen      uint64
	cancel   context.CancelFunc
	launched chan launchResult

	current saver
	tried   []string
	blanked bool
}

// arm registers the idle watches and the user-active watch.
//
// No cold-start handling is needed: verified against gnome-shell 50.1, an
// idle watch whose threshold has already passed fires as soon as it is added,
// so a daemon starting on an already-idle session catches up by itself.
func (m *machine) arm() error {
	m.watches = make(map[idle.WatchID]watchKind, 4)
	cfg := m.d.cfg

	if err := m.addIdle(cfg.SaverDelay, kindSaver); err != nil {
		return err
	}
	if cfg.LockEnabled() {
		if err := m.addIdle(cfg.SaverDelay+cfg.LockAfter, kindLock); err != nil {
			return err
		}
	}
	// BlankEnabled is already gated on LockAfter > 0 by internal/config:
	// blanking without locking is meaningless.
	if cfg.BlankEnabled() {
		if err := m.addIdle(cfg.SaverDelay+cfg.LockAfter+cfg.BlankAfter, kindBlank); err != nil {
			return err
		}
	}
	return m.armActive()
}

func (m *machine) addIdle(after time.Duration, kind watchKind) error {
	id, err := m.mon.AddIdleWatch(after)
	if err != nil {
		return fmt.Errorf("daemon: arming the %v watch at %v: %w", kind, after, err)
	}
	m.watches[id] = kind
	return nil
}

// armActive registers the one-shot user-active watch. Mutter drops a
// user-active watch as soon as it fires, so it must be re-registered after
// every reset. The idle watches persist and re-arm themselves.
func (m *machine) armActive() error {
	id, err := m.mon.AddUserActiveWatch()
	if err != nil {
		return fmt.Errorf("daemon: arming the user-active watch: %w", err)
	}
	m.watches[id] = kindActive
	return nil
}

func (m *machine) handleWatch(id idle.WatchID) {
	kind, known := m.watches[id]
	if !known {
		// A watch from a previous arming, or an ID Mutter reused. Ignoring
		// unknown IDs is the whole stale-ID defence.
		m.d.log.Debug("ignoring an unknown watch id", "id", id)
		return
	}
	switch kind {
	case kindSaver:
		m.onSaver()
		m.d.trace("watch:saver")
	case kindLock:
		m.onLock()
		m.d.trace("watch:lock")
	case kindBlank:
		m.onBlank()
		m.d.trace("watch:blank")
	case kindActive:
		m.onActive(id)
		m.d.trace("watch:active")
	}
}

func (m *machine) onSaver() {
	if m.stage >= stageSaver {
		return
	}
	m.stage = stageSaver
	m.d.log.Info("idle: starting the screensaver")
	m.startLaunch()
}

func (m *machine) onLock() {
	if m.stage >= stageLock {
		return
	}
	m.stage = stageLock
	m.d.log.Info("idle: locking the session")
	m.stopSaver()
	if err := m.d.session.Lock(); err != nil {
		m.d.log.Error("locking the session", "err", err)
	}
}

func (m *machine) onBlank() {
	if m.stage >= stageBlank {
		return
	}
	m.stage = stageBlank
	m.d.log.Info("idle: blanking the display")
	m.stopSaver()
	if err := m.d.session.SetIdleDelay(blankIdleDelay); err != nil {
		m.d.log.Error("setting idle-delay for the blank stage", "err", err)
		return
	}
	m.blanked = true
}

func (m *machine) onActive(id idle.WatchID) {
	m.d.log.Info("user is active: tearing down and re-arming")
	m.reset()
	if err := m.rearm(); err != nil {
		// Without watches the daemon is deaf, which is worse than dying:
		// systemd restarts us cleanly.
		m.d.log.Error("re-arming the watches", "err", err)
	}
}

// rearm drops every watch and registers a fresh set.
//
// Whether Mutter's idle watches survive a reset and fire again could not be
// settled experimentally: XTEST input through XWayland does not reset Mutter's
// idle clock (verified -- idletime keeps climbing straight through an
// xdotool mousemove), because the idle monitor watches libinput rather than
// synthetic X events. Nothing short of a human touching the hardware drives
// it, so the daemon does not depend on the answer. Re-registering is correct
// under either behaviour and costs four D-Bus calls once per return from idle.
//
// RemoveWatch is safe to call with any ID: Mutter accepts an unknown or
// already-removed watch without complaint, verified against gnome-shell 50.1.
// That is also why a successful RemoveWatch proves nothing about whether a
// fired user-active watch was auto-removed -- so this does not try to infer it.
func (m *machine) rearm() error {
	for id := range m.watches {
		if err := m.mon.RemoveWatch(id); err != nil {
			m.d.log.Debug("removing a watch while re-arming", "id", id, "err", err)
		}
	}
	return m.arm()
}

// reset returns to the idle state, undoing everything the stages did.
func (m *machine) reset() {
	m.gen++ // anything still in flight is now stale
	m.stopSaver()
	if m.blanked {
		if err := m.d.session.SetIdleDelay(0); err != nil {
			m.d.log.Error("restoring idle-delay after blanking", "err", err)
		} else {
			m.blanked = false
		}
	}
	m.stage = stageIdle
	m.tried = nil
}

// startLaunch picks a module and launches it on a worker goroutine.
//
// The launch must not run on the event loop. Window discovery blocks for up
// to five seconds, and a user-active signal arriving behind it would leave a
// module flashing onto the screen of someone who is already back at the
// keyboard. The generation counter makes a late result harmless.
func (m *machine) startLaunch() {
	name, err := m.d.modules.Pick(m.tried...)
	if err != nil {
		// No module could be chosen. Do not abandon the cycle: idle-delay is
		// 0, so returning here would leave the session with no auto-lock at
		// all. The lock and blank stages must still run.
		m.d.log.Error("no module available to launch", "err", err, "tried", m.tried)
		m.d.trace("launch:unavailable")
		return
	}
	m.tried = append(m.tried, name)

	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	gen := m.gen
	go func() {
		s, err := m.d.modules.Launch(ctx, name)
		m.launched <- launchResult{gen: gen, name: name, saver: s, err: err}
	}()
}

func (m *machine) handleLaunch(r launchResult) {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}

	if r.gen != m.gen || m.stage >= stageLock {
		// The user came back, or we locked, while this module was starting.
		// Whatever it produced is unwanted.
		if r.saver != nil {
			_ = r.saver.Stop()
		}
		m.d.trace("launch:discarded")
		return
	}

	if r.err != nil {
		m.d.log.Warn("module failed to start", "module", r.name, "err", r.err)
		if len(m.tried) < 2 {
			m.startLaunch() // retry once, with a module Pick has not tried
		}
		m.d.trace("launch:failed:" + r.name)
		return
	}

	m.current = r.saver
	m.d.log.Info("screensaver running", "module", r.name)
	m.d.trace("launch:ok:" + r.name)
}

// stopSaver cancels any launch in flight and stops any running module.
func (m *machine) stopSaver() {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	if m.current != nil {
		if err := m.current.Stop(); err != nil {
			m.d.log.Error("stopping the module", "err", err)
		}
		m.current = nil
	}
}

// shutdown is the single teardown path, run from Run's defer. It is safe
// after any partial startup and returns nothing: there is nobody left to
// report to, and the process must still exit 0 on SIGTERM.
func (m *machine) shutdown() {
	m.gen++
	m.stopSaver()

	// A launch may have completed between the last select and here.
	select {
	case r := <-m.launched:
		if r.saver != nil {
			_ = r.saver.Stop()
		}
	default:
	}

	// Backstop: kills anything the in-process handle lost track of and
	// clears the runtime state files.
	if m.d.backstop != nil {
		if err := m.d.backstop(); err != nil {
			m.d.log.Error("backstop teardown", "err", err)
		}
	}

	// Give the idle policy back. ExecStopPost does this too; both are
	// idempotent and either alone is sufficient.
	if err := m.d.session.RestoreIdleDelay(); err != nil {
		m.d.log.Error("restoring idle-delay", "err", err)
	}
}
