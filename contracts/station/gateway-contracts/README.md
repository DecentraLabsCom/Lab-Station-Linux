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

## Profile-gated operator actions

The normalized profiles have the same operator meaning on Windows and Linux:

| Profile | Prepare / Release | Enable Local |
| --- | --- | --- |
| `dedicated` | Supported for the physical lab. Prepare fails closed if session inventory is unavailable or an unexpected interactive user is active. | Not available. Disable remains available only to clear an already active stale flag. |
| `hybrid` | Supported for the physical lab. Prepare refuses active or unknown local mode and unavailable session inventory; local-session eviction follows the Station policy. Release closes only the managed remote lab-user session. | Supported for instructor on-site use when no reservation or remote lab-user session is active. It expires after eight hours by default on current Windows and Linux Stations. |
| `fmu-only` | Not supported. | Not supported. Disable remains available only to clear an already active stale flag. |

The Gateway checks a fresh Station status and a successful session inventory
before manual Prepare, Release, or Enable Local requests. Current Station
agents also enforce the profile rules and serialize local-mode enable against
reservation operations, so using WinRM or SSH does not change the action's
meaning. Disabling local mode is allowed for cleanup even if the profile has
since changed.

Dispatcher v2 is negotiated through authenticated Station identity before
Gateway sends a lease-bound lifecycle request. Retries retain the same
operation ID and payload; `operation.status` is used to reconcile uncertain
outcomes. A `recovery-required` result is a classified failure and must not be
sent again automatically.
