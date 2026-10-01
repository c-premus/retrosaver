//go:build linux

package window

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// settlePoll is how often openScreen re-reads a window while waiting for the
// window manager to finish placing it.
const settlePoll = 20 * time.Millisecond

// screen is the set of windows retrosaver draws its savers in, one per monitor,
// and the X connection that keeps them alive.
//
// The windows are retrosaver's own rather than the module's because a module
// sizes itself once, from the window it starts in. Some modules never look
// again -- anemone allocates its drawing pixmaps at startup and its resize
// handler's re-allocation is compiled out -- so a module that creates its own
// window at the default 1280x720 and is fullscreened afterwards keeps drawing
// into a 1280x720 corner. Creating the window already fullscreen, waiting until
// it covers its monitor, and only then starting the module inside it with
// -window-id is how XScreenSaver itself does it, and every module then starts
// at its final size.
//
// The windows exist only as long as conn. Closing it, or the process that holds
// it exiting, destroys them.
type screen struct {
	conn    *xgb.Conn
	cursor  xproto.Cursor
	windows []xproto.Window
	mons    []monitor

	closeOnce sync.Once
}

// openScreen creates one fullscreen, always-on-top window per monitor on
// display and waits until the window manager has placed each one over its
// monitor. modules only names the windows: monitor i's is named after
// modules[i % len(modules)], the module that will draw in it.
//
// A window the window manager never manages within windowDeadline is an error.
// A managed window that does not cover its monitor exactly is logged and kept:
// a module drawing slightly short of an edge beats no screensaver at all.
func openScreen(ctx context.Context, display string, modules []string) (*screen, error) {
	conn, err := xgb.NewConnDisplay(display)
	if err != nil {
		return nil, fmt.Errorf("window: connecting to X display %q: %w", display, err)
	}
	s := &screen{conn: conn}

	root := xproto.Setup(conn).DefaultScreen(conn)
	s.mons = layout(conn, root)

	if err := s.create(root, modules); err != nil {
		s.close()
		return nil, err
	}
	if err := s.settle(ctx); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

// layout is the monitors to cover: Xinerama's, or the whole X screen when
// those cannot be read.
func layout(conn *xgb.Conn, root *xproto.ScreenInfo) []monitor {
	mons, err := monitors(conn)
	if err != nil {
		// Covering the whole X screen beats covering nothing.
		slog.Warn("reading the monitor layout; covering the whole screen", "err", err)
	}
	if len(mons) == 0 {
		mons = []monitor{{width: int(root.WidthInPixels), height: int(root.HeightInPixels)}}
	}
	return mons
}

// Monitors reports how many monitors a saver would cover: one window, and one
// module, per monitor. The daemon asks before a launch so it can pick a module
// for each.
func Monitors() (int, error) {
	conn, err := xgb.NewConnDisplay(moduleDisplay())
	if err != nil {
		return 0, fmt.Errorf("window: connecting to X display %q: %w", moduleDisplay(), err)
	}
	defer conn.Close()
	return len(layout(conn, xproto.Setup(conn).DefaultScreen(conn))), nil
}

// create makes and maps one window per monitor.
func (s *screen) create(root *xproto.ScreenInfo, modules []string) error {
	atoms, err := internAtoms(s.conn,
		"_NET_WM_STATE", "_NET_WM_STATE_FULLSCREEN", "_NET_WM_STATE_ABOVE",
		"_NET_WM_NAME", "UTF8_STRING")
	if err != nil {
		return err
	}

	// A blank cursor: a 1x1 bitmap with an empty mask. The pointer is then
	// invisible over the saver whether or not unclutter is running.
	pix, err := xproto.NewPixmapId(s.conn)
	if err != nil {
		return fmt.Errorf("window: allocating a pixmap id: %w", err)
	}
	xproto.CreatePixmap(s.conn, 1, pix, xproto.Drawable(root.Root), 1, 1)
	if s.cursor, err = xproto.NewCursorId(s.conn); err != nil {
		return fmt.Errorf("window: allocating a cursor id: %w", err)
	}
	xproto.CreateCursor(s.conn, s.cursor, pix, pix, 0, 0, 0, 0, 0, 0, 0, 0)
	xproto.FreePixmap(s.conn, pix)

	for i, m := range s.mons {
		name := "retrosaver: " + modules[i%len(modules)]
		w, err := xproto.NewWindowId(s.conn)
		if err != nil {
			return fmt.Errorf("window: allocating a window id: %w", err)
		}
		// Values are listed in the order of their mask bits.
		// Depth and visual 0 are CopyFromParent: the root's, which GL modules
		// render into as happily as the rest.
		err = xproto.CreateWindowChecked(s.conn, 0, w, root.Root,
			int16(m.x), int16(m.y), uint16(m.width), uint16(m.height), 0,
			xproto.WindowClassInputOutput, 0,
			xproto.CwBackPixel|xproto.CwCursor,
			[]uint32{root.BlackPixel, uint32(s.cursor)}).Check()
		if err != nil {
			return fmt.Errorf("window: creating a window at %s: %w", m, err)
		}
		s.windows = append(s.windows, w)

		setString(s.conn, w, xproto.AtomWmName, xproto.AtomString, name)
		setString(s.conn, w, atoms["_NET_WM_NAME"], atoms["UTF8_STRING"], name)
		setString(s.conn, w, xproto.AtomWmClass, xproto.AtomString, "retrosaver\x00retrosaver\x00")
		// USPosition and USSize: place the window exactly where it was asked
		// to go, which puts it on its monitor before it is fullscreened.
		hints := make([]uint32, 18)
		hints[0] = 1 | 2
		hints[1], hints[2], hints[3], hints[4] = uint32(m.x), uint32(m.y), uint32(m.width), uint32(m.height)
		setCards(s.conn, w, xproto.AtomWmNormalHints, xproto.AtomWmSizeHints, hints...)
		// Fullscreen and above from the moment it is managed, set before
		// mapping. Mutter honours an initial _NET_WM_STATE, so the window
		// never appears at any other size.
		setCards(s.conn, w, atoms["_NET_WM_STATE"], xproto.AtomAtom,
			uint32(atoms["_NET_WM_STATE_FULLSCREEN"]), uint32(atoms["_NET_WM_STATE_ABOVE"]))

		if err := xproto.MapWindowChecked(s.conn, w).Check(); err != nil {
			return fmt.Errorf("window: mapping the window at %s: %w", m, err)
		}
	}
	return nil
}

// settle waits until the window manager has managed every window, then checks
// each covers its monitor.
//
// Managed means the window manager has set ICCCM WM_STATE on it. Until then the
// window's geometry is just what was requested, which says nothing about where
// it will end up.
func (s *screen) settle(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, windowDeadline)
	defer cancel()

	wmState, err := internAtoms(s.conn, "WM_STATE")
	if err != nil {
		return err
	}
	for i, w := range s.windows {
		for {
			if managed(s.conn, w, wmState["WM_STATE"]) && s.geometry(w) == s.mons[i] {
				break
			}
			select {
			case <-ctx.Done():
				if errors.Is(ctx.Err(), context.Canceled) {
					return ctx.Err()
				}
				if !managed(s.conn, w, wmState["WM_STATE"]) {
					return fmt.Errorf("%w: the window manager did not manage the window for %s within %v",
						ErrNoWindow, s.mons[i], windowDeadline)
				}
				slog.Warn("saver window does not cover its monitor",
					"monitor", s.mons[i].String(), "window", s.geometry(w).String())
				return nil
			case <-time.After(settlePoll):
			}
		}
	}
	return nil
}

