//go:build linux

package window

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestFirstWindowID(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		// xdotool prints one decimal ID per line. wmctrl -i parses with
		// strtoul base 0, so the 0x form is handed over unambiguously.
		{name: "single id", in: "56623111\n", want: "0x03600007"},
		{name: "no trailing newline", in: "56623111", want: "0x03600007"},
		{name: "several ids takes the first", in: "56623111\n56623112\n", want: "0x03600007"},
		{name: "blank lines skipped", in: "\n\n56623111\n", want: "0x03600007"},
		{name: "surrounding whitespace", in: "  56623111  \n", want: "0x03600007"},
		{name: "empty", in: "", wantErr: true},
		{name: "whitespace only", in: "   \n\n", wantErr: true},
		{name: "not a number", in: "banana\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := firstWindowID(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("firstWindowID(%q) = %q, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("firstWindowID(%q) = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("firstWindowID(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRingBufferKeepsTheTail(t *testing.T) {
	r := newRingBuffer(8)
	if _, err := r.Write([]byte("0123456789abcdef")); err != nil {
		t.Fatal(err)
	}
	if got, want := r.String(), "89abcdef"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestRingBufferShortInputIsKeptWhole(t *testing.T) {
	r := newRingBuffer(64)
	if _, err := r.Write([]byte("  boom  \n")); err != nil {
		t.Fatal(err)
	}
	if got, want := r.String(), "boom"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestRingBufferAcrossWrites(t *testing.T) {
	r := newRingBuffer(4)
	for _, chunk := range []string{"aa", "bb", "cc"} {
		if _, err := r.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := r.String(), "bbcc"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestReadPID(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		content string
		want    int
		wantOK  bool
	}{
		{name: "plain", content: "1234\n", want: 1234, wantOK: true},
		{name: "no newline", content: "1234", want: 1234, wantOK: true},
		{name: "whitespace", content: "  1234  \n", want: 1234, wantOK: true},
		{name: "empty", content: "", wantOK: false},
		{name: "garbage", content: "nope\n", wantOK: false},
		// PID 1 is init and 0 is not a process; refusing them keeps a corrupt
		// file from making stop signal something catastrophic.
		{name: "pid 1 refused", content: "1\n", wantOK: false},
		{name: "pid 0 refused", content: "0\n", wantOK: false},
		{name: "negative refused", content: "-1\n", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, tt.name)
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, ok := readPID(path)
			if ok != tt.wantOK {
				t.Fatalf("readPID(%q) ok = %v, want %v", tt.content, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("readPID(%q) = %d, want %d", tt.content, got, tt.want)
			}
		})
	}
}

func TestReadPIDMissingFile(t *testing.T) {
	if _, ok := readPID(filepath.Join(t.TempDir(), "absent")); ok {
		t.Error("readPID on a missing file reported ok")
	}
}

// The /proc guard is what stops a stale PID file from killing a stranger
// after PID reuse.
func TestProcessMatchesRejectsAForeignProcess(t *testing.T) {
	self := os.Getpid()

	if processMatches(self, moduleBinDir) {
		t.Errorf("processMatches(self, %q) = true; the test binary is not a module",
			moduleBinDir)
	}
	// Sanity check the other direction, so a always-false bug cannot pass.
	if !processMatches(self, filepath.Base(os.Args[0])) {
		t.Errorf("processMatches(self, %q) = false, want true", filepath.Base(os.Args[0]))
	}
}

func TestProcessMatchesOnADeadPID(t *testing.T) {
	// A PID that will not exist: one past the max allowed.
	b, err := os.ReadFile("/proc/sys/kernel/pid_max")
	if err != nil {
		t.Skip("no /proc/sys/kernel/pid_max on this system")
	}
	max, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Skipf("unparseable pid_max: %v", err)
	}
	if processMatches(max+1, "anything") {
		t.Error("processMatches on a nonexistent pid = true, want false")
	}
}

func TestRuntimePathsHonourXDGRuntimeDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	for _, tc := range []struct {
		got  string
		want string
	}{
		{pidPath(), filepath.Join(dir, "retrosaver.pid")},
		{modulePath(), filepath.Join(dir, "retrosaver.module")},
		{unclutterPIDPath(), filepath.Join(dir, "retrosaver.unclutter.pid")},
	} {
		if tc.got != tc.want {
			t.Errorf("path = %q, want %q", tc.got, tc.want)
		}
	}
}

func TestClearStateIsANoOpWhenNothingExists(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if err := clearState(); err != nil {
		t.Errorf("clearState() with no files = %v, want nil", err)
	}
}

func TestWriteAndClearState(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	if err := writeState(4321, "atlantis", 4322); err != nil {
		t.Fatalf("writeState() = %v", err)
	}
	if got := readTrimmed(t, pidPath()); got != "4321" {
		t.Errorf("pid file = %q, want \"4321\"", got)
	}
	if got := readTrimmed(t, modulePath()); got != "atlantis" {
		t.Errorf("module file = %q, want \"atlantis\"", got)
	}
	if got := readTrimmed(t, unclutterPIDPath()); got != "4322" {
		t.Errorf("unclutter pid file = %q, want \"4322\"", got)
	}

	if err := clearState(); err != nil {
		t.Fatalf("clearState() = %v", err)
	}
	for _, p := range []string{pidPath(), modulePath(), unclutterPIDPath()} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("os.Stat(%s) = %v, want ErrNotExist", p, err)
		}
	}
}

// writeState must not record an unclutter PID when unclutter never started.
func TestWriteStateSkipsAbsentUnclutter(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	if err := writeState(4321, "flame", 0); err != nil {
		t.Fatalf("writeState() = %v", err)
	}
	if _, err := os.Stat(unclutterPIDPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unclutter pid file exists with pid 0: %v", err)
	}
}

// fakePkill replaces the process-wide backstop for the duration of a test and
// returns a pointer to its call count.
//
// StopRunning must never run the real pkill from a unit test: it matches by
// executable prefix across the whole system, so on a developer's own desktop
// `go test ./...` would kill the module the live daemon is showing.
// t.Setenv("XDG_RUNTIME_DIR", ...) isolates the state files and nothing else.
func fakePkill(t *testing.T, err error) *int {
	t.Helper()
	calls := 0
	prev := pkill
	pkill = func() error {
		calls++
		return err
	}
	t.Cleanup(func() { pkill = prev })
	return &calls
}

// tests/smoke.sh asserts that stop exits 0 when nothing is running, so this
// is the unit-level version of that contract.
func TestStopRunningIsACleanNoOp(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	calls := fakePkill(t, nil)

	if err := StopRunning(); err != nil {
		t.Errorf("StopRunning() with nothing running = %v, want nil", err)
	}
	if *calls != 1 {
		t.Errorf("pkill backstop called %d times, want 1", *calls)
	}
}

// A stale PID file pointing at a live but unrelated process must be ignored,
// not acted on. The test binary itself is that unrelated process.
func TestStopRunningIgnoresAStalePIDFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	fakePkill(t, nil)

	if err := os.WriteFile(pidPath(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := StopRunning(); err != nil {
		t.Fatalf("StopRunning() = %v", err)
	}
	// Reaching here at all means the test process was not signalled.
}

// A real pkill exits 1 for "no processes matched", which is the normal case
// and must not surface as an error.
//
// The prefix is deliberately one that cannot match anything, so this exercises
// the real binary and the real exit code without firing a system-wide pkill at
// the module directory -- which is the whole reason the seam above exists.
func TestPkillTreatsNoMatchAsSuccess(t *testing.T) {
	if err := pkillPrefix("/nonexistent/retrosaver-test-no-such-dir/"); err != nil {
		t.Errorf("pkillPrefix() with nothing matching = %v, want nil", err)
	}
}

// A backstop that genuinely fails must be reported, not swallowed.
func TestStopRunningReportsAFailingBackstop(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	sentinel := errors.New("pkill exploded")
	fakePkill(t, sentinel)

	if err := StopRunning(); !errors.Is(err, sentinel) {
		t.Errorf("StopRunning() = %v, want it to wrap %v", err, sentinel)
	}
}

func TestRunningModuleReportsNothingWhenIdle(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if name, ok := RunningModule(); ok {
		t.Errorf("RunningModule() = %q, true; want no module", name)
	}
}

func TestProcessReturnsNilForAZeroSaver(t *testing.T) {
	var s *Saver
	if p := s.Process(); p != nil {
		t.Errorf("(*Saver)(nil).Process() = %v, want nil", p)
	}
	if p := (&Saver{}).Process(); p != nil {
		t.Errorf("(&Saver{}).Process() = %v, want nil", p)
	}
}

func readTrimmed(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.TrimSpace(string(b))
}
