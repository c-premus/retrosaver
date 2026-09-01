//go:build linux

// Linux-only in fact, not merely by intent: inotify has no portable
// equivalent. The constraint makes a build on another OS report "no Go files"
// rather than fail with a page of undefined symbols.

// Package watch reports changes to a single file using inotify.
//
// It exists so the daemon can pick up an edited config without a restart. It
// touches no part of the GNOME stack -- it is a thin wrapper over the Linux
// inotify syscalls in golang.org/x/sys/unix.
//
// That import makes x/sys a direct dependency, which is deliberate and
// recorded in AGENTS.md: the project's rule is two pure-Go dependencies, not
// one. The stdlib syscall package is frozen and its own documentation points
// callers at x/sys, so hand-rolling these calls to avoid the requirement would
// trade an endorsed dependency for a discouraged one. x/sys is pure Go, so
// CGO_ENABLED=0 still yields a static binary either way.
package watch

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// debounce is how long to wait for the dust to settle after an event.
//
// A single save produces several inotify events, and an editor that writes a
// temp file and renames it produces a different set again. Coalescing them
// means one reload per save rather than three.
const debounce = 200 * time.Millisecond

// eventBufSize is the read buffer for inotify events. Each event is a fixed
// header plus a NUL-padded name, so this holds a comfortable batch.
const eventBufSize = 4096

// File watches path and sends on the returned channel whenever it changes.
//
// It watches the *directory* containing path rather than the file itself, and
// this is not an implementation detail that can be simplified away: editors
// save by writing a temp file and renaming it over the target. That replaces
// the inode, and a watch registered on the old inode survives as a handle
// that never fires again -- the watch looks healthy and silently reports
// nothing. Watching the directory and filtering on the basename catches the
// rename, the plain write, and a file that did not exist yet.
//
// Sends are coalesced and non-blocking: the channel has room for one pending
// event, and further events while one is unread are dropped. The receiver
// re-reads the file anyway, so a queue would only cause redundant reloads.
//
// The channel is closed when ctx is cancelled.
func File(ctx context.Context, path string) (<-chan struct{}, error) {
	dir := filepath.Dir(path)
	name := filepath.Base(path)

	// The directory must exist. The config file itself need not: watching for
	// it to appear is a legitimate case, and a first `retrosaver setup` on a
	// fresh account creates the file after the daemon may already be running.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("watch: creating %s: %w", dir, err)
	}

	// IN_NONBLOCK is load-bearing and must not be "simplified" away. os.NewFile
	// hands the descriptor to the runtime poller only when O_NONBLOCK is
	// already set on it -- see os.newFile, which computes
	// `pollable := kind == kindOpenFile || kind == kindPipe || kind == kindSock
	// || nonBlocking` and gets nonBlocking from unix.HasNonblockFlag(flags).
	// A blocking inotify fd lands on the kindNewFile path, is never registered
	// with epoll, and its Read then parks in read(2) where Close cannot reach
	// it. With the flag set, Close unblocks the reader as run() assumes.
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("watch: inotify_init1: %w", err)
	}

	// IN_CLOSE_WRITE covers a direct write; IN_MOVED_TO covers the
	// write-temp-then-rename that most editors do; IN_CREATE covers the file
	// appearing for the first time. IN_DELETE and IN_MOVED_FROM cover the file
	// going away, which is a real config change: Load treats a missing file as
	// "use the defaults", so a delete must reload rather than be ignored.
	const mask = unix.IN_CLOSE_WRITE | unix.IN_MOVED_TO | unix.IN_CREATE |
		unix.IN_DELETE | unix.IN_MOVED_FROM
	if _, err := unix.InotifyAddWatch(fd, dir, mask); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("watch: adding an inotify watch on %s: %w", dir, err)
	}

	// Wrap the descriptor in an *os.File rather than reading it raw, for two
	// distinct reasons -- only the first of which is about Close.
	//
	// The runtime reference-counts an os.File, so it cannot yield the
	// descriptor number while a goroutine still holds it. Closing a raw fd out
	// from under a blocked unix.Read is a real race: observed here as EBADF
	// once the number was recycled by another goroutine, after which the watch
	// silently reported nothing.
	//
	// Separately, os.File.Read retries EINTR internally, which matters because
	// the Go runtime's preemption signals interrupt a blocking read often.
	//
	// Close unblocking a pending Read is a property of the poller, not of
	// os.File as such, and it is IN_NONBLOCK above that buys it.
	f := os.NewFile(uintptr(fd), "inotify")

	out := make(chan struct{}, 1)
	go run(ctx, f, name, out)
	return out, nil
}