// geometry is w's rectangle in root-window coordinates, or the zero monitor if
// it cannot be read.
func (s *screen) geometry(w xproto.Window) monitor {
	g, err := xproto.GetGeometry(s.conn, xproto.Drawable(w)).Reply()
	if err != nil {
		return monitor{}
	}
	root := xproto.Setup(s.conn).DefaultScreen(s.conn).Root
	t, err := xproto.TranslateCoordinates(s.conn, w, root, 0, 0).Reply()
	if err != nil {
		return monitor{}
	}
	return monitor{x: int(t.DstX), y: int(t.DstY), width: int(g.Width), height: int(g.Height)}
}

// close destroys the windows and closes the connection. It is idempotent.
func (s *screen) close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		for _, w := range slices.Backward(s.windows) {
			xproto.DestroyWindow(s.conn, w)
		}
		if s.cursor != 0 {
			xproto.FreeCursor(s.conn, s.cursor)
		}
		// A round trip, so the requests above reach the server before the
		// connection goes; closing alone would destroy the windows anyway, but
		// this keeps teardown ordered behind the modules being stopped.
		_, _ = xproto.GetInputFocus(s.conn).Reply()
		s.conn.Close()
	})
}

// managed reports whether the window manager has set WM_STATE on w.
func managed(conn *xgb.Conn, w xproto.Window, wmState xproto.Atom) bool {
	r, err := xproto.GetProperty(conn, false, w, wmState, xproto.GetPropertyTypeAny, 0, 2).Reply()
	return err == nil && r.Format != 0
}

// internAtoms looks up atoms by name, sending every request before waiting on
// any reply.
func internAtoms(conn *xgb.Conn, names ...string) (map[string]xproto.Atom, error) {
	cookies := make([]xproto.InternAtomCookie, len(names))
	for i, n := range names {
		cookies[i] = xproto.InternAtom(conn, false, uint16(len(n)), n)
	}
	atoms := make(map[string]xproto.Atom, len(names))
	for i, c := range cookies {
		r, err := c.Reply()
		if err != nil {
			return nil, fmt.Errorf("window: interning atom %s: %w", names[i], err)
		}
		atoms[names[i]] = r.Atom
	}
	return atoms, nil
}

func setString(conn *xgb.Conn, w xproto.Window, prop, typ xproto.Atom, v string) {
	xproto.ChangeProperty(conn, xproto.PropModeReplace, w, prop, typ, 8, uint32(len(v)), []byte(v))
}

func setCards(conn *xgb.Conn, w xproto.Window, prop, typ xproto.Atom, v ...uint32) {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		xgb.Put32(b[4*i:], x)
	}
	xproto.ChangeProperty(conn, xproto.PropModeReplace, w, prop, typ, 32, uint32(len(v)), b)
}

// String is the monitor as an X geometry string, WxH+X+Y.
func (m monitor) String() string {
	return fmt.Sprintf("%dx%d+%d+%d", m.width, m.height, m.x, m.y)
}
