// Package modules discovers the XScreenSaver display modules installed on the
// system.
//
// Terminology: XScreenSaver upstream calls these "hacks", in the demoscene
// sense. This project says "module" throughout to avoid the connotation.
package modules

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Default locations on Debian and Ubuntu.
const (
	DefaultConfigDir = "/usr/share/xscreensaver/config"
	DefaultBinDir    = "/usr/libexec/xscreensaver"
)

// Finder locates display modules. The directories are fields rather than
// constants so tests can point at fixtures.
type Finder struct {
	// ConfigDir holds one <module>.xml per genuine display module.
	ConfigDir string
	// BinDir holds the module executables, alongside helper binaries.
	BinDir string
}

// NewFinder returns a Finder pointing at the distro's locations.
func NewFinder() *Finder {
	return &Finder{ConfigDir: DefaultConfigDir, BinDir: DefaultBinDir}
}

// Discover returns the sorted names of installed display modules.
//
// The rule is an intersection: every genuine display module ships an XML
// config file, and helper binaries do not. Intersecting the XML basenames with
// the executables in BinDir therefore yields the real module list without
// maintaining a hardcoded inventory that would rot at every package update.
func (f *Finder) Discover() ([]string, error) {
	configured, err := f.configuredNames()
	if err != nil {
		return nil, err
	}
	executables, err := f.executableNames()
	if err != nil {
		return nil, err
	}

	var out []string
	for name := range configured {
		if executables[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// configuredNames returns the basenames of ConfigDir/*.xml.
func (f *Finder) configuredNames() (map[string]bool, error) {
	matches, err := filepath.Glob(filepath.Join(f.ConfigDir, "*.xml"))
	if err != nil {
		return nil, fmt.Errorf("globbing %s: %w", f.ConfigDir, err)
	}
	names := make(map[string]bool, len(matches))
	for _, m := range matches {
		names[strings.TrimSuffix(filepath.Base(m), ".xml")] = true
	}
	return names, nil
}

// executableNames returns the names of regular, executable files in BinDir.
func (f *Finder) executableNames() (map[string]bool, error) {
	entries, err := os.ReadDir(f.BinDir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", f.BinDir, err)
	}
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if info.Mode().Perm()&0o111 == 0 {
			continue
		}
		names[e.Name()] = true
	}
	return names, nil
}

// Select applies the include allowlist and exclude denylist to a discovered
// list. An empty include list means "all discovered modules". Exclude is
// applied second, so it wins over include.
func Select(discovered, include, exclude []string) []string {
	allow := toSet(include)
	deny := toSet(exclude)

	var out []string
	for _, name := range discovered {
		if len(allow) > 0 && !allow[name] {
			continue
		}
		if deny[name] {
			continue
		}
		out = append(out, name)
	}
	return out
}

// Available returns the selectable modules for a given include/exclude pair.
// It returns an error when the selection is empty, since a caller with nothing
// to launch needs to fail loudly rather than pick nothing.
func (f *Finder) Available(include, exclude []string) ([]string, error) {
	discovered, err := f.Discover()
	if err != nil {
		return nil, err
	}
	selected := Select(discovered, include, exclude)
	if len(selected) == 0 {
		return nil, fmt.Errorf(
			"no modules available: %d discovered in %s, none survived INCLUDE/EXCLUDE",
			len(discovered), f.BinDir)
	}
	return selected, nil
}

// Pick chooses a module at random from the selectable set.
func (f *Finder) Pick(include, exclude []string) (string, error) {
	available, err := f.Available(include, exclude)
	if err != nil {
		return "", err
	}
	return available[rand.IntN(len(available))], nil
}

// Path returns the absolute path to a module's executable.
func (f *Finder) Path(name string) string {
	return filepath.Join(f.BinDir, name)
}

func toSet(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	s := make(map[string]bool, len(names))
	for _, n := range names {
		s[n] = true
	}
	return s
}
