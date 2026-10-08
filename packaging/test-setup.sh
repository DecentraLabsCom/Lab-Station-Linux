#!/bin/sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
if [ ! -f /.dockerenv ]; then
    echo "setup integration test must run in a disposable Docker container" >&2
    exit 2
fi
if [ "$(id -u)" -ne 0 ]; then
    echo "setup integration test requires the disposable container to run as root" >&2
    exit 2
fi

cd "$ROOT"
FAKE_BIN=$(mktemp -d)
trap 'rm -rf "$FAKE_BIN"' EXIT HUP INT TERM
# The OpenRC adapter test runs on systemd-based images too, so create the
# service-script directory that an OpenRC host provides.
mkdir -p /run/systemd/system /etc/ssh /etc/xrdp /etc/init.d
printf 'Include /etc/ssh/sshd_config.d/*.conf\n' > /etc/ssh/sshd_config
printf '[Globals]\nallow_channels=true\nallow_multimon=true\n' > /etc/xrdp/xrdp.ini
printf '[Globals]\nXAuthorityInSystemDir=no\n' > /etc/xrdp/sesman.ini

cat > "$FAKE_BIN/systemctl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> /tmp/labstation-test-systemctl.log
exit 0
EOF

cat > "$FAKE_BIN/sshd" <<'EOF'
#!/bin/sh
case "$1" in
  -t) [ "${LABSTATION_TEST_SSHD_FAIL:-0}" != 1 ] ;;
  -T)
    cat <<'SETTINGS'
port 22
port 2222
authenticationmethods publickey
passwordauthentication no
kbdinteractiveauthentication no
permituserrc no
permituserenvironment no
forcecommand /usr/lib/decentralabs/lab-station/labstation-dispatcher
allowtcpforwarding no
allowagentforwarding no
x11forwarding no
permittty no
permittunnel no
SETTINGS
    ;;
  *) exit 2 ;;
esac
EOF

cat > "$FAKE_BIN/getent" <<'EOF'
#!/bin/sh
case "$1" in
  group) grep -q "^$2:" /etc/group ;;
  passwd) grep -q "^$2:" /etc/passwd ;;
  *) exit 2 ;;
esac
EOF

cat > "$FAKE_BIN/groupadd" <<'EOF'
#!/bin/sh
for group do :; done
gid=$(awk -F: 'BEGIN { max=3000 } $3 >= max { max=$3+1 } END { print max }' /etc/group)
printf '%s:x:%s:\n' "$group" "$gid" >> /etc/group
EOF

cat > "$FAKE_BIN/useradd" <<'EOF'
#!/bin/sh
home=/var/lib/empty
shell=/usr/sbin/nologin
name=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --home-dir) shift; home=$1 ;;
    --shell) shift; shell=$1 ;;
    --*) ;;
    *) name=$1 ;;
  esac
  shift
done
[ -n "$name" ] || exit 2
uid=$(awk -F: 'BEGIN { max=3000 } $3 >= max { max=$3+1 } END { print max }' /etc/passwd)
getent group "$name" >/dev/null 2>&1 || groupadd --system "$name"
gid=$(awk -F: -v name="$name" '$1 == name { print $3; exit }' /etc/group)
printf '%s:x:%s:%s::%s:%s\n' "$name" "$uid" "$gid" "$home" "$shell" >> /etc/passwd
mkdir -p "$home"
EOF

cat > "$FAKE_BIN/usermod" <<'EOF'
#!/bin/sh
if [ "$1" = "--home-dir" ]; then
  home=$2
  account=$3
  awk -F: -v account="$account" -v home="$home" 'BEGIN { OFS=":" } $1 == account { $6=home } { print }' /etc/passwd > /etc/passwd.labstation-new
  mv /etc/passwd.labstation-new /etc/passwd
  exit 0
fi
if [ "$1" = "--append" ] && [ "$2" = "--groups" ]; then
  group=$3
  account=$4
  awk -F: -v group="$group" -v account="$account" 'BEGIN { OFS=":" } $1 == group { if ($4 == "") $4=account; else if (","$4"," !~ ","account",") $4=$4","account } { print }' /etc/group > /etc/group.labstation-new
  mv /etc/group.labstation-new /etc/group
fi
exit 0
EOF

cat > "$FAKE_BIN/passwd" <<'EOF'
#!/bin/sh
exit 0
EOF

cat > "$FAKE_BIN/python3" <<'EOF'
#!/bin/sh
[ "$1" = "-m" ] && [ "$2" = "venv" ] || exit 2
venv=$4
mkdir -p "$venv/bin"
cat > "$venv/bin/python" <<'PYTHON'
#!/bin/sh
exit 0
PYTHON
chmod 0755 "$venv/bin/python"
EOF

cat > "$FAKE_BIN/ethtool" <<'EOF'
#!/bin/sh
if [ "$1" = "--show-wol" ]; then
  printf 'Supports Wake-on: pumbg\nWake-on: %s\n' "$(cat /tmp/labstation-test-wol-mode)"
  exit 0
fi
if [ "$1" = "--change" ] && [ "$3" = "wol" ] && [ "$4" = "g" ]; then
  printf 'g\n' > /tmp/labstation-test-wol-mode
  exit 0
fi
exit 2
EOF

cat > "$FAKE_BIN/rc-service" <<'EOF'
#!/bin/sh
printf 'rc-service %s\n' "$*" >> /tmp/labstation-test-openrc.log
exit 0
EOF

cat > "$FAKE_BIN/rc-update" <<'EOF'
#!/bin/sh
printf 'rc-update %s\n' "$*" >> /tmp/labstation-test-openrc.log
exit 0
EOF

