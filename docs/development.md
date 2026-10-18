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
trigger and a fullscreen wrapper. It ships as one static Go binary plus a systemd user unit,
and an optional settings app, `retrosaver-gtk`, in a package of its own.

**Terminology.** XScreenSaver upstream calls its display modules "hacks", in the demoscene
sense. This project says **module** everywhere — code, docs, config keys, log output — to
avoid the misleading connotation.

**Status: implemented and verified on the host.** All eight packages are real and
unit-tested, and no subcommand returns `ErrNotImplemented`. `idle`, `session`, `window` and
the end-to-end state machine have all been verified against a live GNOME 50.1 session —
**all seven steps of the manual verification procedure below**, including the full
saver → lock → blank → teardown
sequence, a second cycle, and reboot persistence from an installed `.deb`.

### Architecture

Three stages, all timed from when the session goes idle. Any user activity at any stage
tears everything down and re-arms from zero. Cycling is not a fourth stage — it repeats
within the saver stage without advancing the sequence — but it is timed the same way, off
the idle monitor.

| Stage | Default | Action |
|---|---|---|
| Saver | 5 min idle | Launch a random module fullscreen on every monitor, always-on-top, pointer hidden |
| Cycle | every 5 min thereafter | Swap in another module not yet shown this idle period |
| Lock | 20 min idle | Kill the module, `loginctl lock-session` |
| Blank | 22 min idle | Power the display off |

```
cmd/retrosaver/      subcommand dispatch: daemon | run | stop | list | config | setup | teardown (+ version, help)
internal/config/     KEY=value parser (never executes the file)
internal/modules/    discovery: config XML basenames ∩ executables in libexec
internal/idle/       org.gnome.Mutter.IdleMonitor D-Bus client
internal/window/     saver windows (one per monitor, over X via xgb), module processes, unclutter
internal/session/    loginctl lock-session, gsettings idle-delay, logind LockedHint watch
internal/watch/      inotify watch on the config file, for live reload
internal/daemon/     the state machine: five watch kinds, the cycle one self-re-arming
gui/                 retrosaver-settings, the GTK 4 / libadwaita settings app (Python),
                     with its desktop entry, AppStream metainfo and icons
```

The behavioural contract is described here and in `README.md`; the manual verification
procedure that actually proves it is under **Manual verification** below.

## Development commands

### Quick start

```bash
# Needs Go 1.26 or newer. With the default GOTOOLCHAIN=auto any such Go fetches
# the toolchain go.mod names (go1.27.1) and builds with that instead.
go build -o retrosaver ./cmd/retrosaver
./retrosaver help
```

### Testing

```bash
go test ./...          # every package; live tests skip unless RETROSAVER_LIVE=1
go test -race ./...    # what CI runs
tests/smoke.sh         # host-only; needs a real GNOME/Wayland session
python3 -m unittest discover -s gui    # the settings app's logic; no GTK needed
desktop-file-validate gui/us.prem.Retrosaver.desktop
appstreamcli validate --no-net gui/us.prem.Retrosaver.metainfo.xml
```

### Code quality

```bash
gofmt -l .             # must print nothing; CI gates on this
go vet ./...
shellcheck tests/smoke.sh scripts/*.sh
```

CI gates on shellcheck, and the host may not have it. Fetch it without root:

```bash
apt-get download shellcheck && dpkg-deb -x ./shellcheck_*.deb /tmp/sc
/tmp/sc/usr/bin/shellcheck tests/smoke.sh scripts/*.sh
```

### Previewing modules

`scripts/demo.sh` runs modules one after another in an ordinary window, printing each
one's name and description before it starts. It is a preview, not the screensaver:
nothing goes fullscreen, and a running daemon is left alone.

```bash
scripts/demo.sh                 # the configured set, as `retrosaver list` selects it
scripts/demo.sh --all -t 10     # every installed module, 10 s each
scripts/demo.sh atlantis flame  # exactly these, in this order
```

It holds a GNOME idle inhibitor while it runs, so the daemon cannot start a fullscreen
module over the demo or lock the session. It stops only the processes it started and
never calls `retrosaver stop`, which stops every running module rather than just the
demo's, and which in older releases also gave `idle-delay` back to GNOME while the daemon
still owned it.

### Packaging

Use the release workflow's exact flags, or `main.version` stays `"dev"`. The man page must
be gzipped into `dist/` first: nfpm does not compress man pages, and `nfpm.yaml` refers to
`./dist/retrosaver.1.gz`. CI and the release workflow use nfpm 2.48.0.

