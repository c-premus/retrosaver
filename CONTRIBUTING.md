# Contributing

Thanks for looking. This is a small, deliberately narrow project: an idle trigger and a
fullscreen wrapper for XScreenSaver display modules on GNOME/Wayland. Contributions that
keep it that size are the easiest to accept.

## This repository is a mirror

The canonical repository is a private Forgejo instance. This GitHub repository is a
scrubbed, published mirror of it.

Two consequences worth knowing before you start:

- **`main` here is force-pushed.** Published history is produced by rewriting the upstream
  history on every sync, so commit SHAs are stable in practice but not guaranteed across a
  change to the publishing rules. If a `git pull` ever refuses to fast-forward, re-clone
  rather than trying to merge.
- **Pull requests opened here are read and applied upstream**, then flow back on the next
  sync. Your commits keep their authorship, but the merge commit you see on GitHub will not
  be the one that lands. This is unusual; it is not a comment on the contribution.

Issues and pull requests are both welcome regardless.

## Before you open a pull request

The full development guide — architecture, code standards, and a long list of hard-won
gotchas that will save you real time — is in
[docs/development.md](docs/development.md). It is worth reading before touching
`internal/`.

Run what CI runs:

```bash
gofmt -l .             # must print nothing
go mod tidy -diff
go vet ./...
go test -race ./...
go run honnef.co/go/tools/cmd/staticcheck@2025.1.1 ./...
shellcheck $(git ls-files '*.sh')
```

`.devcontainer/` provides a working Go toolchain and `shellcheck` in one step.

## What CI cannot tell you

**A green `go test` is not evidence the screensaver works.** The unit tests cover pure logic
and the state machine against fakes. They never touch X, D-Bus or systemd, and neither does
CI — there is no Mutter, no session bus and no XWayland on a runner.

Anything involving a real window, a real idle timer or a real lock is proven only by running
the manual procedure in [docs/spec.md](docs/spec.md) §8 on an actual GNOME/Wayland session.
If your change touches `internal/idle`, `internal/window`, `internal/session` or
`internal/daemon`, please say in the pull request whether you ran it and what happened.

Live tests are gated on an environment variable rather than a build tag, so `go test ./...`
stays green anywhere while one command exercises them for real:

```bash
RETROSAVER_LIVE=1 go test ./internal/... -run Live -v
```

`TestLiveLock` additionally needs `RETROSAVER_LIVE_LOCK=1`, because it genuinely locks your
screen.

## Things that will be declined

These are design boundaries, not preferences. Each is explained at length in
[docs/development.md](docs/development.md) and [README.md](README.md):

- **Making this a screen locker.** X11 grabs do not work under XWayland, so it cannot be
  done securely. Stage 2 hands off to GNOME's own lock screen, and that is the security
  model.
- **Depending on or installing the `xscreensaver` package.** Its daemon is the broken
  component. The `.deb` declares `Conflicts: xscreensaver` deliberately.
- **A hardcoded list of display modules.** Discovery is an intersection of the installed
  config XML basenames and the libexec executables, so it survives a package update.
- **A third dependency**, unless it comes with a recorded reason. A cgo dependency is
  refused outright — it would break the static single-artifact build.
- **Timers in the daemon.** Every deadline comes from the idle monitor. That is what lets
  the tests synchronise on a trace channel instead of sleeping.

## Commits

Conventional commits (`feat:`, `fix:`, `chore:`, `docs:`, `ci:`, `test:`). The release
tooling reads them to work out version bumps, and only `feat`, `fix`, `chore` and BREAKING
reach the changelog.

## Licence

By contributing you agree that your contributions are licensed under the
[MIT Licence](LICENSE).
