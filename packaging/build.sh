#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
VERSION=${VERSION:-$(cat "$ROOT/VERSION")}
OUTPUT_DIR=${OUTPUT_DIR:-$ROOT/dist}
PINNED_FMU_EXECUTOR_VERSION=$(tr -d '\r\n' < "$ROOT/internal/agent/fmu_executor_version.txt")
PINNED_FMU_EXECUTOR_COMMIT=$(tr -d '\r\n' < "$ROOT/internal/agent/fmu_executor_commit.txt")
PINNED_FMU_EXECUTOR_SHA256=$(tr -d '\r\n' < "$ROOT/internal/agent/fmu_executor_sha256.txt")
PINNED_FMU_EXECUTOR_RUNTIME_SHA256=$(tr -d '\r\n' < "$ROOT/internal/agent/fmu_executor_payload_sha256.txt")
FMU_EXECUTOR_SOURCE_DIR=
FMU_EXECUTOR_VERSION_JSON=null
FMU_EXECUTOR_COMMIT_JSON=null
FMU_EXECUTOR_SHA256_JSON=null
FMU_EXECUTOR_RUNTIME_SHA256_JSON=null
FMU_TEMP_DIR=
trap 'if [ -n "$FMU_TEMP_DIR" ]; then rm -rf "$FMU_TEMP_DIR"; fi' EXIT HUP INT TERM
if [ -n "${FMU_EXECUTOR_SOURCE:-}" ]; then
    FMU_EXECUTOR_SOURCE_ROOT=$(CDPATH= cd -- "$FMU_EXECUTOR_SOURCE" && pwd)
    SOURCE_COMMIT=$(git -C "$FMU_EXECUTOR_SOURCE_ROOT" rev-parse HEAD 2>/dev/null) || {
        echo 'FMU Executor source must be a Git checkout at the pinned commit' >&2
        exit 1
    }
    if [ "$SOURCE_COMMIT" != "$PINNED_FMU_EXECUTOR_COMMIT" ]; then
        echo "FMU Executor commit $SOURCE_COMMIT does not match the pinned commit $PINNED_FMU_EXECUTOR_COMMIT" >&2
        exit 1
    fi
    FMU_TEMP_DIR=$(mktemp -d)
    SOURCE_SHA256=$(git -C "$FMU_EXECUTOR_SOURCE_ROOT" ls-tree -r --full-tree \
        "$PINNED_FMU_EXECUTOR_COMMIT" -- VERSION pyproject.toml requirements.txt \
        README.md app tests | sha256sum | awk '{print $1}')
    if [ "$SOURCE_SHA256" != "$PINNED_FMU_EXECUTOR_SHA256" ]; then
        echo "FMU Executor source manifest digest $SOURCE_SHA256 does not match the pinned digest $PINNED_FMU_EXECUTOR_SHA256" >&2
        exit 1
    fi
    FMU_EXECUTOR_SOURCE_DIR="$FMU_TEMP_DIR/source"
    mkdir -p "$FMU_EXECUTOR_SOURCE_DIR"
    git -C "$FMU_EXECUTOR_SOURCE_ROOT" -c core.autocrlf=false archive --format=tar \
        --prefix="decentralabs-fmu-executor-$PINNED_FMU_EXECUTOR_VERSION/" \
        "$PINNED_FMU_EXECUTOR_COMMIT" VERSION pyproject.toml requirements.txt \
        README.md app tests | tar -xf - -C "$FMU_EXECUTOR_SOURCE_DIR" --strip-components=1
    test -d "$FMU_EXECUTOR_SOURCE_DIR/app" && test ! -L "$FMU_EXECUTOR_SOURCE_DIR/app"
    test -f "$FMU_EXECUTOR_SOURCE_DIR/app/main.py" && test ! -L "$FMU_EXECUTOR_SOURCE_DIR/app/main.py"
    test -f "$FMU_EXECUTOR_SOURCE_DIR/requirements.txt" && test ! -L "$FMU_EXECUTOR_SOURCE_DIR/requirements.txt"
    test -f "$FMU_EXECUTOR_SOURCE_DIR/VERSION" && test ! -L "$FMU_EXECUTOR_SOURCE_DIR/VERSION"
    FMU_EXECUTOR_VERSION=$(tr -d '\r\n' < "$FMU_EXECUTOR_SOURCE_DIR/VERSION")
    if [ "$FMU_EXECUTOR_VERSION" != "$PINNED_FMU_EXECUTOR_VERSION" ]; then
        echo "FMU Executor source version $FMU_EXECUTOR_VERSION does not match the pinned station version $PINNED_FMU_EXECUTOR_VERSION" >&2
        exit 1
    fi
    if find "$FMU_EXECUTOR_SOURCE_DIR/app" -type l -print -quit | grep -q .; then
        echo 'FMU Executor runtime payload must not contain symlinks' >&2
        exit 1
    fi
    SOURCE_RUNTIME_SHA256=$(
        cd "$FMU_EXECUTOR_SOURCE_DIR"
        { find app -type f -print; printf 'VERSION\nrequirements.txt\n'; } |
            LC_ALL=C sort |
            while IFS= read -r relative_path; do
                file_sha256=$(sha256sum "$relative_path" | awk '{print $1}')
                printf '%s\t%s\n' "$relative_path" "$file_sha256"
            done |
            sha256sum | awk '{print $1}'
    )
    if [ "$SOURCE_RUNTIME_SHA256" != "$PINNED_FMU_EXECUTOR_RUNTIME_SHA256" ]; then
        echo "FMU Executor runtime payload digest $SOURCE_RUNTIME_SHA256 does not match the pinned digest $PINNED_FMU_EXECUTOR_RUNTIME_SHA256" >&2
        exit 1
    fi
    FMU_EXECUTOR_VERSION_JSON="\"$FMU_EXECUTOR_VERSION\""
    FMU_EXECUTOR_COMMIT_JSON="\"$SOURCE_COMMIT\""
    FMU_EXECUTOR_SHA256_JSON="\"$SOURCE_SHA256\""
    FMU_EXECUTOR_RUNTIME_SHA256_JSON="\"$SOURCE_RUNTIME_SHA256\""
    printf '{"repository":"DecentraLabsCom/FMU-Executor","version":"%s","commit":"%s","sourceTree":{"format":"git-ls-tree-manifest-v1","sha256":"%s"},"runtimePayload":{"format":"sha256-path-manifest-v1","sha256":"%s"},"runtimeDownloads":false}\n' \
        "$FMU_EXECUTOR_VERSION" "$SOURCE_COMMIT" "$SOURCE_SHA256" "$SOURCE_RUNTIME_SHA256" \
        > "$FMU_EXECUTOR_SOURCE_DIR/SOURCE.lock.json"
