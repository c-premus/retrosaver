//go:build linux

// Linux-only in fact, not merely by intent: Setpgid process groups, signalling
// a process group by negative PID, and reading /proc/<pid>/cmdline have no
// portable equivalent. The constraint makes a build on another OS report
// "no Go files" rather than fail with a page of undefined symbols.

// Package window launches an XScreenSaver module on every monitor and makes
// each copy's X11 window fullscreen and always-on-top.
//
// Modules are ordinary X11 clients running under XWayland. Mutter implements
// EWMH for them, which is why wmctrl works. The monitor layout comes from
// XWayland's Xinerama extension, in X coordinates, which is what a module's
// -geometry has to name.
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
	"syscall"
	"time"
)

// ErrNoWindow reports that a module process started but never mapped a window.
// The caller should kill it and try a different module.
var ErrNoWindow = errors.New("window: module mapped no window")

const (
	// windowDeadline bounds the wait for a module's window to appear. A
	// module that has not mapped anything by now is not going to.
	windowDeadline = 5 * time.Second

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

// Saver is a running module -- one process per monitor -- and the
// pointer-hiding process shared by all of them.
//
// Every monitor runs its own copy of the same module, sized to that monitor,
// which is how XScreenSaver itself covers several. Stretching one window across
// all of them with EWMH _NET_WM_FULLSCREEN_MONITORS was tried and does not work
// on a scaled GNOME session: Mutter maps Xinerama indices to its monitors by
// comparing rectangles with no scale conversion, finds no match, and silently
// clears any request naming a monitor other than the first.
type Saver struct {
	module   string
	children []*child

	unclutter *exec.Cmd

	// unclutterDone is closed once unclutter has been reaped. It is nil when
	// no unclutter is running, which reads as "not reaped" and is harmless:
	// pidOf returns 0 for a nil cmd, so nothing is signalled either way.
	unclutterDone chan struct{}

	stopOnce sync.Once
	stopErr  error
}

// child is one module process and the monitor it covers.
type child struct {
	cmd    *exec.Cmd
	module string

	// mon is the monitor this child was placed on, or nil when there is only
	// one to cover and the window manager places the window itself.
	mon *monitor

	// done is closed once cmd has been reaped; waitErr is set before it is
	// closed. It is a closed-channel broadcast rather than a value send
	// because both findWindow and stop wait on it.
	done    chan struct{}
	waitErr error

	stderr *ringBuffer
}

// Launch starts the module at path on every monitor, waits for each window to
// appear, then sets fullscreen and above via EWMH and hides the pointer.
//
// It returns ErrNoWindow when a window does not appear within the timeout, so
// the daemon can retry with a different module rather than failing outright.
func Launch(path string) (*Saver, error) {
	return LaunchContext(context.Background(), path)
}

// LaunchContext is Launch with a cancellable context, so the daemon can
// abandon a launch the moment the user comes back rather than letting a
// module flash onto a screen the user is already looking at.
//
// A launch is all or nothing. If the module fails on any monitor, every copy
// is stopped and the error returned, so the daemon's retry tries a different
// module and a failed swap leaves the outgoing module on screen. Each monitor
// runs the same module, so a failure on one nearly always means a failure on
// all of them anyway.
func LaunchContext(ctx context.Context, path string) (*Saver, error) {
	module := filepath.Base(path)
	env := moduleEnv()

	s := &Saver{module: module}
	for _, mon := range placements(moduleDisplay()) {
		c, err := startChild(path, module, env, mon)
		if err != nil {
			_ = s.Stop()
			return nil, err
		}
		s.children = append(s.children, c)
	}

	// Each child waits up to windowDeadline for its window, so the children
	// are brought up concurrently: serially, three monitors would mean up to
	// fifteen seconds of a half-covered desktop.
	errs := make([]error, len(s.children))
	var wg sync.WaitGroup
	for i, c := range s.children {
		wg.Go(func() { errs[i] = c.show(ctx, env) })
	}
	wg.Wait()

	// A cancelled launch reports the cancellation itself, never a child's
	// ErrNoWindow, for the same reason findWindow does: the daemon must not
	// spend its single retry on a launch it abandoned on purpose.
	if err := ctx.Err(); errors.Is(err, context.Canceled) {
		_ = s.Stop()
		return nil, err
	}
	for _, err := range errs {
		if err != nil {
			_ = s.Stop()
			return nil, err
		}
	}

	// Pointer hiding is cosmetic. A missing or unhappy unclutter must not
	// cost the user a working screensaver. It hides the pointer everywhere,
	// so one serves every monitor.
	s.unclutter, s.unclutterDone = startUnclutter(env)

	if err := writeState(s.PIDs(), module, pidOf(s.unclutter)); err != nil {
		// Non-fatal, but `retrosaver stop` from another shell needs these.
		slog.Warn("writing runtime state", "err", err)
	}
	return s, nil
}

// placements decides where the module's copies go: one per monitor, or a
// single nil placement -- one window the window manager places itself, exactly
// as before multi-monitor support -- when there is only one monitor or the
// layout cannot be read.
func placements(display string) []*monitor {
	mons, err := monitors(display)
	if err != nil {
		// Covering one monitor beats covering none.
		slog.Warn("reading the monitor layout; covering one monitor", "err", err)
	}
	if len(mons) < 2 {
		return []*monitor{nil}
	}
	out := make([]*monitor, len(mons))
	for i := range mons {
		out[i] = &mons[i]
	}
	return out
}

// moduleArgs is the command line for a module placed on mon, or left to the
// window manager when mon is nil.
//
// -window: "Draw on a newly-created window. This is the default." There is no
// usable root window under XWayland, so -root is not an option. -geometry is
// Xt's standard option, which every module accepts.
func moduleArgs(mon *monitor) []string {
	if mon == nil {
		return []string{"-window"}
	}
	return []string{"-window", "-geometry", mon.geometry()}
}

// startChild starts one copy of the module at path, placed on mon.
func startChild(path, module string, env []string, mon *monitor) (*child, error) {
	cmd := exec.Command(path, moduleArgs(mon)...)
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
		module: module,
		mon:    mon,
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

// show waits for the child's window, then makes it fullscreen and above.
func (c *child) show(ctx context.Context, env []string) error {
	id, err := c.findWindow(ctx, env)
	if err != nil {
		return err
	}
	return c.fullscreen(ctx, env, id)
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

// findWindow waits for the module to map a window and returns its ID.
//
// xdotool's --sync blocks until a match appears, so there is no poll loop and
// no race against a window that maps between two polls. The surrounding select
// supplies what --sync lacks: a deadline, cancellation, and an early exit when
// the module dies before mapping anything.
func (c *child) findWindow(ctx context.Context, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, windowDeadline)
	defer cancel()

	type result struct {
		id  string
		err error
	}
	res := make(chan result, 1)

	go func() {
		pid := strconv.Itoa(c.cmd.Process.Pid)
		cmd := exec.CommandContext(ctx, "xdotool", "search", "--sync", "--onlyvisible", "--pid", pid)
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			res <- result{err: err}
			return
		}
		id, err := firstWindowID(string(out))
		res <- result{id: id, err: err}
	}()

	select {
	case r := <-res:
		// Cancelling ctx kills xdotool, so r.err may be nothing but the
		// cancellation surfacing as a signal. Both this case and ctx.Done()
		// are ready then and select picks between them at random, so report
		// the cancellation explicitly: otherwise an abandoned launch looks
		// like a broken module about half the time, and the daemon spends its
		// single retry on it.
		if err := ctx.Err(); errors.Is(err, context.Canceled) {
			return "", err
		}
		if r.err != nil {
			return "", fmt.Errorf("%w: %s: xdotool: %v", ErrNoWindow, c.module, r.err)
		}
		return r.id, nil

	case <-c.done:
		// The module exited before mapping a window: a missing GL context, a
		// bad option, an absent data file. Report the tail of its stderr so
		// the journal says something useful instead of just "no window".
		return "", fmt.Errorf("%w: %s exited first (%v): %s",
			ErrNoWindow, c.module, c.waitErr, c.stderr.String())

	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.Canceled) {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("%w: %s mapped nothing within %v", ErrNoWindow, c.module, windowDeadline)
	}
}

// firstWindowID picks a window ID out of xdotool's output.
//
// xdotool prints one decimal ID per line. A module normally maps exactly one
// window; when it maps more, the first is the one that appeared first and is
// the one wmctrl should act on.
func firstWindowID(out string) (string, error) {
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n, err := strconv.ParseUint(line, 10, 64)
		if err != nil {
			return "", fmt.Errorf("unparseable window id %q: %w", line, err)
		}
		// wmctrl -i parses with strtoul base 0, so hand it the unambiguous
		// 0x form rather than relying on decimal being read as decimal.
		return fmt.Sprintf("0x%08x", n), nil
	}
	return "", errors.New("xdotool printed no window id")
}

