package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

type disconnectCleanupRuntime struct {
	*fakeRuntime
	connected  bool
	stateKnown bool
}

func (r disconnectCleanupRuntime) RDPClientsConnected() (bool, bool) {
	return r.connected, r.stateKnown
}

func TestReservationDisconnectCleanupWaitsForGraceAndRecordsClose(t *testing.T) {
	baseRuntime := &fakeRuntime{
		remote:     []map[string]any{{"id": "rdp-1", "user": "labuser", "type": "x11", "remote": true}},
		sessionsOK: true,
	}
	a, calls := newTestAgent(t, "dedicated", baseRuntime)
	a.Runtime = disconnectCleanupRuntime{fakeRuntime: baseRuntime, stateKnown: true}
	writeDisconnectCleanupLease(t, a, time.Now().UTC().Add(time.Hour))

	if err := a.enforceReservationDisconnectClose(context.Background()); err != nil {
		t.Fatalf("first disconnected poll failed: %v", err)
	}
	watch, err := loadReservationDisconnectWatch(a.Config.StateDir)
	if err != nil || watch == nil {
		t.Fatalf("first poll should persist a disconnect watch, watch=%+v err=%v", watch, err)
	}
	if len(*calls) != 0 {
		t.Fatalf("first poll closed the session before grace: %#v", *calls)
	}

	watch.DisconnectedAt = time.Now().UTC().Add(-reservationDisconnectCloseGrace + time.Second).Format(time.RFC3339Nano)
	if err := saveReservationDisconnectWatch(a.Config.StateDir, watch); err != nil {
		t.Fatal(err)
	}
	if err := a.enforceReservationDisconnectClose(context.Background()); err != nil {
		t.Fatalf("poll before grace expiry failed: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("session closed before the 120-second grace elapsed: %#v", *calls)
	}

	watch.DisconnectedAt = time.Now().UTC().Add(-reservationDisconnectCloseGrace).Format(time.RFC3339Nano)
	if err := saveReservationDisconnectWatch(a.Config.StateDir, watch); err != nil {
		t.Fatal(err)
	}
	if err := a.enforceReservationDisconnectClose(context.Background()); err != nil {
		t.Fatalf("poll at grace expiry failed: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0]["operation"] != "terminate-session" || (*calls)[0]["sessionId"] != "rdp-1" {
		t.Fatalf("grace expiry did not perform controlled session termination: %#v", *calls)
	}
	if len(baseRuntime.remote) != 0 {
		t.Fatalf("disconnect cleanup closed apps but left the XRDP desktop session active: %#v", baseRuntime.remote)
	}
	if (*calls)[0]["forceSession"] != true || (*calls)[0]["closeTimeoutSeconds"] != float64(10) || (*calls)[0]["terminateOnCloseFailure"] != false {
		t.Fatalf("disconnect cleanup did not defer forced XRDP termination after an unsuccessful orderly close: %#v", *calls)
	}
	status := a.reservationDisconnectCloseStatus()
	if status["success"] != true || status["method"] != "station-controlled" || status["reservationKey"] != "reservation-1" {
		t.Fatalf("controlled close completion was not recorded: %#v", status)
	}
	watch, err = loadReservationDisconnectWatch(a.Config.StateDir)
	if err != nil || watch != nil {
		t.Fatalf("disconnect watch was not cleared after close: watch=%+v err=%v", watch, err)
	}
}

func TestReservationDisconnectCleanupLeavesXRDPForGatewayWhenLocalCloseFails(t *testing.T) {
	baseRuntime := &fakeRuntime{
		remote:     []map[string]any{{"id": "rdp-1", "user": "labuser", "type": "x11", "remote": true}},
		sessionsOK: true,
	}
	a, calls := newTestAgent(t, "dedicated", baseRuntime)
	a.Runtime = disconnectCleanupRuntime{fakeRuntime: baseRuntime, stateKnown: true}
	writeDisconnectCleanupLease(t, a, time.Now().UTC().Add(time.Hour))
	if err := saveReservationDisconnectWatch(a.Config.StateDir, &reservationDisconnectWatch{
		reservationDisconnectIdentity: reservationDisconnectIdentity{
			ReservationKey: "reservation-1", LabID: "42", LeaseID: "lease-1", Generation: 1,
		},
		DisconnectedAt: time.Now().UTC().Add(-reservationDisconnectCloseGrace).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}
	a.runHelper = func(_ context.Context, raw []byte) error {
		var request map[string]any
		if err := json.Unmarshal(raw, &request); err != nil {
			return err
		}
		*calls = append(*calls, request)
		return errors.New("simulated Tiny Desk close failure")
	}

	if err := a.enforceReservationDisconnectClose(context.Background()); err == nil {
		t.Fatal("local disconnect cleanup reported success after the helper failed")
	}
	if len(baseRuntime.remote) != 1 || len(*calls) != 1 || (*calls)[0]["terminateOnCloseFailure"] != false {
		t.Fatalf("local failure did not preserve the disconnected XRDP session for Gateway: sessions=%#v calls=%#v", baseRuntime.remote, *calls)
	}
	if status := a.reservationDisconnectCloseStatus(); status["success"] == true {
		t.Fatalf("failed local close was reported as completed: %#v", status)
	}
	if watch, err := loadReservationDisconnectWatch(a.Config.StateDir); err != nil || watch == nil {
		t.Fatalf("failed local close did not remain retryable: watch=%+v err=%v", watch, err)
	}
}

func TestReservationDisconnectCleanupCancelsWatchOnReconnect(t *testing.T) {
	baseRuntime := &fakeRuntime{
		remote:     []map[string]any{{"id": "rdp-1", "user": "LABUSER", "type": "x11", "remote": true}},
		sessionsOK: true,
	}
	a, calls := newTestAgent(t, "dedicated", baseRuntime)
	a.Runtime = disconnectCleanupRuntime{fakeRuntime: baseRuntime, connected: true, stateKnown: true}
	writeDisconnectCleanupLease(t, a, time.Now().UTC().Add(time.Hour))
	if err := saveReservationDisconnectWatch(a.Config.StateDir, &reservationDisconnectWatch{
		reservationDisconnectIdentity: reservationDisconnectIdentity{
			ReservationKey: "reservation-1", LabID: "42", LeaseID: "lease-1", Generation: 1,
		},
		DisconnectedAt: time.Now().UTC().Add(-2 * reservationDisconnectCloseGrace).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}

	if err := a.enforceReservationDisconnectClose(context.Background()); err != nil {
		t.Fatalf("reconnected poll failed: %v", err)
	}
	watch, err := loadReservationDisconnectWatch(a.Config.StateDir)
	if err != nil || watch != nil {
		t.Fatalf("reconnect did not cancel the pending disconnect watch: watch=%+v err=%v", watch, err)
	}
	if len(*calls) != 0 || a.reservationDisconnectCloseStatus()["success"] == true {
		t.Fatalf("reconnect triggered a close: calls=%#v status=%#v", *calls, a.reservationDisconnectCloseStatus())
	}
}

func TestReservationDisconnectCleanupLeavesScheduledExpiryToRelease(t *testing.T) {
	baseRuntime := &fakeRuntime{
		remote:     []map[string]any{{"id": "rdp-1", "user": "LABUSER", "type": "x11", "remote": true}},
		sessionsOK: true,
	}
	a, calls := newTestAgent(t, "dedicated", baseRuntime)
	a.Runtime = disconnectCleanupRuntime{fakeRuntime: baseRuntime, stateKnown: true}
	writeDisconnectCleanupLease(t, a, time.Now().UTC().Add(-time.Second))
	if err := saveReservationDisconnectWatch(a.Config.StateDir, &reservationDisconnectWatch{
		reservationDisconnectIdentity: reservationDisconnectIdentity{
			ReservationKey: "reservation-1", LabID: "42", LeaseID: "lease-1", Generation: 1,
		},
		DisconnectedAt: time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}

	if err := a.enforceReservationDisconnectClose(context.Background()); err != nil {
		t.Fatalf("expired reservation poll failed: %v", err)
	}
	watch, err := loadReservationDisconnectWatch(a.Config.StateDir)
	if err != nil || watch != nil {
		t.Fatalf("expired reservation did not clear its disconnect watch: watch=%+v err=%v", watch, err)
	}
	if len(*calls) != 0 || len(baseRuntime.remote) != 1 {
		t.Fatalf("disconnect fallback closed the session after reservation expiry: calls=%#v sessions=%#v", *calls, baseRuntime.remote)
	}
}

func TestExpiredDisconnectedReservationIsReleasedByWatchdogIncludingXRDPDesktop(t *testing.T) {
	baseRuntime := &fakeRuntime{
		remote:     []map[string]any{{"id": "rdp-1", "user": "labuser", "type": "x11", "remote": true}},
		sessionsOK: true,
	}
	a, calls := newTestAgent(t, "dedicated", baseRuntime)
	a.Runtime = disconnectCleanupRuntime{fakeRuntime: baseRuntime, stateKnown: true}
	writeDisconnectCleanupLease(t, a, time.Now().UTC().Add(-time.Second))
	if err := saveReservationDisconnectWatch(a.Config.StateDir, &reservationDisconnectWatch{
		reservationDisconnectIdentity: reservationDisconnectIdentity{
			ReservationKey: "reservation-1", LabID: "42", LeaseID: "lease-1", Generation: 1,
		},
		DisconnectedAt: time.Now().UTC().Add(-3 * time.Minute).Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatal(err)
	}

	// At the reservation boundary, the disconnect worker yields to scheduled
	// release. The lease watchdog then performs the complete station cleanup.
	if err := a.enforceReservationDisconnectClose(context.Background()); err != nil {
		t.Fatalf("expired reservation disconnect poll failed: %v", err)
	}
	if len(*calls) != 0 || len(baseRuntime.remote) != 1 {
		t.Fatalf("disconnect fallback acted after reservation expiry: calls=%#v sessions=%#v", *calls, baseRuntime.remote)
	}
	if err := a.enforceLeaseExpiry(context.Background()); err != nil {
		t.Fatalf("scheduled lease expiry release failed: %v", err)
	}

	state, err := loadLeaseState(a.Config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Active != nil || state.LastReleased == nil || state.LastReleased.State != "expired" {
		t.Fatalf("scheduled expiry did not release the reservation lease: %#v", state)
	}
	if len(*calls) != 1 || (*calls)[0]["operation"] != "terminate-session" || (*calls)[0]["sessionId"] != "rdp-1" {
		t.Fatalf("scheduled expiry did not terminate the complete XRDP desktop session: %#v", *calls)
	}
	if (*calls)[0]["terminateOnCloseFailure"] != true {
		t.Fatalf("scheduled release did not retain forced session termination as its final cleanup step: %#v", *calls)
	}
	if len(baseRuntime.remote) != 0 {
		t.Fatalf("reservation expiry left the XRDP desktop session behind: %#v", baseRuntime.remote)
	}
}

func TestContiguousReservationPrepareWaitsUntilPreviousLeaseIsReleased(t *testing.T) {
	runtime := &fakeRuntime{sessionsOK: true}
	a, _ := newTestAgent(t, "dedicated", runtime)
	now := time.Now().UTC()
	executeBefore := now.Add(time.Minute)
	previousExpiry := now.Add(time.Hour)

	previous := dispatchTestRequest(t, a, v2LeaseRequest(
		"previous-reservation-prepare", "prepare-session", "lease-previous", 0,
		now, executeBefore, previousExpiry,
	))
	if previous.ExitCode != 0 {
		t.Fatalf("previous reservation lease did not prepare: %#v", previous)
	}

	nextRequest := v2LeaseRequest(
		"next-reservation-prepare", "prepare-session", "lease-next", 0,
		now, executeBefore, now.Add(2*time.Hour),
	)
	blocked := dispatchTestRequest(t, a, nextRequest)
	if blocked.ExitCode != 2 || blocked.Metadata["code"] != "STATION_LEASE_CONFLICT" {
		t.Fatalf("contiguous reservation Prepare should wait while the prior lease is active: %#v", blocked)
	}
	state, err := loadLeaseState(a.Config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Active == nil || state.Active.LeaseID != "lease-previous" || state.Active.State != "active" {
		t.Fatalf("blocked next Prepare changed the current station lease: %#v", state.Active)
	}
	if _, recorded := state.Operations["next-reservation-prepare"]; recorded {
		t.Fatal("a lease conflict must leave the next Prepare operation retryable")
	}

	released := dispatchTestRequest(t, a, v2LeaseRequest(
		"previous-reservation-release", "release-session", "lease-previous", 1,
		now, executeBefore, previousExpiry,
	))
	if released.ExitCode != 0 {
		t.Fatalf("previous reservation lease did not release: %#v", released)
	}
	retried := dispatchTestRequest(t, a, nextRequest)
	if retried.ExitCode != 0 || retried.Metadata["lease"].(map[string]any)["state"] != "active" {
		t.Fatalf("the next reservation could not prepare after the previous release: %#v", retried)
	}
	if retried.Metadata["lease"].(map[string]any)["generation"] != float64(2) {
		t.Fatalf("the accepted next reservation should advance the lease generation: %#v", retried.Metadata["lease"])
	}
}

func TestFmuOnlyProfileSkipsReservationDisconnectCleanup(t *testing.T) {
	baseRuntime := &fakeRuntime{
		remote:     []map[string]any{{"id": "rdp-1", "user": "LABUSER", "type": "x11", "remote": true}},
		sessionsOK: true,
	}
	a, calls := newTestAgent(t, "fmu-only", baseRuntime)
	a.Runtime = disconnectCleanupRuntime{fakeRuntime: baseRuntime, stateKnown: true}
	writeDisconnectCleanupLease(t, a, time.Now().UTC().Add(time.Hour))

	if err := a.enforceReservationDisconnectClose(context.Background()); err != nil {
		t.Fatalf("FMU-only disconnect poll failed: %v", err)
	}
	if len(*calls) != 0 || len(baseRuntime.remote) != 1 {
		t.Fatalf("FMU-only profile attempted desktop cleanup: calls=%#v sessions=%#v", *calls, baseRuntime.remote)
	}
	watch, err := loadReservationDisconnectWatch(a.Config.StateDir)
	if err != nil || watch != nil {
		t.Fatalf("FMU-only profile created a desktop disconnect watch: watch=%+v err=%v", watch, err)
	}
}

func writeDisconnectCleanupLease(t *testing.T, a *Agent, expiresAt time.Time) {
	t.Helper()
	now := time.Now().UTC()
	if err := saveLeaseState(a.Config.StateDir, leaseState{
		NextGeneration: 2,
		Active: &leaseRecord{
			LeaseID: "lease-1", LabID: "42", ReservationKey: "reservation-1", Kind: "reservation",
			Generation: 1, State: "active", NotBefore: now.Add(-time.Hour).Format(time.RFC3339Nano),
			ExpiresAt: expiresAt.Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano),
		},
	}); err != nil {
		t.Fatal(err)
	}
}