```bash
mkdir -p dist
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
  -ldflags "-s -w -X main.version=0.0.0-dev" -o dist/retrosaver-linux-amd64 ./cmd/retrosaver
gzip -9 -n -c docs/retrosaver.1 > dist/retrosaver.1.gz
cp dist/retrosaver-linux-amd64 dist/retrosaver
VERSION=0.0.0-dev GOARCH=amd64 nfpm pkg --packager deb --target dist/
gzip -9 -n -c docs/retrosaver-settings.1 > dist/retrosaver-settings.1.gz
VERSION=0.0.0-dev nfpm pkg --config nfpm-gtk.yaml --packager deb --target dist/
```

The second package, `retrosaver-gtk`, is the settings app. It is `Architecture: all`, so
a release builds it once, and it `Depends:` on exactly the same `retrosaver` version.
`build-apt-repo.sh` expects an `all` package to appear in every arch's index.

### The settings app

`gui/retrosaver-settings` is a single Python 3 file using PyGObject, GTK 4 and libadwaita:
the usual way to write a small GNOME settings tool (GNOME Tweaks is one). Run it from the
tree against a local build, pointing it at a scratch config so it cannot touch yours:

```bash
go build -o retrosaver ./cmd/retrosaver
XDG_CONFIG_HOME="$(mktemp -d)" RETROSAVER=./retrosaver gui/retrosaver-settings
```

Rules it follows:

- **It is a front end, not a second config implementation.** It reads and writes
  nothing itself, and calls `retrosaver config show --json`, `retrosaver config set` and
  `retrosaver list --all --json`. Those outputs are the contract between the two packages,
  which is why `retrosaver-gtk` pins its `retrosaver` version exactly. A new config key
  needs a control here too.
- **Logic that can be tested without GTK stays outside it.** Everything above `main()`
  imports no `gi`, and `gui/test_settings.py` covers it anywhere.
- **The window itself is tested too**, by `gui/test_window.py`. It builds a real window
  through `gtk_classes()`, sets controls, waits out the half-second save delay, and reads
  the file. It covers the timings, both module modes, search, the service switch with its
  confirmation and failure dialogs, a failed save, and a broken config, which is about 90%
  of the app. A stub `retrosaver` (real `config`/`list`, recorded `setup`/`teardown`) and a
  stub `systemctl` keep it away from your session. Run it on a session or under Xvfb, with
  a build that has the `config` command:

  ```bash
  go build -o retrosaver ./cmd/retrosaver
  RETROSAVER_BIN=./retrosaver xvfb-run -a python3 -m unittest discover -s gui -v
  ```

  Without GTK, libadwaita 1.2, a display or such a binary, these tests skip rather than
  fail. CI's "Settings app window" job runs them in Debian bookworm, at the 1.2 floor,
  and fails if any of them skipped. Every wait in them has a deadline, so a broken
  window fails the test instead of hanging the run.
- **libadwaita 1.2 API only** (`ActionRow`/`ExpanderRow` with suffix widgets,
  `MessageDialog`, `AboutWindow`), so it runs on Debian bookworm as well as current
  releases. Newer widgets such as `SpinRow` and `PreferencesDialog` are not available
  there.
- **Every user-visible string goes through `_()`**, text domain `retrosaver`.
  `gui/po/POTFILES` lists the sources for `xgettext` when translations arrive.
- **The service toggle runs `retrosaver setup` and `teardown`**, so do not flip it
  while testing on a host whose daemon you depend on.
- **A preview is `retrosaver preview <module>`**, which `exec`s the module in an ordinary
  window, so the app's child process is the module and SIGTERM stops it. While one runs,
  the app holds a GTK idle inhibitor, the mechanism `scripts/demo.sh` relies on, so the
  daemon stays quiet. **Release the inhibitor when the preview is stopped, not when the
  process exits.** On close, the exit arrives after the application has shut down, and
  `uninhibit` then is a GLib CRITICAL that crashed the process. The window tests'
  mutation check, and CI's CRITICAL grep, catch it.

### Packaging for other distributions

The `.deb`s are built here; other distributions package from source, as the Fedora COPR
does. Nothing in the settings app is Debian-specific: it runs `retrosaver` from `PATH` and
`systemctl --user`, and installs to the usual FHS paths. It needs no build step.

Ship it as a separate, architecture-independent package, as `nfpm-gtk.yaml` does, so the
daemon keeps no Python or GTK dependency:

| Source | Installed as |
|---|---|
| `gui/retrosaver-settings` | `/usr/bin/retrosaver-settings` (0755) |
| `gui/us.prem.Retrosaver.desktop` | `/usr/share/applications/` |
| `gui/us.prem.Retrosaver.metainfo.xml` | `/usr/share/metainfo/` |
| `gui/icons/hicolor/...` | `/usr/share/icons/hicolor/...`, same relative paths |
| `docs/retrosaver-settings.1` | `/usr/share/man/man1/`, compressed as usual |

