#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
TEMP_DIR=$(mktemp -d)
trap 'rm -rf "$TEMP_DIR"' EXIT INT TERM
OUTPUT_DIR="$TEMP_DIR/dist"
export OUTPUT_DIR
sh "$ROOT/packaging/build.sh"

for arch in amd64 arm64; do
    version=$(cat "$ROOT/VERSION")
    archive="$OUTPUT_DIR/lab-station-linux-$version-linux-$arch.tar.gz"
    manifest="$OUTPUT_DIR/lab-station-linux-$version-linux-$arch.manifest.json"
    sums="$archive.SHA256SUMS"
    test -s "$archive"
    test -s "$manifest"
    test -s "$sums"
    (cd "$OUTPUT_DIR" && sha256sum -c "$(basename "$sums")")
    grep -F '"contractVersion":"3.0.0"' "$manifest" >/dev/null
    grep -F "\"arch\":\"$arch\"" "$manifest" >/dev/null
    grep -F '"signed":false' "$manifest" >/dev/null
    tar -tzf "$archive" | grep -F 'payload/usr/bin/labstationctl' >/dev/null
    tar -tzf "$archive" | grep -F 'payload/usr/lib/decentralabs/lab-station/labstation-helper-bin' >/dev/null
    tar -tzf "$archive" | grep -F './install.sh' >/dev/null
    tar -tzf "$archive" | grep -F './uninstall.sh' >/dev/null
    tar -tzf "$archive" | grep -F './rollback.sh' >/dev/null
done

echo 'Linux release bundles passed archive, manifest, and checksum smoke checks.'
