#!/usr/bin/env bash
#
# Preview display modules one at a time in an ordinary window, announcing each
# one before it starts. Unlike `retrosaver run`, nothing goes fullscreen, the
# pointer stays visible, and a running daemon is left alone.
#
# Usage: scripts/demo.sh [-t SECONDS] [-g WIDTHxHEIGHT] [--all] [module ...]
#
#   (no modules)  the configured set: whatever `retrosaver list` selects,
#                 which honours INCLUDE and EXCLUDE
#   --all         every installed module except the default EXCLUDE list
#   module ...    exactly these, in this order
#   -t SECONDS    how long each module runs (default 20)
#   -g WxH        window size (default 1280x720)
#
# While a module runs, Enter, space or n moves on to the next one and q quits.

set -o errexit
set -o nounset
set -o pipefail

RETROSAVER="${RETROSAVER:-retrosaver}"
CONFIG_DIR=/usr/share/xscreensaver/config
BIN_DIR=/usr/libexec/xscreensaver

usage() {
    sed -n '3,16s/^# \{0,1\}//p' "$0"
}

die() {
    printf 'demo: %s\n' "$1" >&2
    exit 1
}

# Hold a GNOME idle inhibitor for the length of the demo. Mutter fires no idle
# watches while one is held, so the daemon cannot put a fullscreen module over
# the demo, or lock the session, while nobody is touching the keyboard. The
# inhibitor lives exactly as long as gnome-session-inhibit, which is to say as
# long as this script.
if [ -z "${RETROSAVER_DEMO_INHIBITED:-}" ]; then
    if command -v gnome-session-inhibit >/dev/null 2>&1; then
        export RETROSAVER_DEMO_INHIBITED=1
        exec gnome-session-inhibit --inhibit idle --reason "retrosaver demo" \
            "$BASH" "$0" "$@"
    fi
    printf 'demo: warning: gnome-session-inhibit not found; the daemon may start a fullscreen module over the demo\n' >&2
fi

seconds=20
geometry=1280x720
all=0
names=()

while [ "$#" -gt 0 ]; do
    case "$1" in
        -t)
            [ "$#" -ge 2 ] || die "-t needs a number of seconds"
            seconds="$2"
            shift 2
            ;;
        -g)
            [ "$#" -ge 2 ] || die "-g needs a size, such as 1280x720"
            geometry="$2"
            shift 2
            ;;
        --all)
            all=1
            shift
            ;;
        -h | --help)
            usage
            exit 0
            ;;
        --)
            shift
            names+=("$@")
            break
            ;;
        -*)
            die "unknown option '$1'; see --help"
            ;;
        *)
            names+=("$1")
            shift
            ;;
    esac
done

[[ "$seconds" =~ ^[1-9][0-9]*$ ]] || die "-t must be a whole number of seconds, not '$seconds'"
[[ "$geometry" =~ ^[1-9][0-9]*x[1-9][0-9]*$ ]] || die "-g must look like 1280x720, not '$geometry'"
if [ "$all" -eq 1 ] && [ "${#names[@]}" -gt 0 ]; then
    die "--all and a list of modules are mutually exclusive"
fi

# Selection is retrosaver's own discovery, so the demo shows exactly what the
# screensaver would pick from. The config file is never sourced here: it is
# parsed by retrosaver, and only by retrosaver.
modules=()
if [ "${#names[@]}" -gt 0 ]; then
    for name in "${names[@]}"; do
        # The same two tests discovery applies: an executable, and a config
        # XML, which is what tells a display module apart from a helper.
        if [ ! -x "$BIN_DIR/$name" ] || [ ! -f "$CONFIG_DIR/$name.xml" ]; then
            die "no such module: '$name' (\`$RETROSAVER list\` shows the configured ones)"
        fi
        modules+=("$name")
    done
else
    command -v "$RETROSAVER" >/dev/null 2>&1 || [ -x "$RETROSAVER" ] ||
        die "retrosaver not found at '$RETROSAVER'; set RETROSAVER or name the modules"
    if [ "$all" -eq 1 ]; then
        # An empty config directory makes retrosaver fall back to its built-in
        # defaults: no INCLUDE, and the default EXCLUDE list.
        empty="$(mktemp -d)"
        listed="$(XDG_CONFIG_HOME="$empty" "$RETROSAVER" list)"
        rmdir "$empty"
    else
        listed="$("$RETROSAVER" list)"
    fi
    mapfile -t modules <<<"$listed"
