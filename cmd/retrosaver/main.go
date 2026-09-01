// Command retrosaver brings classic XScreenSaver display modules back to
// GNOME on Wayland, where the traditional screensaver daemon no longer works.
//
// It ships as a single static binary. The subcommands split into two groups:
// runtime (daemon, run, stop, list) and per-user installation (setup,
// teardown). The installation half lives here rather than in Debian maintainer
// scripts because saving and setting org.gnome.desktop.session idle-delay and
// enabling a systemd --user unit are per-user operations that need the user's
// own session bus. A postinst runs as root and cannot do them correctly.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/c-premus/retrosaver/internal/config"
	"github.com/c-premus/retrosaver/internal/daemon"
	"github.com/c-premus/retrosaver/internal/idle"
	"github.com/c-premus/retrosaver/internal/modules"
	"github.com/c-premus/retrosaver/internal/session"
	"github.com/c-premus/retrosaver/internal/watch"
	"github.com/c-premus/retrosaver/internal/window"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

const usage = `retrosaver - retro screensavers for GNOME on Wayland

Usage:
  retrosaver <command> [flags]

Runtime commands:
  daemon              Run the idle state machine (started by the systemd user unit)
  run [module]        Launch one module fullscreen; picks at random when omitted
  stop                Tear down a running module (safe at any time)
  list                Print the discovered, selectable modules

Installation commands:
  setup               Preflight, enable the user unit, take ownership of idle-delay
  teardown            Reverse setup and restore idle-delay

Other:
  version             Print the version
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	if err := dispatch(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "retrosaver: %v\n", err)
		os.Exit(1)
	}
}

func dispatch(cmd string, args []string) error {
	switch cmd {
	case "daemon":
		return cmdDaemon(args)
	case "run":
		return cmdRun(args)
	case "stop":
		return cmdStop(args)
	case "list":
		return cmdList(args)
	case "setup":
		return cmdSetup(args)
	case "teardown":
		return cmdTeardown(args)
	case "version", "--version", "-v":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// loadConfig reads the user's config, falling back to the shipped defaults.
func loadConfig() (config.Config, error) {
	path, err := config.UserConfigPath()
	if err != nil {
		return config.Config{}, err
	}
	return config.Load(path)
}

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	// SIGTERM and SIGINT must tear down cleanly and restore idle-delay.
	// SIGHUP deliberately does NOT go through NotifyContext: that cancels the
	// context, so a reload would stop the daemon instead of re-reading.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	d := daemon.New(cfg)
	d.Reload(reloadTriggers(ctx))
	return d.Run(ctx)
}

// reloadTriggers returns a channel that fires when the config should be
// re-read, fed by SIGHUP and by a watch on the config file itself.
//
// A failure to start the file watch is not fatal. inotify has per-user
// instance and watch limits that a busy desktop can genuinely exhaust, and
// losing automatic reload is a far smaller problem than refusing to run the
// screensaver at all -- SIGHUP still works, so `systemctl --user reload` does
// too.
func reloadTriggers(ctx context.Context) <-chan struct{} {
	out := make(chan struct{}, 1)

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)

	var changed <-chan struct{}
	if path, err := config.UserConfigPath(); err != nil {
		slog.Warn("cannot locate the config file to watch it; SIGHUP still reloads", "err", err)
	} else if ch, err := watch.File(ctx, path); err != nil {
		slog.Warn("cannot watch the config file; SIGHUP still reloads", "path", path, "err", err)
	} else {
		changed = ch
	}

	go func() {
		defer signal.Stop(hup)
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
			case _, ok := <-changed:
				if !ok {
					changed = nil // the watch ended; SIGHUP still works
					continue
				}
			}
			select {
			case out <- struct{}{}:
			default: // a reload is already pending
			}
		}
	}()
	return out
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	// Refuse to stack a second module on top of a running one.
	if name, running := window.RunningModule(); running {
		if name != "" {
			return fmt.Errorf("run: %s is already running; `retrosaver stop` clears it", name)
		}
		return errors.New("run: a module is already running; `retrosaver stop` clears it")
	}

	finder := modules.NewFinder()
	name := fs.Arg(0)
	if name == "" {
		if name, err = finder.Pick(cfg.Include, cfg.Exclude); err != nil {
			return err
		}
	} else {
		// Check against everything discovered rather than the selectable
		// set, so naming an EXCLUDE'd module explicitly still works, while a
		// typo gets a better message than exec's "no such file".
		discovered, err := finder.Discover()
		if err != nil {
			return err
		}
		if !slices.Contains(discovered, name) {
			return fmt.Errorf(
				"run: %q is not a display module on this system; `retrosaver list` shows what is", name)
		}
	}

	saver, err := window.Launch(finder.Path(name))
	if err != nil {
		return err
	}
	// The module outlives this process on purpose: `retrosaver run` hands the
	// screen over and returns, and `retrosaver stop` takes it back.
	fmt.Printf("%s running (pid %d)\n", name, saver.Process().Pid)
	return nil
}

func cmdStop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	keepIdleDelay := fs.Bool("keep-idle-delay", false,
		"do not restore idle-delay (used by the daemon between stages)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// StopRunning treats "nothing was running" as success: stop is the panic
	// button and must be safe to run at any time.
	if err := window.StopRunning(); err != nil {
		return err
	}
	if *keepIdleDelay {
		return nil
	}
	return session.RestoreIdleDelay()
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	available, err := modules.NewFinder().Available(cfg.Include, cfg.Exclude)
	if err != nil {
		return err
	}
	for _, name := range available {
		fmt.Println(name)
	}
	return nil
}

const (
	unitName          = "retrosaver.service"
	packagedUnitPath  = "/usr/lib/systemd/user/" + unitName
	exampleConfigPath = "/usr/share/retrosaver/retrosaver.conf.example"
)

func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	if err := preflight(); err != nil {
		return err
	}

	cfgPath, err := config.UserConfigPath()
	if err != nil {
		return err
	}
	created, err := installConfig(cfgPath)
	if err != nil {
		return err
	}

	// Save before setting: SaveIdleDelay refuses to overwrite an existing
	// file, so running setup twice cannot record the daemon's own 0 as the
	// user's original and destroy their auto-lock.
	if err := session.SaveIdleDelay(); err != nil {
		return err
	}
	if err := session.SetIdleDelay(0); err != nil {
		return err
	}

	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	if err := systemctl("enable", "--now", unitName); err != nil {
		return err
	}

	saved, _ := session.SavedIdleDelayPath()
	fmt.Println("retrosaver is set up and running.")
	fmt.Println()
	if created {
		fmt.Printf("  config written  %s\n", cfgPath)
	} else {
		fmt.Printf("  config kept     %s (already existed, not overwritten)\n", cfgPath)
	}
	fmt.Printf("  idle-delay      saved to %s, now owned by retrosaver\n", saved)
	fmt.Println()
	fmt.Println("Test it:      retrosaver run atlantis   (then: retrosaver stop)")
	fmt.Println("Watch logs:   journalctl --user -u retrosaver -f")
	fmt.Println("Reverse it:   retrosaver teardown")
	fmt.Println()
	fmt.Println("Note: retrosaver now owns org.gnome.desktop.session idle-delay.")
	fmt.Println("If the daemon is not running there is no auto-lock at all; see the README.")
	return nil
}

func cmdTeardown(args []string) error {
	fs := flag.NewFlagSet("teardown", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Disable first so the unit cannot restart mid-teardown. A unit that was
	// never enabled is not an error worth aborting for.
	if err := systemctl("disable", "--now", unitName); err != nil {
		fmt.Fprintf(os.Stderr, "retrosaver: %v (continuing)\n", err)
	}

	var errs []error
	errs = append(errs, window.StopRunning())
	// Restore before clearing, or there is nothing left to restore from.
	errs = append(errs, session.RestoreIdleDelay())
	errs = append(errs, session.ClearSavedIdleDelay())
	if err := errors.Join(errs...); err != nil {
		return err
	}

	cfgPath, _ := config.UserConfigPath()
	fmt.Println("retrosaver has been torn down.")
	fmt.Println()
	fmt.Println("  idle-delay      restored; GNOME owns the idle policy again")
	fmt.Printf("  config kept     %s (remove it by hand if you want it gone)\n", cfgPath)
	fmt.Println()
	fmt.Println("Remove the package with: sudo apt remove retrosaver")
	return nil
}

// preflight refuses to install into a session retrosaver cannot drive, with
// an explanation rather than a failure later at runtime.
func preflight() error {
	if got := os.Getenv("XDG_SESSION_TYPE"); got != "wayland" {
		return fmt.Errorf(
			"setup: XDG_SESSION_TYPE is %q, not \"wayland\". retrosaver exists for GNOME on "+
				"Wayland; on X11 the upstream xscreensaver daemon works and is a better fit", got)
	}
	if desktop := os.Getenv("XDG_CURRENT_DESKTOP"); !strings.Contains(strings.ToUpper(desktop), "GNOME") {
		return fmt.Errorf(
			"setup: XDG_CURRENT_DESKTOP is %q, which does not look like GNOME. KDE and wlroots "+
				"compositors support the Wayland idle protocols, so upstream xscreensaver 6.11+ "+
				"works there natively and retrosaver is unnecessary", desktop)
	}
	if os.Getenv("DISPLAY") == "" {
		return errors.New(
			"setup: DISPLAY is unset, so XWayland is not reachable. Display modules are X11 " +
				"clients and cannot run without it")
	}

	// The idle monitor is the whole basis of the daemon, so prove it now
	// rather than at the first idle timeout.
	mon, err := idle.Connect()
	if err != nil {
		return fmt.Errorf("setup: %w", err)
	}
	mon.Close()

	if err := requireBinaries(); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	available, err := modules.NewFinder().Available(cfg.Include, cfg.Exclude)
	if err != nil {
		return fmt.Errorf("setup: %w (install the xscreensaver-data and -gl packages)", err)
	}
	fmt.Printf("preflight ok: GNOME on Wayland, idle monitor reachable, %d modules available\n",
		len(available))

	if _, err := os.Stat(packagedUnitPath); err != nil {
		return fmt.Errorf(
			"setup: %s is missing, so there is no unit to enable. Install the .deb rather "+
				"than running setup from a source build", packagedUnitPath)
	}
	return nil
}

// requireBinaries checks the external tools internal/window shells out to.
func requireBinaries() error {
	var missing []string
	for _, bin := range []string{"wmctrl", "xdotool"} {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	// Either name satisfies the pointer-hiding requirement: the
	// unclutter-xfixes package installs unclutter-xfixes, not unclutter.
	if _, err := exec.LookPath("unclutter-xfixes"); err != nil {
		if _, err := exec.LookPath("unclutter"); err != nil {
			missing = append(missing, "unclutter-xfixes")
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("setup: missing required tools: %s (sudo apt install %s)",
			strings.Join(missing, ", "), strings.Join(missing, " "))
	}
	return nil
}

// installConfig writes the user's config file if it does not exist, reporting
// whether it created one. An existing config is never overwritten.
func installConfig(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("setup: checking %s: %w", path, err)
	}

	// Prefer the commented example the package ships; fall back to rendering
	// the compiled-in defaults so setup also works from a source build.
	content, err := os.ReadFile(exampleConfigPath)
	if err != nil {
		content = []byte(defaultConfigFile())
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("setup: creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return false, fmt.Errorf("setup: writing %s: %w", path, err)
	}
	return true, nil
}

// defaultConfigFile renders config.Defaults as a config file, so a source
// build produces the same settings the package's example documents.
func defaultConfigFile() string {
	d := config.Defaults()
	return fmt.Sprintf(`# Seconds of idle before the screensaver starts.
SAVER_DELAY=%d

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
		int(d.LockAfter.Seconds()),
		int(d.BlankAfter.Seconds()),
		strings.Join(d.Exclude, " "),
		strings.Join(d.Include, " "))
}

func systemctl(args ...string) error {
	full := append([]string{"--user"}, args...)
	out, err := exec.Command("systemctl", full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %w: %s",
			strings.Join(full, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
