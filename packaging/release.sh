#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
VERSION=$(tr -d '\r\n' < "$ROOT/VERSION")
TAG=${TAG:-}
OUTPUT_DIR=${OUTPUT_DIR:-$ROOT/dist/release-$VERSION}
RELEASE_NOTES_PATH=${RELEASE_NOTES_PATH:-$ROOT/dist/release-notes.md}
MINISIGN_SECRET_KEY=${MINISIGN_SECRET_KEY:-}
MINISIGN_PUBLIC_KEY_FILE=${MINISIGN_PUBLIC_KEY_FILE:-}
FMU_EXECUTOR_SOURCE=${FMU_EXECUTOR_SOURCE:-}

fail() {
    echo "$*" >&2
    exit 2
}

printf '%s\n' "$VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$' ||
    fail "VERSION must be a three-part numeric version: $VERSION"
[ "$TAG" = "v$VERSION" ] ||
    fail "TAG must match VERSION (expected v$VERSION)"
[ -n "$MINISIGN_SECRET_KEY" ] ||
    fail 'MINISIGN_SECRET_KEY is required; unsigned releases are disabled'
[ -f "$MINISIGN_SECRET_KEY" ] ||
    fail 'MINISIGN_SECRET_KEY must point to a readable Minisign private-key file'
[ -n "$MINISIGN_PUBLIC_KEY_FILE" ] ||
    fail 'MINISIGN_PUBLIC_KEY_FILE is required'
[ -f "$MINISIGN_PUBLIC_KEY_FILE" ] ||
    fail 'MINISIGN_PUBLIC_KEY_FILE must point to a readable Minisign public-key file'
[ -n "$FMU_EXECUTOR_SOURCE" ] ||
    fail 'FMU_EXECUTOR_SOURCE is required for a complete Linux release'
[ -d "$FMU_EXECUTOR_SOURCE" ] ||
    fail 'FMU_EXECUTOR_SOURCE must point to the pinned FMU Executor Git checkout'
[ -f "$ROOT/CHANGELOG.md" ] || fail 'CHANGELOG.md is required for release notes'

for command in go fpm dpkg-deb rpm minisign sha256sum tar; do
    command -v "$command" >/dev/null 2>&1 || fail "Required release tool is missing: $command"
done

KEY_CHECK_DIR=$(mktemp -d)
trap 'rm -rf "$KEY_CHECK_DIR"' EXIT HUP INT TERM
printf 'Lab Station Linux signing-key check\n' > "$KEY_CHECK_DIR/check.txt"
minisign -S -s "$MINISIGN_SECRET_KEY" -m "$KEY_CHECK_DIR/check.txt" >/dev/null
minisign -Vm "$KEY_CHECK_DIR/check.txt" \
    -p "$MINISIGN_PUBLIC_KEY_FILE" \
    -x "$KEY_CHECK_DIR/check.txt.minisig" >/dev/null ||
    fail 'Minisign public and private keys do not match'
rm -rf "$KEY_CHECK_DIR"
trap - EXIT HUP INT TERM

mkdir -p "$OUTPUT_DIR"
OUTPUT_DIR=$(CDPATH= cd -- "$OUTPUT_DIR" && pwd)
ROOT=$(CDPATH= cd -- "$ROOT" && pwd)
[ "$OUTPUT_DIR" != "$ROOT" ] || fail 'OUTPUT_DIR must not be the repository root'
if find "$OUTPUT_DIR" -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
    fail "OUTPUT_DIR must be empty to prevent stale assets: $OUTPUT_DIR"
fi

RELEASE_NOTES_DIR=$(dirname -- "$RELEASE_NOTES_PATH")
mkdir -p "$RELEASE_NOTES_DIR"
awk -v version="$VERSION" '
    $0 == "## [" version "]" || index($0, "## [" version "] - ") == 1 {
        found = 1
        next
    }
    found && /^## \[/ { exit }
    found { print; if ($0 !~ /^[[:space:]]*$/) wrote = 1 }
    END { if (!found || !wrote) exit 1 }
' "$ROOT/CHANGELOG.md" > "$RELEASE_NOTES_PATH" ||
    fail "CHANGELOG.md has no notes for version $VERSION"

export VERSION OUTPUT_DIR FMU_EXECUTOR_SOURCE MINISIGN_SECRET_KEY
export REQUIRE_SIGNATURE=1
sh "$ROOT/packaging/build.sh"
sh "$ROOT/packaging/build-packages.sh"

assert_count() {
    pattern=$1
    expected=$2
    actual=$(find "$OUTPUT_DIR" -maxdepth 1 -type f -name "$pattern" | wc -l | tr -d '[:space:]')
    [ "$actual" = "$expected" ] ||
        fail "Expected $expected release files matching $pattern; found $actual"
}

