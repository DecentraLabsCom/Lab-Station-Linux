#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
OUTPUT_DIR=${OUTPUT_DIR:-$ROOT/dist}
VERSION=${VERSION:-$(cat "$ROOT/VERSION")}
command -v fpm >/dev/null 2>&1 || { echo 'fpm is required to build .deb/.rpm packages from the portable payloads' >&2; exit 1; }

for GOARCH_VALUE in amd64 arm64; do
    PAYLOAD="$OUTPUT_DIR/payload-linux-$GOARCH_VALUE"
    test -d "$PAYLOAD" || { echo "missing $PAYLOAD; run packaging/build.sh first" >&2; exit 1; }
    if [ "$GOARCH_VALUE" = amd64 ]; then DEB_ARCH=amd64; RPM_ARCH=x86_64; else DEB_ARCH=arm64; RPM_ARCH=aarch64; fi
    fpm -s dir -t deb -n lab-station-linux -v "$VERSION" -a "$DEB_ARCH" \
        --license proprietary --description 'DecentraLabs native Linux laboratory station agent' \
        --depends openssh-server --depends ethtool --depends sudo \
        -C "$PAYLOAD" -p "$OUTPUT_DIR/lab-station-linux_${VERSION}_${DEB_ARCH}.deb" etc usr
    fpm -s dir -t rpm -n lab-station-linux -v "$VERSION" -a "$RPM_ARCH" \
        --license proprietary --description 'DecentraLabs native Linux laboratory station agent' \
        --depends openssh-server --depends ethtool --depends sudo \
        -C "$PAYLOAD" -p "$OUTPUT_DIR/lab-station-linux-${VERSION}.${RPM_ARCH}.rpm" etc usr
done
