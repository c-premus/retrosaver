# retrosaver

Retro screensavers for GNOME on Wayland.

`retrosaver` brings back late-1990s and early-2000s XScreenSaver display modules — fractals,
the `atlantis` fish tank, flying toasters, spinning pipes — on modern GNOME desktops running
Wayland, where the traditional screensaver daemon no longer works.

It does **not** fork, patch, or vendor XScreenSaver. It reuses the display modules that
Debian and Ubuntu already package, and supplies only the piece GNOME dropped: an idle
trigger and a fullscreen wrapper. `retrosaver` is glue. All the actual artwork belongs to
XScreenSaver.

> **Status: implemented and verified, not yet released.** All the pieces are written and
> unit-tested, and steps 1–5 of the `docs/spec.md` §8 procedure are verified against a live
> GNOME 50.1 session — idle detection, session control, the fullscreen wrapper, and the full
> saver → lock → blank → teardown sequence. Reboot persistence (§8 step 6) is the one
> remaining check, and no release has been cut yet. See `memory-bank/progress.md`.

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
| Fullscreen and always-on-top for an XWayland window | Mutter implements EWMH for X11 clients |
| Locking the session | `loginctl lock-session` hands off to GNOME's lock screen |
| Blanking the display | GNOME's own `org.gnome.desktop.session idle-delay` |

## How it works

Three stages, all timed from when the session goes idle. Any keyboard or mouse activity at
any stage tears everything down and re-arms from zero.

| Stage | Default trigger | Action |
|---|---|---|
| 1. Saver | 5 min idle | Launch a random module fullscreen, always-on-top, pointer hidden |
| 2. Lock | 20 min idle | Kill the module, `loginctl lock-session` |
| 3. Blank | 22 min idle | Power the display off |

All three delays are configurable, and stages 2 and 3 can be disabled individually.

## Install

Download the `.deb` for your architecture from the
[latest release](https://github.com/c-premus/retrosaver/releases), then:

```bash
sudo apt install ./retrosaver_<version>_amd64.deb
retrosaver setup
```

`apt` pulls the XScreenSaver module packages as dependencies. The package also declares
`Conflicts: xscreensaver`, because the daemon is the broken component and would otherwise
autostart and emit errors alongside this one.

`retrosaver setup` is a separate step by design. It runs the preflight checks, enables the
`systemd --user` unit and takes ownership of `idle-delay` — all per-user operations that
need your own session bus, which a package's root install script cannot do correctly.

To reverse it:

```bash
retrosaver teardown
sudo apt remove retrosaver
```

## Configuration

`~/.config/retrosaver/retrosaver.conf`, created by `retrosaver setup` and never overwritten:

```sh
SAVER_DELAY=300     # idle seconds before the screensaver starts
LOCK_AFTER=900      # seconds after the saver starts before locking. 0 disables
BLANK_AFTER=120     # seconds after locking before the display powers off. 0 disables

# Modules never to pick: these need image assets, network access, or elevated
# capabilities and misbehave standalone.
EXCLUDE="webcollage vidwhacker glslideshow photopile carousel sonar"

# If non-empty, pick only from this list.
INCLUDE=""
```

Useful commands:

```bash
retrosaver list              # print the modules that would be picked from
retrosaver run atlantis      # launch one module now
retrosaver stop              # tear it down
journalctl --user -u retrosaver -f
```

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

See [`AGENTS.md`](AGENTS.md) for the full development guide, and
[`.devcontainer/README.md`](.devcontainer/README.md) for the container's scope — notably
that it **cannot** run or integration-test the daemon, since there is no Mutter, session
bus, XWayland or `systemd --user` inside a container.

```bash
go build ./...
go test ./...
gofmt -l .
```

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
