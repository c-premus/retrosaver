package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	got, err := Load(filepath.Join(t.TempDir(), "absent.conf"))
	if err != nil {
		t.Fatalf("missing file must not be an error, got %v", err)
	}
	if got.SaverDelay != 300*time.Second {
		t.Errorf("SaverDelay = %v, want 300s", got.SaverDelay)
	}
	if len(got.Exclude) != 6 {
		t.Errorf("Exclude = %v, want the 6 shipped defaults", got.Exclude)
	}
}

func TestParseOverridesAndQuoting(t *testing.T) {
	in := `
# a comment
SAVER_DELAY=15

LOCK_AFTER=20
BLANK_AFTER=10
EXCLUDE="webcollage sonar"
INCLUDE='atlantis flame ifs'
`
	cfg := Defaults()
	if err := parse(strings.NewReader(in), &cfg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.SaverDelay != 15*time.Second {
		t.Errorf("SaverDelay = %v, want 15s", cfg.SaverDelay)
	}
	if cfg.BlankAfter != 10*time.Second {
		t.Errorf("BlankAfter = %v, want 10s", cfg.BlankAfter)
	}
	if want := []string{"webcollage", "sonar"}; !equal(cfg.Exclude, want) {
		t.Errorf("Exclude = %v, want %v", cfg.Exclude, want)
	}
	if want := []string{"atlantis", "flame", "ifs"}; !equal(cfg.Include, want) {
		t.Errorf("Include = %v (single quotes must strip too), want %v", cfg.Include, want)
	}
}

// The config file must be parsed, never executed. A line that would be a
// command substitution in shell has to survive as inert text.
func TestParseDoesNotEvaluateShell(t *testing.T) {
	// A path under the test's own directory, not a fixed /tmp name: a leftover
	// file from an earlier run, another checkout, or another user would
	// otherwise make a perfectly correct parser fail this test forever.
	canary := filepath.Join(t.TempDir(), "pwned")
	payload := "$(touch " + canary + ")"

	cfg := Defaults()
	if err := parse(strings.NewReader("INCLUDE=\""+payload+"\"\n"), &cfg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := strings.Join(cfg.Include, " "); got != payload {
		t.Errorf("Include = %q, want the literal text unevaluated", got)
	}
	if _, err := os.Stat(canary); err == nil {
		t.Fatal("parser executed the config file")
	}
}

func TestZeroDisablesStages(t *testing.T) {
	cfg := Defaults()
	if err := parse(strings.NewReader("LOCK_AFTER=0\n"), &cfg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.LockEnabled() {
		t.Error("LOCK_AFTER=0 must disable the lock stage")
	}
	// Blanking is meaningless without locking, per spec 6.3.
	if cfg.BlankEnabled() {
		t.Error("blank must be disabled when lock is disabled, regardless of BLANK_AFTER")
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, in := range []string{"SAVER_DELAY=abc\n", "SAVER_DELAY=-5\n", "not an assignment\n"} {
		cfg := Defaults()
		if err := parse(strings.NewReader(in), &cfg); err == nil {
			t.Errorf("parse(%q) = nil error, want failure", in)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Load is all-or-nothing: a file that fails to parse yields the defaults, not
// the half-applied state the parser had reached when it gave up.
//
// The daemon depends on this. It logs a bad config and carries on with what
// Load returned, because refusing to start would leave the session with no
// screensaver and -- since setup hands idle-delay to the daemon -- no auto-lock
// either. Returning a partly-applied config would silently arm a mixture of the
// user's file and the defaults.
func TestLoadReturnsDefaultsOnAParseError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retrosaver.conf")
	// SAVER_DELAY parses; LOCK_AFTER does not. A partial application would
	// keep the 60 and leave LockAfter at its default.
	body := "SAVER_DELAY=60\nLOCK_AFTER=banana\nBLANK_AFTER=30\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err == nil {
		t.Fatal("Load() = nil error, want a parse error")
	}
	if want := Defaults(); cfg.SaverDelay != want.SaverDelay {
		t.Errorf("SaverDelay = %v, want the default %v: the config was partly applied",
			cfg.SaverDelay, want.SaverDelay)
	}
	if want := Defaults(); cfg.BlankAfter != want.BlankAfter {
		t.Errorf("BlankAfter = %v, want the default %v", cfg.BlankAfter, want.BlankAfter)
	}
}

// A seconds value large enough to overflow time.Duration must be rejected.
// time.Duration is an int64 of nanoseconds, so ~9.2e9 seconds wraps negative
// and sails past the "must not be negative" check.
func TestParseRejectsAnOverflowingDuration(t *testing.T) {
	var cfg Config
	if err := parse(strings.NewReader("SAVER_DELAY=9223372037\n"), &cfg); err == nil {
		t.Errorf("parse() accepted 9223372037 seconds, want an error; got SaverDelay = %v",
			cfg.SaverDelay)
	}
}
