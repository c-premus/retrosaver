package idle

import (
	"os"
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
//
// # Why nothing here fakes user activity
//
// It cannot. Mutter refuses ResetIdletime unless gnome-shell was started with
// MUTTER_DEBUG_RESET_IDLETIME ("This method is for testing purposes only"),
// which is not how a real desktop runs. Injecting XTEST input with
// `xdotool mousemove` does not work either: verified on gnome-shell 50.1, the
// idle clock keeps climbing straight through it (17.0s -> 17.3s), because the
// idle monitor watches libinput rather than synthetic X events. GNOME also
// treats XTEST injection through XWayland as remote control and raises an
// "allow remote interaction" prompt, so attempting it is both ineffective and
// intrusive. Do not reintroduce it.
//
// Anything that needs the idle clock to reset therefore needs a human, and
// lives in TestLiveIdleWatchReArmsWithRealInput below.

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
// It does, so daemon.arm deliberately carries no cold-start synthesis.
func TestLiveIdleWatchAlreadyOverdue(t *testing.T) {
	m := liveMonitor(t)

	now, err := m.Idletime()
	if err != nil {
		t.Fatalf("Idletime() = %v", err)
	}
	if now < 5*time.Second {
		t.Skipf("session has only been idle %v; leave the machine alone and re-run", now)
	}

	overdue := now / 2 // deliberately below the current idle time
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
		t.Errorf("an already-overdue watch (%v, idle %v) did NOT fire; "+
			"daemon.arm would need cold-start synthesis after all", overdue, now)
	}
}

// TestLiveRemoveWatchIsLenient documents that Mutter accepts any watch ID.
//
// daemon.rearm relies on this: it removes every watch it knows about without
// caring whether Mutter already dropped it. It is also why a successful
// RemoveWatch after a fire proves nothing about auto-removal, so no code
// tries to infer that.
func TestLiveRemoveWatchIsLenient(t *testing.T) {
	m := liveMonitor(t)

	id, err := m.AddIdleWatch(time.Hour)
	if err != nil {
		t.Fatalf("AddIdleWatch() = %v", err)
	}
	if err := m.RemoveWatch(id); err != nil {
		t.Errorf("RemoveWatch(%d) = %v, want nil", id, err)
	}
	if err := m.RemoveWatch(id); err != nil {
		t.Errorf("RemoveWatch(%d) a second time = %v, want nil", id, err)
	}
	if err := m.RemoveWatch(WatchID(999999)); err != nil {
		t.Errorf("RemoveWatch(bogus) = %v, want nil", err)
	}
}

// TestLiveIdleWatchReArmsWithRealInput needs a human at the keyboard, because
// the idle clock cannot be reset any other way (see the package comment).
//
//	RETROSAVER_LIVE=1 RETROSAVER_LIVE_INPUT=1 \
//	  go test ./internal/idle -run LiveIdleWatchReArms -v -timeout 5m
//
// Leave the machine alone until it says to move the mouse, then move it.
//
// This is informational: daemon.rearm re-registers its watches unconditionally,
// so the daemon is correct whichever way this comes out. A PASS means that
// belt-and-braces re-registration is redundant but harmless; a report that it
// did not re-fire means it is load-bearing.
func TestLiveIdleWatchReArmsWithRealInput(t *testing.T) {
	m := liveMonitor(t)
	if os.Getenv("RETROSAVER_LIVE_INPUT") == "" {
		t.Skip("set RETROSAVER_LIVE_INPUT=1; this test needs a human to move the mouse")
	}

	const interval = 3 * time.Second
	id, err := m.AddIdleWatch(interval)
	if err != nil {
		t.Fatalf("AddIdleWatch(%v) = %v", interval, err)
	}
	defer m.RemoveWatch(id) //nolint:errcheck // best effort in a test

	t.Logf(">>> LEAVE THE MACHINE ALONE for about %v <<<", interval)
	select {
	case got := <-m.Fired():
		if got != id {
			t.Fatalf("first fire = watch %d, want %d", got, id)
		}
		t.Log("idle watch fired once")
	case <-time.After(60 * time.Second):
		t.Fatal("no first fire within 60s; was the machine being used?")
	}

	t.Log(">>> NOW MOVE THE MOUSE <<<")
	if !waitForActivity(t, m, 60*time.Second) {
		t.Skip("the idle clock never reset; no input was given, so this is inconclusive")
	}
	t.Log("activity detected; now leave the machine alone again")

	select {
	case got := <-m.Fired():
		if got != id {
			t.Fatalf("second fire = watch %d, want %d", got, id)
		}
		t.Log("RESULT: idle watches re-arm themselves; daemon.rearm is belt-and-braces")
	case <-time.After(60 * time.Second):
		t.Log("RESULT: idle watches are one-shot; daemon.rearm's re-registration " +
			"is load-bearing and must stay")
	}
}

// waitForActivity polls until the idle clock goes backwards, which is the only
// reliable signal that real input arrived.
func waitForActivity(t *testing.T, m *Monitor, timeout time.Duration) bool {
	t.Helper()
	prev, err := m.Idletime()
	if err != nil {
		t.Fatalf("Idletime() = %v", err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		now, err := m.Idletime()
		if err != nil {
			t.Fatalf("Idletime() = %v", err)
		}
		if now < prev {
			return true
		}
		prev = now
	}
	return false
}