assert_count "lab-station-linux-$VERSION-linux-*.tar.gz" 2
assert_count "lab-station-linux-$VERSION-linux-*.tar.gz.SHA256SUMS" 2
assert_count "lab-station-linux-$VERSION-linux-*.tar.gz.SHA256SUMS.minisig" 2
assert_count "lab-station-linux-$VERSION-linux-*.manifest.json" 2
assert_count "lab-station-linux_${VERSION}_*.deb" 2
assert_count "lab-station-linux-$VERSION.*.rpm" 2

for arch in amd64 arm64; do
    archive="$OUTPUT_DIR/lab-station-linux-$VERSION-linux-$arch.tar.gz"
    manifest="$OUTPUT_DIR/lab-station-linux-$VERSION-linux-$arch.manifest.json"
    sums="$archive.SHA256SUMS"
    test -s "$archive" && test -s "$manifest" && test -s "$sums" && test -s "$sums.minisig" ||
        fail "Incomplete signed archive set for linux/$arch"
    grep -F '"version":"'"$VERSION"'"' "$manifest" >/dev/null ||
        fail "Manifest version does not match $VERSION: $manifest"
    grep -F '"arch":"'"$arch"'"' "$manifest" >/dev/null ||
        fail "Manifest architecture does not match linux/$arch: $manifest"
    grep -F '"signed":true' "$manifest" >/dev/null ||
        fail "Manifest does not mark linux/$arch as signed: $manifest"
    grep -F '"runtimeDownloads":false' "$manifest" >/dev/null ||
        fail "Manifest permits release-time runtime downloads: $manifest"
    minisign -Vm "$sums" -p "$MINISIGN_PUBLIC_KEY_FILE" -x "$sums.minisig"
    (cd "$OUTPUT_DIR" && sha256sum -c "$(basename -- "$sums")")
done

for arch in amd64 arm64; do
    if [ "$arch" = amd64 ]; then
        deb_arch=amd64
        rpm_arch=x86_64
    else
        deb_arch=arm64
        rpm_arch=aarch64
    fi
    deb="$OUTPUT_DIR/lab-station-linux_${VERSION}_${deb_arch}.deb"
    rpm_package="$OUTPUT_DIR/lab-station-linux-${VERSION}.${rpm_arch}.rpm"
    test "$(dpkg-deb -f "$deb" Package)" = lab-station-linux || fail "Invalid Debian package name: $deb"
    test "$(dpkg-deb -f "$deb" Version)" = "$VERSION" || fail "Invalid Debian package version: $deb"
    test "$(dpkg-deb -f "$deb" Architecture)" = "$deb_arch" || fail "Invalid Debian package architecture: $deb"
    test "$(rpm -qp --queryformat '%{NAME}' "$rpm_package")" = lab-station-linux || fail "Invalid RPM package name: $rpm_package"
    test "$(rpm -qp --queryformat '%{VERSION}' "$rpm_package")" = "$VERSION" || fail "Invalid RPM package version: $rpm_package"
    test "$(rpm -qp --queryformat '%{ARCH}' "$rpm_package")" = "$rpm_arch" || fail "Invalid RPM package architecture: $rpm_package"
    dpkg-deb -c "$deb" | grep -F '/usr/bin/labstationctl' >/dev/null || fail "Debian package is missing labstationctl: $deb"
    rpm -qlp "$rpm_package" | grep -F '/usr/bin/labstationctl' >/dev/null || fail "RPM package is missing labstationctl: $rpm_package"
done

cp -- "$MINISIGN_PUBLIC_KEY_FILE" "$OUTPUT_DIR/minisign.pub"
(
    cd "$OUTPUT_DIR"
    find . -maxdepth 1 -type f \
        ! -name 'SHA256SUMS' \
        ! -name 'SHA256SUMS.minisig' \
        ! -name '.release-assets' \
        -printf '%f\n' |
        LC_ALL=C sort > .release-assets
    asset_count=$(wc -l < .release-assets | tr -d '[:space:]')
    [ "$asset_count" = 13 ] || {
        echo "Expected 13 signed release assets before aggregate checksums; found $asset_count" >&2
        exit 2
    }
    while IFS= read -r asset; do
        sha256sum "$asset"
    done < .release-assets > SHA256SUMS
    rm -f .release-assets
)
minisign -S -s "$MINISIGN_SECRET_KEY" -m "$OUTPUT_DIR/SHA256SUMS"
minisign -Vm "$OUTPUT_DIR/SHA256SUMS" \
    -p "$MINISIGN_PUBLIC_KEY_FILE" \
    -x "$OUTPUT_DIR/SHA256SUMS.minisig"
(cd "$OUTPUT_DIR" && sha256sum -c SHA256SUMS)

echo "Prepared signed Lab Station Linux v$VERSION assets in $OUTPUT_DIR"
