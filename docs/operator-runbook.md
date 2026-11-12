# Lab Station Linux operator runbook

This runbook covers a controlled preview installation and its Gateway onboarding.
The current release reports `supportTier: unverified`; use a canary host and do
not treat a successful setup as distro, desktop, hardware, or production
certification.

## Before installation

1. Select a Linux host on the private management network and record its OS,
   architecture, supervisor, NIC, and current xrdp/SSH configuration.
2. Choose `dedicated`, `hybrid`, or `fmu-only`. The last profile does not install
   Tiny Desk GUI components. Use `hybrid` only when the host has a local
   instructor session that must be protected.
3. In Lab Manager, create or select the Linux Station. Set its SSH address and
   port, profile, and configured station application where applicable. Gateway
   generates and stores its private SSH key encrypted; copy the displayed
   public key for installation.
4. Obtain the host's SSH fingerprint through a trusted local console or another
   out-of-band channel. Do not confirm a fingerprint based only on discovery.
5. Obtain a release bundle from the approved release channel and verify its
   published checksum and signature when available. Do not deploy an unsigned
   or unverifiable bundle as a production release.

## Install the station

Extract the portable bundle and run its installer as root. The selected profile
and Gateway public key are required:

```sh
sudo ./install.sh \
  --profile dedicated \
  --management-public-key 'ssh-ed25519 AAAA...'
```

Replace `dedicated` with the approved profile. On hosts managed by a
configuration tool, install OS dependencies there and add `--no-install-deps`.
For an approved Wake-on-LAN interface, add `--wake-interface <interface>` only
after checking that the NIC and firmware support the required mode.

Setup creates the restricted `labstation-ops` SSH account and service users. It
does not generate, request, or display an RDP password for `labuser`; a newly
created `labuser` account remains password-locked until an administrator sets
one. Before enabling Tiny Desk, set a unique password as root:

```sh
passwd labuser
```

The password cannot be retrieved later from Linux; if it is forgotten, run
`passwd labuser` again to replace it. Re-running setup preserves the password
state of an existing `labuser` account. Do not reuse the Gateway key or an
administrator's password.

## Configure Tiny Desk applications

Tiny Desk supports the existing single-application settings and a curated list
of up to eight applications. Every command must be an executable installed
under `/opt/lab/apps`; do not point it at a shell. With multiple entries, all
configured applications start together when the Tiny Desk session begins.
Openbox provides `Alt+Tab` and `Alt+Shift+Tab` window switching and `Alt+F4`
window closing. The list is selected by the station administrator in the
root-managed configuration; it is not a general-purpose application menu.

For a multi-application session, edit
`/etc/decentralabs/lab-station/station.toml`. Clear the legacy `command` and
`args` values in `[application]`, then add one `[[application.apps]]` table per
approved application. For example:

```toml
[application]
id = "lab-app"
command = ""
args = []
user = "labuser"
close_timeout_seconds = 15

[[application.apps]]
id = "instrument-panel"
command = "/opt/lab/apps/instrument-panel"
args = []

[[application.apps]]
id = "reference-viewer"
command = "/opt/lab/apps/reference-viewer"
args = ["--kiosk"]
```

The example paths are placeholders: install the approved programs there first.
Keep `id` values unique. After editing, validate and apply the profile as root:

```sh
sudo labstationctl config-check
sudo labstationctl app apply
sudo labstationctl status-json
```

`app apply` updates the protected launcher profile and Tiny Desk window
bindings, then restarts the station daemon so readiness reflects the new
configuration. It does not rerun host setup or change SSH and XRDP
configuration. The new application set takes effect the next time the
`labuser` Tiny Desk session starts. `status-json` reports the application
unavailable until every configured executable exists and is executable. For
one application, continue using `application.command` and `application.args`
as before.

An authorized provider may use the provider-managed RDP connection to start
Tiny Desk outside a reservation or demo. This operator path does not require an
active lease. End-user access still follows Gateway's authorized reservation or
demo flow; leases continue to govern `prepare-session` and `release-session`.
Both paths use `labuser`, so disconnect provider maintenance sessions before a
reservation begins. `release-session` signs out `labuser` sessions at the end
of the reservation. Keep the direct Guacamole connection and RDP network path
restricted to trusted operators; the station does not enforce a lease at
application launch.

## Confirm Gateway trust and readiness

In Lab Manager, review the discovered SSH host fingerprint against the
out-of-band value and confirm it explicitly. Then run the Station verification
and wait for an authenticated Station Contract v3 identity and readiness result.
An SSH banner or an unverified discovery result does not establish station
identity.

On the host, inspect local status and the service without printing secrets:

```sh
sudo labstationctl status-json
sudo systemctl status decentralabs-labstation.service
sudo journalctl -u decentralabs-labstation.service --since today
```

On OpenRC, use `rc-service decentralabs-labstation status` and inspect
`/var/log/decentralabs/lab-station/labstation.log`. Station state is under
`/var/lib/decentralabs/lab-station`; logs are under
`/var/log/decentralabs/lab-station`. Never copy the root-only FMU token into a
terminal transcript, issue, heartbeat, or log.

For `fmu-only`, enroll the token through Gateway's authenticated station secret
operation, then check service health and capacity from the managed workflow. Do
not put the token in command-line arguments or queue files.

## Routine checks and recovery

Use `labstationctl status-json` to review platform, profile, capabilities, and
readiness. Local mode is time-limited:

```sh
sudo labstationctl local-mode status
sudo labstationctl local-mode set --ttl=28800
sudo labstationctl local-mode clear
```

Before changing energy or reboot state, inspect the report and make sure no
active user or FMU work would be interrupted:

```sh
sudo labstationctl energy audit
sudo labstationctl recovery reboot-if-needed
```

If Gateway reports a host-key mismatch, stop remote actions and compare the
fingerprint through the trusted console before changing the pin. If lease state,
session ownership, or operation outcome is uncertain, reconcile station status
and the operation journal before retrying `prepare-session` or `release-session`;
do not retry under a new operation ID. Disconnect provider maintenance sessions
before a reservation begins.

For an agent rollback from the portable bundle, run `rollback.sh` as root. It
restores the previous agent binaries and restarts the station service. For a
package installation, use the approved package-manager rollback procedure and
verify the package version and service status afterwards. Do not remove station
state as part of routine rollback.

To uninstall a portable installation, run `uninstall.sh` as root. It preserves
configuration, accounts, FMU models, secrets, and operational evidence by
default. Use `--purge-data` only after an approved backup and explicit data
retention decision. If setup detects that an administrator changed a managed
SSH, xrdp, or Wake-on-LAN file, it preserves that file for manual reconciliation.

## Evidence to keep with a canary

Record the artifact version and hashes, OS image and architecture, supervisor,
profile, setup result, Station identity and capabilities, SSH fingerprint
confirmation, relevant service logs, and any recovery action. Keep credentials,
private keys, FMU tokens, and model data out of the evidence bundle. A canary
does not by itself establish distro-matrix, GUI, WoL, hardware,
release-signing, or upgrade/rollback readiness.
