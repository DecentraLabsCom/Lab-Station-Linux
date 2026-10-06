#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then echo 'Run install.sh as root.' >&2; exit 2; fi
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PAYLOAD=${LABSTATION_PAYLOAD:-$ROOT/payload}
PROFILE=dedicated
PUBLIC_KEY=
WAKE_INTERFACE=
NO_INSTALL_DEPS=0
while [ "$#" -gt 0 ]; do
    case "$1" in
        --profile) PROFILE=${2:?profile required}; shift 2 ;;
        --management-public-key) PUBLIC_KEY=${2:?public key required}; shift 2 ;;
        --wake-interface) WAKE_INTERFACE=${2:?network interface required}; shift 2 ;;
        --no-install-deps) NO_INSTALL_DEPS=1; shift ;;
        *) echo "Unknown install option: $1" >&2; exit 2 ;;
    esac
done
case "$PROFILE" in dedicated|hybrid|fmu-only) ;; *) echo 'Profile must be dedicated, hybrid, or fmu-only.' >&2; exit 2 ;; esac
case "$PUBLIC_KEY" in 'ssh-ed25519 '* ) ;; *) echo 'A Gateway ssh-ed25519 public key is required.' >&2; exit 2 ;; esac
test -x "$PAYLOAD/usr/bin/labstationctl" || { echo 'The portable payload is missing.' >&2; exit 2; }

STATE=/var/lib/decentralabs/lab-station/upgrade
mkdir -p "$STATE/previous"
chmod 0700 "$STATE" "$STATE/previous"
for name in labstationctl labstationd; do
    if [ -f "/usr/bin/$name" ]; then cp -p "/usr/bin/$name" "$STATE/previous/$name"; fi
done
for name in labstation-dispatcher-bin labstation-helper-bin; do
    if [ -f "/usr/lib/decentralabs/lab-station/$name" ]; then cp -p "/usr/lib/decentralabs/lab-station/$name" "$STATE/previous/$name"; fi
done
cp -a "$PAYLOAD/." /
chmod 0755 /usr/bin/labstationctl /usr/bin/labstationd
chmod 0755 /usr/lib/decentralabs/lab-station/labstation-dispatcher-bin /usr/lib/decentralabs/lab-station/labstation-helper-bin
FMU_SOURCE=/usr/share/decentralabs/lab-station/fmu-executor-source
set -- --profile "$PROFILE" --management-public-key "$PUBLIC_KEY"
if [ -n "$WAKE_INTERFACE" ]; then set -- "$@" --wake-interface "$WAKE_INTERFACE"; fi
if [ -f "$FMU_SOURCE/requirements.txt" ] && [ -f "$FMU_SOURCE/VERSION" ]; then
    set -- "$@" --fmu-executor-source "$FMU_SOURCE"
fi
if [ "$NO_INSTALL_DEPS" = 1 ]; then set -- "$@" --no-install-deps; fi
/usr/bin/labstationctl setup "$@"
