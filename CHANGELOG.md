# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and this project adheres to
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

> **Keep this file and `changelog.yaml` in sync by hand.** nfpm builds the package's
> `changelog.Debian.gz` from `changelog.yaml`, which uses the
> [goreleaser/chglog](https://github.com/goreleaser/chglog) schema and cannot read Markdown.
> Nothing enforces the correspondence, so an entry added here needs the same entry there.

## [Unreleased]

## [0.0.4] - 2026-09-01

### Fixed

- Releasing no longer races with itself. `version-release.yaml` both pushed the tag *and*
  dispatched `release.yaml`, and on Forgejo 16 the tag push fires `release.yaml` on its
  own — so two runs competed for one release and the loser died with `KeyError: 'id'`
  trying to create a release that already existed. The tag push is now the only trigger.
- Creating a release is idempotent and fails loudly. It reuses an existing release for the
  tag, skips assets already attached, uses `--fail-with-body` on every call, and asserts
  four assets before finishing. Previously the upload `curl` had no `--fail`, so a 401
  printed its status code and the job still went green with an assetless release.

## [0.0.3] - 2026-09-01

### Fixed

- Upgrading the package now restarts the daemon. `apt install` replaces
  `/usr/bin/retrosaver` on disk but does not restart a `systemd --user` service, so the old
  process kept running — `/proc/<pid>/exe` reading `"(deleted)"` — and a newly installed
  feature was on disk but not actually running. A new `postinst` runs
  `deb-systemd-invoke --user daemon-reload` and `--user restart retrosaver.service`, which
  also makes a changed unit file visible to systemd. Users who never ran `retrosaver setup`
  are skipped, so this cannot start the daemon for someone who did not opt in.

## [0.0.2] - 2026-09-01

### Added

- The daemon re-reads its configuration without a restart. It watches
  `~/.config/retrosaver/retrosaver.conf` with inotify, so saving the file is enough, and it
  also reloads on `SIGHUP` — exposed as `systemctl --user reload retrosaver`. Reloading
  keeps the process, its D-Bus connection and its ownership of `idle-delay`, all of which a
  restart drops for a moment.

### Notes

- A reload behaves like user activity: any module on screen is torn down and the stages
  re-arm from zero, counted from the moment the file was saved.
- A configuration that fails to parse is not applied. The error is logged and the previous
  settings stay in force, because exiting would leave the session with no screensaver and
  no auto-lock.

## [0.0.1] - 2026-09-01

First release.

### Added

- Idle daemon for GNOME on Wayland, driven by `org.gnome.Mutter.IdleMonitor` — the only
  idle signal GNOME exposes, since its compositor implements no Wayland idle protocol.
- Three stages, all timed from when the session goes idle, and all torn down and re-armed
  by any user activity: a random XScreenSaver display module fullscreen at 5 minutes,
  GNOME's own lock screen at 20 minutes, display off at 22 minutes.
- Six subcommands in one static binary: `daemon`, `run`, `stop`, `list`, `setup`,
  `teardown`.
- Per-user installation through `retrosaver setup` / `teardown` rather than Debian
  maintainer scripts, since enabling a `systemd --user` unit and setting `idle-delay`
  need the user's own session bus.
- Module discovery by intersecting `/usr/share/xscreensaver/config/*.xml` basenames with
  the executables in `/usr/libexec/xscreensaver/`, so the list follows installed packages
  instead of a hardcoded inventory.
- `.deb` packages for amd64 and arm64, declaring `Conflicts: xscreensaver` — the
  xscreensaver daemon is the broken component and would autostart and emit errors.
- Manual page, README and `changelog.Debian.gz` shipped in the package.

### Known limitations

- **This is not a screen locker** and cannot be one: a module is an ordinary X11 window
  and X11 grabs do not work under XWayland. Security comes from the lock stage handing off
  to GNOME.
- `setup` sets `idle-delay` to `0`, so the daemon owns the whole idle policy. If it is not
  running there is no auto-lock; `Restart=always`, `ExecStopPost` and `teardown` all exist
  to bound that.
- GNOME on Wayland only. KDE and wlroots support the Wayland idle protocols, so upstream
  xscreensaver 6.11+ works there natively.

[Unreleased]: https://github.com/c-premus/retrosaver/compare/v0.0.4...HEAD
[0.0.4]: https://github.com/c-premus/retrosaver/releases/tag/v0.0.4
[0.0.3]: https://github.com/c-premus/retrosaver/releases/tag/v0.0.3
[0.0.2]: https://github.com/c-premus/retrosaver/releases/tag/v0.0.2
[0.0.1]: https://github.com/c-premus/retrosaver/releases/tag/v0.0.1
