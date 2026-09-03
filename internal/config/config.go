// Package config parses retrosaver's shell-style KEY=value configuration file.
//
// The file is deliberately parsed with a line parser rather than sourced by a
// shell: it lives in the user's home directory and must never be executed.
package config

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds the resolved configuration. Delays are stored as durations;
// the file expresses them in seconds.
type Config struct {
	// SaverDelay is the idle time before the screensaver starts.
	SaverDelay time.Duration
	// LockAfter is the time after the saver starts before the session locks.
	// Zero disables locking.
	LockAfter time.Duration
	// BlankAfter is the time after locking before the display powers off.
	// Zero disables blanking.
	BlankAfter time.Duration
	// CycleAfter is how long each module stays on screen before the saver
	// swaps to another one. Zero disables cycling, leaving the module that
	// started the saver stage up until the lock stage or user activity.
	CycleAfter time.Duration
	// Exclude lists module names never to pick.
	Exclude []string
	// Include, when non-empty, restricts selection to these modules.
	Include []string
}

// Defaults returns the shipped defaults, matching config/retrosaver.conf.example.
func Defaults() Config {
	return Config{
		SaverDelay: 300 * time.Second,
		LockAfter:  900 * time.Second,
		BlankAfter: 120 * time.Second,
		CycleAfter: 300 * time.Second,
		Exclude: []string{
			"webcollage", "vidwhacker", "glslideshow",
			"photopile", "carousel", "sonar",
		},
		Include: nil,
	}
}

// LockEnabled reports whether the lock stage is active.
func (c Config) LockEnabled() bool { return c.LockAfter > 0 }

// BlankEnabled reports whether the blank stage is active. Blanking is
// meaningless without locking, so it is gated on both.
func (c Config) BlankEnabled() bool { return c.LockAfter > 0 && c.BlankAfter > 0 }

// CycleEnabled reports whether the saver swaps modules while it runs.
func (c Config) CycleEnabled() bool { return c.CycleAfter > 0 }

// UserConfigPath returns ~/.config/retrosaver/retrosaver.conf, honouring
// XDG_CONFIG_HOME.
func UserConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locating user config dir: %w", err)
	}
	return filepath.Join(dir, "retrosaver", "retrosaver.conf"), nil
}

// Load reads the config at path.
//
// Parsing is all-or-nothing. On any error the returned Config is Defaults(),
// never a half-applied file, so a caller may use the returned Config whether
// or not err is nil. That is what lets the daemon log a bad config and keep
// running: refusing to start would leave the session with no screensaver and,
// because setup hands idle-delay to the daemon, no auto-lock either.
//
// A missing file is not an error: the defaults are returned unchanged, which
// is what a fresh install should do.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Defaults(), nil
		}
		return Defaults(), fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	cfg := Defaults()
	if err := parse(f, &cfg); err != nil {
		return Defaults(), fmt.Errorf("parsing %s: %w", path, err)
	}
	return cfg, nil
}

// parse applies KEY=value assignments from r onto cfg. Unknown keys are
// ignored so a newer config file does not break an older binary.
func parse(r io.Reader, cfg *Config) error {
	sc := bufio.NewScanner(r)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, value, ok := strings.Cut(text, "=")
		if !ok {
			return fmt.Errorf("line %d: not a KEY=value assignment: %q", line, text)
		}
		key = strings.TrimSpace(key)
		value = unquote(strings.TrimSpace(value))

		switch key {
		case "SAVER_DELAY":
			d, err := seconds(value)
			if err != nil {
				return fmt.Errorf("line %d: SAVER_DELAY: %w", line, err)
			}
			cfg.SaverDelay = d
		case "LOCK_AFTER":
			d, err := seconds(value)
			if err != nil {
				return fmt.Errorf("line %d: LOCK_AFTER: %w", line, err)
			}
			cfg.LockAfter = d
		case "BLANK_AFTER":
			d, err := seconds(value)
			if err != nil {
				return fmt.Errorf("line %d: BLANK_AFTER: %w", line, err)
			}
			cfg.BlankAfter = d
		case "CYCLE_AFTER":
			d, err := seconds(value)
			if err != nil {
				return fmt.Errorf("line %d: CYCLE_AFTER: %w", line, err)
			}
			cfg.CycleAfter = d
		case "EXCLUDE":
			cfg.Exclude = strings.Fields(value)
		case "INCLUDE":
			cfg.Include = strings.Fields(value)
		}
	}
	return sc.Err()
}

// unquote strips one matching pair of surrounding single or double quotes.
// It does not interpret escapes: the file is data, not shell.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// maxSeconds is the largest value that survives conversion to a Duration.
// A Duration is an int64 nanosecond count, so a larger number of seconds wraps
// silently negative -- past the n < 0 check, which is why it is rejected here.
const maxSeconds = int64(math.MaxInt64 / time.Second)

func seconds(s string) (time.Duration, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("want an integer number of seconds, got %q", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("must not be negative, got %d", n)
	}
	if int64(n) > maxSeconds {
		return 0, fmt.Errorf("must not exceed %d, got %d", maxSeconds, n)
	}
	return time.Duration(n) * time.Second, nil
}