fi
if [ "${#modules[@]}" -eq 0 ] || [ -z "${modules[0]}" ]; then
    die "no modules selected; check INCLUDE and EXCLUDE in your retrosaver.conf"
fi

# Keys come from the terminal itself rather than stdin, so they still arrive
# when gnome-session-inhibit is the parent. Without a terminal the demo simply
# runs through on the timer.
interactive=0
if { : </dev/tty; } 2>/dev/null; then
    interactive=1
fi

export DISPLAY="${DISPLAY:-:0}"
pid=""
log="$(mktemp)"

# Stop only the module this script started. Never `retrosaver stop`: it hands
# idle-delay back to GNOME, which would break a daemon that still owns it.
stop_module() {
    if [ -n "$pid" ]; then
        kill "$pid" 2>/dev/null || true
        wait "$pid" 2>/dev/null || true
        pid=""
    fi
}

cleanup() {
    stop_module
    rm -f "$log"
    if [ "$interactive" -eq 1 ]; then
        stty echo </dev/tty 2>/dev/null || true
    fi
}
trap cleanup EXIT
trap 'printf "\n"; exit 130' INT
trap 'exit 143' TERM

# label prints the module's display name, falling back to its file name.
label() {
    local text
    text="$(sed -n 's/.*<screensaver [^>]*_label="\([^"]*\)".*/\1/p' \
        "$CONFIG_DIR/$1.xml" 2>/dev/null | head -n 1)"
    printf '%s\n' "${text:-$1}"
}

# describe prints the first paragraph of the module's description, which is
# the summary XScreenSaver's own settings dialog leads with.
describe() {
    awk '
        /<_description>/ { inside = 1; next }
        inside && /<\/_description>/ { exit }
        inside && /^[[:space:]]*$/ { if (text != "") exit; next }
        inside { gsub(/^[[:space:]]+|[[:space:]]+$/, ""); text = text (text == "" ? "" : " ") $0 }
        END { print text }
    ' "$CONFIG_DIR/$1.xml" 2>/dev/null |
        sed -e 's/  */ /g' -e 's/&lt;/</g' -e 's/&gt;/>/g' -e 's/&quot;/"/g' -e 's/&amp;/\&/g'
}

# tick waits one second, or less if a key arrives, and leaves the key in
# $key: "next", "quit", "other", or empty on a timeout.
tick() {
    local ch
    key=""
    if [ "$interactive" -eq 0 ]; then
        sleep 1
        return
    fi
    if IFS= read -r -s -n 1 -t 1 ch </dev/tty; then
        case "$ch" in
            '' | ' ' | n | N) key=next ;;
            q | Q) key=quit ;;
            *) key=other ;;
        esac
    fi
}

total="${#modules[@]}"
shown=0

for name in "${modules[@]}"; do
    shown=$((shown + 1))
    printf '\n[%d/%d] %s  (%s)\n' "$shown" "$total" "$(label "$name")" "$name"
    summary="$(describe "$name")"
    if [ -n "$summary" ]; then
        printf '%s\n' "$summary" | fold -s -w 72 | sed 's/^/      /'
    fi

    for n in 3 2 1; do
        printf '\r      starting in %d... ' "$n"
        tick
        case "$key" in
            quit) printf '\n'; exit 0 ;;
            next) break ;;
        esac
    done

    "$BIN_DIR/$name" -window -geometry "$geometry" >"$log" 2>&1 &
    pid=$!

    left="$seconds"
    while [ "$left" -gt 0 ]; do
        if ! kill -0 "$pid" 2>/dev/null; then
            status=0
            wait "$pid" || status=$?
            pid=""
            printf '\r      %s exited early (status %d)          \n' "$name" "$status"
            if [ -s "$log" ]; then
                tail -n 3 "$log" | sed 's/^/        /'
            fi
            break
        fi
        printf '\r      %3ds left   Enter: next   q: quit ' "$left"
        tick
        case "$key" in
            quit) printf '\n'; exit 0 ;;
            next) break ;;
        esac
        left=$((left - 1))
    done
    stop_module
    printf '\r%-60s\r' ''
done

printf '\nShown all %d.\n' "$total"
