//go:build linux

package main

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

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

	noPackagedExample(t)

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
	noPackagedExample(t)
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

// SIGHUP must never be one of the signals handed to signal.NotifyContext.
//
// NotifyContext cancels the context on any signal it is given, so listing
// SIGHUP there turns `systemctl --user reload` into a shutdown: the daemon
// exits, and with it goes the screensaver and -- because setup handed it
// idle-delay -- the session's auto-lock until systemd restarts it.
func TestStopSignalsExcludeSIGHUP(t *testing.T) {
	if slices.Contains(stopSignals, os.Signal(syscall.SIGHUP)) {
		t.Fatal("SIGHUP is in stopSignals: a reload would shut the daemon down")
	}
	// Sanity check the other direction, so an empty slice cannot pass.
	for _, want := range []os.Signal{syscall.SIGTERM, syscall.SIGINT} {
		if !slices.Contains(stopSignals, want) {
			t.Errorf("stopSignals is missing %v", want)
		}
	}
}

// reloadTriggers must deliver a reload on SIGHUP, and must not die doing it.
func TestReloadTriggersFireOnSIGHUP(t *testing.T) {
	// Keep the config watch inside the test's own directory.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Model what cmdDaemon builds, using the very same signal list, so this
	// fails if SIGHUP is ever added to it.
	ctx, stop := signal.NotifyContext(context.Background(), stopSignals...)
	defer stop()

	ch := reloadTriggers(ctx)

	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("raising SIGHUP: %v", err)
	}
	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatal("the reload channel closed instead of firing")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reload after SIGHUP")
	}

	// The whole point: SIGHUP reloads, it does not shut the daemon down.
	if err := ctx.Err(); err != nil {
		t.Fatalf("SIGHUP cancelled the daemon context: %v", err)
	}
}

// A burst of signals collapses to one pending reload: the channel holds one
// and further sends are dropped, because the receiver re-reads the file anyway.
func TestReloadTriggersCoalesce(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := reloadTriggers(ctx)

	for range 5 {
		if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
			t.Fatalf("raising SIGHUP: %v", err)
		}
	}
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no reload after SIGHUP")
	}

	// Drain whatever the burst left, then assert it goes quiet rather than
	// delivering one reload per signal forever.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-ch:
			// Another coalesced batch; keep draining.
		case <-deadline:
			t.Fatal("reloads kept arriving; the burst did not coalesce")
		case <-time.After(500 * time.Millisecond):
			return // quiet
		}
	}
}

// Cancelling the context must stop the goroutine and release the SIGHUP
// handler, so a later SIGHUP does not resurrect a reload for a dead daemon.
func TestReloadTriggersStopOnCancel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())

	ch := reloadTriggers(ctx)
	cancel()

	// Drain anything already in flight, then require quiet.
	time.Sleep(200 * time.Millisecond)
	select {
	case <-ch:
	default:
	}

	signal.Reset(syscall.SIGHUP) // the goroutine's signal.Stop may not have run yet
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("a reload arrived after the context was cancelled")
		}
	case <-time.After(500 * time.Millisecond):
		// Nothing arrived, which is what we want.
	}
}

// noPackagedExample points exampleConfigPath at a file that does not exist, so
// installConfig falls back to the compiled-in defaults.
//
// Without this, any test calling installConfig reads the real
// /usr/share/retrosaver/retrosaver.conf.example whenever the .deb is installed,
// and the suite then asserts one thing on a packaging host and another in the
// devcontainer.
func noPackagedExample(t *testing.T) {
	t.Helper()
	prev := exampleConfigPath
	exampleConfigPath = filepath.Join(t.TempDir(), "no-such-example.conf")
	t.Cleanup(func() { exampleConfigPath = prev })
}

// packagedExample points exampleConfigPath at a file the test controls.
func packagedExample(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "retrosaver.conf.example")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := exampleConfigPath
	exampleConfigPath = path
	t.Cleanup(func() { exampleConfigPath = prev })
}

// installConfig prefers the packaged example over the compiled-in defaults.
func TestInstallConfigPrefersThePackagedExample(t *testing.T) {
	const packaged = "# packaged\nSAVER_DELAY=123\n"
	packagedExample(t, packaged)

	path := filepath.Join(t.TempDir(), "retrosaver.conf")
	if _, err := installConfig(path); err != nil {
		t.Fatalf("installConfig() = %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != packaged {
		t.Errorf("installConfig wrote %q, want the packaged example %q", b, packaged)
	}
}

// With no packaged example, setup still works from a source build.
func TestInstallConfigFallsBackToTheCompiledDefaults(t *testing.T) {
	noPackagedExample(t)

	path := filepath.Join(t.TempDir(), "retrosaver.conf")
	if _, err := installConfig(path); err != nil {
		t.Fatalf("installConfig() = %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != defaultConfigFile() {
		t.Error("installConfig did not fall back to the compiled-in defaults")
	}
}