chmod 0755 "$FAKE_BIN"/*
export PATH="$FAKE_BIN:$PATH"
rm -f /tmp/labstation-test-systemctl.log
rm -f /tmp/labstation-test-openrc.log
printf 'd\n' > /tmp/labstation-test-wol-mode

export LABSTATION_SETUP_E2E=1
if [ -n "${LABSTATION_SETUP_TEST_BINARY:-}" ]; then
    "$LABSTATION_SETUP_TEST_BINARY" -test.run '^TestSetupEndToEnd$'
else
    COVERAGE_REPORT=$(go test -coverprofile=/tmp/labstation-setup.cover ./internal/agent && go tool cover -func=/tmp/labstation-setup.cover)
    printf '%s\n' "$COVERAGE_REPORT" | grep -E 'setup.go:.*|total:'
    AGENT_COVERAGE=$(printf '%s\n' "$COVERAGE_REPORT" | awk '$1 == "total:" { gsub("%", "", $3); print $3 }')
    COVERAGE_MINIMUM=${LABSTATION_COVERAGE_MINIMUM:-65.0}
    if ! awk -v coverage="$AGENT_COVERAGE" -v minimum="$COVERAGE_MINIMUM" 'BEGIN { exit !(coverage >= minimum) }'; then
        echo "Lab Station agent coverage is below the ${COVERAGE_MINIMUM}% installer test gate: ${AGENT_COVERAGE:-unavailable}%" >&2
        exit 1
    fi
fi

CONFIG=/etc/decentralabs/lab-station/e2e.toml
KEYS=/var/lib/labstation-ops/.ssh/authorized_keys
test -f "$CONFIG"
grep -q '^profile = "hybrid"$' "$CONFIG"
grep -q '^management_port = 2222$' "$CONFIG"
test "$(stat -c '%a' "$CONFIG")" = 640
grep -q '^restrict,command="/usr/lib/decentralabs/lab-station/labstation-dispatcher" ssh-ed25519 ' "$KEYS"
grep -q '^PermitRootLogin no$' /etc/ssh/sshd_config
test ! -e /etc/ssh/sshd_config.decentralabs-lab-station.sha256
test -f /usr/lib/decentralabs/lab-station/labstation-dispatcher
grep -q "LABSTATION_CONFIG='/etc/decentralabs/lab-station/e2e.toml'" /usr/lib/decentralabs/lab-station/labstation-dispatcher
grep -q 'sudo -n --preserve-env=LABSTATION_CONFIG --user=labstationd' /usr/lib/decentralabs/lab-station/labstation-dispatcher
test "$(stat -c '%a' /etc/sudoers.d/decentralabs-lab-station)" = 440
grep -q '^labstationd ALL=(root) NOPASSWD:' /etc/sudoers.d/decentralabs-lab-station
grep -q '^labuser ALL=(root) NOPASSWD:' /etc/sudoers.d/decentralabs-lab-station
! id -nG labstation-ops | tr ' ' '\n' | grep -qx labstation
! id -nG labstation-fmu | tr ' ' '\n' | grep -qx labstation
id -nG labstation-fmu | tr ' ' '\n' | grep -qx labstation-fmu
test -d /var/lib/decentralabs/lab-station/data/commands/inbox
test "$(stat -c '%a' /var/lib/decentralabs/lab-station/data)" = 2770
test -f /var/lib/decentralabs/lab-station/tiny-desk-session/.xsession
test "$(stat -c '%u:%g:%a' /var/lib/decentralabs/lab-station/tiny-desk-session)" = '0:0:755'
test "$(stat -c '%u:%g:%a' /var/lib/decentralabs/lab-station/tiny-desk-session/.xsession)" = '0:0:755'
test "$(readlink /var/lib/decentralabs/lab-station/tiny-desk-session/.xsession-errors)" = '/var/lib/decentralabs/lab-station/tiny-desk-home/.xsession-errors'
LABUSER_UID=$(awk -F: '$1 == "labuser" { print $3 }' /etc/passwd)
test "$(stat -c '%u:%a' /var/lib/decentralabs/lab-station/tiny-desk-home)" = "$LABUSER_UID:700"
test "$(awk -F: '$1 == "labuser" { print $6 }' /etc/passwd)" = '/var/lib/decentralabs/lab-station/tiny-desk-session'
test "$(stat -c '%a' /usr/share/decentralabs/lab-station/app-profile.json)" = 644
test "$(stat -c '%a' /usr/share/decentralabs/lab-station/tiny-desk-rc.xml)" = 644
grep -q '^allow_channels=false$' /etc/xrdp/xrdp.ini
grep -q '^allow_multimon=false$' /etc/xrdp/xrdp.ini
test -f /etc/xrdp/xrdp.ini.decentralabs-lab-station.bak
grep -q '^XAuthorityInSystemDir=yes$' /etc/xrdp/sesman.ini
test -f /etc/xrdp/sesman.ini.decentralabs-lab-station.bak
test -f /opt/decentralabs/fmu-executor/app/main.py
test -f /etc/systemd/system/decentralabs-labstation-fmu.service
test "$(stat -c '%a' /var/lib/decentralabs/fmu-executor/fmu-data)" = 750
test "$(cat /tmp/labstation-test-wol-mode)" = g
test -f /etc/systemd/system/decentralabs-labstation-wol.service
grep -q 'daemon-reload' /tmp/labstation-test-systemctl.log
grep -q 'enable --now ssh.service' /tmp/labstation-test-systemctl.log
grep -q 'enable --now decentralabs-labstation.service' /tmp/labstation-test-systemctl.log
grep -q 'enable --now xrdp.service' /tmp/labstation-test-systemctl.log
grep -q '^labuser:' /etc/passwd

echo "Linux setup end-to-end checks passed in the disposable container."
