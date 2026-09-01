#!/bin/sh
#
# Debian postinst. Deliberately tiny, and deliberately NOT where per-user
# installation happens.
#
# Enabling the unit and taking ownership of idle-delay stay in
# `retrosaver setup`, because they need the user's own session bus and their
# consent. This script does the narrower thing an upgrade genuinely requires:
# tell each running systemd --user manager that the unit file changed, and
# restart the daemon so it runs the binary that was just installed.
#
# Without this, `apt install` replaces /usr/bin/retrosaver on disk while the
# old process keeps running -- /proc/<pid>/exe reads "(deleted)" -- so a new
# feature appears to be installed but is not actually running, and a changed
# unit file stays invisible to systemd.

set -e

# Only after a successful unpack, and never into a chroot or an image build:
# there is no user manager to talk to there.
if [ "$1" != "configure" ]; then
    exit 0
fi
if [ -n "${DPKG_ROOT:-}" ] || [ ! -d /run/systemd/system ]; then
    exit 0
fi
if ! command -v deb-systemd-invoke >/dev/null 2>&1; then
    exit 0
fi

# deb-systemd-invoke --user walks every user@<id>.service instance and talks to
# it with `systemctl --user --machine <id>@`. That needs systemd 249.10/250 or
# newer; on anything older it prints a notice and skips, which is why none of
# this is allowed to fail the install.
deb-systemd-invoke --user daemon-reload >/dev/null 2>&1 || true

# Restart, so the running daemon picks up the new binary.
#
# Safe for users who never ran `retrosaver setup`: deb-systemd-invoke skips a
# restart when the unit is neither enabled nor active, so this cannot start the
# daemon -- and take ownership of idle-delay -- for someone who never opted in.
deb-systemd-invoke --user restart retrosaver.service >/dev/null 2>&1 || true

exit 0
