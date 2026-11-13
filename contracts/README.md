# Station Contract

Lab Station Linux implements Station Contract v3 (`3.0.0`). This repository
vendors the Gateway schemas and fixtures under `station/gateway-contracts/`;
`station/schema-bundle.lock.json` pins every file to an immutable Gateway
commit and SHA-256 digest. The same bundle is included in Windows Lab Station.
CI compares the vendored bytes with the pinned Gateway commit, and the verifier
can also validate the bundle offline.

Both station test suites consume the portable scenario matrix at
`contracts/station/v3/test-parity.json`, vendored from the canonical matrix in
`Lab Gateway/docs/station-test-parity.json`. The Windows AHK and Linux Go tests
run matching status, session, power, and recovery scenarios while retaining
platform-specific adapters.

The SSH command envelope is versioned separately from Station Contract. The
bundle contains Gateway's dispatcher v1/v2 request schemas and the v2 response
schema. V2 is required for lease-bound `prepare-session` and
`release-session`; the request carries a durable lease identity and bounded
UTC execution window. Dispatcher v1 `release-session` is reserved for trusted
operator cleanup when no lease is active, and it is serialized against lease
changes. `operation.status` reports `not-found`, `processing`,
`completed`, or `recovery-required`. An interrupted operation is never replayed
automatically; its lease remains closed until an operator reconciles it.
Linux's `labstationd` daemon and Windows' setup-managed background task also
attempt to release active leases when their expiry passes. If local cleanup
fails, the lease stays closed to new work for operator reconciliation.

The Linux status and heartbeat payloads are emitted by `labstationd` and
`labstationctl heartbeat`. CI validates the pinned bundle and exercises the
shared Windows/Linux fixtures.
The Linux contract boundary includes `platform`, `profile`, `management`,
`remoteAccess`, `summary`, per-capability `readiness`, typed `sessions`,
`operations`, and `localModeEnabled`.

`summary.ready` and `summary.issues` describe the profile's primary capability:
`readiness.physicalLab` for `dedicated` and `hybrid`, and `readiness.fmu` for
`fmu-only`. Wake and optional FMU readiness remain independent; read the
corresponding `readiness` entry when making a capability-specific decision.