// run reads events until ctx is cancelled, then closes out.
func run(ctx context.Context, f *os.File, name string, out chan<- struct{}) {
	defer close(out)

	// Closing the file is what unblocks the read in the reader goroutine, so
	// cancellation has to go through it rather than through a select.
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		f.Close()
		close(done)
	}()

	raw := make(chan struct{}, 1)
	readErr := make(chan error, 1)
	go read(f, name, raw, readErr)

	var timer <-chan time.Time
	for {
		select {
		case <-done:
			return

		case _, ok := <-raw:
			if !ok {
				// The reader stopped. Cancellation is the ordinary case: the
				// fd was closed on purpose, so wait for done and let the
				// caller see the channel close at a predictable point.
				//
				// Test ctx, not done: cancel() closes the fd and closes done
				// in that order, so raw can close first and a select on done
				// would report an ordinary shutdown as a failure. ctx.Err()
				// is already set by the time either happens.
				if ctx.Err() != nil {
					<-done
					return
				}
				// Otherwise the read failed for good and this watch is dead.
				// Return rather than parking on done forever: closing out is
				// how the caller learns to stop relying on it and fall back
				// to SIGHUP, and a watch that reports nothing while looking
				// healthy is the exact failure this package exists to avoid.
				slog.Error("config watch stopped; reload now needs SIGHUP",
					"file", name, "err", <-readErr)
				return
			}
			// Restart the settle timer on every event, so a burst produces
			// exactly one send once it goes quiet.
			timer = time.After(debounce)

		case <-timer:
			timer = nil
			select {
			case out <- struct{}{}:
			default: // one reload is already pending; nothing to add
			}
		}
	}
}

// read turns inotify events naming the watched file into sends on raw.
//
// It closes raw when it stops and reports why on errc, which is buffered so
// this never blocks. os.File.Read retries EINTR internally, which matters
// because the Go runtime signals threads often enough to interrupt a read
// regularly.
func read(f *os.File, name string, raw chan<- struct{}, errc chan<- error) {
	defer close(raw)

	buf := make([]byte, eventBufSize)
	for {
		n, err := f.Read(buf)
		if err != nil {
			// The file was closed, or the read failed for good. run
			// distinguishes the two by whether ctx was cancelled.
			errc <- err
			return
		}
		if n <= 0 {
			continue
		}
		if !mentions(buf[:n], name) {
			continue
		}
		select {
		case raw <- struct{}{}:
		default: // an event is already queued; the batch collapses to one
		}
	}
}

// mentions reports whether any event in buf names the watched file.
//
// The struct is decoded field by field rather than through an unsafe pointer
// cast. inotify_event is { int32 wd; uint32 mask, cookie, len; char name[] }
// in native byte order, so binary.NativeEndian reads it exactly and keeps the
// package free of unsafe.
func mentions(buf []byte, name string) bool {
	const hdr = unix.SizeofInotifyEvent // 16: wd, mask, cookie, len
	for i := 0; i+hdr <= len(buf); {
		nameLen := int(binary.NativeEndian.Uint32(buf[i+12 : i+16]))
		end := i + hdr + nameLen
		if end > len(buf) {
			return false // a truncated trailing event; nothing more to read
		}
		if nameLen > 0 {
			// The name is NUL-padded to an alignment boundary.
			if trimNUL(string(buf[i+hdr:end])) == name {
				return true
			}
		}
		i = end
	}
	return false
}

func trimNUL(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			return s[:i]
		}
	}
	return s
}
