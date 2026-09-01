// Package idle wraps org.gnome.Mutter.IdleMonitor on the session bus.
//
// GNOME's compositor exposes no Wayland idle protocol, so this D-Bus interface
// is the only idle signal available. See docs/spec.md section 6.3.
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
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	busName    = "org.gnome.Mutter.IdleMonitor"
	iface      = "org.gnome.Mutter.IdleMonitor"
	objectPath = dbus.ObjectPath("/org/gnome/Mutter/IdleMonitor/Core")

	// callTimeout bounds every method call. dbus.BusObject.Call blocks
	// forever by default, and a wedged gnome-shell must not wedge the daemon.
	callTimeout = 5 * time.Second

	// signalBuffer absorbs D-Bus deliveries. godbus drops signals on a full
	// channel rather than blocking its own reader goroutine, and a dropped
	// WatchFired is a stage that silently never happens.
	signalBuffer = 32

	// firedBuffer decouples the pump from the daemon's event loop. At most
	// four watches exist, so anything beyond this means the loop is stuck.
	firedBuffer = 8
)

// WatchID identifies a watch registered with the idle monitor.
type WatchID uint32

// Monitor is a connection to the GNOME idle monitor.
type Monitor struct {
	conn *dbus.Conn
	obj  dbus.BusObject

	signals chan *dbus.Signal // registered with conn.Signal
	fired   chan WatchID      // handed to callers by Fired
	done    chan struct{}     // closed by Close, releases the pump

	once     sync.Once
	closeErr error
	wg       sync.WaitGroup
}

// Connect dials the session bus and verifies the idle monitor is reachable.
// It must fail with an actionable error rather than spin when the name is
// unavailable, which is the case on any non-GNOME session.
func Connect() (*Monitor, error) {
	// ConnectSessionBus, not SessionBus: the latter returns a process-wide
	// shared connection whose Close is a no-op, and we rely on closing the
	// connection to make Mutter drop our watches.
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, fmt.Errorf(
			"idle: connecting to the session bus (is DBUS_SESSION_BUS_ADDRESS set?): %w", err)
	}

	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()

	// Ask the bus daemon who owns the name rather than calling the object.
	// A method call on an unowned name can trigger service activation and
	// block for the activation timeout; NameHasOwner answers immediately.
	var owned bool
	if err := conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, busName).Store(&owned); err != nil {
		return nil, fmt.Errorf("idle: asking the bus who owns %s: %w", busName, err)
	}
	if !owned {
		return nil, fmt.Errorf(
			"idle: nothing owns %s on the session bus. retrosaver needs a running "+
				"GNOME session (gnome-shell owns this name); it does not work on other "+
				"desktops or over ssh", busName)
	}

	m := &Monitor{
		conn:    conn,
		obj:     conn.Object(busName, objectPath),
		signals: make(chan *dbus.Signal, signalBuffer),
		fired:   make(chan WatchID, firedBuffer),
		done:    make(chan struct{}),
	}

	// Register the channel before installing the match rule. Signals that
	// arrive with no channel registered are discarded, so the other order
	// leaves a window in which a WatchFired is lost.
	conn.Signal(m.signals)

	if err := conn.AddMatchSignal(
		dbus.WithMatchSender(busName),
		dbus.WithMatchObjectPath(objectPath),
		dbus.WithMatchInterface(iface),
		dbus.WithMatchMember("WatchFired"),
	); err != nil {
		conn.RemoveSignal(m.signals)
		return nil, fmt.Errorf("idle: adding a match rule for %s.WatchFired: %w", iface, err)
	}

	m.wg.Add(1)
	go m.pump()

	ok = true
	return m, nil
}

// pump translates WatchFired signals into watch IDs.
//
// This goroutine is the sole owner of m.fired: the only sender and the only
// closer, closing exactly once on its way out. That invariant is what makes a
// send on a closed channel structurally impossible.
func (m *Monitor) pump() {
	defer m.wg.Done()
	defer close(m.fired)

	for {
		select {
		case <-m.done:
			return
		case sig, ok := <-m.signals:
			if !ok {
				// godbus closes registered signal channels when the
				// connection drops -- gnome-shell died, or the bus went away.
				return
			}
			id, ok := watchFiredID(sig)
			if !ok {
				continue
			}
			select {
			case m.fired <- id:
			case <-m.done:
				return
			}
		}
	}
}

