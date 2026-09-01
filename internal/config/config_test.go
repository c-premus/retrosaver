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
	cfg := Defaults()
	err := parse(strings.NewReader("INCLUDE=\"$(touch /tmp/retrosaver-pwned)\"\n"), &cfg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := strings.Join(cfg.Include, " "); got != "$(touch /tmp/retrosaver-pwned)" {
		t.Errorf("Include = %q, want the literal text unevaluated", got)
	}
	if _, err := os.Stat("/tmp/retrosaver-pwned"); err == nil {
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
