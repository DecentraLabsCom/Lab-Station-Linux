# Linux release packaging

`build.sh` cross-compiles static Go payloads for Linux amd64 and arm64 and
creates one portable archive per architecture from the same FHS file layout.
It emits `SHA256SUMS` and a JSON manifest. When `MINISIGN_SECRET_KEY` points to
a protected key, it signs the checksums. Do not publish an unsigned archive as
a release.

`build-packages.sh` packages each portable payload as `.deb` and `.rpm` using
`fpm`, keeping binaries and defaults identical between archive and packages.
The package manager installs dependencies; `labstationctl setup` applies the
selected station profile after an operator supplies the Gateway public key.

Set `FMU_EXECUTOR_SOURCE` to a checkout of the separately versioned shared
FMU Executor to include its source tree in the payload. This is a build-time
input; a station never downloads executable source during setup. When omitted,
FMU remains an optional capability and can be installed separately.

The portable `install.sh` installer creates a `.previous` copy of replaced
agent binaries. `rollback.sh` restores that copy. `uninstall.sh` stops the
station services, restores the xrdp configuration backup when present, and
removes the managed Tiny Desk launcher and Openbox policy only when they still
match the installed versions. It preserves station configuration, user
accounts, FMU models, secrets, and operational evidence. `--purge-data` removes
only station state and secrets.
