#!/usr/bin/env bash
# generate-changelog.sh — render one release's changelog section from git history.
#
# Adapted from the same script in chris/mcp-gate, with two deliberate
# differences:
#
#   1. It emits ONE version's section rather than regenerating the whole file.
#      retrosaver's released entries carry hand-written prose that no commit
#      subject holds -- the 0.0.1 "Known limitations" notes, for instance --
#      and a full regeneration would replace that with terse bullets. The
#      workflow splices the new section in and leaves history alone.
#   2. It can emit the goreleaser/chglog YAML that nfpm turns into
#      changelog.Debian.gz, so CHANGELOG.md and changelog.yaml are generated
#      from the same commits and cannot drift apart.
#
# Conventions, matching mcp-gate:
#   - Only feat, fix, chore and BREAKING reach the changelog. ci/test/refactor/
#     docs/style/perf are end-user noise; promote anything that matters to a
#     fix: or chore:. This is why `docs: prepare the v0.0.2 release` and the
#     memory-bank bookkeeping commits stay out.
#   - BREAKING is detected from `!:` after the type or `BREAKING CHANGE` in the
#     body.
#
# Usage:
#   generate-changelog.sh --version v0.0.3                 # markdown section
#   generate-changelog.sh --version v0.0.3 --format chglog # changelog.yaml block
#   generate-changelog.sh --version v0.0.3 --since v0.0.2  # explicit range start

set -euo pipefail

VERSION=""
SINCE=""
FORMAT="markdown"
PACKAGER="${PACKAGER:-Chris <chris@example.com>}"

while [ "$#" -gt 0 ]; do
    case "$1" in
        --version) VERSION="$2"; shift 2 ;;
        --since) SINCE="$2"; shift 2 ;;
        --format) FORMAT="$2"; shift 2 ;;
        -h | --help)
            sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
            exit 0
            ;;
        *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

if [ -z "$VERSION" ]; then
    echo "generate-changelog.sh: --version is required" >&2
    exit 2
fi

# Default the range start to the newest existing tag.
if [ -z "$SINCE" ]; then
    SINCE=$(git tag --list 'v*' --sort=-v:refname | head -n1)
fi
if [ -n "$SINCE" ]; then
    RANGE="${SINCE}..HEAD"
else
    RANGE="HEAD" # first ever release: everything
fi

# list_commits prints "<hash> <subject>" for the range.
list_commits() {
    git log "$RANGE" --no-merges --pretty=format:'%h %s' || true
}

# list_breaking_body prints "<hash> <subject>" for commits whose BODY declares a
# breaking change, which the subject-line pattern alone would miss.
list_breaking_body() {
    git log "$RANGE" --no-merges --pretty=format:'%H%x00%s%x00%b%x1e' \
        | awk -v RS=$'\x1e' -F'\x00' '$3 ~ /BREAKING CHANGE/ { print $1 " " $2 }' \
        || true
}

# strip_type turns "fix(packaging): restart the daemon" into
# "restart the daemon", and capitalises. The type already appears as the
# section heading, so repeating it in every bullet is noise.
strip_type() {
    printf '%s' "$1" \
        | sed -E 's/^[a-z]+(\([^)]*\))?!?: *//' \
        | sed -E 's/^(.)/\U\1/'
}

# subjects_matching prints the cleaned subjects for one conventional type.
subjects_matching() {
    local pattern="$1" line
    { list_commits | grep -E "$pattern" || true; } | while IFS= read -r line; do
        [ -z "$line" ] && continue
        # Drop the leading hash; only the subject reaches the changelog.
        printf '%s\n' "$(strip_type "${line#* }")"
    done
}

breaking_subjects() {
    local subj body line
    subj=$(list_commits | grep -E '^[0-9a-f]+ [a-z]+(\([^)]*\))?!:' || true)
    body=$(list_breaking_body)
    # awk de-duplicates on the hash: a commit can be breaking by both its
    # subject marker and its body, and must appear once.
    { printf '%s\n%s\n' "$subj" "$body" | grep -vE '^$' || true; } \
        | awk '!seen[$1]++' \
        | while IFS= read -r line; do
            [ -z "$line" ] && continue
            printf '%s\n' "$(strip_type "${line#* }")"
        done
}

BREAKING=$(breaking_subjects || true)
FEATURES=$(subjects_matching '^[0-9a-f]+ feat(\([^)]*\))?!?:' || true)
FIXES=$(subjects_matching '^[0-9a-f]+ fix(\([^)]*\))?!?:' || true)
CHORES=$(subjects_matching '^[0-9a-f]+ chore(\([^)]*\))?!?:' || true)

if [ -z "$BREAKING$FEATURES$FIXES$CHORES" ]; then
    echo "generate-changelog.sh: no feat/fix/chore/BREAKING commits in ${RANGE}" >&2
    exit 3
fi

case "$FORMAT" in
    markdown)
        printf '## [%s] - %s\n' "${VERSION#v}" "$(date -u +%Y-%m-%d)"
        emit_md() {
            [ -z "$2" ] && return 0
            printf '\n### %s\n\n' "$1"
            printf '%s\n' "$2" | sed 's/^/- /'
        }
        emit_md "BREAKING CHANGES" "$BREAKING"
        emit_md "Added" "$FEATURES"
        emit_md "Fixed" "$FIXES"
        emit_md "Maintenance" "$CHORES"
        ;;
    chglog)
        # goreleaser/chglog schema, which nfpm renders into changelog.Debian.gz.
        # Every note stays on ONE line: chglog turns each continuation line into
        # a "-" sub-bullet, which makes a wrapped sentence look like a broken
        # list.
        printf -- '- semver: %s\n' "${VERSION#v}"
        printf '  date: %sT00:00:00Z\n' "$(date -u +%Y-%m-%d)"
        printf '  packager: %s\n' "$PACKAGER"
        printf '  changes:\n'
        { printf '%s\n%s\n%s\n%s\n' "$BREAKING" "$FEATURES" "$FIXES" "$CHORES" \
            | grep -vE '^$' || true; } \
            | while IFS= read -r note; do
                # Escape backslashes then double quotes, so a subject containing
                # either cannot break the YAML string.
                escaped=$(printf '%s' "$note" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')
                printf '    - note: "%s"\n' "$escaped"
            done
        ;;
    *)
        echo "generate-changelog.sh: unknown --format $FORMAT" >&2
        exit 2
        ;;
esac
