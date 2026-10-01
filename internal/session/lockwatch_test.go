package session

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestLockedHintChange(t *testing.T) {
	const path = dbus.ObjectPath("/org/freedesktop/login1/session/_32")
	changed := func(props map[string]dbus.Variant) *dbus.Signal {
		return &dbus.Signal{
			Path: path,
			Name: propertiesIface + ".PropertiesChanged",
			Body: []any{logindSession, props, []string{}},
		}
	}

	tests := []struct {
		name       string
		sig        *dbus.Signal
		wantLocked bool
		wantOK     bool
	}{
		{"locked", changed(map[string]dbus.Variant{"LockedHint": dbus.MakeVariant(true)}), true, true},
		{"unlocked", changed(map[string]dbus.Variant{"LockedHint": dbus.MakeVariant(false)}), false, true},
		{"alongside other properties", changed(map[string]dbus.Variant{
			"IdleHint":   dbus.MakeVariant(true),
			"LockedHint": dbus.MakeVariant(true),
		}), true, true},
		// logind emits PropertiesChanged for IdleHint and others far more often
		// than for LockedHint. None of them may read as an unlock.
		{"another property only", changed(map[string]dbus.Variant{"IdleHint": dbus.MakeVariant(true)}), false, false},
		{"wrong value type", changed(map[string]dbus.Variant{"LockedHint": dbus.MakeVariant("yes")}), false, false},
		{"nil", nil, false, false},
		{"another session", &dbus.Signal{
			Path: "/org/freedesktop/login1/session/_33",
			Name: propertiesIface + ".PropertiesChanged",
			Body: []any{logindSession, map[string]dbus.Variant{"LockedHint": dbus.MakeVariant(true)}, []string{}},
		}, false, false},
		{"another interface", &dbus.Signal{
			Path: path,
			Name: propertiesIface + ".PropertiesChanged",
			Body: []any{"org.freedesktop.login1.User", map[string]dbus.Variant{"LockedHint": dbus.MakeVariant(true)}, []string{}},
		}, false, false},
		{"another signal", &dbus.Signal{
			Path: path,
			Name: logindSession + ".Lock",
		}, false, false},
		{"short body", &dbus.Signal{
			Path: path,
			Name: propertiesIface + ".PropertiesChanged",
			Body: []any{logindSession},
		}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			locked, ok := lockedHintChange(tt.sig, path)
			if locked != tt.wantLocked || ok != tt.wantOK {
				t.Errorf("lockedHintChange() = %v, %v; want %v, %v", locked, ok, tt.wantLocked, tt.wantOK)
			}
		})
	}
}