fi
mkdir -p "$OUTPUT_DIR"

for GOARCH_VALUE in amd64 arm64; do
    PAYLOAD="$OUTPUT_DIR/payload-linux-$GOARCH_VALUE"
    rm -rf "$PAYLOAD"
    mkdir -p "$PAYLOAD/usr/bin" \
        "$PAYLOAD/usr/lib/decentralabs/lab-station" \
        "$PAYLOAD/etc/decentralabs/lab-station" \
        "$PAYLOAD/usr/share/doc/lab-station-linux"
    for name in labstationctl labstationd labstation-dispatcher labstation-helper; do
        case "$name" in
            labstationctl) package=./cmd/labstationctl ;;
            labstationd) package=./cmd/labstationd ;;
            labstation-dispatcher) package=./cmd/labstation-dispatcher ;;
            labstation-helper) package=./cmd/labstation-helper ;;
        esac
        case "$name" in
            labstationctl|labstationd) target="$PAYLOAD/usr/bin/$name" ;;
            *) target="$PAYLOAD/usr/lib/decentralabs/lab-station/$name-bin" ;;
        esac
        (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH_VALUE" \
            go build -trimpath -ldflags='-s -w' -o "$target" "$package")
        chmod 0755 "$target"
    done
    install -m 0644 "$ROOT/config/station.toml" "$PAYLOAD/etc/decentralabs/lab-station/station.toml.example"
    install -m 0644 "$ROOT/README.md" "$PAYLOAD/usr/share/doc/lab-station-linux/README.md"
    if [ -n "${FMU_EXECUTOR_SOURCE:-}" ]; then
        mkdir -p "$PAYLOAD/usr/share/decentralabs/lab-station/fmu-executor-source"
        cp -R "$FMU_EXECUTOR_SOURCE_DIR/app" "$PAYLOAD/usr/share/decentralabs/lab-station/fmu-executor-source/"
        install -m 0644 "$FMU_EXECUTOR_SOURCE_DIR/requirements.txt" "$PAYLOAD/usr/share/decentralabs/lab-station/fmu-executor-source/requirements.txt"
        install -m 0644 "$FMU_EXECUTOR_SOURCE_DIR/VERSION" "$PAYLOAD/usr/share/decentralabs/lab-station/fmu-executor-source/VERSION"
        install -m 0644 "$FMU_EXECUTOR_SOURCE_DIR/SOURCE.lock.json" "$PAYLOAD/usr/share/decentralabs/lab-station/fmu-executor-source/SOURCE.lock.json"
    fi
    ARCHIVE="$OUTPUT_DIR/lab-station-linux-$VERSION-linux-$GOARCH_VALUE.tar.gz"
    BUNDLE_ROOT=$(mktemp -d)
    mkdir -p "$BUNDLE_ROOT/payload"
    cp -a "$PAYLOAD/." "$BUNDLE_ROOT/payload/"
    cp "$ROOT/packaging/install.sh" "$BUNDLE_ROOT/install.sh"
    cp "$ROOT/packaging/uninstall.sh" "$BUNDLE_ROOT/uninstall.sh"
    cp "$ROOT/packaging/rollback.sh" "$BUNDLE_ROOT/rollback.sh"
    tar -czf "$ARCHIVE" -C "$BUNDLE_ROOT" .
    rm -rf "$BUNDLE_ROOT"
    (cd "$OUTPUT_DIR" && sha256sum "$(basename "$ARCHIVE")" > "$(basename "$ARCHIVE").SHA256SUMS")
    if [ -n "${MINISIGN_SECRET_KEY:-}" ]; then
        command -v minisign >/dev/null 2>&1 || { echo 'minisign is required to sign this release' >&2; exit 1; }
        minisign -S -s "$MINISIGN_SECRET_KEY" -m "$OUTPUT_DIR/$(basename "$ARCHIVE").SHA256SUMS"
    elif [ "${REQUIRE_SIGNATURE:-0}" = 1 ]; then
        echo 'MINISIGN_SECRET_KEY is required for a signed release' >&2
        exit 1
    fi
    signed=false
    if [ -n "${MINISIGN_SECRET_KEY:-}" ]; then signed=true; fi
    printf '{"version":"%s","os":"linux","arch":"%s","contractVersion":"3.0.0","fmuExecutorVersion":%s,"fmuExecutorCommit":%s,"fmuExecutorSha256":%s,"fmuExecutorRuntimeSha256":%s,"runtimeDownloads":false,"signed":%s}\n' \
        "$VERSION" "$GOARCH_VALUE" "$FMU_EXECUTOR_VERSION_JSON" "$FMU_EXECUTOR_COMMIT_JSON" "$FMU_EXECUTOR_SHA256_JSON" "$FMU_EXECUTOR_RUNTIME_SHA256_JSON" "$signed" \
        > "$OUTPUT_DIR/lab-station-linux-$VERSION-linux-$GOARCH_VALUE.manifest.json"
done
