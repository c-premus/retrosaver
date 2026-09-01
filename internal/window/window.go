// Package window launches an XScreenSaver module and makes its X11 window
// fullscreen and always-on-top.
//
// Modules are ordinary X11 clients running under XWayland. Mutter implements
// EWMH for them, which is why wmctrl works. See docs/spec.md section 6.2.
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

// Saver is a running module and its associated pointer-hiding process.
type Saver struct {
	cmd       *exec.Cmd
	unclutter *exec.Cmd
	module    string
	winID     string

	// done is closed once cmd has been reaped; waitErr is set before it is
	// closed. It is a closed-channel broadcast rather than a value send
	// because both findWindow and Stop wait on it.
	done    chan struct{}
	waitErr error

	stderr *ringBuffer

	stopOnce sync.Once
	stopErr  error
}

// Launch starts the module at path, waits for its window to appear, then sets
// fullscreen and above via EWMH and hides the pointer.
//
// It returns ErrNoWindow when no window appears within the timeout, so the
// daemon can retry with a different module rather than failing outright.
func Launch(path string) (*Saver, error) {
	return LaunchContext(context.Background(), path)
}

// LaunchContext is Launch with a cancellable context, so the daemon can
// abandon a launch the moment the user comes back rather than letting a
// module flash onto a screen the user is already looking at.
func LaunchContext(ctx context.Context, path string) (*Saver, error) {
	module := filepath.Base(path)
	env := moduleEnv()

	// -window: "Draw on a newly-created window. This is the default." There
	// is no usable root window under XWayland, so -root is not an option.
	cmd := exec.Command(path, "-window")
	cmd.Env = env
	// Own process group: a module may fork helpers, and Stop must be able to
	// take the whole tree rather than orphaning children onto the screen.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	s := &Saver{
		cmd:    cmd,
		module: module,
		done:   make(chan struct{}),
		stderr: newRingBuffer(stderrTail),
	}
	cmd.Stderr = s.stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("window: starting %s: %w", path, err)
	}
	go func() {
		s.waitErr = cmd.Wait()
		close(s.done)
	}()

	id, err := s.findWindow(ctx, env)
	if err != nil {
		_ = s.Stop()
		return nil, err
	}
	s.winID = id

	if err := s.fullscreen(ctx, env, id); err != nil {
		_ = s.Stop()
		return nil, err
	}

	// Pointer hiding is cosmetic. A missing or unhappy unclutter must not
	// cost the user a working screensaver.
	s.unclutter = startUnclutter(env)

	if err := writeState(cmd.Process.Pid, module, pidOf(s.unclutter)); err != nil {
		// Non-fatal, but `retrosaver stop` from another shell needs these.
		slog.Warn("writing runtime state", "err", err)
	}
	return s, nil
}

// moduleEnv returns the environment for a module, defaulting DISPLAY.
//
// The systemd user unit inherits DISPLAY from the session, but a manual
// `retrosaver run` from a bare shell may not have it.
func moduleEnv() []string {
	display := os.Getenv("DISPLAY")
	if display == "" {
		display = ":0"
	}
	return append(os.Environ(), "DISPLAY="+display)
}

// findWindow waits for the module to map a window and returns its ID.
//
// xdotool's --sync blocks until a match appears, so there is no poll loop and
// no race against a window that maps between two polls. The surrounding select
// supplies what --sync lacks: a deadline, cancellation, and an early exit when
// the module dies before mapping anything.
func (s *Saver) findWindow(ctx context.Context, env []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, windowDeadline)
	defer cancel()

	type result struct {
		id  string
		err error
	}
	res := make(chan result, 1)

	go func() {
		pid := strconv.Itoa(s.cmd.Process.Pid)
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
		if r.err != nil {
			return "", fmt.Errorf("%w: %s: xdotool: %v", ErrNoWindow, s.module, r.err)
		}
		return r.id, nil

	case <-s.done:
		// The module exited before mapping a window: a missing GL context, a
		// bad option, an absent data file. Report the tail of its stderr so
		// the journal says something useful instead of just "no window".
		return "", fmt.Errorf("%w: %s exited first (%v): %s",
			ErrNoWindow, s.module, s.waitErr, s.stderr.String())

	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.Canceled) {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("%w: %s mapped nothing within %v", ErrNoWindow, s.module, windowDeadline)
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

// fullscreen asks the window manager to make the window cover the screen and
// sit above everything, including the top bar.
func (s *Saver) fullscreen(ctx context.Context, env []string, id string) error {
	cmd := exec.CommandContext(ctx, "wmctrl", "-i", "-r", id, "-b", "add,fullscreen,above")
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("window: wmctrl fullscreen/above on %s (%s): %w: %s",
			id, s.module, err, strings.TrimSpace(string(out)))
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
func startUnclutter(env []string) *exec.Cmd {
	var bin string
	for _, name := range []string{"unclutter-xfixes", "unclutter"} {
		if p, err := exec.LookPath(name); err == nil {
			bin = p
			break
		}
	}
	if bin == "" {
		slog.Warn("no unclutter binary found; the pointer will stay visible")
		return nil
	}

	cmd := exec.Command(bin, "--timeout", "0", "--jitter", "0")
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		slog.Warn("starting unclutter", "bin", bin, "err", err)
		return nil
	}
	// Reap it so a short-lived failure does not become a zombie.
	go func() { _ = cmd.Wait() }()
	return cmd
}

// Process returns the module's OS process, so the daemon can hold it directly
// instead of round-tripping through the PID file.
func (s *Saver) Process() *os.Process {
	if s == nil || s.cmd == nil {
		return nil
	}
	return s.cmd.Process
}

// Stop terminates the module and the pointer-hiding process.
//
// It is idempotent: the daemon may call it from a teardown that races the
// launch that created it, and `retrosaver stop` may already have done the job
// from another shell.
func (s *Saver) Stop() error {
	s.stopOnce.Do(func() {
		var errs []error

		// unclutter first. It hides the pointer globally while it runs, so
		// outliving the module would be a visible bug.
		if p := pidOf(s.unclutter); p > 0 {
			_ = syscall.Kill(-p, syscall.SIGTERM)
		}

		if p := s.Process(); p != nil {
			// A negative PID signals the whole process group created with
			// Setpgid, so forked helpers go too.
			_ = syscall.Kill(-p.Pid, syscall.SIGTERM)
			select {
			case <-s.done:
			case <-time.After(stopGrace):
				_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
				<-s.done
			}
		}

		errs = append(errs, clearState())
		s.stopErr = errors.Join(errs...)
	})
	return s.stopErr
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
	if pid, ok := readPID(pidPath()); ok && processMatches(pid, moduleBinDir) {
		terminate(pid)
	}
	if pid, ok := readPID(unclutterPIDPath()); ok && processMatches(pid, "unclutter") {
		terminate(pid)
	}

	// Backstop for anything the state files lost track of.
	if err := pkillModules(); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, clearState())
	return errors.Join(errs...)
}

// pkillModules kills any straggling module by executable prefix.
func pkillModules() error {
	cmd := exec.Command("pkill", "-f", "^"+moduleBinDir)
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

func readPID(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		return 0, false
	}
	return pid, true
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
// shell and after a daemon crash.
func writeState(pid int, module string, unclutterPID int) error {
	var errs []error
	errs = append(errs, writeFile(pidPath(), strconv.Itoa(pid)))
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
	pid, ok := readPID(pidPath())
	if !ok || !processMatches(pid, moduleBinDir) {
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
