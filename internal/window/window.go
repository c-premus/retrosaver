//go:build linux

// Linux-only in fact, not merely by intent: Setpgid process groups, signalling
// a process group by negative PID, and reading /proc/<pid>/cmdline have no
// portable equivalent. The constraint makes a build on another OS report
// "no Go files" rather than fail with a page of undefined symbols.

// Package window puts an XScreenSaver module on every monitor: one copy per
// monitor, each drawing in a fullscreen, always-on-top X11 window that
// retrosaver creates itself.
//
// Modules are ordinary X11 clients running under XWayland, and so are these
// windows. Mutter implements EWMH for them, which is what makes fullscreen and
// above work. The monitor layout comes from XWayland's Xinerama extension, in
// the X coordinates the windows are created at.
package window

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// ErrNoWindow reports that a saver could not be put on screen: the window
// manager never managed its window, or the module exited as soon as it started.
// The caller should try a different module.
var ErrNoWindow = errors.New("window: module mapped no window")

const (
	// windowDeadline bounds the wait for the window manager to put a saver
	// window up. One it has not managed by now is not going to be.
	windowDeadline = 5 * time.Second

	// startupGrace is how long a freshly started module has to fail before
	// the launch counts as a success. A module that cannot run exits well
	// within it.
	startupGrace = 500 * time.Millisecond

	// stopGrace is how long a module gets to exit on SIGTERM before SIGKILL.
	stopGrace = 2 * time.Second

	// stderrTail is how much of a failing module's stderr to keep, so the
	// journal says why it died rather than just that it did.
	stderrTail = 2048

	// moduleBinDir is the prefix every genuine module executable lives under.
	// StopRunning checks it before signalling a PID read off disk.
	moduleBinDir = "/usr/libexec/xscreensaver/"
)

// pkill is the process-wide module backstop, indirected so tests can replace
// it. Calling the real one from a unit test kills the module belonging to the
// live daemon on a developer's own desktop -- t.Setenv isolates the state files
// but nothing isolates a system-wide pkill.
var pkill = pkillModules

// Saver is a running saver -- one module process per monitor -- and the
// pointer-hiding process shared by all of them.
//
// Every monitor runs its own module process, sized to that monitor, which is
// how XScreenSaver itself covers several. Usually it is the same module on
// each; with MONITORS=different each monitor has its own. Stretching one
// window across all of them with EWMH _NET_WM_FULLSCREEN_MONITORS was tried
// and does not work on a scaled GNOME session: Mutter maps Xinerama indices to
// its monitors by comparing rectangles with no scale conversion, finds no
// match, and silently clears any request naming a monitor other than the
// first.
type Saver struct {
	// children holds one module process per monitor, primary first. Replace
	// swaps an entry; nothing else changes it after LaunchContext.
	children []*child

	// screen holds the windows the copies draw in. It is nil only on a Saver
	// built by a test.
	screen *screen

	// done is closed when any copy of the module exits. See Done.
	done     chan struct{}
	doneOnce sync.Once

	// exits reports each module process that exits, one event per process.
	// stopping is closed by Stop, so a saver being torn down reports nothing
	// more and no reporting goroutine is left blocked on a send. Both are
	// created by watchChildren.
	exits    chan Exit
	stopping chan struct{}

	unclutter *exec.Cmd

	// unclutterDone is closed once unclutter has been reaped. It is nil when
	// no unclutter is running, which reads as "not reaped" and is harmless:
	// pidOf returns 0 for a nil cmd, so nothing is signalled either way.
	unclutterDone chan struct{}

	stopOnce sync.Once
	stopErr  error
}

// child is one module process and the window it draws in.
type child struct {
	cmd    *exec.Cmd
	module string
	// monitor is the index of the window it draws in, primary first.
	monitor int
	// retired is set when Replace stops this process on purpose, so its exit
	// is not reported as a death.
	retired atomic.Bool

	// done is closed once cmd has been reaped; waitErr is set before it is
	// closed. It is a closed-channel broadcast rather than a value send
	// because Launch, Done and stop all wait on it.
	done    chan struct{}
	waitErr error

	stderr *ringBuffer
}

