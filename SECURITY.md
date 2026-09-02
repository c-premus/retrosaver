# Security policy

## What retrosaver is not

**retrosaver is not a screen locker, and it must never be mistaken for one.**

A display module is an ordinary X11 client window. X11 grabs do not work under XWayland, so
nothing in this project can hold the keyboard or prevent input from reaching the session
beneath it. A module on screen is decoration, not a barrier.

Actual locking is delegated entirely to GNOME: at stage 2 the daemon kills the module and
calls `loginctl lock-session`, and GNOME's own lock screen takes over. Any weakness in that
lock screen belongs to GNOME, not here.

So "the screensaver can be dismissed without a password" is expected behaviour during
stage 1, not a vulnerability. Please do not report it as one.

## The one real security-relevant behaviour

While the daemon runs it sets `org.gnome.desktop.session idle-delay` to `0`, taking
ownership of the session's idle policy. That is what stops GNOME blanking the screen out
from under a running module.

**The consequence is that if the daemon is not running, there is no automatic lock at all.**
This is documented in the README, and several mitigations exist and are load-bearing:
`Restart=always` in the systemd unit, `ExecStopPost=retrosaver stop`, and restoration of the
original value on `retrosaver teardown`.

A report showing a way to leave a session with `idle-delay 0` and no running daemon — so the
machine silently stops locking itself — is a genuine finding and very much wanted.

## Supported versions

The latest release only. This is a single-maintainer project; there are no backport
branches.

## Reporting a vulnerability

Please report privately rather than opening a public issue:

- Use [GitHub's private vulnerability reporting](https://github.com/c-premus/retrosaver/security/advisories/new)
  on this repository.

Include what you did, what happened, and the GNOME Shell and distribution versions you saw
it on. A proof of concept helps enormously; the failure modes here tend to be timing- and
session-dependent.

Expect an acknowledgement within a week. Given the scope of the project — a userspace idle
daemon with no network surface, no privileged component and no setuid binary — most reports
will be either configuration issues or genuine logic bugs in the state machine, and both are
worth sending.

## Scope notes

- The daemon runs as your own user, under `systemd --user`. It requires no root, and the
  Debian package's maintainer scripts deliberately do not enable it for you.
- The config file at `~/.config/retrosaver/retrosaver.conf` is **parsed, never sourced**, so
  it cannot execute anything even though it is shell-shaped.
- Every PID read from the runtime state file is checked against `/proc/<pid>/cmdline` before
  being signalled, because state files go stale and PIDs are recycled.
