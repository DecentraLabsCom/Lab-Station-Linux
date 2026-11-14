# Changelog

## [Unreleased]

## [0.4.5] - 2026-11-14

### Fixed
- Route privileged station operations through a root-owned helper socket with
  peer credential checks so systemd's `NoNewPrivileges` hardening does not
  prevent watchdog cleanup.
- Release unreserved Tiny Desk sessions after 10 seconds without an RDP client.

## [0.4.4] - 2026-11-14

### Added
- Automatically release an unreserved Tiny Desk session after its RDP client
  has remained disconnected for one minute.

### Fixed
- Preserve Tiny Desk sessions while a reservation/demo lease is active, cancel
  automatic cleanup when an RDP client reconnects, and fail closed when the
  station cannot determine the RDP connection state.

## [0.4.3] - 2026-11-14

### Fixed
- Release Tiny Desk sessions when logind reports a seatless `labuser` X11
  session as `Remote=no`, while preserving the local-seat termination guard.

## [0.4.2] - 2026-11-14

### Added
- Support configured application close coordinates for Linux single-app and
  workspace sessions, followed by graceful window-close and process fallbacks.

### Fixed
- Run Tiny Desk and FMU temporary-data cleanup as the owning service account,
  avoiding privileged symlink races.
- Keep cancelled or expired session preparation from activating a reservation
  lease.
- Preserve `TMPDIR` for launched applications and allow the systemd service to
  clean Tiny Desk and already-installed FMU data during upgrades.
- Include optional FMU demo data in Debian and RPM packages.

## [0.4.1] - 2026-11-14

### Fixed
- Read heartbeat and status artifacts from the live agent state so SSH
  dispatchers report readiness correctly when the daemon runs with systemd
  privilege hardening.

### Changed
- Allow portable installation to skip the optional bundled FMU Executor with
  `--no-fmu-executor`.

## [0.4.0] - 2026-11-13

### Fixed
- Correct the pinned FMU Executor runtime and Smart Warehouse app digests so release bundles validate against the pinned source.

### Added
- Include the Linux x64 Smart Warehouse FMU, companion GLB and Three.js app in
  amd64 Station releases, pinned to the canonical FMU Executor assets.
- Expose companion GLB assets to the authenticated Gateway through the shared
  FMU Executor service.

### Changed
- Keep the arm64 Station packages free of the x86_64-only demo model.

## [0.3.0] - 2026-10-09

### Added
- Bundle FMU Executor 0.2.1 with reservation-scoped batch jobs, cancellation,
  persistent result history, and bounded retention.

### Changed
- Pin the shared FMU Executor 0.2.1 source, commit, and runtime payload.

## [0.2.0] - 2026-10-09

### Fixed
- Validate Linux account and owner IDs before integer conversion and `chown`, including bounds for signed system-call arguments.

### Changed
- Upload Debian `.deb` packages for amd64 and arm64 alongside the signed portable bundles and RPM packages.
- Update the embedded shared FMU Executor source and runtime payload to version 0.1.1.

## [0.1.0] - 2026-10-09

Initial preview release of the native Linux Lab Station agent.

- Publishes signed portable bundles and `.deb`/`.rpm` packages for Linux amd64 and arm64.
- Includes the pinned FMU Executor source and runtime payload in each release bundle.
- Provides SHA-256 checksums, Minisign signatures, and per-architecture manifests.
- Linux distro, desktop, and hardware support remains unverified; review the support boundary in the README before deployment.
