// Package idle wraps org.gnome.Mutter.IdleMonitor on the session bus.
//
// GNOME's compositor exposes no Wayland idle protocol, so this D-Bus interface
// is the only idle signal available. See the project specification, section 6.3.
//
// Interface reference:
//
//	name:      org.gnome.Mutter.IdleMonitor
//	path:      /org/gnome/Mutter/IdleMonitor/Core
//	methods:   AddIdleWatch(t) -> u, AddUserActiveWatch() -> u,
//	           GetIdletime() -> t, RemoveWatch(u), ResetIdletime()
//	signal:    WatchFired(u)
package idle

import (
	"errors"
	"time"
)

// ErrNotImplemented is returned by every stub in this package.
var ErrNotImplemented = errors.New("idle: not implemented")

// WatchID identifies a watch registered with the idle monitor.
type WatchID uint32

// Monitor is a connection to the GNOME idle monitor.
type Monitor struct{}

// Connect dials the session bus and verifies the idle monitor is reachable.
// It must fail with an actionable error rather than spin when the name is
// unavailable, which is the case on any non-GNOME session.
func Connect() (*Monitor, error) { return nil, ErrNotImplemented }

// Close releases the bus connection.
func (m *Monitor) Close() error { return ErrNotImplemented }

// Idletime reports how long the session has been idle.
func (m *Monitor) Idletime() (time.Duration, error) { return 0, ErrNotImplemented }

// AddIdleWatch fires once when the session has been idle for d.
func (m *Monitor) AddIdleWatch(d time.Duration) (WatchID, error) { return 0, ErrNotImplemented }

// AddUserActiveWatch fires once when the user becomes active again.
func (m *Monitor) AddUserActiveWatch() (WatchID, error) { return 0, ErrNotImplemented }

// RemoveWatch cancels a previously registered watch.
func (m *Monitor) RemoveWatch(id WatchID) error { return ErrNotImplemented }

// Fired returns a channel delivering the ID of each watch as it fires.
func (m *Monitor) Fired() <-chan WatchID { return nil }
