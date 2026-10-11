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
#
# The signing key comes from $APT_SIGNING_KEY (an armored secret key) and is
# imported into a throwaway GNUPGHOME, so nothing persists on the runner.
#
# Usage:
#   APT_SIGNING_KEY="$(cat key.asc)" build-apt-repo.sh SITE_DIR

set -o errexit -o nounset -o pipefail

if [ "$#" -ne 1 ]; then
    echo "usage: build-apt-repo.sh SITE_DIR" >&2
    exit 2
fi
site="$1"
suite=stable
component=main
arches="amd64 arm64"

if [ -z "${APT_SIGNING_KEY:-}" ]; then
    echo "build-apt-repo.sh: APT_SIGNING_KEY is empty" >&2
    exit 1
fi

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

gpg --batch --yes --local-user "$fpr" --clearsign \
    --output "dists/$suite/InRelease" "dists/$suite/Release"
gpg --batch --yes --local-user "$fpr" --armor --detach-sign \
    --output "dists/$suite/Release.gpg" "dists/$suite/Release"
gpg --batch --armor --export "$fpr" > retrosaver-archive-keyring.asc
gpg --batch --export "$fpr" > retrosaver-archive-keyring.gpg

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

echo "apt repository built: ${#debs[@]} packages, signed by $fpr"
