package session

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// call records one invocation of the run hook.
type call struct {
	name string
	args []string
}

// fakeRun replaces the package's run variable for the duration of a test.
// stdout is keyed by the first argument after the command name, which is
// enough to distinguish `gsettings get` from `gsettings set` and
// `loginctl show-user` from `loginctl lock-session`.
type fakeRun struct {
	stdout map[string]string
	err    map[string]error
	calls  []call
}

func (f *fakeRun) install(t *testing.T) {
	t.Helper()
	prev := run
	run = func(name string, args ...string) (string, error) {
		f.calls = append(f.calls, call{name: name, args: args})
		k := runKey(name, args)
		if err, ok := f.err[k]; ok {
			return "", err
		}
		return f.stdout[k], nil
	}
	t.Cleanup(func() { run = prev })
}

func runKey(name string, args []string) string {
	if len(args) == 0 {
		return name
	}
	return name + " " + args[0]
}

func (f *fakeRun) lastArgs(t *testing.T, name string) []string {
	t.Helper()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if f.calls[i].name == name {
			return f.calls[i].args
		}
	}
	t.Fatalf("no call to %q was made; calls were %+v", name, f.calls)
	return nil
}

// configHome points os.UserConfigDir at a temporary directory.
func configHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return dir
}

func TestParseIdleDelay(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    int
		wantErr bool
	}{
		// What GNOME 50 actually prints: a GVariant carrying its type.
		{name: "typed uint32", in: "uint32 300\n", want: 300},
		{name: "bare integer", in: "300\n", want: 300},
		{name: "zero", in: "uint32 0\n", want: 0},
		{name: "no trailing newline", in: "uint32 42", want: 42},
		{name: "surrounding whitespace", in: "  uint32 7  \n", want: 7},
		{name: "empty", in: "", wantErr: true},
		{name: "whitespace only", in: "  \n", wantErr: true},
		{name: "type with no value", in: "uint32 \n", wantErr: true},
		{name: "not a number", in: "uint32 banana\n", wantErr: true},
		{name: "negative", in: "-5\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseIdleDelay(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseIdleDelay(%q) = %d, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseIdleDelay(%q) = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("parseIdleDelay(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestIdleDelayReadsGsettings(t *testing.T) {
	f := &fakeRun{stdout: map[string]string{"gsettings get": "uint32 300\n"}}
	f.install(t)

	got, err := IdleDelay()
	if err != nil {
		t.Fatalf("IdleDelay() = %v", err)
	}
	if got != 300 {
		t.Errorf("IdleDelay() = %d, want 300", got)
	}
	want := []string{"get", schema, key}
	if args := f.lastArgs(t, "gsettings"); !slices.Equal(args, want) {
		t.Errorf("gsettings args = %v, want %v", args, want)
	}
}

func TestSetIdleDelayWritesGsettings(t *testing.T) {
	f := &fakeRun{}
	f.install(t)

	if err := SetIdleDelay(0); err != nil {
		t.Fatalf("SetIdleDelay(0) = %v", err)
	}
	want := []string{"set", schema, key, "0"}
	if args := f.lastArgs(t, "gsettings"); !slices.Equal(args, want) {
		t.Errorf("gsettings args = %v, want %v", args, want)
	}
}

func TestSetIdleDelayRejectsNegative(t *testing.T) {
	f := &fakeRun{}
	f.install(t)

	if err := SetIdleDelay(-1); err == nil {
		t.Fatal("SetIdleDelay(-1) = nil, want an error")
	}
	if len(f.calls) != 0 {
		t.Errorf("SetIdleDelay(-1) still shelled out: %+v", f.calls)
	}
}

func TestSavedIdleDelayPathHonoursXDGConfigHome(t *testing.T) {
	dir := configHome(t)

	got, err := SavedIdleDelayPath()
	if err != nil {
		t.Fatalf("SavedIdleDelayPath() = %v", err)
	}
	want := filepath.Join(dir, "retrosaver", "idle-delay.orig")
	if got != want {
		t.Errorf("SavedIdleDelayPath() = %q, want %q", got, want)
	}
}

// The whole point of SaveIdleDelay's existence check: setup run twice, while
// the daemon holds idle-delay at 0, must not record 0 as "the original" and
// destroy the user's auto-lock.
func TestSaveIdleDelayNeverOverwrites(t *testing.T) {
	configHome(t)
	f := &fakeRun{stdout: map[string]string{"gsettings get": "uint32 300\n"}}
	f.install(t)

	if err := SaveIdleDelay(); err != nil {
		t.Fatalf("first SaveIdleDelay() = %v", err)
	}

	path, err := SavedIdleDelayPath()
	if err != nil {
		t.Fatal(err)
	}
	if got := readTrimmed(t, path); got != "300" {
		t.Fatalf("saved value = %q, want \"300\"", got)
	}

	// Second run, with the daemon now holding idle-delay at 0.
	f.stdout["gsettings get"] = "uint32 0\n"
	if err := SaveIdleDelay(); err != nil {
		t.Fatalf("second SaveIdleDelay() = %v", err)
	}
	if got := readTrimmed(t, path); got != "300" {
		t.Errorf("saved value after a second setup = %q, want it still to be \"300\"", got)
	}
}

func TestRestoreIdleDelayUsesSavedValue(t *testing.T) {
	configHome(t)
	f := &fakeRun{stdout: map[string]string{"gsettings get": "uint32 1800\n"}}
	f.install(t)

	if err := SaveIdleDelay(); err != nil {
		t.Fatalf("SaveIdleDelay() = %v", err)
	}
	if err := RestoreIdleDelay(); err != nil {
		t.Fatalf("RestoreIdleDelay() = %v", err)
	}
	want := []string{"set", schema, key, "1800"}
	if args := f.lastArgs(t, "gsettings"); !slices.Equal(args, want) {
		t.Errorf("gsettings args = %v, want %v", args, want)
	}
}

// An unclean state must still leave a working auto-lock, so a missing or
// corrupt saved value falls back to GNOME's own default rather than failing.
func TestRestoreIdleDelayFallsBackToDefault(t *testing.T) {
	tests := []struct {
		name    string
		content *string
	}{
		{name: "file missing", content: nil},
		{name: "empty file", content: ptr("")},
		{name: "garbage", content: ptr("not-a-number\n")},
		{name: "negative", content: ptr("-1\n")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configHome(t)
			f := &fakeRun{}
			f.install(t)

			path, err := SavedIdleDelayPath()
			if err != nil {
				t.Fatal(err)
			}
			if tt.content != nil {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(*tt.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			if err := RestoreIdleDelay(); err != nil {
				t.Fatalf("RestoreIdleDelay() = %v", err)
			}
			want := []string{"set", schema, key, "300"}
			if args := f.lastArgs(t, "gsettings"); !slices.Equal(args, want) {
				t.Errorf("gsettings args = %v, want %v", args, want)
			}
		})
	}
}

func TestClearSavedIdleDelayIsANoOpWhenMissing(t *testing.T) {
	configHome(t)
	if err := ClearSavedIdleDelay(); err != nil {
		t.Fatalf("ClearSavedIdleDelay() with no file = %v, want nil", err)
	}
}

func TestClearSavedIdleDelayRemovesTheFile(t *testing.T) {
	configHome(t)
	f := &fakeRun{stdout: map[string]string{"gsettings get": "uint32 300\n"}}
	f.install(t)

	if err := SaveIdleDelay(); err != nil {
		t.Fatal(err)
	}
	if err := ClearSavedIdleDelay(); err != nil {
		t.Fatalf("ClearSavedIdleDelay() = %v", err)
	}
	path, err := SavedIdleDelayPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("os.Stat(%s) = %v, want ErrNotExist", path, err)
	}
}

func TestLockNamesTheSessionExplicitly(t *testing.T) {
	t.Setenv("XDG_SESSION_ID", "")
	f := &fakeRun{stdout: map[string]string{"loginctl show-user": "2\n"}}
	f.install(t)

	if err := Lock(); err != nil {
		t.Fatalf("Lock() = %v", err)
	}
	want := []string{"lock-session", "2"}
	if args := f.lastArgs(t, "loginctl"); !slices.Equal(args, want) {
		t.Errorf("loginctl args = %v, want %v", args, want)
	}
}

func TestLockPrefersXDGSessionID(t *testing.T) {
	t.Setenv("XDG_SESSION_ID", "7")
	f := &fakeRun{}
	f.install(t)

	if err := Lock(); err != nil {
		t.Fatalf("Lock() = %v", err)
	}
	want := []string{"lock-session", "7"}
	if args := f.lastArgs(t, "loginctl"); !slices.Equal(args, want) {
		t.Errorf("loginctl args = %v, want %v", args, want)
	}
	for _, c := range f.calls {
		if len(c.args) > 0 && c.args[0] == "show-user" {
			t.Error("Lock() consulted show-user despite XDG_SESSION_ID being set")
		}
	}
}

// If the session cannot be resolved, fall back to letting logind work it out
// rather than refusing to lock: an unlocked session is the worse failure.
func TestLockFallsBackToBareLockSession(t *testing.T) {
	t.Setenv("XDG_SESSION_ID", "")
	f := &fakeRun{err: map[string]error{"loginctl show-user": errors.New("boom")}}
	f.install(t)

	if err := Lock(); err != nil {
		t.Fatalf("Lock() = %v", err)
	}
	want := []string{"lock-session"}
	if args := f.lastArgs(t, "loginctl"); !slices.Equal(args, want) {
		t.Errorf("loginctl args = %v, want %v", args, want)
	}
}

func TestLockReportsFailure(t *testing.T) {
	t.Setenv("XDG_SESSION_ID", "2")
	f := &fakeRun{err: map[string]error{"loginctl lock-session": errors.New("no such session")}}
	f.install(t)

	err := Lock()
	if err == nil {
		t.Fatal("Lock() = nil, want an error")
	}
	if !strings.Contains(err.Error(), "no such session") {
		t.Errorf("Lock() error %q does not carry the underlying cause", err)
	}
}

func ptr(s string) *string { return &s }

func readTrimmed(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return strings.TrimSpace(string(b))
}
