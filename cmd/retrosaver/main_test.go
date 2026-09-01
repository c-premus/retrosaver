//go:build linux

package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/c-premus/retrosaver/internal/config"
)

// The config setup writes when the packaged example is absent must parse back
// to exactly the compiled-in defaults. Without this, the renderer and
// config.Defaults can drift and a source-built setup would silently install
// different settings from the ones the package documents.
func TestDefaultConfigFileRoundTripsThroughTheParser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retrosaver.conf")
	if err := os.WriteFile(path, []byte(defaultConfigFile()), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load on the generated file = %v\n---\n%s", err, defaultConfigFile())
	}

	want := config.Defaults()
	if got.SaverDelay != want.SaverDelay {
		t.Errorf("SaverDelay = %v, want %v", got.SaverDelay, want.SaverDelay)
	}
	if got.LockAfter != want.LockAfter {
		t.Errorf("LockAfter = %v, want %v", got.LockAfter, want.LockAfter)
	}
	if got.BlankAfter != want.BlankAfter {
		t.Errorf("BlankAfter = %v, want %v", got.BlankAfter, want.BlankAfter)
	}
	if !slices.Equal(got.Exclude, want.Exclude) {
		t.Errorf("Exclude = %v, want %v", got.Exclude, want.Exclude)
	}
	if !slices.Equal(got.Include, want.Include) {
		t.Errorf("Include = %v, want %v", got.Include, want.Include)
	}
}

// The example the .deb ships must agree with the compiled-in defaults too,
// since setup prefers it over the generated form.
func TestPackagedExampleAgreesWithDefaults(t *testing.T) {
	const repoExample = "../../config/retrosaver.conf.example"
	if _, err := os.Stat(repoExample); err != nil {
		t.Skipf("example config not found at %s", repoExample)
	}

	got, err := config.Load(repoExample)
	if err != nil {
		t.Fatalf("config.Load(%s) = %v", repoExample, err)
	}

	want := config.Defaults()
	if got.SaverDelay != want.SaverDelay ||
		got.LockAfter != want.LockAfter ||
		got.BlankAfter != want.BlankAfter {
		t.Errorf("example delays = %v/%v/%v, defaults = %v/%v/%v",
			got.SaverDelay, got.LockAfter, got.BlankAfter,
			want.SaverDelay, want.LockAfter, want.BlankAfter)
	}
	if !slices.Equal(got.Exclude, want.Exclude) {
		t.Errorf("example EXCLUDE = %v, defaults = %v", got.Exclude, want.Exclude)
	}
}

func TestInstallConfigNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retrosaver.conf")
	const existing = "SAVER_DELAY=42\n"
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}

	created, err := installConfig(path)
	if err != nil {
		t.Fatalf("installConfig() = %v", err)
	}
	if created {
		t.Error("installConfig reported creating a file that already existed")
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != existing {
		t.Errorf("installConfig overwrote an existing config: got %q, want %q", b, existing)
	}
}

func TestInstallConfigCreatesMissingDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "retrosaver", "retrosaver.conf")

	created, err := installConfig(path)
	if err != nil {
		t.Fatalf("installConfig() = %v", err)
	}
	if !created {
		t.Error("installConfig reported not creating a file that was absent")
	}
	if _, err := config.Load(path); err != nil {
		t.Errorf("the written config does not parse: %v", err)
	}
}

func TestDispatchRejectsAnUnknownCommand(t *testing.T) {
	if err := dispatch("nonsense", nil); err == nil {
		t.Error("dispatch(\"nonsense\") = nil, want an error")
	}
}

func TestDispatchHandlesVersionAndHelp(t *testing.T) {
	for _, cmd := range []string{"version", "--version", "-v", "help", "--help", "-h"} {
		if err := dispatch(cmd, nil); err != nil {
			t.Errorf("dispatch(%q) = %v, want nil", cmd, err)
		}
	}
}