Runtime dependencies:

| | Debian, Ubuntu | Fedora |
|---|---|---|
| Python bindings | `python3-gi` | `python3-gobject` |
| GTK 4 | `gir1.2-gtk-4.0` | `gtk4` |
| libadwaita (1.2 or newer) | `gir1.2-adw-1` | `libadwaita` |
| The daemon | `retrosaver (= version)` | `retrosaver = %{version}-%{release}` |

**Pin the daemon to the exact same version.** The app's only interface is the output of
`retrosaver config show --json`, `config set` and `list --all --json`, which ship in the
same release.

Checks that need no display, suitable for `%check` or a build's test step:

```bash
python3 -m unittest discover -s gui
desktop-file-validate gui/us.prem.Retrosaver.desktop
appstreamcli validate --no-net gui/us.prem.Retrosaver.metainfo.xml
appstream-util validate-relax --nonet gui/us.prem.Retrosaver.metainfo.xml  # Fedora's older tool
```

Tested on Fedora 44 (GTK 4.22, libadwaita 1.9, xscreensaver 6.16), where module
discovery, all four checks and a scripted run of the window pass.

### The apt repository

The public apt repository at `https://retrosaver.prem.us` is generated, never
edited by hand. The `apt-repo` job in `.github/workflows/release.yaml` runs after each
release. It downloads the `.deb` files of **every** GitHub release, indexes and signs them
with `scripts/build-apt-repo.sh`, and deploys the result to GitHub Pages. A Pages deploy
replaces the whole site, so the repository is rebuilt from scratch each time. A version
appears in it only while its GitHub release still has its `.deb` assets. The same script
writes the `index.html` a browser sees at that address, filling in the fingerprint and the
version list from what it has just built.

To republish the repository without cutting a release, dispatch `release.yaml` on `main`.
The release job is skipped there and only `apt-repo` runs. Dispatching on an older tag
does not work, because a dispatch runs the workflow file as it was at that tag.

The public key is committed as `packaging/retrosaver-archive-keyring.asc`, and the same
file is used three ways: the `.deb` installs it to
`/usr/share/keyrings/retrosaver-archive-keyring.asc`, `build-apt-repo.sh` publishes it as
the key users curl, and the README tells them to curl it to that same path. dpkg takes over
a file no package owns, so after the first install the package keeps the key up to date.
That is the point: a key held only in the user's own `/etc/apt/keyrings` cannot be
renewed. The secret half is the `APT_SIGNING_KEY` Actions secret.

`build-apt-repo.sh` checks its own output before anything is deployed. It fails if the
committed key's fingerprint is not the signing key's, verifies `InRelease` and
`Release.gpg` with the committed key, the same way a client does, and fails if the indexes
list a different number of packages than the pool holds. To test a change, run it locally
against a throwaway key, with `APT_PUBLIC_KEY` pointing at that key's public half in place
of the committed one. It needs `apt-ftparchive`, from the `apt-utils` package. The scratch
`GNUPGHOME` keeps the test key out of your own keyring:

```bash
export GNUPGHOME="$(mktemp -d)"
gpg --batch --passphrase '' --quick-gen-key test@example.com ed25519 sign never
gpg --armor --export test@example.com > "$GNUPGHOME/public.asc"
mkdir -p site/pool/main && cp dist/*.deb site/pool/main/
APT_SIGNING_KEY="$(gpg --armor --export-secret-keys test@example.com)" \
  APT_PUBLIC_KEY="$GNUPGHOME/public.asc" scripts/build-apt-repo.sh site
rm -rf "$GNUPGHOME"; unset GNUPGHOME
```

**Renewing the key.** It expires on 2029-10-10. Extend the expiry of the same key; never
rotate to a new one, which would break every installed client. Then, in one change, update
the `APT_SIGNING_KEY` secret and `packaging/retrosaver-archive-keyring.asc`, and cut a
release. The release must reach users **before** the old expiry: a client whose key has
already expired cannot verify the repository, so it can never download the upgrade that
carries the renewed key. The fingerprint check compares fingerprints only, which an
extension does not change, so it cannot tell you that one of the two was missed. Check
with `gpg --show-keys` that the committed file shows the new expiry.

## Manual verification

**A green `go test` is not evidence the screensaver works.** The unit tests cover pure logic
and the state machine against fakes; they never touch X, D-Bus or systemd, and neither does
CI. Anything involving a real window, a real idle timer or a real lock is proven only by
running this procedure on an actual GNOME/Wayland session.

Seven steps, in order. They are written against the installed package; the commands assume
`retrosaver setup` has been run.

1. **A module runs at all.**
   `DISPLAY=:0 /usr/libexec/xscreensaver/atlantis -window` → a window of swimming dolphins
   and sharks. Repeat with `flame` and `ifs`.
