# Development guide

How to build, test and package `retrosaver`, and the accumulated list of things
that will bite you if you do not know them. Read [CONTRIBUTING.md](../CONTRIBUTING.md)
first for how to get a change accepted.

## Project overview

`retrosaver` brings back late-1990s XScreenSaver display modules — fractals, the `atlantis`
fish tank, flying toasters, spinning pipes — on GNOME desktops running Wayland, where the
traditional screensaver daemon no longer functions.

It does **not** fork, patch, or vendor XScreenSaver. It reuses the display modules that
Debian and Ubuntu already package, and supplies only the piece GNOME dropped: an idle
trigger and a fullscreen wrapper. It ships as one static Go binary plus a systemd user unit.

**Terminology.** XScreenSaver upstream calls its display modules "hacks", in the demoscene
sense. This project says **module** everywhere — code, docs, config keys, log output — to
avoid the misleading connotation.

**Status: implemented and verified on the host.** All seven packages are real and
unit-tested, and no subcommand returns `ErrNotImplemented`. `idle`, `session`, `window` and
the end-to-end state machine have all been verified against a live GNOME 50.1 session —
**all six steps of `docs/spec.md` §8**, including the full saver → lock → blank → teardown
sequence, a second cycle, and reboot persistence from an installed `.deb`.

### Architecture

Three stages, all timed from when the session goes idle. Any user activity at any stage
tears everything down and re-arms from zero.

| Stage | Default | Action |
|---|---|---|
| Saver | 5 min idle | Launch a random module fullscreen, always-on-top, pointer hidden |
| Lock | 20 min idle | Kill the module, `loginctl lock-session` |
| Blank | 22 min idle | Power the display off |

```
cmd/retrosaver/      subcommand dispatch: daemon | run | stop | list | setup | teardown
internal/config/     KEY=value parser (never executes the file)
internal/modules/    discovery: config XML basenames ∩ executables in libexec
internal/idle/       org.gnome.Mutter.IdleMonitor D-Bus client
internal/window/     wmctrl / xdotool / unclutter wrappers
internal/session/    loginctl lock-session, gsettings idle-delay
internal/watch/      inotify watch on the config file, for live reload
internal/daemon/     the four-watch state machine
docs/spec.md         the original implementation specification
```

`docs/spec.md` is the behavioural source of truth, and the Go doc comments cite it by
section number. Its §5 and §6.6 are **superseded** — they describe the original Python +
bash + `install.sh` design, not the Go one-binary/`.deb` one. Its §3, §6.2–6.4, §7 and §8
still hold.

## Development commands

### Quick start

```bash
# Requires Go 1.26. The devcontainer provides it.
go build ./...
./retrosaver help
```

### Testing

```bash
go test ./...          # unit tests (config + modules only; see gotchas)
go test -race ./...    # what CI runs
tests/smoke.sh         # host-only; needs a real GNOME/Wayland session
```

### Code quality

```bash
gofmt -l .             # must print nothing; CI gates on this
go vet ./...
shellcheck tests/smoke.sh scripts/postinstall.sh scripts/generate-changelog.sh
```

CI gates on shellcheck, and the host may not have it while the devcontainer does. Fetch it
without root:

```bash
apt-get download shellcheck && dpkg-deb -x ./shellcheck_*.deb /tmp/sc
/tmp/sc/usr/bin/shellcheck tests/smoke.sh scripts/*.sh
```

### Packaging

