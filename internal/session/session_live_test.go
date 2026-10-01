package session

import (
	"os"
	"testing"
	"time"
)

// Live tests against a real GNOME session. Gated on RETROSAVER_LIVE so that
// CI and any non-GNOME machine skip them:
//
//	RETROSAVER_LIVE=1 go test ./internal/session -run Live -v

func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("RETROSAVER_LIVE") == "" {
		t.Skip("set RETROSAVER_LIVE=1 to run against a real GNOME session")
	}
}

// TestLiveIdleDelayRoundTrip exercises the real gsettings path and puts the
// user's original value back, whatever happens.
func TestLiveIdleDelayRoundTrip(t *testing.T) {
	requireLive(t)

	original, err := IdleDelay()
	if err != nil {
		t.Fatalf("IdleDelay() = %v", err)
	}
	t.Logf("original idle-delay = %d", original)

	// Restore first thing, so a failure below cannot leave the session
	// without an auto-lock.
	t.Cleanup(func() {
		if err := SetIdleDelay(original); err != nil {
			t.Errorf("restoring idle-delay to %d: %v", original, err)
		}
	})

	for _, want := range []int{0, 10, original} {
		if err := SetIdleDelay(want); err != nil {
			t.Fatalf("SetIdleDelay(%d) = %v", want, err)
		}
		got, err := IdleDelay()
		if err != nil {
			t.Fatalf("IdleDelay() after setting %d = %v", want, err)
		}
		if got != want {
			t.Errorf("round trip through gsettings: set %d, read back %d", want, got)
		}
	}
}

func TestLiveGraphicalSessionID(t *testing.T) {
	requireLive(t)

	id, err := graphicalSessionID()
	if err != nil {
		t.Fatalf("graphicalSessionID() = %v", err)
	}
	if id == "" {
		t.Fatal("graphicalSessionID() = \"\", want a logind session id")
	}
	t.Logf("graphical session id = %q", id)
}

// TestLiveLocked reads the real LockedHint. It only reads, so it needs no
// opt-in beyond RETROSAVER_LIVE.
//
// This is the only thing that proves the invocation is right: the fake in
// session_test.go asserts the arguments we chose, not that loginctl accepts
// them or that logind spells the value the way parseLockedHint expects.
func TestLiveLocked(t *testing.T) {
	requireLive(t)

	locked, err := Locked()
	if err != nil {
		t.Fatalf("Locked() = %v", err)
	}
	t.Logf("session LockedHint = %v", locked)
}

// TestLiveLock genuinely locks the screen, so it needs its own opt-in beyond
// RETROSAVER_LIVE. Unlock the session afterwards to continue.
//
//	RETROSAVER_LIVE=1 RETROSAVER_LIVE_LOCK=1 go test ./internal/session -run LiveLock -v
func TestLiveLock(t *testing.T) {
	requireLive(t)
	if os.Getenv("RETROSAVER_LIVE_LOCK") == "" {
		t.Skip("set RETROSAVER_LIVE_LOCK=1 as well: this test locks the screen for real")
	}

	if err := Lock(); err != nil {
		t.Fatalf("Lock() = %v", err)
	}
	t.Log("Lock() returned; the session should now be showing GNOME's lock screen")
}

// TestLiveWatchLocksSubscribes proves the system bus subscription resolves the
// graphical session and installs its match rule, without locking anything.
func TestLiveWatchLocksSubscribes(t *testing.T) {
	requireLive(t)

	w, err := WatchLocks()
	if err != nil {
		t.Fatalf("WatchLocks() = %v", err)
	}
	t.Logf("watching %s", w.path)
	if err := w.Close(); err != nil {
		t.Errorf("Close() = %v", err)
	}
	if _, open := <-w.Changes(); open {
		t.Error("Changes() still open after Close")
	}
}

// TestLiveWatchLocksSeesALock locks the session for real and expects the
// watcher to report it. Gated like TestLiveLock:
//
//	RETROSAVER_LIVE=1 RETROSAVER_LIVE_LOCK=1 go test ./internal/session -run LiveWatchLocksSeesALock -v
func TestLiveWatchLocksSeesALock(t *testing.T) {
	requireLive(t)
	if os.Getenv("RETROSAVER_LIVE_LOCK") == "" {
		t.Skip("set RETROSAVER_LIVE_LOCK=1 as well: this test locks the screen for real")
	}

	w, err := WatchLocks()
	if err != nil {
		t.Fatalf("WatchLocks() = %v", err)
	}
	defer w.Close()

	if err := Lock(); err != nil {
		t.Fatalf("Lock() = %v", err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case locked := <-w.Changes():
			if locked {
				return
			}
		case <-deadline:
			t.Fatal("no LockedHint=true within 10s of locking")
		}
	}
}
