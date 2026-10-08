# Lab Station Linux

Native Linux station agent for Lab Gateway. The service is a Go daemon with a
restricted SSH dispatcher, a small privileged helper, a host-local command
queue, and an optional Tiny Desk session using xrdp/Xorg. It is a separate
implementation from the Windows AutoHotkey station and speaks Station Contract
v3 (`3.0.0`).

## Current support boundary

The source contains systemd and OpenRC service adapters and package-manager
adapters for apt, dnf/yum, zypper, and pacman. These adapters are implemented,
but none of the distro/desktop/hardware combinations is certified by this
source checkout. The agent reports `supportTier: unverified` until a release
team records and publishes a tested support matrix. Release builds target
Linux `amd64` and `arm64`.

The graphical profile uses xrdp with a separate Xorg session and a locked-down
Openbox configuration with no desktop menus or keyboard/mouse bindings. Its
session starts the configured lab application directly. It does not install
GNOME/KDE or replace a local display manager. Setup disables xrdp channel
redirection because xrdp listens host-wide. The `labuser` account
needs an operator-managed RDP password before Tiny Desk can become ready. The
service never sets that password. For managed hosts, install OS dependencies
through the normal configuration tool and pass `--no-install-deps`.

`fmu-only` skips Xorg, xrdp, and Openbox. The shared FMU Executor can be
installed from its pinned source checkout with `--fmu-executor-source`;
dependency installation uses the configured Python package index. Keep TCP
8091 reachable only from the private Lab Gateway network, and enroll the
internal token through Gateway's station secret operation.

## Build

Use Go 1.23 or newer:

```sh
go build ./cmd/labstationctl
go build ./cmd/labstationd
go build ./cmd/labstation-dispatcher
go build ./cmd/labstation-helper
```

Portable payloads and package roots are produced by `packaging/build.sh`.
`packaging/build-packages.sh` derives `.deb` and `.rpm` files from those same
roots and requires `fpm`. Release signing uses Minisign when
`MINISIGN_SECRET_KEY` is supplied; a release pipeline must require signing and
publish the corresponding public key separately.

## Install

Extract the portable release archive and run `install.sh` from the extracted
bundle root, which contains `install.sh` and `payload/`. The installer places
the binaries and defaults; the operator supplies the station's SSH key and
profile:

```sh
sudo ./install.sh --profile dedicated --management-public-key 'ssh-ed25519 AAAA...'
```

To have setup enable Wake-on-LAN and reapply it at boot for an explicitly
selected network interface, add `--wake-interface enp1s0`. The setup records
the previous driver mode and the uninstaller restores it only if the interface
still has the mode Lab Station applied.

Choose `hybrid` to allow local instructor sessions, or `fmu-only` for a
headless FMU endpoint. `labstationctl setup --help` lists setup options. The
management key is installed for `labstation-ops`; SSH password login, PTY,
forwarding, user startup files, and shell commands are disabled for that
account. The Gateway host key must still be confirmed and pinned in Lab
Gateway before it sends station commands.

For an optional shared FMU Executor checkout:

```sh
sudo ./install.sh --profile fmu-only \
  --management-public-key 'ssh-ed25519 AAAA...'
```

Build the bundle with `FMU_EXECUTOR_SOURCE` set to a clean FMU Executor checkout
at the pinned version and commit. The installer detects the bundled source and
setup verifies its lock plus runtime file digest before installation. The
station pins the source tree in `internal/agent/fmu_executor_sha256.txt` and the
installed runtime content in `fmu_executor_payload_sha256.txt`.

For direct setup outside the installer, pass `--fmu-executor-source` pointing
to the packaged `fmu-executor-source` directory, which must include
`SOURCE.lock.json`; a raw checkout without that generated lock is rejected.
The source tree is copied into `/opt/decentralabs/fmu-executor`; FMUs are stored
under `/var/lib/decentralabs/fmu-executor/fmu-data`. Setup creates a Python
virtual environment and installs the pinned project requirements. Use an
approved PyPI mirror or pre-populated package cache on isolated hosts.

Before exposing Tiny Desk, set a unique `labuser` password through the
organization's approved credential process. The profile is not ready until
xrdp, the configured application, the session launcher, channel restrictions,
and the RDP account are all ready.

## Operations

```sh
sudo labstationctl status-json
sudo labstationctl local-mode status
sudo labstationctl local-mode set --ttl=28800
sudo labstationctl local-mode clear
sudo labstationctl energy audit
sudo labstationctl recovery reboot-if-needed
```

Local mode expires after eight hours by default. Use `--ttl=seconds` to set a
bounded lifetime from 60 seconds to 24 hours.

The SSH dispatcher accepts a single structured JSON request on stdin and
returns a JSON result. Exit code `0` means success, `1` means completed with a
warning, and `2+` means failure. It accepts only named station operations; the
Gateway never receives a shell on the station.

Status, heartbeat, operation, command queue, and session audit data are stored
under `/var/lib/decentralabs/lab-station` and
`/var/log/decentralabs/lab-station`. The queue accepts only JSON allowlisted
operations and never accepts secret values.

## Contract and security

The schema source and shared fixtures are in
`Lab Gateway/contracts/station/v3/`. A contract release must validate both
Windows and Linux payloads against this source. Private SSH credentials stay
encrypted in Lab Gateway. Station secrets are sent through the authenticated
dispatcher channel, written root-only, and never accepted from the command
queue. The privileged helper has a fixed operation set for session termination,
power, service control, and FMU token file operations.

Read [contracts/README.md](contracts/README.md) and
[packaging/README.md](packaging/README.md) for release details. The
[implementation plan](docs/PLAN_LAB_STATION_LINUX.md) tracks the target matrix,
acceptance gates, and remaining evidence.
The [operator runbook](docs/operator-runbook.md) covers canary setup, Gateway
trust confirmation, routine checks, rollback, and evidence handling.
