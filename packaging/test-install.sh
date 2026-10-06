#!/bin/sh
# Run this only in a disposable Linux container: install.sh writes system paths.
set -eu

if [ "$(id -u)" -ne 0 ]; then
    echo 'Installer integration smoke must run as root inside a disposable container.' >&2
    exit 2
fi

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
TEMP_DIR=$(mktemp -d)
trap 'rm -rf "$TEMP_DIR"' EXIT INT TERM
PAYLOAD="$TEMP_DIR/payload"
mkdir -p "$PAYLOAD/usr/bin" "$PAYLOAD/usr/lib/decentralabs/lab-station"
cat > "$PAYLOAD/usr/bin/labstationctl" <<'EOF'
#!/bin/sh
printf '%s\n' "$@" > "$LABSTATION_TEST_SETUP_ARGS"
EOF
chmod 0755 "$PAYLOAD/usr/bin/labstationctl"
printf '#!/bin/sh\nexit 0\n' > "$PAYLOAD/usr/bin/labstationd"
printf '#!/bin/sh\nexit 0\n' > "$PAYLOAD/usr/lib/decentralabs/lab-station/labstation-dispatcher-bin"
printf '#!/bin/sh\nexit 0\n' > "$PAYLOAD/usr/lib/decentralabs/lab-station/labstation-helper-bin"
chmod 0755 "$PAYLOAD/usr/bin/labstationd" "$PAYLOAD/usr/lib/decentralabs/lab-station/labstation-dispatcher-bin" "$PAYLOAD/usr/lib/decentralabs/lab-station/labstation-helper-bin"

export LABSTATION_PAYLOAD="$PAYLOAD"
export LABSTATION_TEST_SETUP_ARGS="$TEMP_DIR/setup-args"
sh "$ROOT/packaging/install.sh" \
    --profile hybrid \
    --management-public-key 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA station-test' \
    --wake-interface enp0s1 \
    --no-install-deps

expected="$TEMP_DIR/expected-args"
cat > "$expected" <<'EOF'
setup
--profile
hybrid
--management-public-key
ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA station-test
--wake-interface
enp0s1
--no-install-deps
EOF
diff -u "$expected" "$LABSTATION_TEST_SETUP_ARGS"

set +e
sh "$ROOT/packaging/install.sh" --profile invalid --management-public-key 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA station-test' >/dev/null 2>&1
invalid_profile_status=$?
sh "$ROOT/packaging/install.sh" --profile dedicated --management-public-key 'ssh-rsa invalid' >/dev/null 2>&1
invalid_key_status=$?
set -e
test "$invalid_profile_status" -eq 2
test "$invalid_key_status" -eq 2

echo 'Linux installer option-forwarding and validation smoke checks passed.'
