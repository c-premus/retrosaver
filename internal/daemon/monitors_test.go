package daemon

import (
	"errors"
	"slices"
	"testing"

	"github.com/c-premus/retrosaver/internal/config"
)

// The tests for MONITORS=different: a module per monitor, a swap that changes
// the whole set, and a death that is made good on its own monitor.

// different turns on a module per monitor, on top of cfg.
func different(cfg config.Config) config.Config {
	cfg.Monitors = config.MonitorsDifferent
	return cfg
}

// twoMonitors gives the fake launcher two monitors and an avoid-honouring
// pick over names.
func twoMonitors(names ...string) func(*harness) {
	return func(h *harness) {
		h.lau.names = names
		h.lau.honourAvoid = true
		h.lau.monitors = 2
	}
}

func TestEachMonitorGetsItsOwnModule(t *testing.T) {
	h := start(t, different(defaultConfig()), twoMonitors("atlantis", "flame", "ifs"))

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis+flame")
	if got, want := h.lau.askedFor(), []string{"atlantis+flame"}; !slices.Equal(got, want) {
		t.Errorf("modules launched = %v, want %v (one launch, one module per monitor)", got, want)
	}
}

// The default must be exactly what it was: one module, a copy on each monitor.
func TestSameModeIgnoresTheMonitorCount(t *testing.T) {
	h := start(t, defaultConfig(), twoMonitors("atlantis", "flame"))

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")
}

func TestAFailedMonitorCountFallsBackToOneModule(t *testing.T) {
	h := start(t, different(defaultConfig()), twoMonitors("atlantis", "flame"), func(h *harness) {
		h.lau.monitorsErr = errors.New("no X display")
	})

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis")

	// And a death then takes the whole-saver path, as in same mode.
	h.lau.saverAt(t, 0).die()
	h.want("module:exited:atlantis")
	h.want("launch:ok:flame")
}

func TestASwapChangesEveryMonitor(t *testing.T) {
	h := start(t, different(cyclingConfig()), twoMonitors("a", "b", "c", "d", "e"))

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:a+b")
	h.fire(wCycle, "watch:cycle")
	h.want("launch:ok:c+d")

	if got := h.lau.saverAt(t, 0).stopCount(); got != 1 {
		t.Errorf("outgoing set stopped %d times, want 1", got)
	}
}

func TestFewerModulesThanMonitorsRepeatsOne(t *testing.T) {
	h := start(t, different(defaultConfig()), twoMonitors("a", "b"), func(h *harness) {
		h.lau.monitors = 3
	})

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:a+b+a")
}

// With exactly as many modules as monitors, every swap would pick the same
// set again. Tearing it down to put it straight back is the flicker the
// single-module skip exists to avoid.
func TestASwapToTheSameSetIsSkipped(t *testing.T) {
	h := start(t, different(cyclingConfig()), twoMonitors("a", "b"))

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:a+b")
	h.fire(wCycle, "cycle:skipped")
	h.want("watch:cycle")

	if got := h.lau.saverAt(t, 0).stopCount(); got != 0 {
		t.Errorf("running set stopped %d times, want 0", got)
	}
}

func TestADeadModuleIsReplacedOnItsOwnMonitor(t *testing.T) {
	h := start(t, different(defaultConfig()), twoMonitors("atlantis", "flame", "ifs"))

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:atlantis+flame")

	s := h.lau.saverAt(t, 0)
	s.dieOn(1, "flame")
	h.want("module:exited:1:flame")
	h.want("relaunch:1:ifs")

	if got, want := s.replacements(), []string{"1:ifs"}; !slices.Equal(got, want) {
		t.Errorf("Replace calls = %v, want %v", got, want)
	}
	// The other monitor is left alone: no new launch, nothing stopped.
	if got := len(h.lau.askedFor()); got != 1 {
		t.Errorf("Launch called %d times, want 1", got)
	}
	if got := s.stopCount(); got != 0 {
		t.Errorf("saver stopped %d times, want 0", got)
	}
}

// The replacement must differ from what the other monitor is showing, not
// just from what has been shown.
func TestAReplacementAvoidsTheOtherMonitorsModule(t *testing.T) {
	h := start(t, different(defaultConfig()), twoMonitors("a", "b", "c"))

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:a+b")
	s := h.lau.saverAt(t, 0)
	s.dieOn(0, "a")
	h.want("module:exited:0:a")
	h.want("relaunch:0:c")

	// Every module has now been shown. The pool starts over, but monitor 1
	// still shows b, so monitor 0 must not get it.
	s.dieOn(0, "c")
	h.want("module:exited:0:c")
	h.want("relaunch:0:a")
}

