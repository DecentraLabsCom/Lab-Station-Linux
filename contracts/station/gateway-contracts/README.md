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
| `dedicated` | Supported for the physical lab. Prepare requires a known session inventory. Administrative Prepare may hand off a remote LABUSER session only when Gateway confirms no active/imminent reservation for mapped labs. Reservation-bound Prepare may hand it off when the incoming reservation is the only active/imminent mapped-lab reservation and has no prior access-session evidence. Dedicated warns, waits through the configured grace period, and logs off eligible local and authorized remote sessions without asking for confirmation. If any warning cannot be sent, Prepare fails before logoff. Matching reservation Release closes active local non-LABUSER console sessions and the managed remote LABUSER session. Administrative Release closes those local sessions when allowed by the existing lease and remote-session protections. Local LABUSER console sessions remain signed in. | Not available. Disable remains available only to clear an already active stale flag. |
| `hybrid` | Supported for the physical lab. Prepare refuses active or unknown local mode and unavailable session inventory. Administrative Prepare may hand off a remote LABUSER session only when Gateway confirms no active/imminent reservation for mapped labs. Reservation-bound Prepare may hand it off when the incoming reservation is the only active/imminent mapped-lab reservation and has no prior access-session evidence. An authorized remote LABUSER session follows the same confirmation policy as a local console session. When local-session eviction is enabled, each eligible session must affirmatively acknowledge a graphical warning before eviction. The configured grace deadline starts with the first prompt, so confirmation time counts against it and any remaining interval allows voluntary sign-out. A declined, timed-out, or unavailable prompt leaves an active session open and fails Prepare before cleanup or lease activation; a session that signs out during its prompt is already released. When eviction is disabled it fails safely. Prepare also clears bounded user working data and FMU temporary state. Release closes only the managed remote lab-user session and preserves the local console session. | Supported for local on-site use on Hybrid after a successful inventory confirms no remote LABUSER session and the Station has no active persisted lease. The current v3 path does not query the Gateway reservation projection, so operate outside active reservation windows. It expires after eight hours by default on current Windows and Linux Stations. |
| `fmu-only` | Not supported. | Not supported. Disable remains available only to clear an already active stale flag. |

`sessions.localSessionActive` has the same platform-neutral meaning on both
agents: at least one interactive physical-console/seat session exists,
regardless of the account name. It includes a local `LABUSER` session and
excludes Windows RemoteApp/RDP and Linux XRDP/Tiny Desk sessions regardless of account. Dedicated uses this
signal to warn and hand off every local session during Prepare, and to close
active local non-LABUSER sessions during Release. Marketplace status ignores
those local non-LABUSER sessions for Dedicated occupancy; local LABUSER
sessions remain visible. Hybrid uses the signal with local-mode and the
configured confirmation policy. If the legacy top-level field is also present,
it must agree with the nested value.

When `sessions.queryOk` is true, `sessions.remoteLabUserSessionIds` reports the
OS session IDs for remote managed-account sessions, including disconnected
Windows RDP sessions. Gateway uses those IDs only after checking the mapped-lab
reservation projection. The Station rechecks the exact snapshot before each
handoff stage; an unreported or changed session remains protected.

Station Contract v3 also exposes `sessions.remoteClientConnected` and
`sessions.remoteClientStateKnown`. These describe whether a remote client is
currently attached to the physical-lab access service, independently of a
disconnected desktop session. An unavailable session or connection inventory
must be treated as unknown.

Both agents expire local mode only from a valid explicit expiry or a recognized
legacy timestamp. A malformed or marker-only flag stays active until an
operator clears it; file modification time is not treated as proof of expiry.

The Gateway checks a fresh Station status and a successful session inventory
before manual Prepare, Release, or Enable Local requests. An active remote
LABUSER session is protected from administrative Prepare if any mapped-lab
reservation is active/imminent or reservation state is unknown. When the
Gateway projection proves no such reservation is active, it passes the exact
session IDs for the profile-specific handoff. Reservation-bound Prepare may
hand off a residual session when its own reservation is the only active/imminent
reservation and no access-session evidence exists for it; another active
reservation or unknown state blocks that handoff. Administrative Release
continues to protect every remote LABUSER session. Current Station agents also enforce the profile rules and serialize
administrative Prepare and Release against persisted Station leases, plus
local-mode enable against the persisted Station lease journal. The current v3
local-mode path does not query the Gateway reservation projection, so operators
should use it outside active reservation windows. Using WinRM, SSH, CLI, or the
local command queue does not change those Station-side lease guards. An active
lease must be released by its reservation flow with matching context. Disabling
local mode is allowed for cleanup even if the profile has since changed.

Both Station agents attempt to release an active reservation lease locally
when `expiresAt` passes, even if Gateway cannot send the normal release
request. Windows runs this watchdog in its setup-managed background task;
Linux runs it in `labstationd`. A failed lease-bound Release can also trigger
reconciliation before expiry. The Station keeps new lease admission closed
while the lease is `recovery-required`, then retries a fresh lease-bound
Release every 60 seconds until cleanup succeeds. A failed Prepare waits until
the reservation expires before the reconciler attempts Release.

Dispatcher v2 is negotiated through authenticated Station identity before
Gateway sends a lease-bound lifecycle request. SSH and WinRM use the same
durable operation flow: query `operation.status` before sending and again after
an uncertain transport result. Reuse the same operation ID only to retrieve or
confirm that operation's recorded outcome; it does not execute a failed
operation again. A `recovery-required` result is a classified failure; the
Gateway never replays that operation ID. For a failed scheduled end whose
Release step failed, the orchestrator may issue a fresh lease-bound Release ID,
skip completed `pre_end` work, and retry `post_end` after Release succeeds.

For a lease-bound `prepare-session` or `release-session`, exit code `1` is a
nonfatal warning only when the response lease state is respectively `active` or
`released`. A response with exit code `0` or `1` and lease state
`recovery-required` is inconsistent and must fail closed at the Gateway.
Incomplete Prepare cleanup or failed Release session termination uses exit code
`2` and leaves the lease `recovery-required`. A declined, timed-out, suppressed,
or unavailable confirmation on Hybrid, or a failed warning delivery on
Dedicated, is a retryable exit code `2` before the affected local session is
logged off or lease activation.
A completed Release with a
deferred reboot can use exit code `1` after its lease transition. A known
Prepare refusal before any session changes returns `retryable: true` without
retaining an active lease or caching the operation ID, so the same operation
ID can be retried after its precondition is corrected. If operation status is
`not-found` after an uncertain refusal, resending the same request is safe
because the station made no session changes.

The local reconciler uses a fresh operation ID for every Release retry and
never repeats an uncertain ID. It can recover known pre-effect failures,
partial cleanup failures, and interrupted lease-bound operations. Before each
attempt, the Station checks that the stored lease identity is valid, local mode
is off, and session inventory is available. Hybrid waits for local sessions;
both profiles wait for remote non-LABUSER sessions. Dedicated Release closes
active local non-LABUSER console sessions and the managed remote LABUSER
session, while a local LABUSER session remains signed in. Linux also waits if
LABUSER has a remote session outside the managed Tiny Desk X11 session.
The Gateway's scheduled-end retry uses the same session-protection guard.
Invalid lease identity or journal state still needs operator repair; a failed
Prepare does not evict users or run Release before the reservation expires.
