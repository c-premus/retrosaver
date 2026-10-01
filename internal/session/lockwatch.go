package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// logind on the system bus. A session object's LockedHint is the property
// Locked reads through loginctl; here it is watched rather than polled.
const (
	logindName      = "org.freedesktop.login1"
	logindPath      = dbus.ObjectPath("/org/freedesktop/login1")
	logindManager   = "org.freedesktop.login1.Manager"
	logindSession   = "org.freedesktop.login1.Session"
	propertiesIface = "org.freedesktop.DBus.Properties"

	// dbusCallTimeout bounds the method calls made while subscribing.
	// dbus.BusObject.Call blocks forever by default.
	dbusCallTimeout = 5 * time.Second

	// lockSignalBuffer absorbs D-Bus deliveries. godbus drops signals on a
	// full channel rather than blocking its reader, and a session object
	// emits PropertiesChanged for more than LockedHint.
	lockSignalBuffer = 16
)

// LockWatcher reports changes to the graphical session's LockedHint.
//
// The daemon needs it for a lock that arrives with no user activity --
// `loginctl lock-session` over SSH, or a lid close -- which leaves a module
// running behind GNOME's lock shield: invisible, still burning GPU, and still
// keeping the display lit. GNOME does not stop it; measured on the reference
// host, the module was still running 60 s into such a lock.
type LockWatcher struct {
	conn    *dbus.Conn
	path    dbus.ObjectPath
	signals chan *dbus.Signal
	changes chan bool
	done    chan struct{}

	once     sync.Once
	closeErr error
	wg       sync.WaitGroup
}

// WatchLocks subscribes to LockedHint on the graphical session.
//
// The session is named explicitly, for the reason Locked gives: the daemon
// runs under user@<uid>.service, outside the graphical session's cgroup, so
// logind cannot work out which session is meant.
func WatchLocks() (*LockWatcher, error) {
	id, err := graphicalSessionID()
	if err != nil {
		return nil, fmt.Errorf("session: resolving the graphical session: %w", err)
	}
	if id == "" {
		return nil, errors.New("session: no graphical session to watch LockedHint on")
	}

	// ConnectSystemBus, not SystemBus: the shared connection's Close is a
	// no-op, and Close here must actually release the match rule.
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("session: connecting to the system bus: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), dbusCallTimeout)
	defer cancel()
	var path dbus.ObjectPath
	if err := conn.Object(logindName, logindPath).CallWithContext(
		ctx, logindManager+".GetSession", 0, id).Store(&path); err != nil {
		return nil, fmt.Errorf("session: asking logind for session %s: %w", id, err)
	}

	w := &LockWatcher{
		conn:    conn,
		path:    path,
		signals: make(chan *dbus.Signal, lockSignalBuffer),
		changes: make(chan bool, 1),
		done:    make(chan struct{}),
	}

	// Register the channel before installing the match rule, or a signal can
	// arrive in between with nowhere to go.
	conn.Signal(w.signals)
	if err := conn.AddMatchSignal(
		dbus.WithMatchSender(logindName),
		dbus.WithMatchObjectPath(path),
		dbus.WithMatchInterface(propertiesIface),
		dbus.WithMatchMember("PropertiesChanged"),
		dbus.WithMatchArg(0, logindSession),
	); err != nil {
		conn.RemoveSignal(w.signals)
		return nil, fmt.Errorf("session: adding a match rule for %s: %w", path, err)
	}

	w.wg.Add(1)
	go w.pump()

	ok = true
	return w, nil
}

// Changes delivers the new LockedHint each time it changes. It is closed when
// the watcher is closed or the system bus connection drops.
func (w *LockWatcher) Changes() <-chan bool { return w.changes }

// pump is the sole sender on, and closer of, w.changes.
func (w *LockWatcher) pump() {
	defer w.wg.Done()
	defer close(w.changes)

	for {
		select {
		case <-w.done:
			return
		case sig, ok := <-w.signals:
			if !ok {
				return
			}
			locked, ok := lockedHintChange(sig, w.path)
			if !ok {
				continue
			}
			select {
			case w.changes <- locked:
			case <-w.done:
				return
			}
		}
	}
}

// lockedHintChange extracts LockedHint from a PropertiesChanged signal on
// path, reporting whether sig carried it.
//
// As in the idle package, the sender is not compared: the match rule names
// the well-known name, but the delivered signal carries the unique one.
func lockedHintChange(sig *dbus.Signal, path dbus.ObjectPath) (bool, bool) {
	if sig == nil || sig.Path != path || sig.Name != propertiesIface+".PropertiesChanged" {
		return false, false
	}
	if len(sig.Body) < 2 {
		return false, false
	}
	if iface, ok := sig.Body[0].(string); !ok || iface != logindSession {
		return false, false
	}
	changed, ok := sig.Body[1].(map[string]dbus.Variant)
	if !ok {
		return false, false
	}
	v, ok := changed["LockedHint"]
	if !ok {
		return false, false
	}
	locked, ok := v.Value().(bool)
	return locked, ok
}

// Close releases the system bus connection. It is safe to call more than once.
func (w *LockWatcher) Close() error {
	w.once.Do(func() {
		// Release the pump first, as idle.Monitor.Close does, so it cannot be
		// left blocked on a send while wg.Wait waits for it.
		close(w.done)
		w.conn.RemoveSignal(w.signals)
		w.closeErr = w.conn.Close()
		w.wg.Wait()
	})
	return w.closeErr
}