func TestRelaunchingIsCappedPerMonitor(t *testing.T) {
	h := start(t, different(defaultConfig()), twoMonitors("a", "b", "c", "d", "e", "f", "g"))

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:a+b")
	s := h.lau.saverAt(t, 0)
	dying := "a"
	for _, next := range []string{"c", "d", "e"} {
		s.dieOn(0, dying)
		h.want("module:exited:0:" + dying)
		h.want("relaunch:0:" + next)
		dying = next
	}
	s.dieOn(0, dying)
	h.want("module:exited:0:e")
	h.want("relaunch:capped")

	// The cap is monitor 0's alone: monitor 1 is still looked after.
	s.dieOn(1, "b")
	h.want("module:exited:1:b")
	h.want("relaunch:1:f")
}

// A replacement that cannot even be started counts against the cap and is
// followed by another, rather than leaving the monitor black at once.
func TestAReplacementThatFailsToStartIsFollowedByAnother(t *testing.T) {
	h := start(t, different(defaultConfig()), twoMonitors("a", "b", "c", "d"), func(h *harness) {
		h.lau.replaceErr = map[string]error{"c": errors.New("exec format error")}
	})

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:a+b")
	s := h.lau.saverAt(t, 0)
	s.dieOn(1, "b")
	h.want("module:exited:1:b")
	h.want("relaunch:1:d")

	if got, want := s.replacements(), []string{"1:c", "1:d"}; !slices.Equal(got, want) {
		t.Errorf("Replace calls = %v, want %v", got, want)
	}
}

func TestADeadModuleIsNotReplacedBehindALockOnItsMonitor(t *testing.T) {
	h := start(t, different(defaultConfig()), twoMonitors("a", "b", "c"))

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:a+b")
	h.sess.setLocked(true)
	s := h.lau.saverAt(t, 0)
	s.dieOn(1, "b")
	h.want("module:exited:1:b")
	h.want("launch:suppressed")

	if got := s.replacements(); len(got) != 0 {
		t.Errorf("Replace calls = %v, want none behind the lock", got)
	}
}

// Once a swap has replaced the set, the outgoing saver is no longer watched:
// anything it reports must not reach the new one.
func TestTheOutgoingSetIsNoLongerWatched(t *testing.T) {
	h := start(t, different(cyclingConfig()), twoMonitors("a", "b", "c", "d", "e"))

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:a+b")
	h.fire(wCycle, "watch:cycle")
	h.want("launch:ok:c+d")

	h.lau.saverAt(t, 0).dieOn(0, "a")
	// A relaunch would put a trace tag ahead of this one.
	h.fire(wLock, "watch:lock")
	if got := h.lau.saverAt(t, 1).replacements(); len(got) != 0 {
		t.Errorf("Replace calls on the new set = %v, want none", got)
	}
}

func TestUserActivityResetsThePerMonitorCap(t *testing.T) {
	h := start(t, different(defaultConfig()), twoMonitors("a", "b", "c", "d", "e", "f"))

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:a+b")
	s := h.lau.saverAt(t, 0)
	dying := "a"
	for _, next := range []string{"c", "d", "e"} {
		s.dieOn(0, dying)
		h.want("module:exited:0:" + dying)
		h.want("relaunch:0:" + next)
		dying = next
	}
	h.fire(wActive, "watch:active")

	h.fire(wSaver2, "watch:saver")
	h.want("launch:ok:a+b")
	s2 := h.lau.saverAt(t, 1)
	s2.dieOn(0, "a")
	h.want("module:exited:0:a")
	h.want("relaunch:0:c")
}

// sameConfig must see MONITORS, or switching it in the file would be
// swallowed as an unchanged reload.
func TestReloadAppliesAChangedMonitorsSetting(t *testing.T) {
	h := start(t, defaultConfig(), twoMonitors("a", "b", "c"))

	h.reload(different(defaultConfig()), "reload:ok")
	// No stage ran before the reload, so no user-active watch took id 4 and
	// the re-armed saver watch has it.
	h.fire(4, "watch:saver")
	h.want("launch:ok:a+b")
}

// The monitor count is read afresh for every launch, so a swap after a monitor
// is unplugged covers the layout as it now is. The skip check must compare the
// whole set: the new pick can start with the module already on monitor 0 and
// still differ.
func TestASwapAfterAMonitorIsUnpluggedIsNotSkipped(t *testing.T) {
	h := start(t, different(cyclingConfig()), twoMonitors("a", "b"))

	h.fire(wSaver, "watch:saver")
	h.want("launch:ok:a+b")

	h.lau.mu.Lock()
	h.lau.monitors = 1
	h.lau.mu.Unlock()
	h.fire(wCycle, "watch:cycle")
	h.want("launch:ok:a")
}
