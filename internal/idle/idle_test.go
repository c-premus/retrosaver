package idle

import (
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// watchFiredID is the guard that stops arbitrary bus traffic being dispatched
// as a watch ID. It is pure, so it needs no session bus and runs in CI, unlike
// everything in idle_live_test.go.
func TestWatchFiredID(t *testing.T) {
	const member = iface + ".WatchFired"

	tests := []struct {
		name string
		sig  *dbus.Signal
		want WatchID
		ok   bool
	}{
		{
			name: "a genuine signal",
			// Sender is the unique name, not busName: that is what a real
			// delivery looks like, and matching on it would reject every one.
			sig:  &dbus.Signal{Sender: ":1.25", Path: objectPath, Name: member, Body: []any{uint32(7)}},
			want: 7,
			ok:   true,
		},
		{
			name: "watch id zero is still a valid id",
			sig:  &dbus.Signal{Path: objectPath, Name: member, Body: []any{uint32(0)}},
			want: 0,
			ok:   true,
		},
		{
			name: "the largest uint32 survives the conversion",
			sig:  &dbus.Signal{Path: objectPath, Name: member, Body: []any{uint32(4294967295)}},
			want: 4294967295,
			ok:   true,
		},
		{name: "nil signal", sig: nil},
		{
			name: "wrong object path",
			sig:  &dbus.Signal{Path: dbus.ObjectPath("/org/gnome/Mutter/Elsewhere"), Name: member, Body: []any{uint32(7)}},
		},
		{
			name: "wrong member",
			sig:  &dbus.Signal{Path: objectPath, Name: iface + ".SomethingElse", Body: []any{uint32(7)}},
		},
		{
			name: "right member on the wrong interface",
			sig:  &dbus.Signal{Path: objectPath, Name: "org.example.Other.WatchFired", Body: []any{uint32(7)}},
		},
		{
			name: "no body",
			sig:  &dbus.Signal{Path: objectPath, Name: member, Body: nil},
		},
		{
			name: "too many body elements",
			sig:  &dbus.Signal{Path: objectPath, Name: member, Body: []any{uint32(7), uint32(8)}},
		},
		{
			name: "body of the wrong type",
			sig:  &dbus.Signal{Path: objectPath, Name: member, Body: []any{"7"}},
		},
		{
			name: "a uint64 body is not accepted",
			sig:  &dbus.Signal{Path: objectPath, Name: member, Body: []any{uint64(7)}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := watchFiredID(tt.sig)
			if ok != tt.ok {
				t.Fatalf("watchFiredID() ok = %v, want %v", ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Errorf("watchFiredID() = %d, want %d", got, tt.want)
			}
		})
	}
}

// A non-positive interval is rejected before the bus is touched, so this needs
// no connection: the method returns on a nil *Monitor's behalf only because it
// never reaches m.call.
func TestAddIdleWatchRejectsNonPositiveIntervals(t *testing.T) {
	var m *Monitor // never dereferenced: the guard returns first

	for _, d := range []time.Duration{0, -time.Second, 999 * time.Microsecond} {
		if _, err := m.AddIdleWatch(d); err == nil {
			t.Errorf("AddIdleWatch(%v) = nil error, want a rejection", d)
		}
	}
}

// Connect must fail fast against an unreachable bus rather than spin.
//
// This is deliberately NOT gated on RETROSAVER_LIVE. It points
// DBUS_SESSION_BUS_ADDRESS at a socket that does not exist, so it needs no
// GNOME session, no bus and no desktop -- which makes it the one Connect
// error path CI can actually cover.
func TestConnectFailsFastWithoutABus(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/nonexistent/retrosaver-test-bus")

	start := time.Now()
	m, err := Connect()
	if err == nil {
		m.Close()
		t.Fatal("Connect() succeeded against a nonexistent bus, want an error")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Connect() took %v to fail; it must fail fast, not spin", elapsed)
	}
	t.Logf("failed fast with: %v", err)
}
