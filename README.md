---
description: Linux implementation of DecentraLabs Lab Station, paired with the Windows station agent.
---

# Lab Station (Linux)

[![Build](https://github.com/DecentraLabsCom/Lab-Station-Linux/actions/workflows/build.yml/badge.svg)](https://github.com/DecentraLabsCom/Lab-Station-Linux/actions/workflows/build.yml)
[![Security](https://github.com/DecentraLabsCom/Lab-Station-Linux/actions/workflows/codeql.yml/badge.svg)](https://github.com/DecentraLabsCom/Lab-Station-Linux/actions/workflows/codeql.yml)
[![Release](https://github.com/DecentraLabsCom/Lab-Station-Linux/actions/workflows/release.yml/badge.svg)](https://github.com/DecentraLabsCom/Lab-Station-Linux/actions/workflows/release.yml)

This is the Linux implementation of DecentraLabs **Lab Station**. It is the
Linux counterpart to [Lab Station (Windows)](https://github.com/DecentraLabsCom/Lab-Station/blob/main/README.md).
Both implementations integrate with Lab Gateway and implement Station
Contract v3; the management transport and host-specific controls differ by
platform.

The Linux agent is a Go service with a restricted SSH dispatcher. It can manage
station operations and, when configured, a Tiny Desk graphical session or the
optional FMU Executor.

## Product and platform support

Linux packages target `amd64` and `arm64`. The current release reports
`supportTier: unverified` and is published as a prerelease: the distribution,
desktop, and hardware combinations have not yet been certified. Use the
[operator runbook](docs/operator-runbook.md) for a canary installation and
readiness checks.

## Deployment profiles

| Profile | Intended use |
|---|---|
| `dedicated` | Linux host dedicated to remote laboratory use. |
| `hybrid` | Host shared with a local instructor; local mode protects declared on-site use, and remote preparation follows the configured local-session eviction policy. |
| `fmu-only` | Headless FMU endpoint; does not install Tiny Desk graphical components. |

Windows uses `server` and `hybrid`; Linux defines `dedicated`, `hybrid`, and
`fmu-only`. Profile setup behavior is platform-specific.

## Install and operate

Download and verify a bundle from the
[Linux releases](https://github.com/DecentraLabsCom/Lab-Station-Linux/releases),
extract it, and run `install.sh` as root. For example:

```sh
sudo ./install.sh \
  --profile dedicated \
  --management-public-key 'ssh-ed25519 AAAA...'
```

Choose the approved profile and use the management public key configured for
the Station in Lab Gateway. The
[operator runbook](docs/operator-runbook.md) covers Gateway host-key
confirmation, dependency management, graphical-session credentials, and
readiness checks.

Useful local checks:

```sh
sudo labstationctl status-json
sudo labstationctl energy audit
sudo labstationctl local-mode status
```

Tiny Desk can launch one configured application or a curated set of up to
eight applications together. Applications must be installed as executables
under `/opt/lab/apps`; the root-managed station configuration chooses which
ones are available in the session. In a multi-application session, use
`Alt+Tab` / `Alt+Shift+Tab` to switch windows and `Alt+F4` to close one. See
the [operator runbook](docs/operator-runbook.md#configure-tiny-desk-applications)
for configuration and update steps. Reservation release requests a normal X11
window close for each configured app before signaling any remaining processes.
It can first click a configured button in the single app or primary multi-app
window when that window is active. Multi-app closes share a 10-second maximum
deadline.

## Lab Gateway integration

Gateway manages Linux stations through a restricted SSH account and a
structured, allowlisted command dispatcher; it does not receive a shell on the
host. Gateway's private key is stored in its protected credential store, and
the station host key must be confirmed and pinned before remote operations.
Linux emits Station Contract v3 (`3.0.0`) and shares cross-platform status,
session, power, and recovery test scenarios with the Windows implementation.

See the [Linux operator runbook](docs/operator-runbook.md) and
[Station Contract guide](contracts/README.md) for the operational and schema
details.

## Optional FMU execution

The shared FMU Executor can be included from its pinned source when building a
Linux release. It is a separate optional service; FMU models and companion
visual assets are stored separately under the FMU data directory. Release
bundles may include selected examples; provider-specific models are provisioned
separately. Model availability depends on the release architecture. See
[Linux release packaging](packaging/README.md) for bundle contents and the
[FMU Executor repository](https://github.com/DecentraLabsCom/FMU-Executor) for
the shared runtime contract.

## Development and releases

The station agent is written in Go and release bundles target Linux `amd64`
and `arm64`. Packaging, signing, verification, installation, and rollback
instructions are in [Linux release packaging](packaging/README.md). The
release remains a prerelease until the platform support matrix is certified.

### Run Go tests from Windows

The Go packages use Linux system calls, so run them through Docker Desktop's
Linux engine rather than the native Windows Go toolchain. From PowerShell:

```powershell
.\scripts\test-go.ps1
```

The script reads the pinned sibling Git checkout at `..\FMU-Executor`, archives
the exact revision pinned by Lab Station inside the container, and runs
`go test -race -cover ./...`. Archiving the Git revision preserves canonical
file bytes even when Windows has converted line endings in the working tree.
To use a checkout in another location, pass its path:

```powershell
.\scripts\test-go.ps1 -FmuExecutorSource 'D:\work\FMU-Executor'
```

Docker Desktop must be running with Linux containers enabled. The
[`build.yml`](.github/workflows/build.yml) CI workflow already runs the Go test
suite on Ubuntu for Go 1.23 and the current stable release.

## Documentation

| Topic | Guide |
|---|---|
| Documentation navigation | [Contents](SUMMARY.md) |
| Installation, Gateway onboarding, and recovery | [Linux operator runbook](docs/operator-runbook.md) |
| Station contract and shared test scenarios | [Station Contract](contracts/README.md) |
| FMU Executor bundle, model contents, build, signing, release, and rollback | [Linux release packaging](packaging/README.md) |
| Shared FMU Executor behavior and API | [FMU Executor repository](https://github.com/DecentraLabsCom/FMU-Executor) |

For the corresponding Windows implementation, see the
[Lab Station Windows README](https://github.com/DecentraLabsCom/Lab-Station/blob/main/README.md).
