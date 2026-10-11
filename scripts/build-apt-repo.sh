#!/usr/bin/env bash
# build-apt-repo.sh — turn a directory of .debs into a signed apt repository.
#
# The release workflow downloads the .debs of EVERY GitHub release into
# <site>/pool/main/ and runs this, then deploys <site> to GitHub Pages. A Pages
# deploy replaces the whole site, so the repository is rebuilt from scratch on
# every release rather than appended to.
#
# Layout produced (one suite, one component, flat pool):
#   <site>/pool/main/retrosaver_X.Y.Z_{amd64,arm64}.deb   (input, left in place)
#   <site>/dists/stable/main/binary-{amd64,arm64}/Packages{,.gz}
#   <site>/dists/stable/{Release,Release.gpg,InRelease}
#   <site>/retrosaver-archive-keyring.{asc,gpg}
#   <site>/index.html   (a landing page for browsers; apt never asks for it)
#
# The signing key comes from $APT_SIGNING_KEY (an armored secret key) and is
# imported into a throwaway GNUPGHOME, so nothing persists on the runner.
#
# The PUBLIC key is not exported from that secret. It is the committed
# packaging/retrosaver-archive-keyring.asc, the same file the .deb installs to
# /usr/share/keyrings, so the key a user curls on first install is byte for
# byte the one the package later takes over. The build fails if its
# fingerprint differs from the signing key's, so a renewal that updates the
# secret but not the committed file (or the reverse) goes red here instead of
# shipping a key that cannot verify the repository. $APT_PUBLIC_KEY overrides
# the path, for a local test with a throwaway key.
#
# Usage:
#   APT_SIGNING_KEY="$(cat key.asc)" build-apt-repo.sh SITE_DIR

set -o errexit -o nounset -o pipefail

if [ "$#" -ne 1 ]; then
    echo "usage: build-apt-repo.sh SITE_DIR" >&2
    exit 2
fi
site="$1"
public_key="${APT_PUBLIC_KEY:-$(dirname "$0")/../packaging/retrosaver-archive-keyring.asc}"
suite=stable
component=main
arches="amd64 arm64"

if [ -z "${APT_SIGNING_KEY:-}" ]; then
    echo "build-apt-repo.sh: APT_SIGNING_KEY is empty" >&2
    exit 1
fi
if [ ! -s "$public_key" ]; then
    echo "build-apt-repo.sh: no public key at $public_key" >&2
    exit 1
fi
# Absolute, because the rest of the script runs from inside SITE_DIR.
public_key="$(realpath "$public_key")"

