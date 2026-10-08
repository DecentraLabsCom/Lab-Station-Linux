#!/bin/sh
set -eu
if [ "$(id -u)" -ne 0 ]; then echo 'Run uninstall.sh as root.' >&2; exit 2; fi
PURGE=0
if [ "${1:-}" = --purge-data ]; then PURGE=1; elif [ "$#" -gt 0 ]; then echo 'Only --purge-data is supported.' >&2; exit 2; fi
managed_matches() {
    [ -f "$1" ] && [ -f "$1.sha256" ] && [ "$(sha256sum "$1" | awk '{print $1}')" = "$(cat "$1.sha256")" ]
}
managed_remove() {
    if [ -e "$1" ] || [ -e "$1.sha256" ]; then
        if managed_matches "$1"; then rm -f "$1" "$1.sha256"; else echo "Preserved modified managed file: $1" >&2; fi
    fi
}
WAKE_CONFIG=/etc/decentralabs/lab-station/wake-interfaces
WAKE_ORIGINAL=/etc/decentralabs/lab-station/wake-original
WAKE_CONFIG_OWNED=0
if managed_matches "$WAKE_CONFIG"; then WAKE_CONFIG_OWNED=1; fi
if [ "$WAKE_CONFIG_OWNED" = 1 ]; then
    systemctl disable --now decentralabs-labstation-wol.service 2>/dev/null || true
    rc-service decentralabs-labstation-wol stop 2>/dev/null || true
    rc-update del decentralabs-labstation-wol default 2>/dev/null || true
    if managed_matches "$WAKE_ORIGINAL"; then
        read -r WAKE_IFACE WAKE_OLD_MODE < "$WAKE_ORIGINAL"
        case "$WAKE_IFACE:$WAKE_OLD_MODE" in *[!A-Za-z0-9_.:-]*:*) WAKE_IFACE= ;; esac
        case "$WAKE_OLD_MODE" in *[!a-z]*|'') WAKE_IFACE= ;; esac
        if [ -n "$WAKE_IFACE" ]; then
            WAKE_CURRENT_MODE=$(ethtool --show-wol "$WAKE_IFACE" 2>/dev/null | awk '$1 == "Wake-on:" {print $2; exit}') || WAKE_CURRENT_MODE=
            if [ "$WAKE_CURRENT_MODE" = g ]; then
                ethtool --change "$WAKE_IFACE" wol "$WAKE_OLD_MODE" || echo 'Could not restore the previous Wake-on-LAN mode.' >&2
            else
                echo 'Wake-on-LAN mode changed after setup; preserved the live device setting.' >&2
            fi
        else
            echo 'Wake-on-LAN restore record was invalid; preserved it for manual reconciliation.' >&2
        fi
    else
        echo 'Wake-on-LAN restore record changed; preserved device configuration for manual reconciliation.' >&2
    fi
    managed_remove /etc/systemd/system/decentralabs-labstation-wol.service
    managed_remove /etc/init.d/decentralabs-labstation-wol
    managed_remove /usr/lib/decentralabs/lab-station/labstation-wol
    managed_remove "$WAKE_CONFIG"
    managed_remove "$WAKE_ORIGINAL"
else
    if [ -e "$WAKE_CONFIG" ]; then echo 'Wake-on-LAN config changed after setup; preserved it and its service files.' >&2; fi
fi
systemctl disable --now decentralabs-labstation.service 2>/dev/null || true
systemctl disable --now decentralabs-labstation-fmu.service 2>/dev/null || true
rc-service decentralabs-labstation stop 2>/dev/null || true
rc-service decentralabs-labstation-fmu stop 2>/dev/null || true
rc-update del decentralabs-labstation default 2>/dev/null || true
rc-update del decentralabs-labstation-fmu default 2>/dev/null || true
rm -f /etc/systemd/system/decentralabs-labstation.service /etc/systemd/system/decentralabs-labstation-fmu.service
rm -f /etc/init.d/decentralabs-labstation /etc/init.d/decentralabs-labstation-fmu
systemctl daemon-reload 2>/dev/null || true
rm -f /etc/ssh/sshd_config.d/90-decentralabs-lab-station.conf
SSH_CONFIG=/etc/ssh/sshd_config
SSH_BACKUP=$SSH_CONFIG.decentralabs-lab-station.bak
SSH_MARKER=$SSH_CONFIG.decentralabs-lab-station.sha256
if [ -f "$SSH_BACKUP" ] && [ -f "$SSH_MARKER" ]; then
    if [ "$(sha256sum "$SSH_CONFIG" | awk '{print $1}')" = "$(cat "$SSH_MARKER")" ]; then
        cp -p "$SSH_BACKUP" "$SSH_CONFIG"
        rm -f "$SSH_BACKUP" "$SSH_MARKER"
    else
        echo 'sshd_config changed after setup; preserved the live file and its backup for manual reconciliation.' >&2
    fi
