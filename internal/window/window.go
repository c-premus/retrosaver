// Package window launches an XScreenSaver module and makes its X11 window
// fullscreen and always-on-top.
//
// Modules are ordinary X11 clients running under XWayland. Mutter implements
// EWMH for them, which is why wmctrl works. See specification section 6.2.
package window

import (
	"errors"
	"os"
)

// ErrNotImplemented is returned by every stub in this package.
var ErrNotImplemented = errors.New("window: not implemented")

// ErrNoWindow reports that a module process started but never mapped a window.
// The caller should kill it and try a different module.
var ErrNoWindow = errors.New("window: module mapped no window")

// Saver is a running module and its associated pointer-hiding process.
type Saver struct{}

// Launch starts the module at path, waits for its window to appear, then sets
// fullscreen and above via EWMH and hides the pointer.
//
// It returns ErrNoWindow when no window appears within the timeout, so the
// daemon can retry with a different module rather than failing outright.
func Launch(path string) (*Saver, error) { return nil, ErrNotImplemented }

// Process returns the module's OS process, so the daemon can hold it directly
// instead of round-tripping through the PID file.
func (s *Saver) Process() *os.Process { return nil }

// Stop terminates the module and the pointer-hiding process.
func (s *Saver) Stop() error { return ErrNotImplemented }