Use the release workflow's exact flags, or `main.version` stays `"dev"`. The man page must
be gzipped into `dist/` first: nfpm does not compress man pages, and `nfpm.yaml` refers to
`./dist/retrosaver.1.gz`.

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X main.version=0.0.1" -o dist/retrosaver-linux-amd64 ./cmd/retrosaver
gzip -9 -n -c docs/retrosaver.1 > dist/retrosaver.1.gz
cp dist/retrosaver-linux-amd64 dist/retrosaver
VERSION=0.0.1 GOARCH=amd64 nfpm pkg --packager deb --target dist/
```

## Code standards

- Go 1.24 is the language floor; `toolchain go1.26.7` is what CI and the devcontainer
  build with. Raise the floor only when the code needs a newer language feature — it is
  what lets a host with an older Go and no toolchain download still build. `gofmt` is a
  hard CI gate; `go vet` and `go test -race` likewise.
- Standard library only wherever possible. There are **exactly two** direct dependencies,
  both pure Go, and that is what keeps `CGO_ENABLED=0` viable and the artifact genuinely
  static:
  - `github.com/godbus/dbus/v5` — the session bus.
  - `golang.org/x/sys` — the inotify syscalls, imported directly by `internal/watch`.
    This is not a slip. The stdlib `syscall` package is frozen and its own documentation
    points callers at `x/sys`, so removing it would trade an endorsed dependency for a
    discouraged one.

  A third needs a recorded decision. **A cgo dependency is forbidden outright** — the
  invariant that matters is pure-Go, not the count. `go mod tidy -diff` in CI is what
  enforces the list mechanically.
- Wrap errors with `%w` and enough context to name the file or D-Bus call that failed.
- Exported identifiers carry doc comments; every package has a package comment explaining
  what part of the GNOME stack it touches.
- Shell scripts: `set -o errexit -o nounset -o pipefail`, and they must pass `shellcheck`.

## Common gotchas

- **`idle-delay 0` makes retrosaver the owner of the entire idle policy.** That is what
  stops gnome-shell blanking the screen out from under the screensaver, and it also
  disables GNOME's idle-dim. **Consequence: if the daemon is not running, there is no
  auto-lock at all.** Every mitigation must stay: `Restart=always` in the unit,
  `ExecStopPost=retrosaver stop`, and restoration on teardown.
- **This is not a screen locker and must never try to be one.** A module is an ordinary
  X11 window and X11 grabs do not work under XWayland. Security comes from stage 2 handing
  off to GNOME's own lock screen.
- **Never `Depends:` or install the `xscreensaver` package.** The daemon is the broken
  component — it does not work under GNOME's compositor and will autostart and emit errors.
  The `.deb` declares `Conflicts: xscreensaver` for exactly this reason. The
  `xscreensaver-data`/`-gl` packages are a separate and required matter; they only
  `Suggests:` the daemon, so they install without it.
- **Module discovery is an intersection, never a hardcoded list.** Every genuine display
  module ships `/usr/share/xscreensaver/config/<name>.xml`; helper binaries do not.
  Intersect those basenames with the executables in `/usr/libexec/xscreensaver/`. A
  hardcoded inventory rots at the next package update.
- **A config reload must re-point the launcher, not just `Daemon.cfg`.** `realLauncher`
  keeps its own copy of `include`/`exclude`, taken in `New`, and `Pick` reads those fields
  rather than `d.cfg`. Updating `d.cfg` alone reloads the timings and silently ignores a
  changed `INCLUDE` — the most likely edit. That is what `launcher.SetFilters` exists for,
  and `TestReloadRepointsTheLauncher` is what stops it regressing.
- **Watch the config file's directory, never the file.** Editors save by writing a temp
  file and renaming it over the target, which replaces the inode. A watch on the old inode
  survives as a handle that never fires again — it looks healthy and reports nothing.
  `internal/watch` watches `filepath.Dir(path)` and filters on the basename, which catches
  the rename, a plain write, and a file that did not exist yet.
- **Wrap the inotify fd in an `os.File`; never read it as a raw int.** Closing a raw fd
  while a goroutine is blocked in `unix.Read` is a real race, not a theoretical one: the
  descriptor number gets recycled and the reader starts reading someone else's fd. It
  showed up as intermittent `EBADF` and a watch that silently reported nothing, failing
  about one run in six. `os.File` reference-counts the descriptor, which fixes that, and
  `os.File.Read` retries `EINTR` internally — which matters because the Go runtime's
  preemption signals interrupt a blocking read often.
- **…and create it with `IN_NONBLOCK`, or `Close` will not unblock the reader.** These are
  two separate guarantees and only the refcount one comes from `os.File` alone.
  `os.NewFile` hands a descriptor to the runtime poller only when `O_NONBLOCK` is already
  set on it — `os.newFile` computes `pollable := kind == kindOpenFile || kind == kindPipe
  || kind == kindSock || nonBlocking`, and `nonBlocking` comes from
  `unix.HasNonblockFlag`. A blocking inotify fd lands on the `kindNewFile` path, is never
  registered with epoll, and its `Read` parks in `read(2)` where `Close` cannot reach it,
  so the reader goroutine leaks until an unrelated event happens to land in the watched
  directory. `unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)` is what buys the
  unblock, and `TestCancelStopsTheReaderGoroutine` fails the moment the flag is dropped.
- **SIGHUP must not go through `signal.NotifyContext`.** That cancels the context, so a
  reload would stop the daemon instead of re-reading. It gets its own `signal.Notify`
  channel in `cmdDaemon`.
- **The "no timers in the daemon" rule is why the reload debounce lives in
  `internal/watch`.** One save emits several inotify events; they are coalesced there, so
  `internal/daemon` still owns no timers and its tests still synchronise on the trace
  channel rather than sleeping.
- **The config file is parsed, never sourced.** It is shell-shaped `KEY=value`, but
  `internal/config` uses a line parser. Do not "simplify" this by shelling out.
- **The devcontainer cannot run or integration-test the daemon.** No Mutter, no session
  bus, no XWayland, no `systemd --user`. It builds, vets, unit-tests and packages. Real
  verification is the manual procedure in `docs/spec.md` (§8), on the host. See `.devcontainer/README.md`.
- **A green `go test` is not evidence the screensaver works.** The unit tests cover pure
  logic and the state machine against fakes; they never touch X, D-Bus or systemd. Anything
  involving a real window is proven only by `docs/spec.md` §8 on a host.
- **Live tests are gated on `RETROSAVER_LIVE`, not a build tag**, so `go test ./...` stays
  green anywhere while one command exercises them on a real session:
  `RETROSAVER_LIVE=1 go test ./internal/... -run Live -v`. `TestLiveLock` needs
  `RETROSAVER_LIVE_LOCK=1` as well, because it genuinely locks the screen.
- **An overdue idle watch fires as soon as it is added.** Verified against gnome-shell 50.1,
  so the daemon needs no cold-start handling when it starts on an already-idle session.
  Do not add any.
- **User activity cannot be faked, so do not try.** Mutter gates `ResetIdletime` behind
  `MUTTER_DEBUG_RESET_IDLETIME`, and injecting XTEST input with `xdotool mousemove` does
  **not** move the idle clock either (verified: 17.0s → 17.3s straight through it), because
  the idle monitor watches libinput, not synthetic X events. Worse, GNOME treats XTEST
  injection through XWayland as remote control and prompts the user to "allow remote
  interaction". Anything needing a reset idle clock needs a human, and lives behind
  `RETROSAVER_LIVE_INPUT=1`. `xdotool search` in `internal/window` is fine — it queries
  windows and injects nothing.
- **Whether idle watches re-arm after a reset is unknown**, and the daemon does not depend
  on it: `daemon.rearm` drops every watch and registers a fresh set, which is correct under
  either behaviour. `RemoveWatch` accepts unknown and already-removed IDs without
  complaint, which is what makes that safe — and also why a successful `RemoveWatch` after
  a fire proves nothing about auto-removal.
- **Do not override `XDG_CONFIG_HOME` when verifying `idle-delay`.** dconf *writes* go
  through the dconf D-Bus service, which uses the session's real `HOME`, but dconf *reads*
  come from `$XDG_CONFIG_HOME/dconf/user`. Point that at a temp dir and `gsettings get`
  silently falls back to the schema default (300) while the daemon's writes land in the
  real database — so a correct daemon looks broken. Read with
  `env -u XDG_CONFIG_HOME gsettings get ...`. Overriding it for retrosaver's *own* config
  is fine and is how the compressed-timing runs are done.
- **`unclutter-xfixes` installs `/usr/bin/unclutter-xfixes`, not `unclutter`**, and declares
  no `Provides: unclutter`. `internal/window` looks up both names. Pointer hiding is
  cosmetic, so failing to start it must never cost a working screensaver.
- **`pkill` exits 1 for "no processes matched"**, which is the normal case. Treating it as a
  failure would break the rule that `retrosaver stop` is a clean no-op.
- **Every PID read off disk is checked against `/proc/<pid>/cmdline` before being signalled.**
  The runtime state file can be minutes stale after a crash and PIDs are recycled.
- **nfpm does not expand environment variables in `contents[].src`.** The binary is staged
  at a fixed path before packaging; a `${GOARCH}` there fails with "Glob failed".
- Per-user installation lives in `retrosaver setup` / `teardown`, **not** in Debian
  maintainer scripts. Enabling a `systemd --user` unit and setting `idle-delay` need the
  user's own session bus and the user's consent; a root `postinst` should not do them.
  **The narrow exception is upgrade handling.** `scripts/postinstall.sh` runs
  `deb-systemd-invoke --user daemon-reload` and `--user restart retrosaver.service`, and
  nothing else. That is not the same as installing for a user: it refreshes systemd's view
  of a unit file that the package itself just replaced, and restarts a daemon that was
  already running. `deb-systemd-invoke --user` walks each `user@<id>.service` with
  `systemctl --user --machine <id>@` (systemd 249.10/250+), and it **skips a restart when
  the unit is neither enabled nor active**, so it cannot start the daemon, and seize
  `idle-delay`, for someone who never ran `setup`. `openssh-client` sets the precedent for
  the `daemon-reload` half.
- **`apt install` replaces the binary but does not restart the daemon by itself.** Without
  the postinst above, the old process keeps running with `/proc/<pid>/exe` reading
  `"(deleted)"`, so a freshly installed feature is on disk but not running, and a changed
  unit file stays invisible to systemd until `daemon-reload`. Observed for real on the
  0.0.1 → 0.0.2 upgrade: the daemon kept the timings it had armed hours earlier and ignored
  every config edit, because the running image predated config reload entirely. When
  diagnosing "my change did nothing", check `/proc/$(systemctl --user show retrosaver -p
  MainPID --value)/exe` before anything else.
