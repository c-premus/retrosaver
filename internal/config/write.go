package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Keys lists every key the parser understands, in the order the shipped file
// documents them. It is the one inventory Set and any machine-readable output
// share, so a front end cannot write a key the parser would silently ignore.
var Keys = []string{
	"SAVER_DELAY",
	"CYCLE_AFTER",
	"MONITORS",
	"LOCK_AFTER",
	"BLANK_AFTER",
	"EXCLUDE",
	"INCLUDE",
}

// listKeys hold space-separated module names and are written quoted.
var listKeys = map[string]bool{"EXCLUDE": true, "INCLUDE": true}

// DefaultFile renders Defaults as a commented config file. It is what setup
// installs when the packaged example is absent, and what Set starts from when
// there is no file yet, so both produce the settings the package documents.
func DefaultFile() string {
	d := Defaults()
	return fmt.Sprintf(`# Seconds of idle before the screensaver starts.
SAVER_DELAY=%d

# Seconds each module stays on screen before the saver switches to another
# one. 0 disables switching, leaving the first module up until the lock.
CYCLE_AFTER=%d

# What a second monitor shows: "same" runs one module on every monitor,
# "different" picks a different module for each.
MONITORS=%s

# Seconds after the screensaver starts before the session locks. 0 disables.
LOCK_AFTER=%d

# Seconds after locking before the display powers off. 0 disables.
BLANK_AFTER=%d

# Space-separated module names never to pick. These need image assets,
# network access, or elevated capabilities and misbehave standalone.
EXCLUDE="%s"

# Optional: if non-empty, pick only from this space-separated list.
# e.g. INCLUDE="atlantis flame ifs apollonian discrete coral sierpinski"
INCLUDE="%s"
`,
		int(d.SaverDelay.Seconds()),
		int(d.CycleAfter.Seconds()),
		d.Monitors,
		int(d.LockAfter.Seconds()),
		int(d.BlankAfter.Seconds()),
		strings.Join(d.Exclude, " "),
		strings.Join(d.Include, " "))
}

// Set assigns the given keys in the config file at path and leaves everything
// else in it alone: comments, blank lines, keys this version does not know,
// and the order of the lines.
//
// It is all or nothing. Every key must be known and every value well formed,
// and the edited file must parse; otherwise an error is returned and the file
// is not touched. A missing file is created from DefaultFile first. The write
// goes through a temporary file renamed over the original, which the daemon's
// directory watch sees as a single change.
func Set(path string, updates map[string]string) error {
	if len(updates) == 0 {
		return nil
	}
	rendered := make(map[string]string, len(updates))
	for key, value := range updates {
		line, err := assignment(key, value)
		if err != nil {
			return err
		}
		rendered[key] = line
	}

	// Edit the file a symlink points at, not the link: a rename over the
	// link would replace it with a plain file.
	target := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		target = resolved
	}

	content, err := os.ReadFile(target)
	mode := fs.FileMode(0o644)
	switch {
	case err == nil:
		if info, statErr := os.Stat(target); statErr == nil {
			mode = info.Mode().Perm()
		}
	case errors.Is(err, fs.ErrNotExist):
		content = []byte(DefaultFile())
	default:
		return fmt.Errorf("reading %s: %w", target, err)
	}

	edited := apply(content, rendered)

	cfg := Defaults()
	if err := parse(bytes.NewReader(edited), &cfg); err != nil {
		return fmt.Errorf("refusing to write %s: %w", target, err)
	}
	return writeAtomic(target, edited, mode)
}

// assignment validates one update and renders it as a config line.
func assignment(key, value string) (string, error) {
	if !slices.Contains(Keys, key) {
		return "", fmt.Errorf("unknown key %q (known: %s)", key, strings.Join(Keys, " "))
	}
	if listKeys[key] {
		names := strings.Fields(value)
		for _, n := range names {
			if !isModuleName(n) {
				return "", fmt.Errorf("%s: %q is not a module name", key, n)
			}
		}
		return fmt.Sprintf("%s=%q", key, strings.Join(names, " ")), nil
	}
	if value == "" || strings.ContainsAny(value, " \t\r\n\"'#") {
		return "", fmt.Errorf("%s: want a single word, got %q", key, value)
	}
	return key + "=" + value, nil
}

// isModuleName accepts the characters XScreenSaver module names use. It keeps
// quotes, '#' and whitespace out of a quoted value, which the line parser
// would otherwise misread.
func isModuleName(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '+':
		default:
			return false
		}
	}
	return s != ""
}

// apply replaces the last assignment of each key, which is the one the
// last-wins parser honours, and appends keys the file does not assign.
func apply(content []byte, rendered map[string]string) []byte {
	lines := strings.Split(string(content), "\n")
	last := make(map[string]int, len(rendered))
	for i, l := range lines {
		text := strings.TrimSpace(l)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		key, _, ok := strings.Cut(text, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if _, wanted := rendered[key]; wanted {
			last[key] = i
		}
	}
	for key, i := range last {
		lines[i] = rendered[key]
	}

	out := strings.Join(lines, "\n")
	var missing []string
	for _, key := range Keys {
		if _, done := last[key]; !done {
			if line, ok := rendered[key]; ok {
				missing = append(missing, line)
			}
		}
	}
	if len(missing) > 0 {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += strings.Join(missing, "\n") + "\n"
	}
	return []byte(out)
}

// writeAtomic writes data to a temporary file beside path and renames it into
// place, so a reader never sees a half-written config.
func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Removing after a successful rename fails harmlessly: the name is gone.
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("setting the mode of %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	return nil
}
