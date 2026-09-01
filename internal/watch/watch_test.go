package watch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// eventTimeout bounds the wait for an inotify event. Generous, because it is
// a real syscall round trip plus the debounce, and CI runners are slow.
const eventTimeout = 5 * time.Second

// wantEvent fails unless an event arrives.
func wantEvent(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case _, ok := <-ch:
		if !ok {
			t.Fatal("the watch channel closed instead of reporting a change")
		}
	case <-time.After(eventTimeout):
		t.Fatal("timed out waiting for a change event")
	}
}

// wantNoEvent fails if an event arrives within d.
func wantNoEvent(t *testing.T, ch <-chan struct{}, d time.Duration) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("got a change event, want none")
	case <-time.After(d):
	}
}

func start(t *testing.T) (path string, ch <-chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "retrosaver.conf")
	if err := os.WriteFile(path, []byte("SAVER_DELAY=300\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ch, err := File(ctx, path)
	if err != nil {
		t.Fatalf("File(%q) = %v", path, err)
	}
	return path, ch
}

func TestFileReportsAPlainWrite(t *testing.T) {
	path, ch := start(t)

	if err := os.WriteFile(path, []byte("SAVER_DELAY=120\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantEvent(t, ch)
}

// TestFileReportsARenameOverTheTarget is the one that matters. Editors save by
// writing a temp file and renaming it over the target, which replaces the
// inode. A watch registered on the file itself would survive as a handle that
// never fires again, so this is what proves the directory is being watched.
func TestFileReportsARenameOverTheTarget(t *testing.T) {
	path, ch := start(t)

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte("SAVER_DELAY=120\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	wantEvent(t, ch)
}

// A second edit must still be reported: one reload cannot consume the watch.
func TestFileKeepsReportingAfterARename(t *testing.T) {
	path, ch := start(t)

	for i := range 3 {
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte("SAVER_DELAY=120\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
		t.Logf("edit %d", i+1)
		wantEvent(t, ch)
	}
}

func TestFileIgnoresOtherFilesInTheDirectory(t *testing.T) {
	path, ch := start(t)

	other := filepath.Join(filepath.Dir(path), "idle-delay.orig")
	if err := os.WriteFile(other, []byte("300\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// setup writes idle-delay.orig into this very directory, so a false
	// positive here would reload the daemon for an unrelated file.
	wantNoEvent(t, ch, 2*debounce)
}

// A burst of writes must collapse into a single event, or every save would
// reload the daemon several times over.
func TestFileCoalescesABurst(t *testing.T) {
	path, ch := start(t)

	for range 5 {
		if err := os.WriteFile(path, []byte("SAVER_DELAY=120\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wantEvent(t, ch)
	wantNoEvent(t, ch, 3*debounce)
}

// The config file need not exist yet: setup creates it, possibly after the
// daemon is already running.
func TestFileReportsAFileThatDidNotExistYet(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "retrosaver.conf")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := File(ctx, path)
	if err != nil {
		t.Fatalf("File(%q) = %v", path, err)
	}
	if err := os.WriteFile(path, []byte("SAVER_DELAY=120\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantEvent(t, ch)
}

func TestFileClosesTheChannelOnCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "retrosaver.conf")

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := File(ctx, path)
	if err != nil {
		t.Fatalf("File(%q) = %v", path, err)
	}

	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			// A pending event may arrive first; the close must still follow.
			select {
			case _, ok := <-ch:
				if ok {
					t.Fatal("the channel delivered a second event after cancel")
				}
			case <-time.After(eventTimeout):
				t.Fatal("the channel did not close after cancel")
			}
		}
	case <-time.After(eventTimeout):
		t.Fatal("the channel did not close after cancel")
	}
}

// A directory that does not exist yet is created rather than refused: the
// daemon can start before `retrosaver setup` has made ~/.config/retrosaver.
func TestFileCreatesAMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "retrosaver")
	path := filepath.Join(dir, "retrosaver.conf")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := File(ctx, path); err != nil {
		t.Fatalf("File(%q) = %v", path, err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the watched directory was not created: %v", err)
	}
}
