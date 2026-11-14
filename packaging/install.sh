#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then echo 'Run install.sh as root.' >&2; exit 2; fi
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PAYLOAD=${LABSTATION_PAYLOAD:-$ROOT/payload}
PROFILE=dedicated
PUBLIC_KEY=
WAKE_INTERFACE=
NO_INSTALL_DEPS=0
INSTALL_FMU_EXECUTOR=1
while [ "$#" -gt 0 ]; do
    case "$1" in
        --profile) PROFILE=${2:?profile required}; shift 2 ;;
        --management-public-key) PUBLIC_KEY=${2:?public key required}; shift 2 ;;
        --wake-interface) WAKE_INTERFACE=${2:?network interface required}; shift 2 ;;
        --no-install-deps) NO_INSTALL_DEPS=1; shift ;;
        --no-fmu-executor) INSTALL_FMU_EXECUTOR=0; shift ;;
        *) echo "Unknown install option: $1" >&2; exit 2 ;;
    esac
done
case "$PROFILE" in dedicated|hybrid|fmu-only) ;; *) echo 'Profile must be dedicated, hybrid, or fmu-only.' >&2; exit 2 ;; esac
case "$PUBLIC_KEY" in 'ssh-ed25519 '* ) ;; *) echo 'A Gateway ssh-ed25519 public key is required.' >&2; exit 2 ;; esac
test -x "$PAYLOAD/usr/bin/labstationctl" || { echo 'The portable payload is missing.' >&2; exit 2; }

STATE=/var/lib/decentralabs/lab-station/upgrade
mkdir -p "$STATE/previous"
chmod 0700 "$STATE" "$STATE/previous"
# The daemon executable cannot be replaced while it is running. Stop it before
# saving the old binaries so upgrades do not leave a mixed or partial payload.
if command -v systemctl >/dev/null 2>&1 && systemctl cat decentralabs-labstation.service >/dev/null 2>&1; then
    systemctl stop decentralabs-labstation.service
    systemctl stop decentralabs-labstation-helper.service 2>/dev/null || true
elif command -v rc-service >/dev/null 2>&1 && [ -x /etc/init.d/decentralabs-labstation ]; then
    rc-service decentralabs-labstation stop || true
fi
for name in labstationctl labstationd; do
    if [ -f "/usr/bin/$name" ]; then cp -p "/usr/bin/$name" "$STATE/previous/$name"; fi
done
for name in labstation-dispatcher-bin labstation-helper-bin; do
    if [ -f "/usr/lib/decentralabs/lab-station/$name" ]; then cp -p "/usr/lib/decentralabs/lab-station/$name" "$STATE/previous/$name"; fi
done
# Bundles can be built and extracted by an unprivileged account. Never copy
# their directory metadata over existing system paths or trust their owner.
umask 022
set --
for directory in etc usr var; do
    if [ -d "$PAYLOAD/$directory" ]; then set -- "$@" "$directory"; fi
done
PAYLOAD_ARCHIVE=$(mktemp "$STATE/payload.XXXXXX")
trap 'rm -f "$PAYLOAD_ARCHIVE"' EXIT HUP INT TERM
tar -C "$PAYLOAD" -cf "$PAYLOAD_ARCHIVE" "$@"
tar -C / --no-overwrite-dir --no-same-owner --no-same-permissions -xf "$PAYLOAD_ARCHIVE"
rm -f "$PAYLOAD_ARCHIVE"
trap - EXIT HUP INT TERM
chown root:root /usr/bin/labstationctl /usr/bin/labstationd \
    /usr/lib/decentralabs/lab-station/labstation-dispatcher-bin \
    /usr/lib/decentralabs/lab-station/labstation-helper-bin
chmod 0755 /usr/bin/labstationctl /usr/bin/labstationd
chmod 0755 /usr/lib/decentralabs/lab-station/labstation-dispatcher-bin /usr/lib/decentralabs/lab-station/labstation-helper-bin
FMU_SOURCE=/usr/share/decentralabs/lab-station/fmu-executor-source
set -- --profile "$PROFILE" --management-public-key "$PUBLIC_KEY"
if [ -n "$WAKE_INTERFACE" ]; then set -- "$@" --wake-interface "$WAKE_INTERFACE"; fi
if [ "$INSTALL_FMU_EXECUTOR" = 1 ] && [ -f "$FMU_SOURCE/requirements.txt" ] && [ -f "$FMU_SOURCE/VERSION" ]; then
    set -- "$@" --fmu-executor-source "$FMU_SOURCE"
fi
if [ "$NO_INSTALL_DEPS" = 1 ]; then set -- "$@" --no-install-deps; fi
/usr/bin/labstationctl setup "$@"