// Exit reports a module process that exited while its saver was running.
type Exit struct {
	// Monitor is the index of the window it drew in, primary first: the index
	// Replace takes.
	Monitor int
	Module  string
	PID     int
	Err     error
	// Stderr is the tail of what it wrote to stderr, which usually says why.
	Stderr string
}

// Launch starts the module at path on every monitor, each copy drawing in a
// fullscreen, always-on-top window retrosaver has already put over its
// monitor, and hides the pointer.
//
// It returns ErrNoWindow when no saver window could be put up, or when a copy
// of the module exits straight away, so the daemon can retry with a different
// module rather than failing outright.
func Launch(path string) (*Saver, error) {
	return LaunchContext(context.Background(), path)
}

// LaunchContext is Launch with a cancellable context, so the daemon can
// abandon a launch the moment the user comes back rather than letting a
// module flash onto a screen the user is already looking at.
//
// Monitor i runs paths[i % len(paths)]: one path puts the same module on every
// monitor, and one path per monitor gives each its own.
//
// A launch is all or nothing. If a module fails on any monitor, every copy
// is stopped and the error returned, so the daemon's retry tries a different
// module and a failed swap leaves the outgoing module on screen. When each
// monitor runs the same module, a failure on one nearly always means a failure
// on all of them anyway.
func LaunchContext(ctx context.Context, paths ...string) (*Saver, error) {
	return launch(ctx, paths, (*Saver).awaitStartup)
}

// LaunchEach is LaunchContext for a launch that is not all or nothing, which
// is what the daemon uses when each monitor picks its own module.
//
// It fails only when every monitor's module exits within the startup grace.
// If some survive, it succeeds, and each module that failed is reported on
// Exits straight away, so the caller can put something else in its window
// with Replace while the other monitors keep what they have.
func LaunchEach(ctx context.Context, paths ...string) (*Saver, error) {
	return launch(ctx, paths, (*Saver).awaitAny)
}

func launch(ctx context.Context, paths []string, await func(*Saver, context.Context) error) (*Saver, error) {
	if len(paths) == 0 {
		return nil, errors.New("window: no module to launch")
	}
	env := moduleEnv()
	names := make([]string, len(paths))
	for i, p := range paths {
		names[i] = filepath.Base(p)
	}

	scr, err := openScreen(ctx, moduleDisplay(), names)
	if err != nil {
		return nil, err
	}
	s := &Saver{screen: scr, done: make(chan struct{})}
	for i, w := range scr.windows {
		c, err := startChild(paths[i%len(paths)], env, uint32(w))
		if err != nil {
			_ = s.Stop()
			return nil, err
		}
		c.monitor = i
		s.children = append(s.children, c)
	}
	s.watchChildren()

	if err := await(s, ctx); err != nil {
		_ = s.Stop()
		return nil, err
	}

	// Pointer hiding is cosmetic. A missing or unhappy unclutter must not
	// cost the user a working screensaver. It hides the pointer everywhere,
	// so one serves every monitor. The saver windows' own blank cursor
	// already covers them; unclutter is belt and braces.
	s.unclutter, s.unclutterDone = startUnclutter(env)

	if err := writeState(s.PIDs(), s.Modules(), pidOf(s.unclutter)); err != nil {
		// Non-fatal, but `retrosaver stop` from another shell needs these.
		slog.Warn("writing runtime state", "err", err)
	}
	return s, nil
}

