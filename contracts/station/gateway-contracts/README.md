# Station Contract

This directory is the canonical, platform-neutral station contract consumed by
Lab Gateway, Lab Station for Windows, and Lab Station Linux.

- `v2/` freezes representative Windows payloads for the compatibility adapter.
- `v3/` defines the common status/heartbeat, capabilities, and command result
  schemas. Platform-specific details belong under `platformSpecific`.
- `dispatcher/v1/` and `dispatcher/v2/` define the SSH request envelopes.
  Dispatcher v2 also defines the correlated response shape and success/warning/
  failure fixtures. The dispatcher protocol is independent of the status
  contract version.

Consumers must normalize payloads once at ingress. Reservation, readiness,
timeline, AAS, and public status code use the normalized model and must not
inspect Windows paths or transport-specific heartbeat fields.

Dispatcher v2 is negotiated through authenticated Station identity before
Gateway sends a lease-bound lifecycle request. Retries retain the same
operation ID and payload; `operation.status` is used to reconcile uncertain
outcomes. A `recovery-required` result is a classified failure and must not be
sent again automatically.