fi
systemctl reload ssh.service 2>/dev/null || systemctl reload sshd.service 2>/dev/null || rc-service sshd reload 2>/dev/null || true
rm -f /etc/sudoers.d/decentralabs-lab-station
rm -f /var/lib/labstation-ops/.ssh/authorized_keys
XRDP_RESTART=0
if [ -f /etc/xrdp/xrdp.ini.decentralabs-lab-station.bak ]; then
    MARKER=/etc/xrdp/xrdp.ini.decentralabs-lab-station.sha256
    if [ -f "$MARKER" ] && [ "$(sha256sum /etc/xrdp/xrdp.ini | awk '{print $1}')" = "$(cat "$MARKER")" ]; then
        cp -p /etc/xrdp/xrdp.ini.decentralabs-lab-station.bak /etc/xrdp/xrdp.ini
        rm -f /etc/xrdp/xrdp.ini.decentralabs-lab-station.bak "$MARKER"
        XRDP_RESTART=1
    else
        echo 'xrdp.ini changed after setup; preserved the live file and its backup for manual reconciliation.' >&2
    fi
fi
if [ -f /etc/xrdp/sesman.ini.decentralabs-lab-station.bak ]; then
    MARKER=/etc/xrdp/sesman.ini.decentralabs-lab-station.sha256
    if [ -f "$MARKER" ] && [ "$(sha256sum /etc/xrdp/sesman.ini | awk '{print $1}')" = "$(cat "$MARKER")" ]; then
        cp -p /etc/xrdp/sesman.ini.decentralabs-lab-station.bak /etc/xrdp/sesman.ini
        rm -f /etc/xrdp/sesman.ini.decentralabs-lab-station.bak "$MARKER"
        XRDP_RESTART=1
    else
        echo 'sesman.ini changed after setup; preserved the live file and its backup for manual reconciliation.' >&2
    fi
fi
if [ "$XRDP_RESTART" = 1 ]; then
    systemctl restart xrdp.service 2>/dev/null || rc-service xrdp restart 2>/dev/null || true
fi
rm -f /usr/bin/labstationctl /usr/bin/labstationd
rm -f /usr/lib/decentralabs/lab-station/labstation-dispatcher /usr/lib/decentralabs/lab-station/labstation-dispatcher-bin /usr/lib/decentralabs/lab-station/labstation-helper-bin
TINY_DESK_CONFIG=/usr/share/decentralabs/lab-station/tiny-desk-rc.xml
TINY_DESK_MARKER=$TINY_DESK_CONFIG.sha256
if [ -f "$TINY_DESK_CONFIG" ] && [ -f "$TINY_DESK_MARKER" ]; then
    if [ "$(sha256sum "$TINY_DESK_CONFIG" | awk '{print $1}')" = "$(cat "$TINY_DESK_MARKER")" ]; then
        rm -f "$TINY_DESK_CONFIG" "$TINY_DESK_MARKER"
    else
        echo 'Tiny Desk Openbox policy changed after setup; preserved it for manual reconciliation.' >&2
    fi
fi
SESSION_HOME=/var/lib/decentralabs/lab-station/tiny-desk-session
managed_remove "$SESSION_HOME/.xsession"
if [ "$(readlink "$SESSION_HOME/.xsession-errors" 2>/dev/null || true)" = /var/lib/decentralabs/lab-station/tiny-desk-home/.xsession-errors ]; then
    rm -f "$SESSION_HOME/.xsession-errors"
fi
rmdir "$SESSION_HOME" 2>/dev/null || true
if id labuser >/dev/null 2>&1; then
    mkdir -p /home/labuser
    usermod --home-dir /home/labuser labuser
fi
managed_remove /usr/share/decentralabs/lab-station/app-profile.json
rm -rf /opt/decentralabs/fmu-executor /usr/share/decentralabs/lab-station/fmu-executor-source
if [ "$PURGE" = 1 ]; then
    rm -rf /var/lib/decentralabs/lab-station /var/log/decentralabs/lab-station /etc/decentralabs/lab-station/secrets
fi
echo 'Lab Station binaries and services removed. Station accounts and preserved configuration/data remain.'
