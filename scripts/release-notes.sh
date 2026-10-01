#!/usr/bin/env bash
# release-notes.sh — print a release's body: its CHANGELOG.md section, then how to install.
#
# The release workflows used to post a hardcoded one-line body, so a release
# page said nothing about what had changed. version-release.yaml has already
# written the version's section into CHANGELOG.md by the time a tag exists, so
# this prints that section's body -- everything under "## [X.Y.Z] - date" up to
# the next "## [" heading or the compare-link block at the foot of the file --
# followed by an Install section. Both forges' release workflows call it, so
# the two release pages say the same thing.
#
# A version with no section gets a warning on stderr and the Install section
# alone. The tag is already pushed when this runs, and a release with a short
# body beats a failed one.
#
# Usage:
#   release-notes.sh v0.3.0                       # reads ./CHANGELOG.md
#   release-notes.sh 0.3.0 path/to/CHANGELOG.md   # the leading v is optional

set -o errexit -o nounset -o pipefail

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
    echo "usage: release-notes.sh VERSION [CHANGELOG]" >&2
    exit 2
fi
version="${1#v}"
changelog="${2:-CHANGELOG.md}"

# Collect the section, then trim leading and trailing blank lines. The version
# is matched as a literal string, so the dots in it are not regex wildcards.
notes="$(awk -v heading="## [${version}]" '
    index($0, "## [") == 1 {
        if (found) exit
        if (index($0, heading) == 1) { found = 1; next }
    }
    # The compare-link definitions at the foot of the file end the last
    # section; they are not part of it.
    found && /^\[[^]]+\]: / { exit }
    found { print }
' "$changelog" | sed -e '/./,$!d' | sed -e ':a' -e '/^\n*$/{$d;N;ba' -e '}')"

if [ -n "$notes" ]; then
    printf '%s\n\n' "$notes"
else
    echo "release-notes.sh: no \"## [${version}]\" section in ${changelog}; install notes only" >&2
fi

cat <<NOTES
### Install

Download the \`.deb\` for your architecture and install it, then run \`retrosaver setup\`:

\`\`\`
sudo apt install ./retrosaver_${version}_amd64.deb   # or _arm64.deb
retrosaver setup
\`\`\`

The README explains what \`setup\` changes and how \`retrosaver teardown\` undoes it.
NOTES
