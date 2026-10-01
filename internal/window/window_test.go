//go:build linux

package window

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

func TestReadPIDs(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		content string
		want    []int
	}{
		{name: "plain", content: "1234\n", want: []int{1234}},
		{name: "no newline", content: "1234", want: []int{1234}},
		{name: "whitespace", content: "  1234  \n", want: []int{1234}},
		{name: "one per monitor", content: "1234\n5678\n", want: []int{1234, 5678}},
		{name: "empty", content: ""},
		{name: "garbage", content: "nope\n"},
		// PID 1 is init and 0 is not a process; refusing them keeps a corrupt
		// file from making stop signal something catastrophic.
		{name: "pid 1 refused", content: "1\n"},
		{name: "pid 0 refused", content: "0\n"},
		{name: "negative refused", content: "-1\n"},
		// One bad line poisons the whole file rather than being skipped: it
		// is not a file this code wrote, so none of it is trusted.
		{name: "bad line refuses all", content: "1234\nnope\n"},
		{name: "blank line refuses all", content: "1234\n\n5678\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(dir, tt.name)
			if err := os.WriteFile(path, []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := readPIDs(path); !slices.Equal(got, tt.want) {
				t.Errorf("readPIDs(%q) = %v, want %v", tt.content, got, tt.want)
			}
		})
	}
}

func TestReadPIDsMissingFile(t *testing.T) {
	if got := readPIDs(filepath.Join(t.TempDir(), "absent")); got != nil {
		t.Errorf("readPIDs on a missing file = %v, want nil", got)
	}
}

func TestModuleArgs(t *testing.T) {
	if got, want := moduleArgs(nil), []string{"-window"}; !slices.Equal(got, want) {
		t.Errorf("moduleArgs(nil) = %q, want %q: a single monitor must launch exactly as before", got, want)
	}
	mon := &monitor{x: 3072, y: 0, width: 3840, height: 2160}
	if got, want := moduleArgs(mon), []string{"-window", "-geometry", "3840x2160+3072+0"}; !slices.Equal(got, want) {
		t.Errorf("moduleArgs(%+v) = %q, want %q", *mon, got, want)
	}
}

func TestDistinctDropsMirroredMonitors(t *testing.T) {
	laptop := monitor{width: 3072, height: 1728}
	external := monitor{x: 3072, width: 3840, height: 2160}
	got := distinct([]monitor{laptop, external, laptop})
	if want := []monitor{laptop, external}; !slices.Equal(got, want) {
		t.Errorf("distinct() = %v, want %v", got, want)
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

	if err := writeState([]int{4321, 4323}, "atlantis", 4322); err != nil {
		t.Fatalf("writeState() = %v", err)
	}
	if got := readTrimmed(t, pidPath()); got != "4321\n4323" {
		t.Errorf("pid file = %q, want one pid per line", got)
	}
	if got := readPIDs(pidPath()); !slices.Equal(got, []int{4321, 4323}) {
		t.Errorf("readPIDs() after writeState = %v, want [4321 4323]", got)
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

	if err := writeState([]int{4321}, "flame", 0); err != nil {
		t.Fatalf("writeState() = %v", err)
	}
	if _, err := os.Stat(unclutterPIDPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unclutter pid file exists with pid 0: %v", err)
	}
}

// reapedSaver returns a Saver with one copy per pid, every one of whose
// processes has already been reaped, so Stop signals nothing and exercises only
// its handling of the state files.
func reapedSaver(pids ...int) *Saver {
	s := &Saver{}
	for _, pid := range pids {
		done := make(chan struct{})
		close(done)
		s.children = append(s.children, &child{cmd: &exec.Cmd{Process: &os.Process{Pid: pid}}, done: done})
	}
	return s
}

// On a swap the replacement writes its state before the daemon stops the
// outgoing module. Stopping the old one must leave the new one's files in
// place, or `retrosaver stop` cannot find the unclutter that is still running.
func TestStopLeavesANewerSaversStateAlone(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	if err := writeState([]int{9001, 9003}, "coral", 9002); err != nil {
		t.Fatalf("writeState() = %v", err)
	}
	if err := reapedSaver(4321, 4323).Stop(); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	for path, want := range map[string]string{
		pidPath():          "9001\n9003",
		modulePath():       "coral",
		unclutterPIDPath(): "9002",
	} {
		if got := readTrimmed(t, path); got != want {
			t.Errorf("%s = %q, want %q", filepath.Base(path), got, want)
		}
	}
}

// The other half of the ownership check: a module stopping while its own state
// is current must still clear it, so a fix that never clears cannot pass.
func TestStopClearsItsOwnState(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	if err := writeState([]int{4321, 4323}, "ifs", 4322); err != nil {
		t.Fatalf("writeState() = %v", err)
	}
	if err := reapedSaver(4321, 4323).Stop(); err != nil {
		t.Fatalf("Stop() = %v", err)
	}
	for _, p := range []string{pidPath(), modulePath(), unclutterPIDPath()} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("os.Stat(%s) = %v, want ErrNotExist", p, err)
		}
	}
}

func TestClearStateForWithNoPIDFile(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	if err := writeFile(modulePath(), "flame"); err != nil {
		t.Fatal(err)
	}
	if err := clearStateFor(4321); err != nil {
		t.Errorf("clearStateFor() with no pid file = %v, want nil", err)
	}
	if got := readTrimmed(t, modulePath()); got != "flame" {
		t.Errorf("module file = %q, want it left alone", got)
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

func TestPIDsListsEveryCopyPrimaryFirst(t *testing.T) {
	s := reapedSaver(4321, 4323)
	if got := s.PIDs(); !slices.Equal(got, []int{4321, 4323}) {
		t.Errorf("PIDs() = %v, want [4321 4323]", got)
	}
	if got := s.Process().Pid; got != 4321 {
		t.Errorf("Process().Pid = %d, want the primary monitor's copy, 4321", got)
	}
	var none *Saver
	if got := none.PIDs(); got != nil {
		t.Errorf("(*Saver)(nil).PIDs() = %v, want nil", got)
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
