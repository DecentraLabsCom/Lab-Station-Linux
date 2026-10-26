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
| `hybrid` | Host shared with a local instructor; local sessions are protected during remote reservations. |
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
