//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/c-premus/retrosaver/internal/config"
	"github.com/c-premus/retrosaver/internal/modules"
)

// The config and list --json commands are the contract the settings app is
// written against. It never reads or writes the file itself, so parsing,
// validation and quoting live only here, and a change to their output is a
// change to that contract.

func cmdConfig(args []string) error {
	if len(args) == 0 {
		return errors.New("config: want a subcommand: show [--json], or set KEY=value [KEY=value ...]")
	}
	path, err := config.UserConfigPath()
	if err != nil {
		return err
	}
	switch args[0] {
	case "show":
		fset := flag.NewFlagSet("config show", flag.ExitOnError)
		asJSON := fset.Bool("json", false, "print JSON for programs")
		_ = fset.Parse(args[1:])
		return showConfig(os.Stdout, path, *asJSON)
	case "set":
		updates, err := parseAssignments(args[1:])
		if err != nil {
			return err
		}
		return config.Set(path, updates)
	default:
		return fmt.Errorf("config: unknown subcommand %q", args[0])
	}
}

// parseAssignments turns KEY=value arguments into a map, refusing a key given
// twice: which of the two should win is not obvious, so it is never guessed.
func parseAssignments(args []string) (map[string]string, error) {
	if len(args) == 0 {
		return nil, errors.New("config set: want at least one KEY=value")
	}
	updates := make(map[string]string, len(args))
	for _, a := range args {
		key, value, ok := strings.Cut(a, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("config set: %q is not KEY=value", a)
		}
		if _, dup := updates[key]; dup {
			return nil, fmt.Errorf("config set: %s given twice", key)
		}
		updates[key] = value
	}
	return updates, nil
}

// configJSON is the shape `config show --json` prints. Keys match the file's,
// and delays are whole seconds, as in the file.
type configJSON struct {
	Path       string   `json:"path"`
	Exists     bool     `json:"exists"`
	SaverDelay int64    `json:"SAVER_DELAY"`
	CycleAfter int64    `json:"CYCLE_AFTER"`
	Monitors   string   `json:"MONITORS"`
	LockAfter  int64    `json:"LOCK_AFTER"`
	BlankAfter int64    `json:"BLANK_AFTER"`
	Exclude    []string `json:"EXCLUDE"`
	Include    []string `json:"INCLUDE"`
}

// showConfig prints the settings in effect: the file's, over the defaults. An
// unparseable file is an error rather than a silent print of the defaults, so
// a front end never presents settings the file does not hold.
func showConfig(w io.Writer, path string, asJSON bool) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	_, statErr := os.Stat(path)
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("checking %s: %w", path, statErr)
	}
	out := configJSON{
		Path:       path,
		Exists:     statErr == nil,
		SaverDelay: int64(cfg.SaverDelay.Seconds()),
		CycleAfter: int64(cfg.CycleAfter.Seconds()),
		Monitors:   string(cfg.Monitors),
		LockAfter:  int64(cfg.LockAfter.Seconds()),
		BlankAfter: int64(cfg.BlankAfter.Seconds()),
		Exclude:    nonNil(cfg.Exclude),
		Include:    nonNil(cfg.Include),
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	fmt.Fprintf(w, "# %s\n", path)
	fmt.Fprintf(w, "SAVER_DELAY=%d\n", out.SaverDelay)
	fmt.Fprintf(w, "CYCLE_AFTER=%d\n", out.CycleAfter)
	fmt.Fprintf(w, "MONITORS=%s\n", out.Monitors)
	fmt.Fprintf(w, "LOCK_AFTER=%d\n", out.LockAfter)
	fmt.Fprintf(w, "BLANK_AFTER=%d\n", out.BlankAfter)
	fmt.Fprintf(w, "EXCLUDE=%q\n", strings.Join(out.Exclude, " "))
	fmt.Fprintf(w, "INCLUDE=%q\n", strings.Join(out.Include, " "))
	return nil
}

// moduleJSON is one entry of `list --all --json`.
type moduleJSON struct {
	modules.Info
	// Selected reports whether INCLUDE and EXCLUDE let the daemon pick it.
	Selected bool `json:"selected"`
}

// listModules prints the selectable modules, or with all every installed one.
// Unlike Finder.Available, an empty selection is not an error here: a front
// end showing the user that nothing is selected needs the list to fix it.
func listModules(w io.Writer, f *modules.Finder, cfg config.Config, all, asJSON bool) error {
	discovered, err := f.Discover()
	if err != nil {
		return err
	}
	selected := modules.Select(discovered, cfg.Include, cfg.Exclude)

	names := selected
	if all {
		names = discovered
	}
	if !asJSON {
		if !all && len(selected) == 0 {
			return errors.New("no modules survived INCLUDE/EXCLUDE")
		}
		for _, n := range names {
			fmt.Fprintln(w, n)
		}
		return nil
	}

	isSelected := make(map[string]bool, len(selected))
	for _, n := range selected {
		isSelected[n] = true
	}
	out := make([]moduleJSON, 0, len(names))
	for _, n := range names {
		info, _ := f.Info(n) // falls back to the bare name; one bad XML must not hide the rest
		out = append(out, moduleJSON{Info: info, Selected: isSelected[n]})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// nonNil makes an empty list encode as [] rather than null, so a consumer can
// iterate it without a special case.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