2. **The fullscreen wrapper.** `retrosaver run atlantis` → covers every monitor, sits above
   the top bar, pointer hidden; it stays in the foreground until Ctrl-C or
   `retrosaver stop` from another shell. Repeat with `anemone`, which sizes itself once at
   startup and so shows at once whether its window was full-size from the start: it must
   fill the screen, not a corner. Each saver window must match a line of
   `xrandr --listmonitors` exactly:
   `xdotool search --name '^retrosaver: ' | while read -r w; do xwininfo -id "$w" | grep -E 'Absolute|Width|Height'; done`.
   `RETROSAVER_LIVE=1 RETROSAVER_LIVE_DISPLAY=1 go test ./internal/window -run Live -v`
   checks the same thing.
3. **Random selection.** Run `retrosaver run` five times: different modules each time, never
   a helper binary and never an excluded one.
4. **The full state machine, on compressed timings.** Set `SAVER_DELAY=15`, `LOCK_AFTER=20`,
   `BLANK_AFTER=10`, then `systemctl --user restart retrosaver`. Leave the machine alone
   with `journalctl --user -u retrosaver -f` visible from another device. Expect the saver
   at 15 s, the lock at 35 s, the display off at 45 s. Touch the trackpad mid-sequence and
   confirm a clean teardown with `idle-delay` back to `0`.
5. **A manual lock does not start the screensaver.** With the same compressed timings,
   press Super+L and leave the machine alone. Expect
   `the session is already locked, not starting the screensaver` in the journal at 15 s,
   `pgrep -f /usr/libexec/xscreensaver/` to find nothing, and the screen to stay dark —
   then the lock and blank stages on their usual schedule, so the display still powers
   off. Unlock, walk away again, and confirm a module does appear at the next 15 s.
6. **Restore the real timings** and restart.
7. **Reboot**, log in, then `systemctl --user status retrosaver` → `active (running)`.

> Step 7 must be checked *after* logging in. `Linger=no`, so the `systemd --user` manager
> only starts at login — a unit that looks dead at the greeter is expected, not a failure.

What cannot be automated, and how the tests that need a real session are kept out of CI:

- **User activity cannot be faked.** Mutter gates `ResetIdletime` behind
  `MUTTER_DEBUG_RESET_IDLETIME`, and injecting XTEST input with `xdotool mousemove` does not
  move the idle clock either, because the idle monitor watches libinput rather than
  synthetic X events. Steps 4 and 5 need a human at the keyboard.
- **Live tests are gated on an environment variable**, not a build tag, so `go test ./...`
  stays green anywhere while one command exercises them for real:
  `RETROSAVER_LIVE=1 go test ./internal/... -run Live -v`. `TestLiveLock` additionally needs
  `RETROSAVER_LIVE_LOCK=1`, because it genuinely locks the screen, and
  `TestLiveLaunchCoversEveryMonitor` needs `RETROSAVER_LIVE_DISPLAY=1`, because it takes
  over every monitor for a moment.

## Code standards

- Go 1.26 is the language floor; `toolchain go1.27.1` is what CI builds
  with. The floor is what lets a host with an older Go and no toolchain download
  still build, so it deliberately lags. **It is set by `golang.org/x/sys`, not by our
  code** — the only post-1.23 feature in the tree is `strings.SplitSeq`, but a module's
  `go` directive must be at least its dependencies', and x/sys keeps raising its own
  (`v0.44.0` → `go 1.25.0`, `v0.48.0` → `go 1.26.0`). **Expect it to move again**, and
  expect `go mod tidy` to do the moving without asking. CI pins the expected floor and
  fails if `go.mod` drifts from it, so a raise cannot arrive unannounced; CI also builds
  and tests at the floor, so the guarantee is exercised rather than asserted. Changing the
  floor means changing four things in one commit: `go.mod`, the `GO_FLOOR` pin in CI (which
  has an upstream counterpart), this paragraph, and the upstream Renovate rule that stops
  it proposing `go` directive bumps. `gofmt` is a hard CI gate; `go vet`
  and `go test -race` likewise.
- Standard library only wherever possible. There are **exactly three** direct dependencies,
  all pure Go, and that is what keeps `CGO_ENABLED=0` viable and the artifact genuinely
  static:
  - `github.com/godbus/dbus/v5` — the session bus, and the system bus for logind.
  - `golang.org/x/sys` — the inotify syscalls, imported directly by `internal/watch`.
    This is not a slip. The stdlib `syscall` package is frozen and its own documentation
    points callers at `x/sys`, so removing it would trade an endorsed dependency for a
    discouraged one.
  - `github.com/jezek/xgb` — the X connection `internal/window` uses to read the monitor
    layout from Xinerama and to create the saver windows. The recorded decision: a module
    has to start in a window that is already fullscreen (see the gotcha below), which
    means owning the window, and no command-line tool can create one and keep it alive.
    A hand-written client would have to redo the connection handshake and the
    Xauthority cookie that xgb already does. It replaced `wmctrl` and `xdotool`.

  A fourth needs a recorded decision. **A cgo dependency is forbidden outright** — the
  invariant that matters is pure-Go, not the count. `go mod tidy -diff` in CI is what
  enforces the list mechanically.
