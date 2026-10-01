//go:build linux

package window

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Live tests against a real GNOME session. Gated on RETROSAVER_LIVE so that
// CI and any non-GNOME machine skip them, and on RETROSAVER_LIVE_DISPLAY as
// well, because a launch takes over every monitor for a few seconds:
//
//	RETROSAVER_LIVE=1 RETROSAVER_LIVE_DISPLAY=1 go test ./internal/window -run Live -v

func requireLiveDisplay(t *testing.T) {
	t.Helper()
	if os.Getenv("RETROSAVER_LIVE") == "" || os.Getenv("RETROSAVER_LIVE_DISPLAY") == "" {
		t.Skip("set RETROSAVER_LIVE=1 and RETROSAVER_LIVE_DISPLAY=1 to launch a module on a real session")
	}
}

// TestLiveLaunchCoversEveryMonitor launches a module and checks that each
// copy's window ends up exactly covering its monitor. The window manager is the
// only judge of that, so the evidence is the geometry X reports afterwards, not
// anything the launch itself returns.
func TestLiveLaunchCoversEveryMonitor(t *testing.T) {
	requireLiveDisplay(t)
	const path = moduleBinDir + "anemone"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("%s not installed: %v", path, err)
	}
	// Keep this test's state files away from a running daemon's.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	display := moduleDisplay()
	mons, err := monitors(display)
	if err != nil {
		t.Fatalf("monitors(%q) = %v", display, err)
	}
	t.Logf("monitors: %+v", mons)

	s, err := Launch(path)
	if err != nil {
		t.Fatalf("Launch(%s) = %v", path, err)
	}
	t.Cleanup(func() { _ = s.Stop() })

	pids := s.PIDs()
	if len(mons) < 2 {
		if len(pids) != 1 {
			t.Fatalf("one monitor, %d copies launched", len(pids))
		}
		return
	}
	if len(pids) != len(mons) {
		t.Fatalf("%d monitors, %d copies launched", len(mons), len(pids))
	}
	for i, pid := range pids {
		want := mons[i]
		// Fullscreen is applied asynchronously after wmctrl returns.
		var got monitor
		for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			if got = windowGeometry(t, pid); got == want {
				break
			}
		}
		if got != want {
			t.Errorf("copy %d (pid %d) covers %+v, want monitor %+v", i, pid, got, want)
		}
	}
}

// windowGeometry is the absolute rectangle of pid's visible window, as xwininfo
// reports it.
func windowGeometry(t *testing.T, pid int) monitor {
	t.Helper()
	out, err := exec.Command("xdotool", "search", "--onlyvisible", "--pid", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("xdotool search --pid %d: %v", pid, err)
	}
	id, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	info, err := exec.Command("xwininfo", "-id", id).Output()
	if err != nil {
		t.Fatalf("xwininfo -id %s: %v", id, err)
	}
	var m monitor
	fields := map[string]*int{
		"Absolute upper-left X:": &m.x,
		"Absolute upper-left Y:": &m.y,
		"Width:":                 &m.width,
		"Height:":                &m.height,
	}
	for line := range strings.SplitSeq(string(info), "\n") {
		line = strings.TrimSpace(line)
		for prefix, dst := range fields {
			if v, ok := strings.CutPrefix(line, prefix); ok {
				*dst, _ = strconv.Atoi(strings.TrimSpace(v))
			}
		}
	}
	return m
}