// awaitStartup gives every copy startupGrace to fail. A module that cannot run
// -- no GL context, a bad option, a missing data file -- exits within moments
// of starting, and reporting that here, with its stderr, is what lets the
// daemon spend its retry on a different module instead of leaving a black
// window up.
func (s *Saver) awaitStartup(ctx context.Context) error {
	timer := time.NewTimer(startupGrace)
	defer timer.Stop()
	select {
	case <-s.done:
		for _, c := range s.children {
			if c.reaped() {
				return fmt.Errorf("%w: %s exited on startup (%v): %s",
					ErrNoWindow, c.module, c.waitErr, c.stderr.String())
			}
		}
		return fmt.Errorf("%w: %s exited on startup", ErrNoWindow, strings.Join(s.Modules(), ", "))
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// awaitAny is awaitStartup for LaunchEach: the launch fails only if every
// module exits within the grace. The ones that exited are already on their way
// to Exits.
func (s *Saver) awaitAny(ctx context.Context) error {
	all, quit := make(chan struct{}), make(chan struct{})
	defer close(quit)
	go func() {
		for _, c := range s.children {
			select {
			case <-c.done:
			case <-quit:
				return // the grace is over; nothing is waiting any more
			}
		}
		close(all)
	}()
	timer := time.NewTimer(startupGrace)
	defer timer.Stop()
	select {
	case <-all:
		var errs []string
		for _, c := range s.children {
			errs = append(errs, fmt.Sprintf("%s (%v): %s", c.module, c.waitErr, c.stderr.String()))
		}
		return fmt.Errorf("%w: every module exited on startup: %s", ErrNoWindow, strings.Join(errs, "; "))
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// watchChildren starts watching every module process: see watch.
func (s *Saver) watchChildren() {
	if s.exits == nil {
		s.exits = make(chan Exit)
	}
	if s.stopping == nil {
		s.stopping = make(chan struct{})
	}
	for _, c := range s.children {
		s.watch(c)
	}
}

// watch closes s.done once c exits, and reports the exit on s.exits unless the
// saver is being stopped or c was retired by Replace.
func (s *Saver) watch(c *child) {
	go func() {
		<-c.done
		s.doneOnce.Do(func() { close(s.done) })
		// Stop closes stopping before it signals anything, so a process it
		// stopped is caught here. The select below cannot be relied on for
		// that: with a reader waiting, both of its cases are ready.
		if c.retired.Load() || closed(s.stopping) {
			return
		}
		e := Exit{Monitor: c.monitor, Module: c.module, PID: pidOf(c.cmd), Err: c.waitErr}
		if c.stderr != nil {
			e.Stderr = c.stderr.String()
		}
		select {
		case s.exits <- e:
		case <-s.stopping:
		}
	}()
}

// Exits reports each module process that exits while the saver runs, with the
// monitor it was on. Unlike Done it fires once per process, including for a
// process Replace started, so a caller can keep each monitor going separately.
// A process Stop stops is never reported.
//
// A caller that never reads it loses nothing: the reports are dropped when the
// saver is stopped.
func (s *Saver) Exits() <-chan Exit { return s.exits }

// Replace puts the module at path on monitor i in place of what runs there,
// leaving every other monitor alone. The window stays: the new module adopts
// the same fullscreen window with -window-id, as XScreenSaver does when it
// cycles, so nothing has to be mapped or fullscreened again.
//
// The previous process is stopped first if it is still running, and is not
// reported on Exits. The new one is not given a startup grace here: if it
// cannot run, it simply exits and is reported on Exits like any other.
//
// Replace must not race Stop. The daemon calls both from one goroutine.
func (s *Saver) Replace(i int, path string) error {
	if i < 0 || i >= len(s.children) {
		return fmt.Errorf("window: no monitor %d to replace a module on", i)
	}
	if closed(s.stopping) {
		return errors.New("window: replacing a module on a saver that is stopping")
	}
	old := s.children[i]
	first := firstPID(s.PIDs())
	old.retired.Store(true)
	old.stop()

	var w uint32
	if s.screen != nil {
		w = uint32(s.screen.windows[i])
	}
	c, err := startChild(path, moduleEnv(), w)
	if err != nil {
		// Leave the retired process in place; the slot is empty either way.
		return err
	}
	c.monitor = i
	s.children[i] = c
	s.watch(c)

	// Rewrite the runtime state while it still names this saver, so
	// `retrosaver stop` finds the new process.
	if pids := readPIDs(pidPath()); len(pids) > 0 && pids[0] == first {
		if err := writeState(s.PIDs(), s.Modules(), pidOf(s.unclutter)); err != nil {
			slog.Warn("writing runtime state", "err", err)
		}
	}
	return nil
}

// Modules names the module on each monitor, primary first.
func (s *Saver) Modules() []string {
	if s == nil {
		return nil
	}
	names := make([]string, len(s.children))
	for i, c := range s.children {
		names[i] = c.module
	}
	return names
}

func firstPID(pids []int) int {
	if len(pids) == 0 {
		return 0
	}
	return pids[0]
}

// Done is closed as soon as any copy of the module exits, for whatever reason:
// Stop, `retrosaver stop` from another shell, or the module quitting by itself.
// `retrosaver run` waits on it.
func (s *Saver) Done() <-chan struct{} { return s.done }

// windowIDArgs is the command line that makes a module draw in window w.
//
// -window-id: "Draw on the specified window." The module adopts the window as
// it is, size included, which is the whole point: it starts at its final size.
func windowIDArgs(w uint32) []string {
	return []string{"-window-id", fmt.Sprintf("0x%x", w)}
}

// startChild starts the module at path, drawing in window w.
func startChild(path string, env []string, w uint32) (*child, error) {
	cmd := exec.Command(path, windowIDArgs(w)...)
	cmd.Env = env
	// Own process group: a module may fork helpers, and stop must be able to
	// take the whole tree rather than orphaning children onto the screen.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Because cmd.Stderr is a ringBuffer rather than an *os.File, os/exec
	// interposes a pipe and Wait will not return until every writer closes it.
	// A forked helper that outlives the module holds that pipe open and would
	// block Wait indefinitely. WaitDelay caps how long Wait waits on the pipe
	// after the process itself has exited.
	cmd.WaitDelay = stopGrace

	c := &child{
		cmd:    cmd,
		module: filepath.Base(path),
		done:   make(chan struct{}),
		stderr: newRingBuffer(stderrTail),
	}
	cmd.Stderr = c.stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("window: starting %s: %w", path, err)
	}
	go func() {
		c.waitErr = cmd.Wait()
		close(c.done)
	}()
	return c, nil
}

// moduleEnv returns the environment for a module, defaulting DISPLAY.
//
// The systemd user unit inherits DISPLAY from the session, but a manual
// `retrosaver run` from a bare shell may not have it.
func moduleEnv() []string {
	return append(os.Environ(), "DISPLAY="+moduleDisplay())
}

// moduleDisplay is the X display modules run on: DISPLAY, or :0 without it.
func moduleDisplay() string {
	if display := os.Getenv("DISPLAY"); display != "" {
		return display
	}
	return ":0"
}

// startUnclutter hides the pointer over the saver window, returning nil when
// it cannot be started. The caller treats that as acceptable.
//
// The binary is looked up under both names on purpose: the unclutter-xfixes
// package installs /usr/bin/unclutter-xfixes and declares no
// Provides: unclutter, so the spec's literal "unclutter" is not present on a
// correctly dependency-satisfied install.
// The returned channel is closed once the process has been reaped, so Stop can
// tell a live PID from a recycled one.
func startUnclutter(env []string) (*exec.Cmd, chan struct{}) {
	var bin string
	for _, name := range []string{"unclutter-xfixes", "unclutter"} {
		if p, err := exec.LookPath(name); err == nil {
			bin = p
			break
		}
	}
	if bin == "" {
		slog.Warn("no unclutter binary found; the pointer will stay visible")
		return nil, nil
	}

	cmd := exec.Command(bin, "--timeout", "0", "--jitter", "0")
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		slog.Warn("starting unclutter", "bin", bin, "err", err)
		return nil, nil
	}
	// Reap it so a short-lived failure does not become a zombie.
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	return cmd, done
}

// Process returns the OS process of the module's first copy -- the one on the
// primary monitor -- so the daemon can hold it directly instead of
// round-tripping through the PID file.
func (s *Saver) Process() *os.Process {
	if s == nil || len(s.children) == 0 || s.children[0].cmd == nil {
		return nil
	}
	return s.children[0].cmd.Process
}

// PIDs returns the process ID of every copy of the module, primary monitor
// first.
func (s *Saver) PIDs() []int {
	if s == nil {
		return nil
	}
	pids := make([]int, 0, len(s.children))
	for _, c := range s.children {
		if p := pidOf(c.cmd); p > 0 {
			pids = append(pids, p)
		}
	}
	return pids
}

// Stop terminates every copy of the module and the pointer-hiding process.
//
// It removes the runtime state files only while they still name this saver,
// so stopping an outgoing module never erases the state of its replacement.
//
// It is idempotent: the daemon may call it from a teardown that races the
// launch that created it, and `retrosaver stop` may already have done the job
// from another shell.
func (s *Saver) Stop() error {
	s.stopOnce.Do(func() {
		// Before anything is signalled, so the modules this stops are not
		// reported as having died.
		if s.stopping != nil {
			close(s.stopping)
		}

		// unclutter first. It hides the pointer globally while it runs, so
		// outliving the module would be a visible bug. Skip it once reaped,
		// for the same PID-recycling reason as the module below.
		if p := pidOf(s.unclutter); p > 0 && !closed(s.unclutterDone) {
			_ = syscall.Kill(-p, syscall.SIGTERM)
		}

		// In parallel: each copy can take two grace periods to go, and the
		// daemon's teardown waits on all of them.
		var wg sync.WaitGroup
		for _, c := range s.children {
			wg.Go(c.stop)
		}
		wg.Wait()

		// Only once every module is gone, so none of them is still drawing
		// into a window that has just been destroyed.
		s.screen.close()

		s.stopErr = clearStateFor(firstPID(s.PIDs()))
	})
	return s.stopErr
}

// stop terminates one copy of the module and waits, boundedly, for it to go.
func (c *child) stop() {
	// Never signal a PID belonging to an already-reaped process. Once the
	// reaper goroutine has run, the kernel may have handed that number to
	// something unrelated, and -pid would then hit whatever process group now
	// holds it. This is the same hazard processMatches guards for PIDs read off
	// disk, reached by a different route: LaunchContext calls Stop on exactly
	// the path where a module has just been reaped.
	p := pidOf(c.cmd)
	if p == 0 || c.reaped() {
		return
	}
	// A negative PID signals the whole process group created with Setpgid, so
	// forked helpers go too.
	_ = syscall.Kill(-p, syscall.SIGTERM)
	select {
	case <-c.done:
	case <-time.After(stopGrace):
		_ = syscall.Kill(-p, syscall.SIGKILL)
		// Bounded, not a bare receive. cmd.Stderr is a ringBuffer, so os/exec
		// interposes a pipe and Wait blocks until every writer closes it -- a
		// grandchild that escaped the process group would wedge Wait, therefore
		// Stop, therefore the daemon's whole teardown. cmd.WaitDelay makes the
		// runtime give up on the pipe, and this select bounds the wait
		// regardless.
		select {
		case <-c.done:
		case <-time.After(stopGrace):
		}
	}
}

// reaped reports whether the module process has already been waited on.
func (c *child) reaped() bool { return closed(c.done) }

// closed reports whether ch has been closed. A nil channel reports false:
// there was no process, so there is nothing that could have been reaped.
func closed(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func pidOf(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	return cmd.Process.Pid
}

// StopRunning tears down whatever the runtime state files describe.
//
// This is the out-of-process path: `retrosaver stop` from another shell, and
// the daemon's own backstop after a crash lost the in-process handle. It
// treats "nothing was running" as success, because stop is the panic button
// and must be safe to run at any time -- tests/smoke.sh asserts exactly that.
func StopRunning() error {
	var errs []error

	// The PID file can be minutes stale after a crash and PIDs get recycled,
	// so every PID read off disk is checked against /proc before it is
	// signalled. Without this, `retrosaver stop` can kill a stranger.
	var wg sync.WaitGroup
	for _, pid := range readPIDs(pidPath()) {
		if processMatches(pid, moduleBinDir) {
			wg.Go(func() { terminate(pid) })
		}
	}
	for _, pid := range readPIDs(unclutterPIDPath()) {
		if processMatches(pid, "unclutter") {
			wg.Go(func() { terminate(pid) })
		}
	}
	wg.Wait()

	// Backstop for anything the state files lost track of.
	if err := pkill(); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, clearState())
	return errors.Join(errs...)
}

// pkillModules kills any straggling module by executable prefix.
func pkillModules() error { return pkillPrefix(moduleBinDir) }

// pkillPrefix is pkillModules with the match prefix supplied, so a test can
// exercise the exit-code handling against a prefix that matches nothing real
// instead of firing a system-wide pkill at the module directory.
func pkillPrefix(prefix string) error {
	cmd := exec.Command("pkill", "-f", "^"+prefix)
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		// pkill exits 1 for "no processes matched", which is the normal case
		// and emphatically not a failure.
		return nil
	}
	if err != nil && !errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("window: pkill backstop: %w", err)
	}
	return nil
}