- Wrap errors with `%w` and enough context to name the file or D-Bus call that failed.
- Exported identifiers carry doc comments; every package has a package comment explaining
  what part of the GNOME stack it touches.
- Shell scripts: `set -o errexit -o nounset -o pipefail`, and they must pass `shellcheck`.

## Dependency updates

Renovate keeps the dependencies and the pinned tooling current. Both its configuration and
the job that runs it live on the upstream instance, so what follows is the policy rather
than a pointer to a file — but the rules it enforces apply to any change made here, and a
contributor will meet them as automated pull requests.

**Renovate targets the default branch directly.** Non-major updates merge themselves once
CI is green; **major updates wait for a human**. A merged update that changes what the
package is built from — Go source, `go.mod`/`go.sum`, or a packaged file — is released
automatically the next day. One that only bumps a tool CI uses waits for the next
release. So does a Go minor toolchain bump, because
a new Go can outrun the linter: the linter has to read the new compiler's export data, and
the fix is to update the linter first. The linter is golangci-lint running staticcheck
only, not standalone staticcheck. It builds the same analysers against a current
`golang.org/x/tools`, which is what let the Go 1.27.2 patch release through when standalone
staticcheck 2026.2.1 could not read it. New releases are held for three
days before an update is even proposed (`minimumReleaseAge`), so a compromised or
immediately-yanked upload does not land here on its release day. Security advisories skip
that wait.

Four constraints shape it. Each is there because omitting it failed *silently* somewhere
first, which is why they are worth knowing even if you never touch the config.

- **`postUpdateOptions: ["gomodTidy"]` is mandatory, not a preference.** CI gates on
  `go mod tidy -diff`, and the gomod updater appends new `go.sum` entries without pruning
  the ones they replace. Drop it and *every* dependency pull request opens red against a
  gate unrelated to its change. The proof it is working is that a dependency diff
  **removes** `go.sum` lines as well as adding them.
- **The `digest` / `pin` / `pinDigest` / `lockFileMaintenance` age-gate exemption must
  stay.** Those update types carry no release timestamp to age against, and an absent
  timestamp counts as "not yet passed" — so with `internalChecksFilter: "strict"` they are
  held *indefinitely*, parking on the dependency dashboard with no branch, no pull request
  and no error logged anywhere.
- **Nothing keyed on the module graph can see a Go standard library CVE.** Those
  advisories file under OSV package `stdlib`, which matches no dependency name. The
  `govulncheck` gate in CI is what covers that class, and the reason `go.mod`'s
  `toolchain` directive is Renovate-managed with the age gate waived — a Go patch release
  is a single trusted upstream shipping security fixes, which the gate cannot tell apart
  from an arbitrary new upload.
- **`go.mod`'s `go` directive is deliberately excluded.** It is a language floor, not a
  build version, and raising it breaks exactly the host it exists for. The `toolchain`
  directive beneath it is the one that moves.

Three version pins are invisible to every built-in updater and are tracked by regex
instead — `golangci-lint` and `govulncheck` (both `go run <module>@<tag>` arguments) and
`NFPM_VERSION`. Each appears in both the upstream and mirrored copy of a workflow, and one
rule covers both so they cannot drift apart. **A new hardcoded version pin in a workflow
needs a matching rule**, or it silently rots.

## Common gotchas

- **A module launched while the session is locked is invisible, but not free.** GNOME's
  lock shield is a compositor layer above every XWayland surface, so a module started
  behind it displays nothing while still burning GPU and keeping the panel lit. Super+L is
  how you get there: the keypress is user activity, so it *resets* Mutter's idle clock
  rather than stopping it, and `SAVER_DELAY` later `onSaver` fires against a locked
  session. `onSaver` therefore reads logind's `LockedHint` and skips the launch — still
  advancing `stage` and still calling `ensureActiveWatch()`, but not `armCycle()`. The
  check **fails open**: any error determining the lock state launches the module as
  before, because a detection failure that silently disabled the screensaver would look
  exactly like a healthy daemon doing nothing.
