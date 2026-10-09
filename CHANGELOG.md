# Changelog

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
