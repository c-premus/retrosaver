//go:build linux

package window

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jezek/xgb"
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

// TestLiveLaunchCoversEveryMonitor launches a module and checks that there is
// one saver window per monitor, each exactly covering it, with a live copy of
// the module drawing in it. The window manager is the only judge of the
// geometry, so the evidence is what xwininfo reports afterwards.
func TestLiveLaunchCoversEveryMonitor(t *testing.T) {
	requireLiveDisplay(t)
	const path = moduleBinDir + "anemone"
	if _, err := os.Stat(path); err != nil {
		t.Skipf("%s not installed: %v", path, err)
	}
	// Keep this test's state files away from a running daemon's.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	display := moduleDisplay()
	conn, err := xgb.NewConnDisplay(display)
	if err != nil {
		t.Fatalf("connecting to %q: %v", display, err)
	}
	mons, err := monitors(conn)
	conn.Close()
	if err != nil {
		t.Fatalf("monitors() = %v", err)
	}
	t.Logf("monitors: %+v", mons)

	s, err := Launch(path)
	if err != nil {
		t.Fatalf("Launch(%s) = %v", path, err)
	}
	t.Cleanup(func() { _ = s.Stop() })

	if len(mons) == 0 {
		t.Fatal("Xinerama reports no monitors")
	}
	if got := len(s.screen.windows); got != len(mons) {
		t.Fatalf("%d monitors, %d saver windows", len(mons), got)
	}
	if got := len(s.PIDs()); got != len(mons) {
		t.Fatalf("%d monitors, %d copies launched", len(mons), got)
	}

	// Give the modules time to fail, then insist every copy is still drawing.
	time.Sleep(time.Second)
	for i, c := range s.children {
		if c.reaped() {
			t.Errorf("copy %d exited (%v): %s", i, c.waitErr, c.stderr.String())
		}
	}
	for i, w := range s.screen.windows {
		if got := windowGeometry(t, fmt.Sprintf("0x%x", uint32(w))); got != mons[i] {
			t.Errorf("saver window %d covers %v, want monitor %v", i, got, mons[i])
		}
	}
}

// windowGeometry is window id's absolute rectangle, as xwininfo reports it --
// an independent reading, not the one the launch itself relied on.
func windowGeometry(t *testing.T, id string) monitor {
	t.Helper()
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

// TestLiveReplaceKeepsTheWindow is the MONITORS=different path: each monitor
// gets its own module through LaunchEach, then Replace puts another module in
// the primary monitor's window. The window must be the same one, still
// covering its monitor, with the new module alive in it and every other
// monitor's process untouched.
func TestLiveReplaceKeepsTheWindow(t *testing.T) {
	requireLiveDisplay(t)
	paths := []string{moduleBinDir + "anemone", moduleBinDir + "flame"}
	for _, p := range append(paths, moduleBinDir+"atlantis") {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("%s not installed: %v", p, err)
		}
	}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	s, err := LaunchEach(t.Context(), paths...)
	if err != nil {
		t.Fatalf("LaunchEach = %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	t.Logf("modules: %v", s.Modules())

	before := s.PIDs()
	window := fmt.Sprintf("0x%x", uint32(s.screen.windows[0]))
	if err := s.Replace(0, moduleBinDir+"atlantis"); err != nil {
		t.Fatalf("Replace = %v", err)
	}

	time.Sleep(time.Second)
	after := s.PIDs()
	if after[0] == before[0] {
		t.Errorf("monitor 0 still runs pid %d, want a new process", before[0])
	}
	if s.children[0].reaped() {
		t.Errorf("the replacement exited (%v): %s", s.children[0].waitErr, s.children[0].stderr.String())
	}
	if got := s.Modules()[0]; got != "atlantis" {
		t.Errorf("monitor 0 runs %s, want atlantis", got)
	}
	for i := 1; i < len(after); i++ {
		if after[i] != before[i] {
			t.Errorf("monitor %d's process changed from %d to %d; Replace must leave it alone", i, before[i], after[i])
		}
	}
	if got := windowGeometry(t, window); got != s.screen.mons[0] {
		t.Errorf("after Replace, window %s covers %v, want monitor %v", window, got, s.screen.mons[0])
	}
	select {
	case e := <-s.Exits():
		t.Errorf("Exits() reported %+v; the process Replace stopped is retired, not dead", e)
	default:
	}
}
