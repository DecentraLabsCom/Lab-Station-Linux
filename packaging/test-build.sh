#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
TEMP_DIR=$(mktemp -d)
trap 'rm -rf "$TEMP_DIR"' EXIT INT TERM
OUTPUT_DIR="$TEMP_DIR/dist"
export OUTPUT_DIR
VALID_FMU_SOURCE=${FMU_EXECUTOR_SOURCE:-}
if [ -n "$VALID_FMU_SOURCE" ]; then
    FMU_EXECUTOR_SOURCE="$VALID_FMU_SOURCE" sh "$ROOT/packaging/build.sh"
else
    unset FMU_EXECUTOR_SOURCE
    sh "$ROOT/packaging/build.sh"
fi

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
    grep -F '"runtimeDownloads":false' "$manifest" >/dev/null
    if [ -n "$VALID_FMU_SOURCE" ]; then
        pinned_fmu_version=$(tr -d '\r\n' < "$ROOT/internal/agent/fmu_executor_version.txt")
        pinned_fmu_commit=$(tr -d '\r\n' < "$ROOT/internal/agent/fmu_executor_commit.txt")
        pinned_fmu_sha256=$(tr -d '\r\n' < "$ROOT/internal/agent/fmu_executor_sha256.txt")
        pinned_fmu_runtime_sha256=$(tr -d '\r\n' < "$ROOT/internal/agent/fmu_executor_payload_sha256.txt")
        grep -F "\"fmuExecutorVersion\":\"$pinned_fmu_version\"" "$manifest" >/dev/null
        grep -F "\"fmuExecutorCommit\":\"$pinned_fmu_commit\"" "$manifest" >/dev/null
        grep -F "\"fmuExecutorSha256\":\"$pinned_fmu_sha256\"" "$manifest" >/dev/null
        grep -F "\"fmuExecutorRuntimeSha256\":\"$pinned_fmu_runtime_sha256\"" "$manifest" >/dev/null
        tar -tzf "$archive" | grep -F 'payload/usr/share/decentralabs/lab-station/fmu-executor-source/SOURCE.lock.json' >/dev/null
        tar -tzf "$archive" | grep -F 'payload/usr/share/decentralabs/lab-station/fmu-executor-source/requirements.txt' >/dev/null
        tar -tzf "$archive" | grep -F 'payload/usr/share/decentralabs/lab-station/fmu-executor-source/app/main.py' >/dev/null
    else
        grep -F '"fmuExecutorVersion":null' "$manifest" >/dev/null
        grep -F '"fmuExecutorCommit":null' "$manifest" >/dev/null
        grep -F '"fmuExecutorSha256":null' "$manifest" >/dev/null
        grep -F '"fmuExecutorRuntimeSha256":null' "$manifest" >/dev/null
    fi
    if [ "$arch" = amd64 ] && [ -n "$VALID_FMU_SOURCE" ]; then
        grep -F '"smartFactoryDemo":true' "$manifest" >/dev/null
        for demo_file in SmartFactoryCell.fmu SmartFactoryCell.glb SmartFactoryCell.LICENSE.txt; do
            tar -tzf "$archive" | grep -F "payload/var/lib/decentralabs/fmu-executor/fmu-data/$demo_file" >/dev/null
        done
    else
        grep -F '"smartFactoryDemo":false' "$manifest" >/dev/null
        if tar -tzf "$archive" | grep -F 'payload/var/lib/decentralabs/fmu-executor/fmu-data/SmartFactoryCell.' >/dev/null; then
            echo "Unexpected Smart Factory assets in the $arch bundle without a compatible x86_64 FMU." >&2
            exit 1
        fi
    fi
    grep -F "\"arch\":\"$arch\"" "$manifest" >/dev/null
    grep -F '"signed":false' "$manifest" >/dev/null
    tar -tzf "$archive" | grep -F 'payload/usr/bin/labstationctl' >/dev/null
    tar -tzf "$archive" | grep -F 'payload/usr/lib/decentralabs/lab-station/labstation-helper-bin' >/dev/null
    tar -tzf "$archive" | grep -F './install.sh' >/dev/null
    tar -tzf "$archive" | grep -F './uninstall.sh' >/dev/null
    tar -tzf "$archive" | grep -F './rollback.sh' >/dev/null
done

INVALID_FMU_SOURCE="$TEMP_DIR/invalid-fmu-source"
mkdir -p "$INVALID_FMU_SOURCE/app"
printf "print('unverified source')\n" > "$INVALID_FMU_SOURCE/app/main.py"
printf '0.1.0\n' > "$INVALID_FMU_SOURCE/VERSION"
printf 'example-dependency==1.0\n' > "$INVALID_FMU_SOURCE/requirements.txt"
if FMU_EXECUTOR_SOURCE="$INVALID_FMU_SOURCE" OUTPUT_DIR="$OUTPUT_DIR" sh "$ROOT/packaging/build.sh" >/dev/null 2>&1; then
    echo 'Build accepted an FMU Executor source outside the pinned Git commit.' >&2
    exit 1
fi

echo 'Linux release bundles passed archive, manifest, and checksum smoke checks.'
