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
does not set an RDP password. Before enabling Tiny Desk, set a unique password
for `labuser` through the organization's approved credential process. Do not
reuse the Gateway key or an administrator's password.

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
session ownership, or operation outcome is uncertain, keep admission closed and
reconcile from station status and the operation journal; do not retry the action
under a new operation ID.

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
