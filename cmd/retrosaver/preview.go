//go:build linux

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"syscall"

	"github.com/c-premus/retrosaver/internal/modules"
)

// defaultPreviewGeometry matches scripts/demo.sh: big enough to see a module
// properly, small enough to sit beside the settings window.
const defaultPreviewGeometry = "1280x720"

var geometryPattern = regexp.MustCompile(`^[1-9][0-9]*x[1-9][0-9]*$`)

// cmdPreview runs one module in an ordinary window, for looking at it before
// selecting it. Nothing goes fullscreen and nothing of the daemon's is touched:
// no state files, no idle-delay, no running saver.
//
// It replaces itself with the module, so whoever started it -- the settings
// app -- holds the module's own process and stops it with one signal.
func cmdPreview(args []string) error {
	fset := flag.NewFlagSet("preview", flag.ExitOnError)
	geometry := fset.String("geometry", defaultPreviewGeometry, "window size, WIDTHxHEIGHT")
	_ = fset.Parse(args)
	if fset.NArg() != 1 {
		return errors.New("preview: want exactly one module name")
	}
	if os.Getenv("DISPLAY") == "" {
		return errors.New("preview: DISPLAY is unset, so XWayland is not reachable")
	}
	argv, err := previewArgv(modules.NewFinder(), fset.Arg(0), *geometry)
	if err != nil {
		return err
	}
	if err := syscall.Exec(argv[0], argv, os.Environ()); err != nil {
		return fmt.Errorf("preview: running %s: %w", argv[0], err)
	}
	return nil // not reached: Exec only returns on failure
}

// previewArgv builds the module's command line. The name must be a discovered
// display module, so a helper binary or a typo cannot be run this way.
// INCLUDE and EXCLUDE do not apply: looking at an excluded module is exactly
// what a preview is for.
func previewArgv(f *modules.Finder, name, geometry string) ([]string, error) {
	if !geometryPattern.MatchString(geometry) {
		return nil, fmt.Errorf("preview: geometry must look like %s, not %q", defaultPreviewGeometry, geometry)
	}
	discovered, err := f.Discover()
	if err != nil {
		return nil, err
	}
	if !slices.Contains(discovered, name) {
		return nil, fmt.Errorf("preview: %q is not an installed display module (see retrosaver list --all)", name)
	}
	return []string{filepath.Join(f.BinDir, name), "-window", "-geometry", geometry}, nil
}