// watchFiredID extracts the watch ID from a WatchFired signal, reporting
// whether sig was one.
//
// The signal is matched on path and member only. The match rule names the
// well-known bus name, but the delivered signal carries the unique sender
// (":1.25" or similar), so comparing sig.Sender to busName would reject
// every genuine signal.
func watchFiredID(sig *dbus.Signal) (WatchID, bool) {
	if sig == nil || sig.Path != objectPath || sig.Name != iface+".WatchFired" {
		return 0, false
	}
	if len(sig.Body) != 1 {
		return 0, false
	}
	id, ok := sig.Body[0].(uint32)
	if !ok {
		return 0, false
	}
	return WatchID(id), true
}

// Close releases the bus connection. It is safe to call more than once.
func (m *Monitor) Close() error {
	m.once.Do(func() {
		// Order matters. Releasing the pump first is what prevents a
		// deadlock: if it were blocked sending on m.fired, closing the
		// connection first would leave it stuck there while wg.Wait below
		// waited on it forever.
		close(m.done)
		m.conn.RemoveSignal(m.signals)
		m.closeErr = m.conn.Close()
		m.wg.Wait()
	})
	return m.closeErr
}

// call invokes a method on the idle monitor under callTimeout.
func (m *Monitor) call(method string, args ...any) *dbus.Call {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()
	return m.obj.CallWithContext(ctx, iface+"."+method, 0, args...)
}

// Idletime reports how long the session has been idle.
func (m *Monitor) Idletime() (time.Duration, error) {
	var ms uint64
	if err := m.call("GetIdletime").Store(&ms); err != nil {
		return 0, fmt.Errorf("idle: %s.GetIdletime: %w", iface, err)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// AddIdleWatch fires once when the session has been idle for d.
func (m *Monitor) AddIdleWatch(d time.Duration) (WatchID, error) {
	ms := d.Milliseconds()
	if ms <= 0 {
		return 0, fmt.Errorf("idle: AddIdleWatch: interval must be positive, got %v", d)
	}
	var id uint32
	// The uint64 conversion is load-bearing: the method signature is "t".
	// Passing the int64 from Milliseconds marshals as "x" and Mutter rejects
	// the call with an unhelpful signature error.
	if err := m.call("AddIdleWatch", uint64(ms)).Store(&id); err != nil {
		return 0, fmt.Errorf("idle: %s.AddIdleWatch(%d ms): %w", iface, ms, err)
	}
	return WatchID(id), nil
}

// AddUserActiveWatch fires once when the user becomes active again.
//
// Mutter removes a user-active watch as soon as it fires, so callers must
// re-register one after every reset.
func (m *Monitor) AddUserActiveWatch() (WatchID, error) {
	var id uint32
	if err := m.call("AddUserActiveWatch").Store(&id); err != nil {
		return 0, fmt.Errorf("idle: %s.AddUserActiveWatch: %w", iface, err)
	}
	return WatchID(id), nil
}

// ResetIdletime sets the session's idle time back to zero, exactly as real
// user input would.
//
// The daemon does not use this: it observes idleness, it does not fabricate
// it. It exists for the live tests.
//
// Mutter refuses the call on a normal desktop -- "This method is for testing
// purposes only. MUTTER_DEBUG_RESET_IDLETIME must be set to use it" -- so
// callers must be prepared for it to fail and fall back to real input.
func (m *Monitor) ResetIdletime() error {
	if err := m.call("ResetIdletime").Store(); err != nil {
		return fmt.Errorf("idle: %s.ResetIdletime: %w", iface, err)
	}
	return nil
}

// RemoveWatch cancels a previously registered watch.
func (m *Monitor) RemoveWatch(id WatchID) error {
	if err := m.call("RemoveWatch", uint32(id)).Store(); err != nil {
		return fmt.Errorf("idle: %s.RemoveWatch(%d): %w", iface, id, err)
	}
	return nil
}

// Fired returns a channel delivering the ID of each watch as it fires.
//
// The channel is closed when the monitor is closed or the bus connection
// drops, so callers must use the two-value receive: treating a closed channel
// as an endless stream of watch 0 turns a lost connection into a hot loop.
func (m *Monitor) Fired() <-chan WatchID { return m.fired }