- **Advancing `stage` is not what keeps the lock and blank stages on schedule.** They fire
  from their own watches, and their `stage >= stageLock` / `>= stageBlank` guards are
  satisfied at `stageIdle` just as well. `m.stage = stageSaver` on the suppressed path is
  there for the monotonicity invariant the duplicate/out-of-order `WatchFired` defence
  rests on, and to stop a repeat fire spawning another pair of `loginctl` processes. The
  distinction matters when writing the test: a lock-and-blank test passes with the
  assignment deleted, so `TestTheLockStateIsCheckedOncePerIdlePeriod` is what pins it.
- **Use logind's `LockedHint`, not `org.gnome.ScreenSaver.GetActive`, to ask whether the
  session is locked.** They are not two spellings of the same question and the difference is
  measurable. Polling both once a second across two Super+L cycles on the reference host
  (GNOME Shell 50.1, 2026-09-08):

  ```
  11:20:18  hint=no   active=true     shield up, hint has not flipped yet
  11:20:19  hint=yes  active=true     hint flips 1s later
  11:20:34  hint=no   active=false    unlocked
  11:20:51  hint=yes  active=false    locked for 49s, GetActive says false
  11:21:40  hint=no   active=false    unlocked
  ```

  `GetActive` reports whether the *shield is covering the screen*, so it goes false the
  moment the password prompt is raised — while the session is still locked. That is
  precisely the window this fix exists for, so `GetActive` alone would miss most of a real
  lock. `LockedHint` was correct in both cycles. It also lags the shield by about a second
  on the way in, which is harmless: the saver stage would have to come due inside that one
  second to be affected, and the cost is one idle period of the old behaviour.
- **A lock that arrives with no user activity stops the module.** `loginctl lock-session`
  over SSH, for one, locks without touching the idle clock, and GNOME leaves an X
  client running behind its shield: measured on the reference host, the module was still
  running 60 s into such a lock. `session.WatchLocks` subscribes to `PropertiesChanged` on
  the graphical session's logind object (system bus, session named explicitly, like
  `session.Locked`), and `onLockChange` stops the saver. It bumps `gen` first, so a launch
  too far along to cancel is discarded rather than put up behind the shield. It leaves
  `stage` alone, so lock and blank stay on schedule. **The subscription fails open**: if it
  cannot be made, or the system bus drops, the daemon logs a warning and runs without it.
  An unlock needs no handling, because unlocking is user activity.
- **`session.Lock` may let logind pick the session; `session.Locked` may not.** A bare
  `loginctl lock-session` resolves to the user's display session, and locking an
  already-locked session is a no-op, so guessing is safe there. A bare
  `loginctl show-session` reports the *caller's* session — and the daemon runs under
  `user@<uid>.service`, not in the graphical session's cgroup, where it prints nothing at
  all rather than failing loudly. So an unresolvable or empty session id is an error, an
  unparseable value is an error, and the daemon fails open on both. Parsing `""` as "not
  locked" would turn the whole check into a no-op nothing would ever notice.
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
- **Cycling reuses the idle monitor, so the daemon still owns no timers.** A module swap
  is driven by a `kindCycle` idle watch at `SAVER_DELAY + n*CYCLE_AFTER`, and each fire
  arms the next rather than the whole series being registered up front — which is what
  lets `LOCK_AFTER=0` run all night holding one cycle watch at a time. `onCycle` must
  `RemoveWatch` **and** `delete` the watch that fired before arming its successor, or the
  `watches` map gains an entry every `CYCLE_AFTER` forever.
- **`armCycle` is called from `onSaver`, not from `arm()`.** A swap only means anything
  once a module is on screen, and on a cold start past the lock threshold `onSaver` never
  runs. The side benefit is that the initial arming order is untouched, so the fixed watch
  IDs the daemon tests rely on (`wSaver=1, wLock=2, wBlank=3, wActive=4`) still hold.
- **`used` and `attempts` are two fields because they were one, and cycling broke it.**
  The retry-once cap used to read `len(m.tried) < 2`, where `tried` doubled as the
  no-repeat pool. Once the pool grows past two entries — which is the second swap — that
  test is permanently false and a failed module is never retried. `used` is the avoid
  list for the whole idle period; `attempts` counts launches for the swap in progress and
  is reset by `onCycle`.
- **An exhausted pool and an empty selection are the same error.** `Pick` reports "none
  survived INCLUDE/EXCLUDE" whether the avoid list emptied the set or `INCLUDE` did, so
  `machine.pick` tells them apart by retrying with the pool cleared. With one selectable
  module the pick then matches what is already running, and `startLaunch` skips the swap
  rather than tearing a good window down and putting an identical one back.
- **The outgoing module is stopped in `handleLaunch`, not when the cycle watch fires.**
  Window discovery blocks for up to five seconds; stopping first would blank the desktop
  for that long on every swap, and leave it blank until the next stage whenever the
  replacement failed to start.
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
- **No container or CI job can run or integration-test the daemon.** No Mutter, no session
  bus, no XWayland, no `systemd --user`. A container builds, vets, unit-tests and packages.
  Real verification is the manual procedure above, on the host.
