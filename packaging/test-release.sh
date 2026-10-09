#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
RELEASE_SCRIPT="$ROOT/packaging/release.sh"
VERSION=$(tr -d '\r\n' < "$ROOT/VERSION")

assert_release_fails_with() {
    expected=$1
    shift
    if output=$("$@" 2>&1); then
        echo "Expected release preparation to fail with: $expected" >&2
        exit 1
    fi
    if ! printf '%s\n' "$output" | grep -Fq "$expected"; then
        echo "Release preparation failed for the wrong reason; expected: $expected" >&2
        printf '%s\n' "$output" >&2
        exit 1
    fi
}

assert_release_fails_with 'TAG must match VERSION' \
    env TAG=v0.0.0 MINISIGN_SECRET_KEY= MINISIGN_PUBLIC_KEY_FILE= FMU_EXECUTOR_SOURCE= \
    sh "$RELEASE_SCRIPT"

assert_release_fails_with 'MINISIGN_SECRET_KEY is required' \
    env TAG="v$VERSION" MINISIGN_SECRET_KEY= MINISIGN_PUBLIC_KEY_FILE= FMU_EXECUTOR_SOURCE= \
    sh "$RELEASE_SCRIPT"

echo 'Linux release validation rejects mismatched tags and unsigned builds.'
