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

Executable availability is a configuration check, not a test launch. In
particular, Chromium installed as a Snap has failed when its launcher tried to
create data in the protected, root-owned `labuser` home. Prefer a compatible
non-Snap browser for this setup and verify it in an actual RDP session.

An authorized provider may use the provider-managed RDP connection to start
Tiny Desk outside a reservation or demo. This operator path does not require an
active lease. End-user access still follows Gateway's authorized reservation or
demo flow; leases govern reservation-bound `prepare-session` and
`release-session`. The Lab Manager operator Release action is available only
when no lease is active and closes remote Tiny Desk sessions before cleaning
up station state. Both paths use `labuser`, so disconnect provider maintenance
sessions before a reservation begins. Keep the direct Guacamole connection and
RDP network path restricted to trusted operators; the station does not enforce
a lease at application launch.

### Configure Guacamole for Linux XRDP

When creating the Gateway's Guacamole RDP connection for a Linux Lab Station,
leave **Security mode** at its default (**Any**) and enable **Ignore server
certificate**. Leave **Trust host certificate on first use** disabled and
**Fingerprints of trusted host certificates** blank. XRDP generates a
self-signed certificate during installation; this setting lets Guacamole
connect without asking the teacher or technician to copy and pin its
fingerprint. RDP still negotiates its security and encryption, but Guacamole
does not verify the station's server identity. Use this setting only when the
RDP connection is restricted to a trusted private laboratory network. For
networks where server identity must be verified, leave **Ignore server
certificate** disabled and set the station's verified SHA-256 fingerprint in
**Fingerprints of trusted host certificates** instead. This guidance is
specific to Linux XRDP, not a general change for Windows RDP connections.

This is a Guacamole connection setting; it is not changed by installing or
updating Lab Station Linux. Apply it to the local RDP connection on each
Gateway that provides access to the station.

### Diagnose a black Tiny Desk screen

XRDP starts Xorg before it runs the Tiny Desk `.xsession` launcher. If the
session manager reports that Xorg timed out or exited, Chromium and the
application launcher have not started yet; an empty `.xsession-errors` file is
expected in that case. Inspect the Xorg log and session-manager journal first:

```sh
sudo find /var/lib/decentralabs/lab-station/tiny-desk-home -maxdepth 1 -type f -name '.xorgxrdp.*.log' -print
sudo tail -n 100 /var/log/xrdp-sesman.log
sudo tail -n 100 /var/lib/decentralabs/lab-station/tiny-desk-home/.xsession-errors
```

On Ubuntu, Xsession may instead direct errors to a temporary file. When
`/tmp/xsession-labuser` exists, read it as its owner:

```sh
sudo -u labuser tail -n 100 /tmp/xsession-labuser
```

The actual `labuser` session home is root-owned and read-only. Lab Station
installs root-owned XRDP startup shims to put that account's Xauthority file
and Xorg log in its private writable workspace. The XRDP session PATH is
prefixed with the managed `xauth` shim, and the configured Xorg executable is
wrapped; both shims pass through the original commands for other account UIDs.
The original Xorg arguments and session PATH remain in the XRDP configuration.

Upstream XRDP 0.9.17 ignores `XAuthorityInSystemDir`, so the presence of that
option does not establish that its Xauthority file is writable. Similarly, a
`status-json` readiness result checks the managed configuration; it does not
prove that a Guacamole RDP desktop has completed an Xorg startup. Verify a real
connection after installation, including any other XRDP accounts in use.

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

A valid expiry or recognized legacy timestamp ends the flag. A malformed or
marker-only flag remains active until `clear`; file modification time is not
used to infer when local use began.

Enable local mode only on a `hybrid` station, with a successful session
inventory and no active reservation or remote lab-user session; it marks
exclusive instructor use and expires after eight hours by default. `clear`
remains available on any profile to remove a stale flag. `Prepare` and `Release` are supported only for
`dedicated` and `hybrid` physical stations. Prepare rejects local mode and an
unexpected local console session on a dedicated station. On hybrid, the default
policy warns and evicts an eligible local session after the 90-second Gateway
grace period (minimum 30 seconds). Set
`station.allow_local_session_eviction = false` to make Prepare fail safely
without terminating an instructor session; Windows uses the equivalent
`AllowLocalSessionEviction=false` entry in `labstation/data/station-profile.ini`.
`--no-guard` skips notification and grace but still evicts immediately when the
policy allows it. Release closes only the managed remote `labuser` session and
is rejected while a reservation or demo lease is active.

Before each physical reservation, Prepare clears Tiny Desk `tmp`, cache,
Downloads, `Documents/LabStationScratch`, the XDG trash and recent-file list.
It restarts an installed FMU Executor to close its sessions and clears its
temporary work directory. It preserves desktop/application configuration and
FMU models. A cleanup problem is reported as an operational warning.

Before changing energy or reboot state, inspect the report and make sure no
active user or FMU work would be interrupted:

```sh
sudo labstationctl energy audit
sudo labstationctl recovery reboot-if-needed
```

Gateway shutdown requests use `power shutdown --delay=60 --require-idle`. Windows
and Linux Stations follow the same guard: the Gateway checks the advertised
capability, local mode, and session inventory before dispatch; the Station
checks local mode and sessions before and after the countdown. It cancels
shutdown if local mode is enabled, the inventory is unavailable, or a user
session is active at either check.

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