- **A green `go test` is not evidence the screensaver works.** The unit tests cover pure
  logic and the state machine against fakes; they never touch X, D-Bus or systemd. Anything
  involving a real window is proven only by the manual verification procedure on a host.
- **Live tests are gated on `RETROSAVER_LIVE`, not a build tag**, so `go test ./...` stays
  green anywhere while one command exercises them on a real session:
  `RETROSAVER_LIVE=1 go test ./internal/... -run Live -v`. `TestLiveLock` needs
  `RETROSAVER_LIVE_LOCK=1` as well, because it genuinely locks the screen, and
  `TestLiveLaunchCoversEveryMonitor` needs `RETROSAVER_LIVE_DISPLAY=1`, because it takes
  over every monitor.
- **An overdue idle watch fires as soon as it is added.** Verified against gnome-shell 50.1,
  so the daemon needs no cold-start handling when it starts on an already-idle session.
  Do not add any.
- **User activity cannot be faked, so do not try.** Mutter gates `ResetIdletime` behind
  `MUTTER_DEBUG_RESET_IDLETIME`, and injecting XTEST input with `xdotool mousemove` does
  **not** move the idle clock either (verified: 17.0s → 17.3s straight through it), because
  the idle monitor watches libinput, not synthetic X events. Worse, GNOME treats XTEST
  injection through XWayland as remote control and prompts the user to "allow remote
  interaction". Anything needing a reset idle clock needs a human, and lives behind
  `RETROSAVER_LIVE_INPUT=1`. Creating and querying windows over X, as `internal/window`
  does, is fine — it injects nothing.
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
- **`retrosaver stop` restores `idle-delay` only when no daemon is running.** A daemon owns
  the setting for as long as it runs, so a manual `run` then `stop` used to hand blanking
  back to gnome-shell under a daemon that still believed it held `0`. `cmdStop` scans
  `/proc` for a `retrosaver daemon` process first. `ExecStopPost` runs after the daemon has
  exited, so the unit's own cleanup still restores. The check **fails toward restoring**:
  an unreadable `/proc` counts as no daemon, because `idle-delay` stuck at `0` with no
  daemon means no auto-lock at all. `--keep-idle-delay` skips the restore unconditionally.
