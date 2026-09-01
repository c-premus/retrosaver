// Package session drives the GNOME session: locking, and the idle-delay
// setting that controls blanking.
//
// retrosaver deliberately reuses GNOME's own blanking and locking paths rather
// than poking DPMS behind the compositor's back. See docs/spec.md section 6.3.
//
// Everything here shells out. That is not laziness: GSettings' own API is
// libgio, which means cgo, which would end CGO_ENABLED=0 and the static
// artifact. The pure-Go alternative -- writing dconf directly over D-Bus --
// is worse, because dconf is the storage backend and holds no schema: a key
// sitting at its default has no dconf entry at all, so reads would have to
// fall back to a hardcoded copy of GNOME's defaults.
package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	schema = "org.gnome.desktop.session"
	key    = "idle-delay"

	// DefaultIdleDelay is GNOME's own default, in seconds. It is the fallback
	// when the saved value is missing or unreadable: leaving the user with a
	// working auto-lock matters more than being exact.
	DefaultIdleDelay = 300

	// runTimeout bounds every helper process. gsettings and loginctl are
	// fast, and a hung one must not wedge the daemon.
	runTimeout = 10 * time.Second
)

// run executes a helper and returns its stdout. It is a package variable so
// tests can substitute a recorder and never spawn a process.
var run = runCommand

func runCommand(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		line := name + " " + strings.Join(args, " ")
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%s: %w: %s", line, err, msg)
		}
		return "", fmt.Errorf("%s: %w", line, err)
	}
	return stdout.String(), nil
}

// Lock hands off to GNOME's own lock screen via loginctl.
//
// retrosaver is not a screen locker and must not try to be one: a module is an
// ordinary window and X11 grabs do not work under XWayland.
//
// The session is named explicitly where it can be resolved. Bare
// `loginctl lock-session` asks logind which session the caller is in, and a
// daemon under user@<uid>.service is not in the graphical session's cgroup.
// On GNOME 50 that still resolves correctly -- logind falls back to the user's
// display session, verified on the reference host -- but it is an implicit
// fallback on a machine that also has a class=manager session, and it is not
// dependable on multi-seat. Naming the session removes the ambiguity.
func Lock() error {
	args := []string{"lock-session"}
	if id, err := graphicalSessionID(); err == nil && id != "" {
		args = append(args, id)
	}
	if _, err := run("loginctl", args...); err != nil {
		return fmt.Errorf("session: locking: %w", err)
	}
	return nil
}

// graphicalSessionID reports the logind session to lock, preferring the
// environment and falling back to the user's display session.
func graphicalSessionID() (string, error) {
	if id := strings.TrimSpace(os.Getenv("XDG_SESSION_ID")); id != "" {
		return id, nil
	}
	out, err := run("loginctl", "show-user", strconv.Itoa(os.Getuid()), "--value", "-p", "Display")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// IdleDelay reads org.gnome.desktop.session idle-delay, in seconds.
func IdleDelay() (int, error) {
	out, err := run("gsettings", "get", schema, key)
	if err != nil {
		return 0, fmt.Errorf("session: reading %s %s: %w", schema, key, err)
	}
	return parseIdleDelay(out)
}

// parseIdleDelay reads gsettings' GVariant rendering of the value.
//
// On GNOME 50 the value prints with its type, "uint32 300", so the number is
// the last field. A bare "300" parses too.
func parseIdleDelay(s string) (int, error) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, fmt.Errorf("session: gsettings printed nothing for %s %s", schema, key)
	}
	n, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil {
		return 0, fmt.Errorf("session: parsing %s %s value %q: %w", schema, key, strings.TrimSpace(s), err)
	}
	if n < 0 {
		return 0, fmt.Errorf("session: %s %s is negative (%d)", schema, key, n)
	}
	return n, nil
}

// SetIdleDelay writes org.gnome.desktop.session idle-delay, in seconds.
//
// Two values matter. Zero makes retrosaver the owner of the whole idle policy,
// which is what stops gnome-shell blanking the screen out from under the
// screensaver. A small value such as 10 sits below the current idle time and
// therefore makes gnome-shell blank essentially immediately, which is how the
// blank stage is implemented.
func SetIdleDelay(seconds int) error {
	if seconds < 0 {
		return fmt.Errorf("session: idle-delay must not be negative, got %d", seconds)
	}
	if _, err := run("gsettings", "set", schema, key, strconv.Itoa(seconds)); err != nil {
		return fmt.Errorf("session: writing %s %s = %d: %w", schema, key, seconds, err)
	}
	return nil
}

// SavedIdleDelayPath returns ~/.config/retrosaver/idle-delay.orig, which holds
// the value to restore on teardown.
func SavedIdleDelayPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("session: locating the user config dir: %w", err)
	}
	return filepath.Join(dir, "retrosaver", "idle-delay.orig"), nil
}

// SaveIdleDelay records the session's current idle-delay so teardown can put
// it back.
//
// It does nothing when the file already exists, and that is the single most
// important idempotency rule in the installer: running setup a second time
// while the daemon holds idle-delay at 0 would otherwise record 0 as "the
// original" and permanently destroy the user's auto-lock.
//
// The file is created with O_EXCL rather than stat-then-write, so that rule
// survives concurrency. A stat and a later write are two steps: two setups
// racing each other, or a setup racing the unit's ExecStopPost, can both see
// no file and the loser then persists 0. O_EXCL makes check-and-create one
// atomic step, so exactly one writer wins and the rest are no-ops.
func SaveIdleDelay() error {
	path, err := SavedIdleDelayPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("session: creating directory %s: %w", filepath.Dir(path), err)
	}

	// Read before creating. A value read after winning the race would be no
	// fresher, and this way the file never exists without a value in it.
	current, err := IdleDelay()
	if err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return fmt.Errorf("session: creating %s: %w", path, err)
	}
	if _, err := f.WriteString(strconv.Itoa(current) + "\n"); err != nil {
		_ = f.Close()
		// Do not leave an empty file behind: it would win every future
		// O_EXCL and pin the restore to DefaultIdleDelay for good.
		_ = os.Remove(path)
		return fmt.Errorf("session: writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("session: closing %s: %w", path, err)
	}
	return nil
}

// RestoreIdleDelay writes the saved idle-delay back to GNOME.
//
// A missing or unparseable saved value falls back to DefaultIdleDelay rather
// than failing: this runs from ExecStopPost and from teardown, and leaving the
// session with no auto-lock at all is a far worse outcome than restoring a
// value that is merely not the user's original.
func RestoreIdleDelay() error {
	path, err := SavedIdleDelayPath()
	if err != nil {
		return err
	}

	seconds := DefaultIdleDelay
	if b, readErr := os.ReadFile(path); readErr == nil {
		if n, convErr := strconv.Atoi(strings.TrimSpace(string(b))); convErr == nil && n >= 0 {
			seconds = n
		}
	}
	return SetIdleDelay(seconds)
}

// ClearSavedIdleDelay removes the saved value, so a later setup captures a
// fresh one. A missing file is not an error.
func ClearSavedIdleDelay() error {
	path, err := SavedIdleDelayPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("session: removing %s: %w", path, err)
	}
	return nil
}
