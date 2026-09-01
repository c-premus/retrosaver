package modules

import (
	"os"
	"path/filepath"
	"testing"
)

// fixture builds a fake install tree: an XML config dir and a bin dir.
// Entries in xml get a <name>.xml; entries in bins get an executable file.
// Entries in helpers get an executable with no XML, i.e. a helper binary.
// Entries in orphans get an XML with no executable.
func fixture(t *testing.T, both, helpers, orphans []string) *Finder {
	t.Helper()
	root := t.TempDir()
	cfgDir := filepath.Join(root, "config")
	binDir := filepath.Join(root, "bin")
	for _, d := range []string{cfgDir, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeXML := func(n string) {
		if err := os.WriteFile(filepath.Join(cfgDir, n+".xml"), []byte("<screensaver/>"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeBin := func(n string) {
		if err := os.WriteFile(filepath.Join(binDir, n), []byte("#!/bin/true\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range both {
		writeXML(n)
		writeBin(n)
	}
	for _, n := range helpers {
		writeBin(n)
	}
	for _, n := range orphans {
		writeXML(n)
	}
	return &Finder{ConfigDir: cfgDir, BinDir: binDir}
}

// The intersection rule is the whole point: a helper binary has no XML config,
// so it must never appear as a selectable module.
func TestDiscoverExcludesHelpersAndOrphans(t *testing.T) {
	f := fixture(t,
		[]string{"atlantis", "flame", "ifs"},
		[]string{"xscreensaver-gl-helper", "xscreensaver-text"},
		[]string{"uninstalled-module"},
	)
	got, err := f.Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := []string{"atlantis", "flame", "ifs"}
	if len(got) != len(want) {
		t.Fatalf("Discover() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Discover() = %v, want %v (sorted)", got, want)
		}
	}
}

// A non-executable file in the bin dir is not a module.
func TestDiscoverRequiresExecutableBit(t *testing.T) {
	f := fixture(t, []string{"atlantis"}, nil, nil)
	if err := os.WriteFile(filepath.Join(f.ConfigDir, "readme.xml"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.BinDir, "readme"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := f.Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(got) != 1 || got[0] != "atlantis" {
		t.Errorf("Discover() = %v, want [atlantis]; non-executable must be skipped", got)
	}
}

func TestSelectIncludeRestrictsAndExcludeWins(t *testing.T) {
	discovered := []string{"atlantis", "flame", "ifs", "sonar"}

	if got := Select(discovered, nil, []string{"sonar"}); len(got) != 3 {
		t.Errorf("empty include must mean all: got %v", got)
	}
	got := Select(discovered, []string{"atlantis", "flame"}, nil)
	if len(got) != 2 || got[0] != "atlantis" || got[1] != "flame" {
		t.Errorf("include must restrict exactly: got %v", got)
	}
	// Exclude is applied after include, so it wins on a conflict.
	got = Select(discovered, []string{"atlantis", "sonar"}, []string{"sonar"})
	if len(got) != 1 || got[0] != "atlantis" {
		t.Errorf("exclude must win over include: got %v", got)
	}
}

// An empty selection is an error, not a silent no-op: the caller has nothing
// to launch and must say so.
func TestAvailableFailsWhenSelectionEmpty(t *testing.T) {
	f := fixture(t, []string{"atlantis"}, nil, nil)
	if _, err := f.Available(nil, []string{"atlantis"}); err == nil {
		t.Fatal("Available() = nil error, want failure when everything is excluded")
	}
}

func TestPickReturnsSelectableModule(t *testing.T) {
	f := fixture(t, []string{"atlantis", "flame", "ifs"}, nil, nil)
	for range 20 {
		got, err := f.Pick(nil, []string{"ifs"})
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		if got == "ifs" {
			t.Fatal("Pick returned an excluded module")
		}
		if got != "atlantis" && got != "flame" {
			t.Fatalf("Pick returned unknown module %q", got)
		}
	}
}
