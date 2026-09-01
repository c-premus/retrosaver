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
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/c-premus/retrosaver/internal/config"
	"github.com/c-premus/retrosaver/internal/daemon"
	"github.com/c-premus/retrosaver/internal/modules"
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
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	return daemon.New(cfg).Run(ctx)
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	_ = fs.Arg(0) // optional module name
	return fmt.Errorf("run: not implemented")
}

func cmdStop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	keepIdleDelay := fs.Bool("keep-idle-delay", false,
		"do not restore idle-delay (used by the daemon between stages)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_ = *keepIdleDelay
	return fmt.Errorf("stop: not implemented")
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

func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return fmt.Errorf("setup: not implemented")
}

func cmdTeardown(args []string) error {
	fs := flag.NewFlagSet("teardown", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return fmt.Errorf("teardown: not implemented")
}
