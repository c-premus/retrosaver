# retrosaver

[![CI](https://github.com/c-premus/retrosaver/actions/workflows/ci.yaml/badge.svg)](https://github.com/c-premus/retrosaver/actions/workflows/ci.yaml)
[![Release](https://img.shields.io/github/v/release/c-premus/retrosaver?sort=semver)](https://github.com/c-premus/retrosaver/releases/latest)
[![Downloads](https://img.shields.io/github/downloads/c-premus/retrosaver/total)](https://github.com/c-premus/retrosaver/releases)
[![Go Version](https://img.shields.io/github/go-mod/go-version/c-premus/retrosaver)](https://go.dev/)
[![Platform](https://img.shields.io/badge/platform-GNOME%20%7C%20Wayland-4a86cf)](#why-this-exists)
[![License](https://img.shields.io/github/license/c-premus/retrosaver)](LICENSE)

Retro screensavers for GNOME on Wayland.

`retrosaver` brings back late-1990s and early-2000s XScreenSaver display modules — fractals,
the `atlantis` fish tank, flying toasters, spinning pipes — on modern GNOME desktops running
Wayland, where the traditional screensaver daemon no longer works.

It does **not** fork, patch, or vendor XScreenSaver. It reuses the display modules that
Debian and Ubuntu already package, and supplies only the piece GNOME dropped: an idle
trigger and a fullscreen wrapper. `retrosaver` is glue. All the actual artwork belongs to
XScreenSaver.

> **Status: implemented and verified on the reference host.** All the pieces are written
> and unit-tested, and **all seven steps** of the manual verification procedure pass
> against a live GNOME 50.1 session — idle detection, session control, the fullscreen
> wrapper, the full saver → lock → blank → teardown sequence, no module behind a manual
> Super+L lock, and reboot persistence.

## Why this exists

The traditional approach is dead on GNOME/Wayland:

- The `xscreensaver` daemon gained partial Wayland support in 6.11, but **GNOME remains
  unsupported and xscreensaver crashes under it** — GNOME's compositor exposes no Wayland
  idle protocol. Debian and Ubuntu currently ship 6.08, which predates even that.
- The historical workaround, logging into an Xorg session, **no longer exists**. GNOME 50
  ships no X11 session at all.
- `gnome-screensaver`, `mate-screensaver`, `cinnamon-screensaver` and `xfce4-screensaver`
  are all X11-era daemons with the same problem.

But the parts needed to rebuild the thin missing layer all work:

| Capability | Status on GNOME 50 / Wayland |
|---|---|
| XScreenSaver **modules** as standalone programs | Ordinary X11 clients, run fine under XWayland |
| Installing modules **without** the daemon | The data and gl packages only `Suggests:` `xscreensaver` |
| Idle detection | `org.gnome.Mutter.IdleMonitor` D-Bus API |
| Fullscreen and always-on-top for an XWayland window | Mutter implements EWMH for X11 clients, so retrosaver can create one fullscreen window per monitor and run a module inside each |
| Locking the session | `loginctl lock-session` hands off to GNOME's lock screen |
| Blanking the display | GNOME's own `org.gnome.desktop.session idle-delay` |

## How it works

Three stages, all timed from when the session goes idle. Any keyboard or mouse activity at
any stage tears everything down and re-arms from zero.

| Stage | Default trigger | Action |
|---|---|---|
| 1. Saver | 5 min idle | Launch a random module fullscreen on every monitor, always-on-top, pointer hidden |
| 2. Lock | 20 min idle | Kill the module, `loginctl lock-session` |
| 3. Blank | 22 min idle | Power the display off |

While the saver stage runs, the module is swapped for another one every 5 minutes, picked
at random from those not yet shown this idle period — so a long break is a slideshow
rather than a quarter of an hour of the same fractal. Once every selectable module has
been shown the set starts over, and if only one module is selectable nothing switches.

If the session is already locked when the saver stage comes due — you pressed Super+L and
walked away — no module starts. GNOME's lock screen sits above every XWayland window, so
a module launched behind it would be invisible while still driving the GPU and keeping
the display awake. Stages 2 and 3 still run on schedule, so the display still powers off.

All the delays are configurable, cycling and stages 2 and 3 can each be disabled, and
changes take effect as soon as you save the file.

## Install

### From a release asset

Download the `.deb` for your architecture from the
[latest release](https://github.com/c-premus/retrosaver/releases/latest), then:

```bash
sudo apt install ./retrosaver_<version>_amd64.deb
retrosaver setup
```

`apt` pulls the XScreenSaver module packages as dependencies, along with
`unclutter-xfixes`, which hides the pointer. The package also declares
`Conflicts: xscreensaver`, because the daemon is the broken component and would otherwise
autostart and emit errors alongside this one.

`retrosaver setup` is a separate step by design. It runs the preflight checks, enables the
`systemd --user` unit and takes ownership of `idle-delay` — all per-user operations that
need your own session bus, which a package's root install script cannot do correctly.

Upgrading is the same command against a newer `.deb` — `retrosaver setup` does not need
re-running, and the package restarts the daemon for you so the new binary actually takes
effect.

> The restart needs systemd 249.10 or 250 or newer to reach your user session. On an
> older systemd the package skips it quietly and the old daemon keeps running until you
> restart it once:
>
> ```bash
> systemctl --user daemon-reload
> systemctl --user restart retrosaver
> ```
>
> To check which binary is actually running:
> `ls -l /proc/$(systemctl --user show retrosaver -p MainPID --value)/exe` — if it ends in
> `(deleted)`, the process predates the installed package.

To reverse it:

```bash
retrosaver teardown
sudo apt remove retrosaver
```

### Fedora (community package)

A Fedora package is maintained by a contributor in the
[`cdamian/retrosaver` COPR](https://copr.fedorainfracloud.org/coprs/cdamian/retrosaver/).
It is community-maintained: it is not built, tested or released from this repository, so
report packaging problems there. `retrosaver setup` and everything below apply unchanged.

## Configuration

`~/.config/retrosaver/retrosaver.conf` is created by `retrosaver setup` and never
overwritten. It is a commented copy of these defaults:

```sh
SAVER_DELAY=300     # idle seconds before the screensaver starts
CYCLE_AFTER=300     # seconds each module lasts before switching to another. 0 disables
LOCK_AFTER=900      # seconds after the saver starts before locking. 0 disables locking and blanking
BLANK_AFTER=120     # seconds after locking before the display powers off. 0 disables

# Modules never to pick: these need image assets, network access, or elevated
# capabilities and misbehave standalone.
EXCLUDE="webcollage vidwhacker glslideshow photopile carousel sonar"

# If non-empty, pick only from this list.
INCLUDE=""
```

Values are whole seconds, and unknown keys are ignored. The file is parsed as
`KEY=value` lines and never run as a shell script.

Changes take effect as soon as you save the file: the daemon watches it and re-arms
itself. `systemctl --user reload retrosaver` does the same thing on demand, and neither
drops the D-Bus connection or the ownership of `idle-delay` the way a restart briefly does.

Two things worth knowing about a reload:

- **It behaves like user activity** when a setting has actually changed. Any module on
  screen is torn down and the stages re-arm from zero, so the new timings are counted from
  the moment you save. Saving without changing anything does nothing.
- **A broken config is ignored.** If the file will not parse, the error is logged and the
  daemon carries on with the settings it already had, rather than exiting and taking your
  auto-lock with it. A daemon that *starts* on a broken file uses the built-in defaults,
  and `run`, `list` and `setup` refuse to run until it is fixed.

Useful commands:

```bash
retrosaver list              # print the modules that would be picked from
retrosaver run atlantis      # launch one module now, even one EXCLUDE lists
retrosaver stop              # tear it down
systemctl --user reload retrosaver   # re-read the config now
journalctl --user -u retrosaver -f
```

`retrosaver stop` also gives `idle-delay` back to GNOME, but only when the daemon is not
running. While it runs the daemon owns that setting, so stopping a module you started by
hand leaves it alone. While the daemon runs, it treats a module that disappears, whether
it crashed or `retrosaver stop` killed it, as one to replace, up to three times per idle
period. Moving the mouse or pressing a key is what ends the screensaver.

For more detail in the journal, set `Environment=RETROSAVER_LOG_LEVEL=debug` under
`[Service]` with `systemctl --user edit retrosaver`, then restart the unit.

## The `idle-delay 0` tradeoff

Read this section before installing.

Setting `org.gnome.desktop.session idle-delay` to `0` is what stops gnome-shell blanking
the screen out from under the screensaver. It also disables GNOME's idle-dim. It makes
`retrosaver` the owner of your entire idle policy.

**Consequence: if the daemon is not running, there is no auto-lock at all.**

Mitigations, all implemented:

- `Restart=always` with `RestartSec=5` in the systemd unit
- `ExecStopPost=retrosaver stop` restores `idle-delay` whenever the service stops
- `retrosaver teardown` restores it, from the value saved at setup time

## Known limitations

- **This is not a screen locker.** The module is an ordinary window and cannot secure the
  session; X11 grabs do not work under XWayland. Real security comes from stage 2 handing
  off to GNOME's own lock screen. Wanting the screensaver itself to lock is not possible on
  Wayland, by design.
- **Battery.** A typical laptop's `sleep-inactive-battery-timeout` is 900 s with type
  `suspend`, so the machine may suspend before stages 2 and 3 land. Raise it if that
  matters to you:
  ```bash
  gsettings set org.gnome.settings-daemon.plugins.power sleep-inactive-battery-timeout 2400
  ```
  `retrosaver setup` deliberately does not change power settings.
- Modules that grab and distort a desktop screenshot show a solid colour instead — `grim`
  does not work under GNOME's compositor.
- GL modules run through XWayland and keep the GPU awake. Stages 2 and 3 exist partly to
  bound that.
- **GNOME-and-Wayland specific.** KDE and wlroots compositors support the Wayland idle
  protocols, so upstream xscreensaver 6.11+ works there natively and this project is
  unnecessary.

## Development

Start with [`CONTRIBUTING.md`](CONTRIBUTING.md), then
[`docs/development.md`](docs/development.md) for the full development guide. Note that
the daemon **cannot** be run or integration-tested in a container or in CI, since there is
no Mutter, session bus, XWayland or `systemd --user` there.

Security policy, and a note on why this is not a screen locker, are in
[`SECURITY.md`](SECURITY.md).

```bash
go build -o retrosaver ./cmd/retrosaver
go test ./...
gofmt -l .
```

A source build can run modules and the daemon, but `retrosaver setup` needs the installed
`.deb`: it enables the packaged systemd unit, and refuses when that unit is missing.

## Credits

All the display modules, and all the artwork in them, are
[XScreenSaver](https://www.jwz.org/xscreensaver/) by Jamie Zawinski and contributors.
`atlantis`, the fish tank, is SGI demo code from 1998. This project only supplies the idle
trigger and the fullscreen wrapper.

Background reading:

- [jwz: XScreenSaver and Wayland (June 2025)](https://www.jwz.org/blog/2025/06/xscreensaver-and-wayland/)
- [jwz: Wayland and screen savers (Sept 2023)](https://www.jwz.org/blog/2023/09/wayland-and-screen-savers/)
- [XScreenSaver manual](https://www.jwz.org/xscreensaver/man1.html)
- [pgn674/xscreensaver-wayland-enhancement](https://github.com/pgn674/xscreensaver-wayland-enhancement)

## License

MIT. See [LICENSE](LICENSE).