// terminate sends SIGTERM to a process group, then SIGKILL if it lingers.
func terminate(pid int) {
	if syscall.Kill(-pid, syscall.SIGTERM) != nil {
		// Not a group leader; fall back to the single process.
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(stopGrace)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return // gone
		}
		time.Sleep(50 * time.Millisecond)
	}
	if syscall.Kill(-pid, syscall.SIGKILL) != nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// processMatches reports whether pid's executable contains want, which is how
// a recycled PID is told apart from the one that was written down.
func processMatches(pid int, want string) bool {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	// /proc cmdline is NUL-separated; the first entry is the executable.
	argv0, _, _ := strings.Cut(string(b), "\x00")
	return strings.Contains(argv0, want)
}

// readPIDs reads a PID file holding one PID per line. A file from before
// multi-monitor support holds exactly one, which reads the same way.
//
// Any line that is not a plausible PID invalidates the whole file rather than
// being skipped: a file that is not what this code wrote is not a list of
// processes to signal.
func readPIDs(path string) []int {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pids []int
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || pid <= 1 {
			return nil
		}
		pids = append(pids, pid)
	}
	return pids
}

// runtimeDir is where the PID and module files live.
func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
}

func pidPath() string          { return filepath.Join(runtimeDir(), "retrosaver.pid") }
func modulePath() string       { return filepath.Join(runtimeDir(), "retrosaver.module") }
func unclutterPIDPath() string { return filepath.Join(runtimeDir(), "retrosaver.unclutter.pid") }

