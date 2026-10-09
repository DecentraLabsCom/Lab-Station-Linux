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

Set `FMU_EXECUTOR_SOURCE` to a clean Git checkout of the separately versioned
shared FMU Executor to include its source tree in the payload. The checkout must
match the pinned version, commit, and normalized Git tree manifest SHA-256 in
`internal/agent/fmu_executor_version.txt`, `fmu_executor_commit.txt`, and
`fmu_executor_sha256.txt`, and the runtime payload digest in
`fmu_executor_payload_sha256.txt`. The portable release manifest records both
digests. Setup verifies the source lock and every runtime file before
installation. This is a build-time input; a station never downloads executable
source during setup. When omitted, FMU remains an optional capability and can
be installed separately.
Update the Linux pin and the Windows station's `fmu-executor/SOURCE.lock.json`
together only after reviewing the shared FMU Executor release.

## Preparing a GitHub release

`release.sh` prepares signed portable archives, `.deb` and `.rpm` packages for
amd64 and arm64, per-architecture manifests, and an aggregate `SHA256SUMS`
file. It requires a matching `vX.Y.Z` tag, release notes in `CHANGELOG.md`,
and a clean checkout of the pinned FMU Executor source. The script refuses to
build unsigned release artifacts or reuse a non-empty output directory. Its
checks verify package metadata, archive signatures, checksums, and the
public/private signing-key pair.

The GitHub release workflow publishes only valid version tags already contained
in `main`. A manual run defaults to a dry run: with no tag supplied it builds
the current revision, signs and verifies the assets, and skips publication.
Manual publication requires an existing version tag and `dry_run` set to
`false`. Published releases remain prereleases because the supported Linux
distro, desktop, and hardware matrix has not yet been certified.

Create a dedicated, unencrypted CI key pair on a trusted offline machine:

```sh
minisign -G -W -s minisign.key -p minisign.pub
base64 -w0 minisign.key
base64 -w0 minisign.pub
```

In the GitHub repository, create a `MiniSign` environment and add the first
base64 value as the `MINISIGN_SECRET_KEY_BASE64` environment secret and the
second as the `MINISIGN_PUBLIC_KEY_BASE64` environment variable. Restrict the
environment to trusted maintainers and require review before deployment. Do
not commit either key file. Keep the private key in an approved secret store,
and distribute the public key fingerprint through a trusted channel separate
from the GitHub release; a public key attached to a release does not establish
its own authenticity.

Verify a downloaded release using the public key whose fingerprint you have
already trusted:

```sh
minisign -Vm SHA256SUMS -p minisign.pub
sha256sum -c SHA256SUMS
```

The aggregate checksum list covers the archives, package files, manifests,
per-archive checksum lists and signatures, and `minisign.pub`.

The portable `install.sh` installer creates a `.previous` copy of replaced
agent binaries. `rollback.sh` restores that copy. `uninstall.sh` stops the
station services, restores the xrdp configuration backup when present, and
removes the managed Tiny Desk launcher and Openbox policy only when they still
match the installed versions. It preserves station configuration, user
accounts, FMU models, secrets, and operational evidence. `--purge-data` removes
only station state and secrets.
