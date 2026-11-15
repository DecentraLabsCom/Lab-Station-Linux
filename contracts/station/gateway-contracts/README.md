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

`summary.ready` and `summary.issues` describe the profile's primary capability:
`readiness.physicalLab` for `dedicated` and `hybrid`, and `readiness.fmu` for
`fmu-only`. Wake and optional FMU readiness remain separate capabilities.
Consumers that need a specific capability must read its `readiness` entry.

## Profile-gated operator actions

The normalized profiles have the same operator meaning on Windows and Linux:

| Profile | Prepare / Release | Enable Local |
| --- | --- | --- |
| `dedicated` | Supported for the physical lab. Prepare fails closed if session inventory is unavailable or an unexpected local console session is active. | Not available. Disable remains available only to clear an already active stale flag. |
| `hybrid` | Supported for the physical lab. Prepare refuses active or unknown local mode and unavailable session inventory. When local-session eviction is enabled, each eligible local console session must affirmatively acknowledge a graphical warning before eviction. The configured grace deadline starts with the first prompt, so confirmation time counts against it and any remaining interval allows voluntary sign-out. A declined, timed-out, or unavailable prompt leaves an active session open and fails Prepare before cleanup or lease activation; a session that signs out during its prompt is already released. When eviction is disabled it fails safely. Prepare also clears bounded user working data and FMU temporary state. Release closes only the managed remote lab-user session. | Supported for instructor on-site use when no reservation or remote lab-user session is active. It expires after eight hours by default on current Windows and Linux Stations. |
| `fmu-only` | Not supported. | Not supported. Disable remains available only to clear an already active stale flag. |

`sessions.localSessionActive` has the same platform-neutral meaning on both
agents: at least one interactive physical-console/seat session exists,
regardless of the account name. It includes a local `LABUSER` session and
excludes RDP/Tiny Desk sessions regardless of account. Dedicated Prepare
preflight and station execution use this same signal; if the legacy top-level
field is also present, it must agree with the nested value.

Both agents expire local mode only from a valid explicit expiry or a recognized
legacy timestamp. A malformed or marker-only flag stays active until an
operator clears it; file modification time is not treated as proof of expiry.

The Gateway checks a fresh Station status and a successful session inventory
before manual Prepare, Release, or Enable Local requests. Current Station
agents also enforce the profile rules and serialize administrative Prepare and
Release against reservation/demo leases, plus local-mode enable against
reservation operations, so using WinRM, SSH, CLI, or the local command queue does
not change the action's meaning. An active lease must be released by its
reservation flow with matching context. Disabling local mode is allowed for
cleanup even if the profile has since changed.

Both Station agents attempt to release an active reservation lease locally
when `expiresAt` passes, even if Gateway cannot send the normal release
request. Windows runs this watchdog in its setup-managed background task;
Linux runs it in `labstationd`. If cleanup fails, the station stays closed to
new leases for operator reconciliation.

Dispatcher v2 is negotiated through authenticated Station identity before
Gateway sends a lease-bound lifecycle request. SSH and WinRM use the same
durable operation flow: query `operation.status` before sending and again after
an uncertain transport result. Reuse the same operation ID only to retrieve or
confirm that operation's recorded outcome; it does not execute a failed
operation again. A `recovery-required` result is a classified failure and must
not be resent automatically by the Gateway.

For a lease-bound `prepare-session` or `release-session`, exit code `1` is a
nonfatal warning only when the response lease state is respectively `active` or
`released`. A response with exit code `0` or `1` and lease state
`recovery-required` is inconsistent and must fail closed at the Gateway.
Incomplete Prepare cleanup or failed Release session termination uses exit code
`2` and leaves the lease `recovery-required`; a declined, timed-out, suppressed,
or unavailable Prepare confirmation while its session remains active is also a
retryable exit code `2` before session eviction, cleanup, or lease activation.
A completed Release with a
deferred reboot can use exit code `1` after its lease transition. A known
Prepare refusal before any session changes returns `retryable: true` without
retaining an active lease or caching the operation ID, so the same operation
ID can be retried after its precondition is corrected. If operation status is
`not-found` after an uncertain refusal, resending the same request is safe
because the station made no session changes.

The local lease-expiry watchdog retries a completed Release only when the
result explicitly has `retryable: true`, reserved for known failures before
session or FMU cleanup starts, such as unavailable session inventory. Each
automatic retry gets a fresh operation ID. A session termination failure,
FMU cleanup failure, or interrupted operation with an unknown outcome requires
operator reconciliation and keeps the station closed to new leases.
