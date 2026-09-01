#!/usr/bin/env bash
#
# Non-interactive smoke checks. Never takes over the display.
#
# These run against an INSTALLED retrosaver on a real GNOME/Wayland session.
# The pure-logic parts (config parsing, the module-discovery intersection) are
# covered by `go test ./...` and are not repeated here; this file checks the
# things that need a real system.
#
# Usage: tests/smoke.sh [path-to-retrosaver]

set -o errexit
set -o nounset
set -o pipefail

RETROSAVER="${1:-retrosaver}"
CONFIG_DIR=/usr/share/xscreensaver/config
BIN_DIR=/usr/libexec/xscreensaver

pass=0
fail=0

ok()   { printf '  ok    %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf '  FAIL  %s\n' "$1"; fail=$((fail + 1)); }
note() { printf '\n%s\n' "$1"; }

note "Module inventory"

if ! command -v "$RETROSAVER" >/dev/null 2>&1 && [ ! -x "$RETROSAVER" ]; then
    bad "retrosaver not found at '$RETROSAVER'"
    printf '\n%d passed, %d failed\n' "$pass" "$fail"
    exit 1
fi

modules="$("$RETROSAVER" list)"
count="$(printf '%s\n' "$modules" | grep -c . || true)"

if [ "$count" -gt 50 ]; then
    ok "discovery returned $count modules (> 50)"
else
    bad "discovery returned $count modules, expected > 50 -- are the xscreensaver-data/gl packages installed?"
fi

for want in atlantis flame ifs; do
    if printf '%s\n' "$modules" | grep -qx "$want"; then
        ok "found expected module: $want"
    else
        bad "missing expected module: $want"
    fi
done

note "Discovery correctness"

# A helper binary has no XML config file, so it must never be listed.
helpers=0
while IFS= read -r name; do
    [ -n "$name" ] || continue
    if [ ! -f "$CONFIG_DIR/$name.xml" ]; then
        bad "listed '$name' has no $CONFIG_DIR/$name.xml -- helper binary leaked into the module list"
        helpers=$((helpers + 1))
    fi
done <<< "$modules"
[ "$helpers" -eq 0 ] && ok "no helper binaries in the module list"

# Every listed module must actually be executable.
missing=0
while IFS= read -r name; do
    [ -n "$name" ] || continue
    if [ ! -x "$BIN_DIR/$name" ]; then
        bad "listed '$name' is not executable at $BIN_DIR/$name"
        missing=$((missing + 1))
    fi
done <<< "$modules"
[ "$missing" -eq 0 ] && ok "every listed module is executable"

# The default EXCLUDE set must be honoured.
for banned in webcollage vidwhacker glslideshow photopile carousel sonar; do
    if printf '%s\n' "$modules" | grep -qx "$banned"; then
        bad "excluded module '$banned' was listed"
    fi
done
ok "default EXCLUDE set is honoured"

note "Session integration"

# The idle monitor is the only idle signal available under GNOME.
if command -v gdbus >/dev/null 2>&1; then
    idle="$(gdbus call --session \
        --dest org.gnome.Mutter.IdleMonitor \
        --object-path /org/gnome/Mutter/IdleMonitor/Core \
        --method org.gnome.Mutter.IdleMonitor.GetIdletime 2>/dev/null \
        | grep -oE '[0-9]+' || true)"
    if [ -n "$idle" ]; then
        ok "org.gnome.Mutter.IdleMonitor GetIdletime returned ${idle}ms"
    else
        bad "GetIdletime returned nothing -- not a GNOME session, or the idle monitor is unavailable"
    fi
else
    bad "gdbus not found; cannot probe the idle monitor"
fi

# stop must be a clean no-op when nothing is running.
if "$RETROSAVER" stop --keep-idle-delay >/dev/null 2>&1; then
    ok "stop is a clean no-op when nothing is running"
else
    bad "stop returned non-zero with nothing running"
fi

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
