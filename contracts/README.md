# Station Contract

Lab Station Linux implements Station Contract v3 (`3.0.0`). The canonical
schemas and cross-platform fixtures live in Lab Gateway at
`contracts/station/v3/`; this repository intentionally does not maintain a
second schema copy.

Both station test suites consume the portable scenario matrix at
`contracts/station/v3/test-parity.json`, vendored from the canonical matrix in
`Lab Gateway/docs/station-test-parity.json`. The Windows AHK and Linux Go tests
run matching status, session, power, and recovery scenarios while retaining
platform-specific adapters.

The Linux status and heartbeat payloads are emitted by `labstationd` and
`labstationctl heartbeat`. Release CI should validate those payloads against
the Gateway schema checkout and exercise the shared Windows/Linux fixtures.
The Linux contract boundary includes `platform`, `profile`, `management`,
`remoteAccess`, `summary`, per-capability `readiness`, typed `sessions`,
`operations`, and `localModeEnabled`.
