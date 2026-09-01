package idle

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// These tests talk to a real gnome-shell on the session bus. They are gated on
// an environment variable rather than a build tag so that CI, the devcontainer
// and `go test ./...` on any non-GNOME machine skip them cleanly, while a
// single command exercises them on the host:
//
//	RETROSAVER_LIVE=1 go test ./internal/idle -run Live -v
//
// Run them with the machine left alone: real input resets the idle clock and
// will make the idle-watch assertions flap.

func liveMonitor(t *testing.T) *Monitor {
	t.Helper()
	if os.Getenv("RETROSAVER_LIVE") == "" {
		t.Skip("set RETROSAVER_LIVE=1 to run against a real GNOME/Wayland session")
	}
	m, err := Connect()
	if err != nil {
		t.Fatalf("Connect() = %v", err)
	}
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("Close() = %v", err)
		}
	})
	return m
}

// simulateActivity resets the session's idle clock the way a user would.
//
// Mutter refuses ResetIdletime unless it was started with
// MUTTER_DEBUG_RESET_IDLETIME ("This method is for testing purposes only"),
// which is not how a real desktop runs. So the fallback is a genuine XTEST
// pointer event through XWayland, which is also a useful check in its own
// right: it is the same path internal/window depends on.
func simulateActivity(t *testing.T, m *Monitor) {
	t.Helper()
	if err := m.ResetIdletime(); err == nil {
		return
	}
	if _, err := exec.LookPath("xdotool"); err != nil {
		t.Skip("cannot simulate activity: Mutter gates ResetIdletime behind " +
			"MUTTER_DEBUG_RESET_IDLETIME, and xdotool is not installed")
	}
	for _, args := range [][]string{
		{"mousemove_relative", "--", "1", "1"},
		{"mousemove_relative", "--", "-1", "-1"},
	} {
		if out, err := exec.Command("xdotool", args...).CombinedOutput(); err != nil {
			t.Skipf("xdotool %v failed, cannot simulate activity: %v: %s", args, err, out)
		}
	}
}

// waitFired reports the next watch ID, or fails after timeout.
func waitFired(t *testing.T, m *Monitor, timeout time.Duration, what string) WatchID {
	t.Helper()
	select {
	case id, ok := <-m.Fired():
		if !ok {
			t.Fatalf("%s: Fired channel closed unexpectedly", what)
		}
		return id
	case <-time.After(timeout):
		t.Fatalf("%s: no watch fired within %v", what, timeout)
		return 0
	}
}

func TestLiveIdletime(t *testing.T) {
	m := liveMonitor(t)

	d, err := m.Idletime()
	if err != nil {
		t.Fatalf("Idletime() = %v", err)
	}
	if d < 0 || d > 24*time.Hour {
		t.Errorf("Idletime() = %v, want a plausible duration", d)
	}
	t.Logf("Idletime() = %v", d)
}

// TestLiveIdleWatchAlreadyOverdue answers the cold-start question: when the
// daemon starts on an already-idle session (boot, systemctl restart, a crash
// loop), does a watch whose threshold is already in the past fire on its own?
//
// If it does not, daemon.Run must synthesise the missed stage by comparing
// Idletime against the configured thresholds at startup.
func TestLiveIdleWatchAlreadyOverdue(t *testing.T) {
	m := liveMonitor(t)

	now, err := m.Idletime()
	if err != nil {
		t.Fatalf("Idletime() = %v", err)
	}
	if now < 5*time.Second {
		t.Skipf("session has only been idle %v; leave the machine alone and re-run", now)
	}

	// Deliberately below the current idle time.
	overdue := now / 2
	id, err := m.AddIdleWatch(overdue)
	if err != nil {
		t.Fatalf("AddIdleWatch(%v) = %v", overdue, err)
	}
	defer m.RemoveWatch(id) //nolint:errcheck // best effort in a test

	select {
	case got := <-m.Fired():
		if got != id {
			t.Fatalf("fire = watch %d, want %d", got, id)
		}
		t.Logf("an already-overdue watch (%v, idle %v) fired on its own: "+
			"no cold-start synthesis needed", overdue, now)
	case <-time.After(5 * time.Second):
		t.Logf("an already-overdue watch (%v, idle %v) did NOT fire: "+
			"daemon.Run must synthesise missed stages at startup", overdue, now)
	}
}

// TestLiveIdleWatchReArms answers the question daemon.reset depends on: after
// the idle clock resets, does an existing idle watch fire a second time, or
// must it be re-registered?
func TestLiveIdleWatchReArms(t *testing.T) {
	m := liveMonitor(t)

	const interval = 3 * time.Second

	simulateActivity(t, m)

	id, err := m.AddIdleWatch(interval)
	if err != nil {
		t.Fatalf("AddIdleWatch(%v) = %v", interval, err)
	}
	defer m.RemoveWatch(id) //nolint:errcheck // best effort in a test

	if got := waitFired(t, m, 20*time.Second, "first idle fire"); got != id {
		t.Fatalf("first fire = watch %d, want %d", got, id)
	}
	t.Log("idle watch fired once")

	// Reset the clock, then go idle again without re-registering anything.
	simulateActivity(t, m)

	select {
	case got := <-m.Fired():
		if got != id {
			t.Fatalf("second fire = watch %d, want %d", got, id)
		}
		t.Log("idle watch re-armed itself: daemon.reset need NOT re-add idle watches")
	case <-time.After(20 * time.Second):
		t.Error("idle watch did not fire a second time: " +
			"daemon.reset MUST remove and re-add the idle watches")
	}
}

// TestLiveUserActiveWatchIsOneShot confirms that Mutter drops a user-active
// watch once it fires, which is why the daemon re-arms one after every reset.
func TestLiveUserActiveWatchIsOneShot(t *testing.T) {
	m := liveMonitor(t)

	// Be idle first: a user-active watch on an already-active session can
	// fire immediately and tell us nothing.
	simulateActivity(t, m)
	time.Sleep(2 * time.Second)

	id, err := m.AddUserActiveWatch()
	if err != nil {
		t.Fatalf("AddUserActiveWatch() = %v", err)
	}
	t.Logf("user-active watch id = %d", id)

	simulateActivity(t, m)

	if got := waitFired(t, m, 20*time.Second, "user-active fire"); got != id {
		t.Fatalf("fire = watch %d, want %d", got, id)
	}

	// If Mutter auto-removed it, removing it again must fail. The daemon
	// relies on this: onActive deletes the ID from its map rather than
	// calling RemoveWatch on an already-removed watch.
	if err := m.RemoveWatch(id); err == nil {
		t.Log("NOTE: RemoveWatch after fire SUCCEEDED; Mutter did not auto-remove it")
	} else {
		t.Logf("confirmed one-shot: RemoveWatch after fire = %v", err)
	}
}

func TestLiveConnectFailsFastWithoutABus(t *testing.T) {
	if os.Getenv("RETROSAVER_LIVE") == "" {
		t.Skip("set RETROSAVER_LIVE=1 to run against a real GNOME/Wayland session")
	}
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
