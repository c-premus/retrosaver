//go:build linux

package window

import (
	"fmt"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xinerama"
)

// monitor is one Xinerama screen's rectangle, in X protocol coordinates.
//
// Those are the coordinates that matter. With a scaled GNOME session Mutter
// lays monitors out in logical pixels, while XWayland's X coordinates are that
// layout multiplied by ceil(highest monitor scale) -- a 1536-pixel-wide logical
// monitor at 125% reads as 3072 pixels to an X client. Mutter converts a
// client's requested position from X to logical coordinates itself, so the
// rectangles Xinerama reports are exactly what -geometry has to name.
type monitor struct {
	x, y          int
	width, height int
}

// geometry is the monitor's rectangle as an X geometry string, WxH+X+Y.
//
// Passed to a module as -geometry, it makes Xt set USPosition, which Mutter
// honours on initial placement instead of running its own placement. The window
// therefore maps on this monitor, and a later EWMH fullscreen request
// fullscreens it here: Mutter picks the monitor holding the centre of the
// window's requested rectangle.
func (m monitor) geometry() string {
	return fmt.Sprintf("%dx%d+%d+%d", m.width, m.height, m.x, m.y)
}

// monitors reads the monitor layout from the Xinerama extension on display.
//
// It returns the monitors in Xinerama order, which lists the primary first,
// with exact duplicates dropped: a mirrored display is one piece of glass and
// gets one module, not two stacked on top of each other. An inactive Xinerama
// extension returns no monitors and no error; the caller treats that, like a
// single monitor, as "one window, placed by the window manager".
func monitors(display string) ([]monitor, error) {
	conn, err := xgb.NewConnDisplay(display)
	if err != nil {
		return nil, fmt.Errorf("window: connecting to X display %q: %w", display, err)
	}
	defer conn.Close()

	if err := xinerama.Init(conn); err != nil {
		return nil, fmt.Errorf("window: Xinerama extension: %w", err)
	}
	active, err := xinerama.IsActive(conn).Reply()
	if err != nil {
		return nil, fmt.Errorf("window: Xinerama IsActive: %w", err)
	}
	if active.State == 0 {
		return nil, nil
	}
	screens, err := xinerama.QueryScreens(conn).Reply()
	if err != nil {
		return nil, fmt.Errorf("window: Xinerama QueryScreens: %w", err)
	}

	mons := make([]monitor, 0, len(screens.ScreenInfo))
	for _, s := range screens.ScreenInfo {
		mons = append(mons, monitor{
			x: int(s.XOrg), y: int(s.YOrg),
			width: int(s.Width), height: int(s.Height),
		})
	}
	return distinct(mons), nil
}

// distinct drops exact duplicates from mons, keeping the first of each and the
// original order.
func distinct(mons []monitor) []monitor {
	out := mons[:0:0]
	seen := make(map[monitor]bool, len(mons))
	for _, m := range mons {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}
