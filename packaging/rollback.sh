#!/bin/sh
set -eu
if [ "$(id -u)" -ne 0 ]; then echo 'Run rollback.sh as root.' >&2; exit 2; fi
STATE=/var/lib/decentralabs/lab-station/upgrade/previous
test -d "$STATE" || { echo 'No previous agent version was saved.' >&2; exit 2; }
for name in labstationctl labstationd; do
    test -f "$STATE/$name" || { echo "Missing previous binary: $name" >&2; exit 2; }
done
for name in labstation-dispatcher-bin labstation-helper-bin; do
    test -f "$STATE/$name" || { echo "Missing previous binary: $name" >&2; exit 2; }
done
systemctl stop decentralabs-labstation.service 2>/dev/null || rc-service decentralabs-labstation stop 2>/dev/null || true
cp -p "$STATE/labstationctl" /usr/bin/labstationctl
cp -p "$STATE/labstationd" /usr/bin/labstationd
cp -p "$STATE/labstation-dispatcher-bin" /usr/lib/decentralabs/lab-station/labstation-dispatcher-bin
cp -p "$STATE/labstation-helper-bin" /usr/lib/decentralabs/lab-station/labstation-helper-bin
systemctl start decentralabs-labstation.service 2>/dev/null || rc-service decentralabs-labstation start