// writeState records what is running, so `retrosaver stop` works from another
// shell and after a daemon crash. The PID file holds one module PID per line,
// primary monitor first, and the module file one name per line in the same
// order.
func writeState(pids []int, modules []string, unclutterPID int) error {
	lines := make([]string, len(pids))
	for i, p := range pids {
		lines[i] = strconv.Itoa(p)
	}
	var errs []error
	errs = append(errs, writeFile(pidPath(), strings.Join(lines, "\n")))
	errs = append(errs, writeFile(modulePath(), strings.Join(modules, "\n")))
	if unclutterPID > 0 {
		errs = append(errs, writeFile(unclutterPIDPath(), strconv.Itoa(unclutterPID)))
	}
	return errors.Join(errs...)
}

func writeFile(path, content string) error {
	if err := os.WriteFile(path, []byte(content+"\n"), 0o644); err != nil {
		return fmt.Errorf("window: writing %s: %w", path, err)
	}
	return nil
}

// clearStateFor removes the runtime files only while they still describe the
// saver whose first module PID is pid.
//
// On a swap the daemon stops the outgoing module after its replacement has
// written its own state, and a replacement that fails to start is stopped while
// the old module is still on screen. An unconditional clear on either path
// erases the state of the module that is actually running, leaving
// `retrosaver stop` unable to find its unclutter and `retrosaver run` blind to
// it.
func clearStateFor(pid int) error {
	if got := readPIDs(pidPath()); len(got) == 0 || got[0] != pid {
		return nil
	}
	return clearState()
}

// clearState removes the runtime files. Missing files are not an error.
func clearState() error {
	var errs []error
	for _, p := range []string{pidPath(), modulePath(), unclutterPIDPath()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("window: removing %s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}

// RunningModule reports the module named in the runtime state, if any. It is
// how `retrosaver run` refuses to start a second saver over a running one.
// When monitors run different modules, it names them all, primary first.
func RunningModule() (string, bool) {
	if !slices.ContainsFunc(readPIDs(pidPath()), func(pid int) bool {
		return processMatches(pid, moduleBinDir)
	}) {
		return "", false
	}
	b, err := os.ReadFile(modulePath())
	if err != nil {
		return "", true // running, but we cannot name it
	}
	return strings.Join(strings.Fields(string(b)), ", "), true
}

// ringBuffer keeps the last n bytes written to it, so a failing module's
// stderr can be quoted without buffering unbounded output from a chatty one.
type ringBuffer struct {
	mu  sync.Mutex
	buf []byte
	n   int
}

func newRingBuffer(n int) *ringBuffer { return &ringBuffer{n: n} }

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.n {
		r.buf = r.buf[len(r.buf)-r.n:]
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.TrimSpace(string(r.buf))
}