// fullscreen asks the window manager to make the window cover its monitor and
// sit above everything, including the top bar.
//
// Mutter fullscreens a window on the monitor holding the centre of the window's
// requested rectangle, so a child started with its monitor's -geometry lands
// on that monitor. The top bar hides while the primary monitor has any
// fullscreen window, and a fullscreen window keeps its place when focus moves
// to another monitor's, so every child stays up.
func (c *child) fullscreen(ctx context.Context, env []string, id string) error {
	cmd := exec.CommandContext(ctx, "wmctrl", "-i", "-r", id, "-b", "add,fullscreen,above")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("window: wmctrl fullscreen/above on %s (%s): %w: %s",
			id, c.module, err, strings.TrimSpace(string(out)))
	}
	return nil
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

		var first int
		if pids := s.PIDs(); len(pids) > 0 {
			first = pids[0]
		}
		s.stopErr = clearStateFor(first)
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
// primary monitor first.
func writeState(pids []int, module string, unclutterPID int) error {
	lines := make([]string, len(pids))
	for i, p := range pids {
		lines[i] = strconv.Itoa(p)
	}
	var errs []error
	errs = append(errs, writeFile(pidPath(), strings.Join(lines, "\n")))
	errs = append(errs, writeFile(modulePath(), module))
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
	return strings.TrimSpace(string(b)), true
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
