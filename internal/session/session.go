// Package session drives the GNOME session: locking, and the idle-delay
// setting that controls blanking.
//
// retrosaver deliberately reuses GNOME's own blanking and locking paths rather
// than poking DPMS behind the compositor's back. See docs/spec.md section 6.3.
package session

import "errors"

// ErrNotImplemented is returned by every stub in this package.
var ErrNotImplemented = errors.New("session: not implemented")

// Lock hands off to GNOME's own lock screen via loginctl.
//
// retrosaver is not a screen locker and must not try to be one: a module is an
// ordinary window and X11 grabs do not work under XWayland.
func Lock() error { return ErrNotImplemented }

// IdleDelay reads org.gnome.desktop.session idle-delay, in seconds.
func IdleDelay() (int, error) { return 0, ErrNotImplemented }

// SetIdleDelay writes org.gnome.desktop.session idle-delay, in seconds.
//
// Two values matter. Zero makes retrosaver the owner of the whole idle policy,
// which is what stops gnome-shell blanking the screen out from under the
// screensaver. A small value such as 10 sits below the current idle time and
// therefore makes gnome-shell blank essentially immediately, which is how the
// blank stage is implemented.
func SetIdleDelay(seconds int) error { return ErrNotImplemented }

// SavedIdleDelayPath returns ~/.config/retrosaver/idle-delay.orig, which holds
// the value to restore on teardown.
func SavedIdleDelayPath() (string, error) { return "", ErrNotImplemented }
