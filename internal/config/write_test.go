package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "retrosaver.conf")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSetKeepsEverythingItDoesNotAssign(t *testing.T) {
	path := writeFile(t, `# my own note
SAVER_DELAY=15

FUTURE_KEY=whatever
  LOCK_AFTER = 20
`)
	if err := Set(path, map[string]string{"SAVER_DELAY": "600", "LOCK_AFTER": "0"}); err != nil {
		t.Fatal(err)
	}
	want := `# my own note
SAVER_DELAY=600

FUTURE_KEY=whatever
LOCK_AFTER=0
`
	if got := readFile(t, path); got != want {
		t.Errorf("file =\n%s\nwant\n%s", got, want)
	}
}

// The parser is last-wins, so editing an earlier duplicate would change
// nothing the daemon sees.
func TestSetEditsTheLastOfDuplicateAssignments(t *testing.T) {
	path := writeFile(t, "SAVER_DELAY=15\nSAVER_DELAY=30\n")
	if err := Set(path, map[string]string{"SAVER_DELAY": "45"}); err != nil {
		t.Fatal(err)
	}
	if got, want := readFile(t, path), "SAVER_DELAY=15\nSAVER_DELAY=45\n"; got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestSetAppendsKeysTheFileLacks(t *testing.T) {
	path := writeFile(t, "SAVER_DELAY=15") // no trailing newline
	err := Set(path, map[string]string{"INCLUDE": "atlantis  flame", "MONITORS": "different"})
	if err != nil {
		t.Fatal(err)
	}
	// Appended in Keys order, whatever the map order.
	want := "SAVER_DELAY=15\nMONITORS=different\nINCLUDE=\"atlantis flame\"\n"
	if got := readFile(t, path); got != want {
		t.Errorf("file = %q, want %q", got, want)
	}
}

func TestSetCreatesAMissingFileFromTheDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "retrosaver.conf")
	if err := Set(path, map[string]string{"CYCLE_AFTER": "0"}); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	want := strings.Replace(DefaultFile(), "CYCLE_AFTER=300", "CYCLE_AFTER=0", 1)
	if got != want {
		t.Errorf("file =\n%s\nwant\n%s", got, want)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CycleAfter != 0 || cfg.SaverDelay != 300*time.Second {
		t.Errorf("Load = %+v, want defaults with cycling off", cfg)
	}
}

func TestSetRefusesBadInputAndLeavesTheFileAlone(t *testing.T) {
	const original = "# keep me\nSAVER_DELAY=15\n"
	for name, updates := range map[string]map[string]string{
		"unknown key":         {"SAVER_DELAYS": "1"},
		"negative":            {"SAVER_DELAY": "-1"},
		"not a number":        {"LOCK_AFTER": "soon"},
		"bad monitors":        {"MONITORS": "both"},
		"empty scalar":        {"BLANK_AFTER": ""},
		"injected line":       {"SAVER_DELAY": "1\nLOCK_AFTER=0"},
		"quote in a list":     {"EXCLUDE": `flame"`},
		"comment in a list":   {"INCLUDE": "flame #x"},
		"one good, one bad":   {"SAVER_DELAY": "600", "MONITORS": "both"},
		"overflowing seconds": {"SAVER_DELAY": "99999999999999999"},
	} {
		t.Run(name, func(t *testing.T) {
			path := writeFile(t, original)
			if err := Set(path, updates); err == nil {
				t.Fatal("Set succeeded, want an error")
			}
			if got := readFile(t, path); got != original {
				t.Errorf("file changed to %q", got)
			}
		})
	}
}

func TestSetLeavesNoTemporaryFileAndKeepsTheMode(t *testing.T) {
	path := writeFile(t, "SAVER_DELAY=15\n")
	if err := Set(path, map[string]string{"SAVER_DELAY": "16"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only the config", names)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want the original 0600", info.Mode().Perm())
	}
}

// A dotfiles setup often symlinks the config. Renaming over the link would
// replace it with a plain file and quietly detach it from the repository.
func TestSetWritesThroughASymlink(t *testing.T) {
	real := writeFile(t, "SAVER_DELAY=15\n")
	link := filepath.Join(t.TempDir(), "retrosaver.conf")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := Set(link, map[string]string{"SAVER_DELAY": "16"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link is no longer a symlink (err %v)", err)
	}
	if got := readFile(t, real); got != "SAVER_DELAY=16\n" {
		t.Errorf("target = %q", got)
	}
}

func TestDefaultFileParsesToDefaults(t *testing.T) {
	cfg := Defaults()
	cfg.SaverDelay = 0 // prove the file sets it rather than inheriting it
	if err := parse(strings.NewReader(DefaultFile()), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.SaverDelay != Defaults().SaverDelay {
		t.Errorf("SaverDelay = %v", cfg.SaverDelay)
	}
}

func TestKeysMatchesTheParser(t *testing.T) {
	// Every key in Keys must change the parsed result; a key Set accepts but
	// parse ignores would be written and then silently do nothing.
	values := map[string]string{
		"SAVER_DELAY": "1", "CYCLE_AFTER": "1", "LOCK_AFTER": "1", "BLANK_AFTER": "1",
		"MONITORS": "different", "EXCLUDE": "x", "INCLUDE": "x",
	}
	for _, key := range Keys {
		v, ok := values[key]
		if !ok {
			t.Fatalf("no test value for %s", key)
		}
		cfg := Defaults()
		if err := parse(strings.NewReader(key+"="+v), &cfg); err != nil {
			t.Fatal(err)
		}
		before := Defaults()
		if cfg.SaverDelay == before.SaverDelay && cfg.CycleAfter == before.CycleAfter &&
			cfg.LockAfter == before.LockAfter && cfg.BlankAfter == before.BlankAfter &&
			cfg.Monitors == before.Monitors && strings.Join(cfg.Exclude, " ") == strings.Join(before.Exclude, " ") &&
			len(cfg.Include) == len(before.Include) {
			t.Errorf("%s=%s changed nothing", key, v)
		}
	}
}
