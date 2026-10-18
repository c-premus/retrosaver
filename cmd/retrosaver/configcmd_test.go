//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/c-premus/retrosaver/internal/config"
	"github.com/c-premus/retrosaver/internal/modules"
)

func TestConfigShowJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retrosaver.conf")

	// Missing: the defaults, reported as not existing.
	var buf bytes.Buffer
	if err := showConfig(&buf, path, true); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["exists"] != false || got["SAVER_DELAY"] != float64(300) || got["MONITORS"] != "same" {
		t.Errorf("defaults = %v", got)
	}
	// An empty INCLUDE must be [] so a consumer can iterate it unconditionally.
	if inc, ok := got["INCLUDE"].([]any); !ok || len(inc) != 0 {
		t.Errorf("INCLUDE = %#v, want []", got["INCLUDE"])
	}
	// Every config key appears, so the contract cannot lose one quietly.
	for _, k := range config.Keys {
		if _, ok := got[k]; !ok {
			t.Errorf("JSON lacks %s", k)
		}
	}

	if err := os.WriteFile(path, []byte("SAVER_DELAY=15\nINCLUDE=\"flame ifs\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := showConfig(&buf, path, true); err != nil {
		t.Fatal(err)
	}
	var cj configJSON
	if err := json.Unmarshal(buf.Bytes(), &cj); err != nil {
		t.Fatal(err)
	}
	if !cj.Exists || cj.SaverDelay != 15 || strings.Join(cj.Include, ",") != "flame,ifs" {
		t.Errorf("show = %+v", cj)
	}
}

// A front end that displayed the defaults for a broken file would then write
// them back over the user's settings.
func TestConfigShowRefusesABrokenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retrosaver.conf")
	if err := os.WriteFile(path, []byte("MONITORS=both\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := showConfig(&bytes.Buffer{}, path, true); err == nil {
		t.Error("showConfig succeeded on an unparseable file")
	}
}

func TestConfigSetThroughTheCommand(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := dispatch("config", []string{"set", "SAVER_DELAY=600", "MONITORS=different"}); err != nil {
		t.Fatal(err)
	}
	path, err := config.UserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SaverDelay.Seconds() != 600 || !cfg.DifferentPerMonitor() {
		t.Errorf("after set: %+v", cfg)
	}
}

func TestParseAssignments(t *testing.T) {
	got, err := parseAssignments([]string{"A=1", "B=", "C=x=y"})
	if err != nil {
		t.Fatal(err)
	}
	if got["A"] != "1" || got["B"] != "" || got["C"] != "x=y" {
		t.Errorf("got %v", got)
	}
	for _, bad := range [][]string{nil, {"A"}, {"=1"}, {"A=1", "A=2"}} {
		if _, err := parseAssignments(bad); err == nil {
			t.Errorf("parseAssignments(%q) succeeded", bad)
		}
	}
}

func fixtureFinder(t *testing.T, names ...string) *modules.Finder {
	t.Helper()
	cfgDir, binDir := t.TempDir(), t.TempDir()
	for _, n := range names {
		xml := `<screensaver _label="` + strings.ToUpper(n) + `"><_description>About ` + n + `.</_description></screensaver>`
		if err := os.WriteFile(filepath.Join(cfgDir, n+".xml"), []byte(xml), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(binDir, n), nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &modules.Finder{ConfigDir: cfgDir, BinDir: binDir}
}

func TestListAllJSON(t *testing.T) {
	f := fixtureFinder(t, "atlantis", "flame", "sonar")
	cfg := config.Defaults() // excludes sonar

	var buf bytes.Buffer
	if err := listModules(&buf, f, cfg, true, true); err != nil {
		t.Fatal(err)
	}
	var got []moduleJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []moduleJSON{
		{modules.Info{Name: "atlantis", Label: "ATLANTIS", Description: "About atlantis."}, true},
		{modules.Info{Name: "flame", Label: "FLAME", Description: "About flame."}, true},
		{modules.Info{Name: "sonar", Label: "SONAR", Description: "About sonar."}, false},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %s", len(got), len(want), buf.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// The JSON key is "name", flattened from the embedded Info.
	if !strings.Contains(buf.String(), `"name": "atlantis"`) {
		t.Errorf("unexpected JSON shape:\n%s", buf.String())
	}
}

func TestListWithNothingSelected(t *testing.T) {
	f := fixtureFinder(t, "flame")
	cfg := config.Defaults()
	cfg.Exclude = []string{"flame"}

	// Plain list keeps failing loudly, as it always has.
	if err := listModules(&bytes.Buffer{}, f, cfg, false, false); err == nil {
		t.Error("plain list with an empty selection succeeded")
	}
	// JSON with --all still lists it, unselected, so it can be re-ticked.
	var buf bytes.Buffer
	if err := listModules(&buf, f, cfg, true, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"selected": false`) {
		t.Errorf("got %s", buf.String())
	}
}
