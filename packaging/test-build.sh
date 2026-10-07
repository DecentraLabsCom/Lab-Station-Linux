#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
TEMP_DIR=$(mktemp -d)
trap 'rm -rf "$TEMP_DIR"' EXIT INT TERM
OUTPUT_DIR="$TEMP_DIR/dist"
export OUTPUT_DIR
FMU_SOURCE="$TEMP_DIR/fmu-executor"
mkdir -p "$FMU_SOURCE/app"
printf "print('fmu test fixture')\n" > "$FMU_SOURCE/app/main.py"
printf 'example-dependency==1.0\n' > "$FMU_SOURCE/requirements.txt"
cp "$ROOT/internal/agent/fmu_executor_version.txt" "$FMU_SOURCE/VERSION"
FMU_EXECUTOR_SOURCE="$FMU_SOURCE" sh "$ROOT/packaging/build.sh"

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
    pinned_fmu_version=$(tr -d '\r\n' < "$ROOT/internal/agent/fmu_executor_version.txt")
    grep -F "\"fmuExecutorVersion\":\"$pinned_fmu_version\"" "$manifest" >/dev/null
    grep -F "\"arch\":\"$arch\"" "$manifest" >/dev/null
    grep -F '"signed":false' "$manifest" >/dev/null
    tar -tzf "$archive" | grep -F 'payload/usr/bin/labstationctl' >/dev/null
    tar -tzf "$archive" | grep -F 'payload/usr/lib/decentralabs/lab-station/labstation-helper-bin' >/dev/null
    tar -tzf "$archive" | grep -F './install.sh' >/dev/null
    tar -tzf "$archive" | grep -F './uninstall.sh' >/dev/null
    tar -tzf "$archive" | grep -F './rollback.sh' >/dev/null
    tar -tzf "$archive" | grep -F 'payload/usr/share/decentralabs/lab-station/fmu-executor-source/app/main.py' >/dev/null
done

printf '0.1.1\n' > "$FMU_SOURCE/VERSION"
if FMU_EXECUTOR_SOURCE="$FMU_SOURCE" OUTPUT_DIR="$OUTPUT_DIR" sh "$ROOT/packaging/build.sh" >/dev/null 2>&1; then
    echo 'Build accepted an FMU Executor version different from its station pin.' >&2
    exit 1
fi

echo 'Linux release bundles passed archive, manifest, and checksum smoke checks.'