- **Every monitor runs its own module process; one window is never stretched across
  them.** It is the same module on each unless `MONITORS=different`. EWMH
  `_NET_WM_FULLSCREEN_MONITORS` looks like the tool for spanning, and it is silently
  ignored on any scaled GNOME session. With fractional scaling XWayland's
  coordinates are Mutter's logical layout multiplied by `ceil(highest monitor scale)`, and
  `meta_x11_display_ensure_xinerama_indices` matches Xinerama rectangles to monitors with
  no scale conversion — nothing matches, every monitor keeps index 0, and a request naming
  any other index is cleared (Mutter 50.1, unchanged on `main`; upstream
  [mutter#4901](https://gitlab.gnome.org/GNOME/mutter/-/work_items/4901)). Measured on the reference host: a 125% laptop panel plus a 100%
  external monitor, and the window stayed 3072×1728. It only works when the two coordinate
  spaces coincide, such as identical monitors at 100%.
- **A module must start in a window that is already its final size.** Many modules size
  themselves once, at startup. `anemone` allocates its drawing pixmaps then, and its resize
  handler's re-allocation is compiled out (`#if 0`), so a module that made its own window
  at Xt's default 1280×720 and was fullscreened afterwards drew into a 1280×720 corner and
  left the rest black — in every release up to 0.2.4. Passing `-geometry` is not enough
  either: Mutter first maps a normal window shrunk to fit around the top bar (2938×1590
  on a 3072×1728 panel). So retrosaver creates the windows itself, as XScreenSaver does,
  and starts each module inside one with `-window-id`.
- **Request fullscreen before mapping, then wait for `WM_STATE`.** Each saver window is
  created at its monitor's rectangle with USPosition/USSize hints and `_NET_WM_STATE =
  [FULLSCREEN, ABOVE]` set *before* `MapWindow`; Mutter honours an initial
  `_NET_WM_STATE`, so the window is never any other size. Until the window manager sets
  ICCCM `WM_STATE` the geometry is only what was requested, so `settle` waits for that
  before checking it. Drop the initial state and Mutter maps it at 2938×1590 —
  `TestLiveLaunchCoversEveryMonitor` fails exactly that way. Read the rectangles from
  Xinerama (or `xrandr --listmonitors`), never from Mutter's `DisplayConfig`, whose
  logical numbers are wrong by the scale factor. The top bar hides while the primary
  monitor has any fullscreen window, and a fullscreen window keeps its layer when focus
  moves to another monitor's, so every copy stays up. GL modules render fine in the
  root's default visual, so no visual selection is needed.
- **The saver windows live as long as the process that created them.** The daemon holds
  them for the life of a launch. `retrosaver run` therefore stays in the foreground, and
  SIGHUP is one of the signals that ends it, so closing the terminal clears the screen
  rather than leaving a module drawing into windows that are gone.
- **A launch is all or nothing.** If any copy exits within the half-second startup grace —
  a module that cannot run does — every copy is stopped and the windows closed, so the
  daemon's single retry picks a different module and a failed swap leaves the outgoing
  module on screen. Copies are stopped concurrently, and the windows are destroyed only
  after every module has gone.
- **A module that dies after the grace is replaced, at most three times per idle period.**
  Its windows outlive it, so without this the monitors stay black until the lock stage.
  `Run` selects on `machine.currentDone`, the running saver's `Done()` channel, and a
  `Saver`'s `Done` closes on `Stop` as well as on death, so **every path that stops or
  replaces `current` must clear `currentDone` too**, or the daemon's own stop reads as a
  death. Missing it in `stopSaver` panics on the next select, which every lock test
  catches. **The replacement goes up before the dead windows close**, the same order as a
  swap, and that is measured: closing first and mapping straight after left the new window
  unfullscreened at 2938×1590 on the reference host, while swaps never did. So the dead
  module stays `current`, flagged `dead`, until `handleLaunch` retires it; every path that
  ends with nothing on its way (capped, locked, retry exhausted, nothing to pick) calls
  `closeDead`. `retrosaver stop` beside a running daemon is indistinguishable from a crash
  and gets a replacement too; `maxRelaunches` bounds it.
- **With `MONITORS=different`, a death is made good on its own monitor, in the same
  window.** `Saver.Replace(i, path)` starts the new module with `-window-id` on the window
  the dead one drew in, which is what XScreenSaver does when it cycles. Nothing is
  mapped or fullscreened again, so the 2938×1590 hazard above cannot arise. Measured
  before building on it (reference host, 2026-10-01): `anemone` centred its drawing on
  the middle of a reused 3072×1728 window, whether the window was fresh or had just held a
  GL module, and `flame` and `atlantis` filled it whatever had come before. The cap is
  per monitor (`monitorDeaths`), and a replacement that cannot run simply exits and comes
  back as another death, so the same cap bounds startup failures. Swaps still change
  the whole set, through an ordinary launch.
- **A per-monitor saver is watched through `Exits()`, never `Done()`.** `Done` closes on
  the first process to exit and stays closed, so it cannot report a second death on
  another monitor. `handleLaunch` selects on `currentExits` when the set has more than one
  module and on `currentDone` otherwise, and **`currentExits` must be cleared wherever
  `currentDone` is**. `Saver` reports nothing for a process `Stop` stopped (it closes
  `stopping` before signalling, and each reporter checks it before its `select`, because
  with a reader waiting both cases of that `select` are ready), nor for one `Replace`
  retired.
- **`LaunchEach` is not all or nothing; `LaunchContext` still is.** The daemon uses
  `LaunchEach` for a set of several modules: it fails only if every module exits within
  the grace, and reports the rest on `Exits()` for `Replace` to make good.
  `retrosaver run` keeps all or nothing, since it replaces nothing.
- **The monitor count is read afresh for each launch** (`window.Monitors()`, one short X
  connection, on the event loop), so a swap after a monitor is unplugged covers the
  layout as it now is. That is why the swap-skip check compares the whole set, not its
  first name.
- **The PID file holds one PID per line, primary monitor first**, and the module file
  one name per line in the same order. `Replace` rewrites both while they still name
  its saver, so `retrosaver stop` finds the new process. A single-line file from an
  older version reads the same way. A file with any line that is not a plausible PID is
  ignored whole rather than partly trusted. `clearStateFor` keys on the first PID.
- **Every PID read off disk is checked against `/proc/<pid>/cmdline` before being signalled.**
  The runtime state file can be minutes stale after a crash and PIDs are recycled.
- **A `Saver` clears the runtime state files only while the PID file still names it.** On a
  swap the replacement writes its state before `handleLaunch` stops the outgoing module, and
  a replacement that fails to start is stopped while the old module is still on screen. An
  unconditional `clearState()` in `Stop` erased the running module's files on both paths —
  observed on every swap on the host — leaving `retrosaver stop` unable to find its
  `unclutter`. `TestStopLeavesANewerSaversStateAlone` pins it. `StopRunning` still clears
  unconditionally, because it is the panic button and has just stopped whatever the files
  named.
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