cd "$site"
shopt -s nullglob
debs=(pool/"$component"/*.deb)
if [ "${#debs[@]}" -eq 0 ]; then
    echo "build-apt-repo.sh: no .deb files under $site/pool/$component" >&2
    exit 1
fi

# Fixed mtimes in the generated indexes are not needed: apt compares content
# hashes, never timestamps, and Release carries its own Date field.
rm -rf dists
for arch in $arches; do
    dir="dists/$suite/$component/binary-$arch"
    mkdir -p "$dir"
    # Paths in Packages are relative to the site root, which is the repo root.
    apt-ftparchive --arch "$arch" packages "pool/$component" > "$dir/Packages"
    gzip -9 -n -k "$dir/Packages"
done

# Generate into a temp file: apt-ftparchive release hashes every file under the
# directory, and a Release written in place would hash a partial copy of itself.
release_tmp="$(mktemp)"
apt-ftparchive \
    -o "APT::FTPArchive::Release::Origin=retrosaver" \
    -o "APT::FTPArchive::Release::Label=retrosaver" \
    -o "APT::FTPArchive::Release::Suite=$suite" \
    -o "APT::FTPArchive::Release::Codename=$suite" \
    -o "APT::FTPArchive::Release::Architectures=$arches" \
    -o "APT::FTPArchive::Release::Components=$component" \
    -o "APT::FTPArchive::Release::Description=retrosaver release packages" \
    release "dists/$suite" > "$release_tmp"
mv "$release_tmp" "dists/$suite/Release"

GNUPGHOME="$(mktemp -d)"
export GNUPGHOME
trap 'rm -rf "$GNUPGHOME"' EXIT
chmod 700 "$GNUPGHOME"
printf '%s\n' "$APT_SIGNING_KEY" | gpg --batch --quiet --import
fpr="$(gpg --batch --list-secret-keys --with-colons | awk -F: '/^fpr/ { print $10; exit }')"
if [ -z "$fpr" ]; then
    echo "build-apt-repo.sh: APT_SIGNING_KEY holds no secret key" >&2
    exit 1
fi

public_fpr="$(gpg --batch --show-keys --with-colons "$public_key" | awk -F: '/^fpr/ { print $10; exit }')"
if [ "$public_fpr" != "$fpr" ]; then
    echo "build-apt-repo.sh: $public_key is ${public_fpr:-not a key}, but APT_SIGNING_KEY is $fpr" >&2
    exit 1
fi

gpg --batch --yes --local-user "$fpr" --clearsign \
    --output "dists/$suite/InRelease" "dists/$suite/Release"
gpg --batch --yes --local-user "$fpr" --armor --detach-sign \
    --output "dists/$suite/Release.gpg" "dists/$suite/Release"
cp "$public_key" retrosaver-archive-keyring.asc
gpg --batch --yes --dearmor --output retrosaver-archive-keyring.gpg "$public_key"

# Assert the finished state rather than trusting the exit codes above. Verify
# with the PUBLISHED key, exactly as a client will.
gpgv --keyring ./retrosaver-archive-keyring.gpg "dists/$suite/InRelease"
gpgv --keyring ./retrosaver-archive-keyring.gpg "dists/$suite/Release.gpg" "dists/$suite/Release"

indexed=0
for arch in $arches; do
    n="$(grep -c '^Package: ' "dists/$suite/$component/binary-$arch/Packages" || true)"
    indexed=$((indexed + n))
done
if [ "$indexed" -ne "${#debs[@]}" ]; then
    echo "build-apt-repo.sh: indexed $indexed packages but the pool holds ${#debs[@]} .debs" >&2
    exit 1
fi

# A landing page for people who open the repository URL in a browser; apt
# never requests it. Everything variable on it is read from what was just
# built, so it cannot drift from the repository it describes. The template is
# a quoted heredoc so the shell leaves the $(...) in the install commands
# alone; @NAME@ placeholders are filled in afterwards.
versions=""
for arch in $arches; do
    list="$(awk '/^Version: / { print $2 }' "dists/$suite/$component/binary-$arch/Packages" \
        | sort -rV | paste -sd ' ' | sed 's/ /, /g')"
    versions="$versions<li><strong>$arch</strong>: $list</li>"
done
built="$(sed -n 's/^Date: //p' "dists/$suite/Release")"

page="$(cat <<'HTML'
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>retrosaver apt repository</title>
<style>
:root {
  --bg: #fdfdfc; --fg: #1d1d1b; --muted: #5c5c57; --code-bg: #f1f0ec;
  --border: #dddcd5; --accent: #2b6cb0;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #161615; --fg: #e8e7e2; --muted: #a3a29b; --code-bg: #232321;
    --border: #3a3936; --accent: #7cb3ec;
  }
}
* { box-sizing: border-box; }
body {
  margin: 0; background: var(--bg); color: var(--fg);
  font: 16px/1.55 system-ui, -apple-system, "Segoe UI", sans-serif;
}
main { max-width: 46rem; margin: 0 auto; padding: 2.5rem 1rem 3rem; }
h1 { font-size: 1.6rem; margin: 0 0 .25rem; }
h2 { font-size: 1.15rem; margin: 2rem 0 .5rem; }
p, ul { margin: .5rem 0; }
.lede { color: var(--muted); margin-bottom: 1.5rem; }
a { color: var(--accent); }
code, pre { font: .9em/1.5 ui-monospace, "SFMono-Regular", Menlo, Consolas, monospace; }
code { background: var(--code-bg); padding: .1em .3em; border-radius: 4px; }
pre {
  background: var(--code-bg); border: 1px solid var(--border); border-radius: 6px;
  padding: .75rem 1rem; overflow-x: auto; white-space: pre;
}
pre code { background: none; padding: 0; }
.fpr { overflow-wrap: anywhere; font-size: 1em; }
footer { margin-top: 2.5rem; color: var(--muted); font-size: .875rem; }
</style>
</head>
<body>
<main>
<h1>retrosaver apt repository</h1>
<p class="lede">Signed Debian packages of
<a href="https://github.com/c-premus/retrosaver">retrosaver</a>, which brings classic
XScreenSaver display modules back to GNOME on Wayland. This address is meant for
<code>apt</code>; this page only explains how to use it.</p>

<h2>Install</h2>
<p>Fetch the signing key and print its fingerprint:</p>
<pre><code>sudo curl -fsSLo /usr/share/keyrings/retrosaver-archive-keyring.asc \
  https://retrosaver.prem.us/retrosaver-archive-keyring.asc
gpg --show-keys /usr/share/keyrings/retrosaver-archive-keyring.asc</code></pre>
<p>The fingerprint must be exactly:</p>
<p><code class="fpr">@FPR@</code></p>
<p>Check it against the
<a href="https://github.com/c-premus/retrosaver#from-the-apt-repository">README on GitHub</a>
too. A fingerprint served from the same site as the key cannot vouch for that key on its
own. If it differs, delete the file and stop. Otherwise add the repository and install:</p>
<pre><code>echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/retrosaver-archive-keyring.asc] https://retrosaver.prem.us stable main" \
  | sudo tee /etc/apt/sources.list.d/retrosaver.list
sudo apt update
sudo apt install retrosaver
retrosaver setup</code></pre>
<p>From then on, <code>apt upgrade</code> picks up each new release. The package installs
the same key file and keeps it up to date, so you never need to fetch it again. The README explains
what <code>retrosaver setup</code> changes and how to remove everything again.</p>

<h2>What is here</h2>
<p>Suite <code>stable</code>, component <code>main</code>. Every version that has a
<a href="https://github.com/c-premus/retrosaver/releases">GitHub release</a>, newest
first:</p>
<ul>@VERSIONS@</ul>
<p>Signing key: <a href="retrosaver-archive-keyring.asc">retrosaver-archive-keyring.asc</a>.</p>

<footer>Generated @BUILT@ by the project's release workflow.</footer>
</main>
</body>
</html>
HTML
)"
page="${page//@FPR@/$fpr}"
page="${page//@VERSIONS@/$versions}"
page="${page//@BUILT@/$built}"
printf '%s\n' "$page" > index.html

if ! grep -q "$fpr" index.html || grep -qE '@[A-Z]+@' index.html; then
    echo "build-apt-repo.sh: index.html is missing the fingerprint or has an unfilled placeholder" >&2
    exit 1
fi

echo "apt repository built: ${#debs[@]} packages, signed by $fpr"
