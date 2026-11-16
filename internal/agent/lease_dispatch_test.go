package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/decentralabs/lab-station-linux/internal/config"
)

func v2LeaseRequest(id, command, leaseID string, generation uint64, issued, before, expiry time.Time) string {
	return fmt.Sprintf(`{"schemaVersion":2,"id":%q,"operation":"execute","command":%q,"args":[],"issuedAt":%q,"executeBefore":%q,"context":{"kind":"reservation","labId":"42","reservationKey":"reservation-123","leaseId":%q,"generation":%d,"notBefore":%q,"expiresAt":%q}}`,
		id, command, issued.UTC().Format(time.RFC3339Nano), before.UTC().Format(time.RFC3339Nano), leaseID, generation,
		issued.UTC().Format(time.RFC3339Nano), expiry.UTC().Format(time.RFC3339Nano))
}

func dispatchTestRequest(t *testing.T, a *Agent, raw string) Result {
	t.Helper()
	var output bytes.Buffer
	if err := Dispatch(bytes.NewBufferString(raw), &output, a); err != nil {
		t.Fatal(err)
	}
	return decodeResult(t, output.Bytes())
}

func makeLeaseReconciliationDue(t *testing.T, stateDir, leaseID string, generation uint64) {
	t.Helper()
	lock, err := lockLeaseJournal(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockState(lock)
	state, err := loadLeaseState(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Active == nil || state.Active.LeaseID != leaseID || state.Active.Generation != generation {
		t.Fatalf("cannot advance reconciliation time for lease %q generation %d: %#v", leaseID, generation, state.Active)
	}
	stale := time.Now().UTC().Add(-2 * leaseReconciliationRetryInterval).Format(time.RFC3339Nano)
	state.Active.NextReconcileAt = stale
	state.Active.UpdatedAt = stale
	for id, operation := range state.Operations {
		if operation.LeaseID == leaseID && operation.Generation == generation {
			operation.UpdatedAt = stale
			state.Operations[id] = operation
		}
	}
	if err := saveLeaseState(stateDir, state); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchV2RejectsExpiredOrMissingLeaseContextBeforeEffects(t *testing.T) {
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	a, calls := newTestAgent(t, "dedicated", &fakeRuntime{sessionsOK: true})
	a.Config = cfg
	now := time.Now().UTC()
	result := dispatchTestRequest(t, a, v2LeaseRequest("expired-1", "prepare-session", "lease-1", 0,
		now.Add(-10*time.Minute), now.Add(-5*time.Minute), now.Add(time.Hour)))
	if result.ExitCode != 2 || result.Metadata["code"] != "STATION_DISPATCHER_DEADLINE_EXPIRED" || len(*calls) != 0 {
		t.Fatalf("expired request was not rejected before effects: %#v calls=%#v", result, *calls)
	}

	missing := `{"schemaVersion":2,"id":"missing-lease","operation":"execute","command":"prepare-session","args":[],"issuedAt":"` + now.Format(time.RFC3339Nano) + `","executeBefore":"` + now.Add(time.Minute).Format(time.RFC3339Nano) + `"}`
	result = dispatchTestRequest(t, a, missing)
	if result.ExitCode != 2 || result.Metadata["code"] != "STATION_LEASE_CONTEXT_REQUIRED" || len(*calls) != 0 {
		t.Fatalf("unscoped v2 prepare was not rejected: %#v calls=%#v", result, *calls)
	}
}

func TestOperatorSessionCommandsRefuseActiveLease(t *testing.T) {
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	runtime := &fakeRuntime{sessionsOK: true}
	a, calls := newTestAgent(t, "dedicated", runtime)
	a.Config = cfg
	now := time.Now().UTC()
	active := &leaseRecord{
		LeaseID: "operator-conflict", Kind: "reservation", Generation: 3, State: "active",
		NotBefore: now.Add(-time.Minute).Format(time.RFC3339Nano),
		ExpiresAt: now.Add(10 * time.Minute).Format(time.RFC3339Nano),
		UpdatedAt: now.Format(time.RFC3339Nano),
	}
	if err := saveLeaseState(cfg.StateDir, leaseState{NextGeneration: 3, Active: active, Operations: map[string]leaseOperation{}}); err != nil {
		t.Fatal(err)
	}

	for _, command := range []string{"prepare-session", "release-session"} {
		t.Run(command, func(t *testing.T) {
			result := a.ExecuteOperator(context.Background(), "operator-"+strings.TrimSuffix(command, "-session"), command, nil)
			if result.ExitCode != 2 || result.Metadata["code"] != "STATION_LEASE_CONFLICT" {
				t.Fatalf("operator %s bypassed the active lease: %#v", command, result)
			}
		})
	}
	if len(*calls) != 0 {
		t.Fatalf("operator refusal made session changes: %#v", *calls)
	}
	state, err := loadLeaseState(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Active == nil || state.Active.LeaseID != active.LeaseID || state.Active.State != "active" {
		t.Fatalf("operator refusal changed reservation lease state: %#v", state.Active)
	}
}

func TestAdministrativeReleaseProtectsRemoteLabUserWithoutLease(t *testing.T) {
	runtime := &fakeRuntime{
		remote:     []map[string]any{{"id": "rdp-unassociated", "user": "labuser", "type": "x11", "seat": ""}},
		sessionsOK: true,
	}
	a, calls := newTestAgent(t, "dedicated", runtime)

	result := a.ExecuteOperator(context.Background(), "operator-release-unassociated", "release-session", nil)

	if result.ExitCode != 2 || result.Metadata["code"] != "STATION_REMOTE_SESSION_ACTIVE" || len(*calls) != 0 {
		t.Fatalf("administrative Release must protect a remote LABUSER session without reservation evidence: %#v helper=%#v", result, *calls)
	}
}

func TestDispatchV1LifecycleCommandsUseOperatorLeaseGuard(t *testing.T) {
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	runtime := &fakeRuntime{sessionsOK: true}
	a, calls := newTestAgent(t, "dedicated", runtime)
	a.Config = cfg
	now := time.Now().UTC()
	active := &leaseRecord{
		LeaseID: "dispatcher-conflict", Kind: "reservation", Generation: 4, State: "active",
		NotBefore: now.Add(-time.Minute).Format(time.RFC3339Nano),
		ExpiresAt: now.Add(10 * time.Minute).Format(time.RFC3339Nano),
		UpdatedAt: now.Format(time.RFC3339Nano),
	}
	if err := saveLeaseState(cfg.StateDir, leaseState{NextGeneration: 4, Active: active, Operations: map[string]leaseOperation{}}); err != nil {
		t.Fatal(err)
	}

	for _, command := range []string{"prepare-session", "release-session"} {
		t.Run(command, func(t *testing.T) {
			raw := fmt.Sprintf(`{"schemaVersion":1,"id":"operator-%s","operation":"execute","command":%q,"args":[]}`,
				strings.TrimSuffix(command, "-session"), command)
			result := dispatchTestRequest(t, a, raw)
			if result.ExitCode != 2 || result.Metadata["code"] != "STATION_LEASE_CONFLICT" {
				t.Fatalf("dispatcher v1 %s bypassed the active lease: %#v", command, result)
			}
		})
	}
	if len(*calls) != 0 {
		t.Fatalf("dispatcher refusal made session changes: %#v", *calls)
	}
}

func TestDispatchV2LeaseJournalIsDurableIdempotentAndRejectsStaleRelease(t *testing.T) {
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	runtime := &fakeRuntime{sessionsOK: true}
	a, _ := newTestAgent(t, "dedicated", runtime)
	a.Config = cfg
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	prepareN := v2LeaseRequest("prepare-n", "prepare-session", "lease-n", 0, now, now.Add(time.Minute), expires)
	prepared := dispatchTestRequest(t, a, prepareN)
	if prepared.ExitCode != 0 || prepared.Metadata["lease"].(map[string]any)["generation"] != float64(1) {
		t.Fatalf("first lease did not become generation 1: %#v", prepared)
	}
	duplicate := dispatchTestRequest(t, a, prepareN)
	if duplicate.ExitCode != 0 || duplicate.Metadata["lease"].(map[string]any)["generation"] != float64(1) {
		t.Fatalf("duplicate prepare changed lease generation: %#v", duplicate)
	}
	changedPayload := strings.Replace(prepareN, `"args":[]`, `"args":["--guard-notify=false"]`, 1)
	conflict := dispatchTestRequest(t, a, changedPayload)
	if conflict.ExitCode != 2 || conflict.Metadata["code"] != "STATION_OPERATION_ID_CONFLICT" {
		t.Fatalf("same operation id with a different payload was not rejected: %#v", conflict)
	}

	releaseN := v2LeaseRequest("release-n", "release-session", "lease-n", 1, now, now.Add(time.Minute), expires)
	released := dispatchTestRequest(t, a, releaseN)
	if released.ExitCode != 0 {
		t.Fatalf("release of current lease failed: %#v", released)
	}
	prepareN1 := v2LeaseRequest("prepare-n-plus-1", "prepare-session", "lease-n-plus-1", 0, now, now.Add(time.Minute), expires)
	preparedN1 := dispatchTestRequest(t, a, prepareN1)
	if preparedN1.ExitCode != 0 || preparedN1.Metadata["lease"].(map[string]any)["generation"] != float64(2) {
		t.Fatalf("next lease did not advance the durable generation: %#v", preparedN1)
	}

	stale := v2LeaseRequest("delayed-release-n", "release-session", "lease-n", 1, now, now.Add(time.Minute), expires)
	staleResult := dispatchTestRequest(t, a, stale)
	if staleResult.ExitCode != 2 || staleResult.Metadata["code"] != "STATION_LEASE_STALE" {
		t.Fatalf("delayed release N was not rejected: %#v", staleResult)
	}

	state, err := loadLeaseState(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Active == nil || state.Active.LeaseID != "lease-n-plus-1" || state.Active.Generation != 2 {
		t.Fatalf("delayed release changed the current lease: %#v", state.Active)
	}

	statusRequest := `{"schemaVersion":2,"id":"status-1","operation":"operation.status","operationId":"prepare-n"}`
	status := dispatchTestRequest(t, a, statusRequest)
	var operation map[string]any
	if err := json.Unmarshal([]byte(status.Stdout), &operation); err != nil || operation["state"] != "completed" {
		t.Fatalf("operation.status did not return the durable result: %#v err=%v", status, err)
	}
}

func TestCancelledPrepareCannotActivateReservationLease(t *testing.T) {
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	runtime := &fakeRuntime{sessionsOK: true}
	a, _ := newTestAgent(t, "dedicated", runtime)
	a.Config = cfg
	ctx, cancel := context.WithCancel(context.Background())
	a.cleanWorkData = func() error {
		cancel()
		return context.Canceled
	}
	now := time.Now().UTC()
	var request Request
	if err := json.Unmarshal([]byte(v2LeaseRequest("cancelled-prepare", "prepare-session", "lease-cancelled", 0, now, now.Add(time.Minute), now.Add(time.Hour))), &request); err != nil {
		t.Fatal(err)
	}

	result := a.executeLeaseRequest(ctx, request)
	if result.ExitCode != 2 || result.Metadata["code"] != "STATION_OPERATION_CANCELLED" {
		t.Fatalf("cancelled lease prepare was not rejected: %#v", result)
	}
	state, err := loadLeaseState(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Active == nil || state.Active.State != "recovery-required" {
		t.Fatalf("cancelled prepare activated or discarded its lease state: %#v", state.Active)
	}
}

func TestKnownPrepareRefusalDoesNotPoisonLeaseOrOperationID(t *testing.T) {
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	cfg.Profile = "hybrid"
	runtime := &fakeRuntime{sessionsOK: true}
	a, _ := newTestAgent(t, "hybrid", runtime)
	a.Config = cfg
	localModePath := filepath.Join(cfg.StateDir, "local-mode.flag")
	if err := os.WriteFile(localModePath, []byte("enabled"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	raw := v2LeaseRequest("retry-same-operation", "prepare-session", "lease-retry", 0, now, now.Add(time.Minute), now.Add(time.Hour))

	refused := dispatchTestRequest(t, a, raw)
	state, err := loadLeaseState(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if refused.ExitCode != 2 || refused.Metadata["code"] != "STATION_LOCAL_MODE_ACTIVE" || refused.Metadata["retryable"] != true {
		t.Fatalf("active local mode should be an explicit retryable refusal: %#v", refused)
	}
	if state.Active != nil {
		t.Fatalf("a refusal before station changes must not retain a lease: %#v", state.Active)
	}
	if _, found := state.Operations["retry-same-operation"]; found {
		t.Fatal("a refusal before station changes must not cache the operation id")
	}
	if err := os.Remove(localModePath); err != nil {
		t.Fatal(err)
	}

	retried := dispatchTestRequest(t, a, raw)
	if retried.ExitCode != 0 || retried.Metadata["lease"].(map[string]any)["state"] != "active" {
		t.Fatalf("the same operation id should prepare after the refusal condition is cleared: %#v", retried)
	}
	if retried.Metadata["lease"].(map[string]any)["generation"] != float64(2) {
		t.Fatalf("retry should keep the station's durable generation monotonic: %#v", retried.Metadata["lease"])
	}
}

func TestIncompletePrepareCleanupLeavesLeaseForRecovery(t *testing.T) {
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	runtime := &fakeRuntime{sessionsOK: true}
	a, _ := newTestAgent(t, "hybrid", runtime)
	a.Config = cfg
	a.cleanWorkData = func() error { return fmt.Errorf("permission denied") }
	now := time.Now().UTC()

	result := dispatchTestRequest(t, a, v2LeaseRequest("cleanup-failure", "prepare-session", "lease-cleanup", 0, now, now.Add(time.Minute), now.Add(time.Hour)))
	state, err := loadLeaseState(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 2 || result.Metadata["code"] != "STATION_PREPARE_CLEANUP_FAILED" {
		t.Fatalf("incomplete cleanup must be a failed prepare: %#v", result)
	}
	lease := result.Metadata["lease"].(map[string]any)
	if lease["state"] != "recovery-required" || state.Active == nil || state.Active.State != "recovery-required" {
		t.Fatalf("incomplete cleanup must retain an explicit recovery lease: result=%#v active=%#v", lease, state.Active)
	}
}

func TestIncompleteFMUReleaseCleanupLeavesLeaseForRecovery(t *testing.T) {
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	runtime := &fakeRuntime{sessionsOK: true}
	a, _ := newTestAgent(t, "hybrid", runtime)
	a.Config = cfg
	preparedAt := time.Now().UTC()
	expires := preparedAt.Add(time.Hour)
	prepared := dispatchTestRequest(t, a, v2LeaseRequest("prepare-before-fmu-release-failure", "prepare-session", "lease-fmu-release", 0, preparedAt, preparedAt.Add(time.Minute), expires))
	if prepared.ExitCode != 0 {
		t.Fatalf("setup prepare failed: %#v", prepared)
	}
	a.cleanFMUExecutorStateOverride = func(context.Context) error { return fmt.Errorf("permission denied") }

	result := dispatchTestRequest(t, a, v2LeaseRequest("fmu-release-cleanup-failure", "release-session", "lease-fmu-release", 1, preparedAt, preparedAt.Add(time.Minute), expires))
	state, err := loadLeaseState(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 2 || result.Metadata["code"] != "STATION_FMU_CLEANUP_FAILED" {
		t.Fatalf("incomplete FMU cleanup must fail reservation release: %#v", result)
	}
	if state.Active == nil || state.Active.State != "recovery-required" {
		t.Fatalf("incomplete FMU cleanup must retain the lease for recovery: %#v", state.Active)
	}
}

func TestOptionalRebootFailureWarnsAfterReleaseAndClearsLease(t *testing.T) {
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	runtime := &fakeRuntime{sessionsOK: true}
	a, calls := newTestAgent(t, "hybrid", runtime)
	a.Config = cfg
	prepareTime := time.Now().UTC()
	expires := prepareTime.Add(time.Hour)
	prepared := dispatchTestRequest(t, a, v2LeaseRequest("prepare-before-reboot-warning", "prepare-session", "lease-reboot-warning", 0, prepareTime, prepareTime.Add(time.Minute), expires))
	if prepared.ExitCode != 0 {
		t.Fatalf("setup prepare failed: %#v", prepared)
	}
	a.runHelper = func(_ context.Context, raw []byte) error {
		var request map[string]any
		if err := json.Unmarshal(raw, &request); err != nil {
			return err
		}
		*calls = append(*calls, request)
		if request["operation"] == "power-reboot" {
			return fmt.Errorf("reboot helper unavailable")
		}
		return nil
	}
	releaseRaw := strings.Replace(
		v2LeaseRequest("release-reboot-warning", "release-session", "lease-reboot-warning", 1, prepareTime, prepareTime.Add(time.Minute), expires),
		`"args":[]`, `"args":["--reboot"]`, 1,
	)

	released := dispatchTestRequest(t, a, releaseRaw)
	state, err := loadLeaseState(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if released.ExitCode != 1 || !released.Success || released.Metadata["lease"].(map[string]any)["state"] != "released" || released.Metadata["rebootDeferred"] != true || released.Metadata["rebootReason"] != "reboot request failed" {
		t.Fatalf("a failed optional reboot must warn after releasing the reservation: %#v", released)
	}
	if state.Active != nil {
		t.Fatalf("successful remote release must clear the lease despite reboot warning: %#v", state.Active)
	}
}

func TestLeaseAdmissionWindowFailsClosedAndWatchdogCleansExpiredLease(t *testing.T) {
	matrix := loadPortableParityMatrix(t)
	now := time.Now().UTC()
	lease := leaseRecord{State: "active", NotBefore: now.Add(-time.Minute).Format(time.RFC3339Nano), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano)}
	if err := checkLeaseWindow(lease, now); err != nil {
		t.Fatalf("active lease was rejected: %v", err)
	}
	lease.NotBefore = now.Add(time.Minute).Format(time.RFC3339Nano)
	if err := checkLeaseWindow(lease, now); err == nil {
		t.Fatal("admission before notBefore was allowed")
	}
	lease.NotBefore = now.Add(-time.Minute).Format(time.RFC3339Nano)
	lease.ExpiresAt = now.Format(time.RFC3339Nano)
	if err := checkLeaseWindow(lease, now); err == nil {
		t.Fatal("admission at lease expiry was allowed")
	}

	runtime := &fakeRuntime{sessionsOK: true, remote: []map[string]any{{"id": "opaque-rdp-7", "user": "labuser", "remote": true, "seat": "", "type": "x11"}}}
	a, calls := newTestAgent(t, "dedicated", runtime)
	now = time.Now().UTC()
	unexpired := leaseRecord{LeaseID: "lease-not-expired", Kind: "reservation", Generation: 6, State: "active", NotBefore: now.Add(-time.Hour).Format(time.RFC3339Nano), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano)}
	if err := saveLeaseState(a.Config.StateDir, leaseState{NextGeneration: 6, Active: &unexpired, Operations: map[string]leaseOperation{}}); err != nil {
		t.Fatal(err)
	}
	if err := a.enforceLeaseExpiry(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := loadLeaseState(a.Config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if matrix.LeaseExpiry.UnexpiredLease != "retain" || state.Active == nil || len(*calls) != 0 {
		t.Fatalf("watchdog changed an unexpired lease: policy=%q state=%#v helper=%#v", matrix.LeaseExpiry.UnexpiredLease, state.Active, *calls)
	}

	expired := leaseRecord{LeaseID: "lease-expired", LabID: "lab-42", ReservationKey: "reservation-123", Kind: "reservation", Generation: 7, State: "active", NotBefore: now.Add(-time.Hour).Format(time.RFC3339Nano), ExpiresAt: now.Add(-time.Second).Format(time.RFC3339Nano)}
	if err := saveLeaseState(a.Config.StateDir, leaseState{NextGeneration: 7, Active: &expired, Operations: map[string]leaseOperation{}}); err != nil {
		t.Fatal(err)
	}
	if err := a.enforceLeaseExpiry(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := a.enforceLeaseExpiry(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err = loadLeaseState(a.Config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if matrix.LeaseExpiry.ExpiredLease != "release" || state.Active != nil || state.LastReleased == nil || state.LastReleased.Generation != 7 || state.LastReleased.State != "expired" {
		t.Fatalf("watchdog did not persist cleanup and generation: %#v", state)
	}
	if len(*calls) != 1 || (*calls)[0]["operation"] != "terminate-session" || (*calls)[0]["sessionId"] != "opaque-rdp-7" {
		t.Fatalf("watchdog did not terminate only the active Tiny Desk session: %#v", *calls)
	}
}

func TestLeaseExpiryRetriesOnlyKnownPreEffectFailures(t *testing.T) {
	matrix := loadPortableParityMatrix(t)
	runtime := &fakeRuntime{sessionsOK: false}
	a, calls := newTestAgent(t, "dedicated", runtime)
	now := time.Now().UTC()
	active := leaseRecord{
		LeaseID: "lease-expiry-retry", LabID: "lab-expiry-retry", ReservationKey: "reservation-expiry-retry", Kind: "reservation",
		Generation: 8, State: "active", NotBefore: now.Add(-time.Hour).Format(time.RFC3339Nano),
		ExpiresAt: now.Add(-time.Second).Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano),
	}
	if err := saveLeaseState(a.Config.StateDir, leaseState{NextGeneration: 8, Active: &active, Operations: map[string]leaseOperation{}}); err != nil {
		t.Fatal(err)
	}
	if err := a.enforceLeaseExpiry(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := loadLeaseState(a.Config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	first, found := state.Operations["lease-expiry-8"]
	if matrix.LeaseExpiry.KnownPreEffectFailure != "retry" || !found || first.Result == nil || first.Result.Metadata["retryable"] != true {
		t.Fatalf("a known failure before session effects must be durably marked retryable: policy=%q operation=%#v", matrix.LeaseExpiry.KnownPreEffectFailure, first)
	}
	if state.Active == nil || state.Active.State != "recovery-required" {
		t.Fatalf("a retryable failure must keep reservation admission closed: %#v", state.Active)
	}

	runtime.sessionsOK = true
	makeLeaseReconciliationDue(t, a.Config.StateDir, active.LeaseID, active.Generation)
	if err := a.enforceLeaseExpiry(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err = loadLeaseState(a.Config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Active != nil || state.LastReleased == nil || state.LastReleased.State != "expired" {
		t.Fatalf("retryable cleanup did not release the expired lease: %#v", state)
	}
	if _, found := state.Operations["lease-expiry-8-retry-2"]; !found || len(*calls) != 0 {
		t.Fatalf("retry must use a fresh durable operation id and avoid station effects after inventory refusal: operations=%#v helper=%#v", state.Operations, *calls)
	}
}

func TestLeaseExpiryDoesNotRetryPartialCleanupFailure(t *testing.T) {
	matrix := loadPortableParityMatrix(t)
	runtime := &fakeRuntime{
		sessionsOK: true,
		remote:     []map[string]any{{"id": "expired-rdp", "user": "labuser", "type": "x11", "remote": true}},
	}
	a, calls := newTestAgent(t, "dedicated", runtime)
	a.runHelper = func(_ context.Context, raw []byte) error {
		var request map[string]any
		if err := json.Unmarshal(raw, &request); err != nil {
			return err
		}
		*calls = append(*calls, request)
		return fmt.Errorf("session termination outcome requires reconciliation")
	}
	now := time.Now().UTC()
	active := leaseRecord{
		LeaseID: "lease-expiry-partial", LabID: "lab-expiry-partial", ReservationKey: "reservation-expiry-partial", Kind: "reservation",
		Generation: 9, State: "active", NotBefore: now.Add(-time.Hour).Format(time.RFC3339Nano),
		ExpiresAt: now.Add(-time.Second).Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano),
	}
	if err := saveLeaseState(a.Config.StateDir, leaseState{NextGeneration: 9, Active: &active, Operations: map[string]leaseOperation{}}); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := a.enforceLeaseExpiry(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	state, err := loadLeaseState(a.Config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if matrix.LeaseExpiry.PartialOrUnknownFailure != "recovery-required" || len(*calls) != 1 || state.Active == nil || state.Active.State != "recovery-required" {
		t.Fatalf("partial cleanup must block automatic retries and retain the lease for reconciliation: policy=%q active=%#v helper=%#v", matrix.LeaseExpiry.PartialOrUnknownFailure, state.Active, *calls)
	}
}

func TestLeaseExpiryDoesNotReplayInterruptedCleanup(t *testing.T) {
	runtime := &fakeRuntime{sessionsOK: true, remote: []map[string]any{{"id": "expired-rdp", "user": "labuser", "type": "x11"}}}
	a, calls := newTestAgent(t, "dedicated", runtime)
	now := time.Now().UTC()
	active := leaseRecord{
		LeaseID: "lease-expiry-unknown", LabID: "lab-expiry-unknown", ReservationKey: "reservation-expiry-unknown", Kind: "reservation",
		Generation: 10, State: "recovery-required", NotBefore: now.Add(-time.Hour).Format(time.RFC3339Nano),
		ExpiresAt: now.Add(-time.Second).Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano),
	}
	unknown := Result{ID: "lease-expiry-10", Command: "release-session", ExitCode: 2, Outcome: "failure",
		Message: "operation outcome is unknown after daemon restart", Metadata: map[string]any{"code": "STATION_OPERATION_RECOVERY_REQUIRED"}}
	operations := map[string]leaseOperation{
		"lease-expiry-10": {State: "recovery-required", Command: "release-session", LeaseID: active.LeaseID, Generation: active.Generation, Result: &unknown},
	}
	if err := saveLeaseState(a.Config.StateDir, leaseState{NextGeneration: 10, Active: &active, Operations: operations}); err != nil {
		t.Fatal(err)
	}

	if err := a.enforceLeaseExpiry(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Fatalf("an interrupted operation with an unknown outcome must not be replayed: %#v", *calls)
	}
}

func TestLeaseExpiryAutomaticallyReconcilesUnclassifiedRecoveryState(t *testing.T) {
	runtime := &fakeRuntime{sessionsOK: true, remote: []map[string]any{{"id": "expired-rdp", "user": "labuser", "type": "x11"}}}
	a, calls := newTestAgent(t, "dedicated", runtime)
	a.runHelper = func(_ context.Context, raw []byte) error {
		var request map[string]any
		if err := json.Unmarshal(raw, &request); err != nil {
			return err
		}
		*calls = append(*calls, request)
		return fmt.Errorf("session termination outcome requires reconciliation")
	}
	now := time.Now().UTC()
	active := leaseRecord{
		LeaseID: "lease-expiry-unclassified", LabID: "lab-unclassified", ReservationKey: "reservation-unclassified", Kind: "reservation",
		Generation: 11, State: "recovery-required", NotBefore: now.Add(-time.Hour).Format(time.RFC3339Nano),
		ExpiresAt: now.Add(-time.Second).Format(time.RFC3339Nano), UpdatedAt: now.Add(-2 * leaseReconciliationRetryInterval).Format(time.RFC3339Nano),
	}
	if err := saveLeaseState(a.Config.StateDir, leaseState{NextGeneration: 11, Active: &active, Operations: map[string]leaseOperation{}}); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := a.enforceLeaseExpiry(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	state, err := loadLeaseState(a.Config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	marker, found := state.Operations["lease-expiry-11"]
	if len(*calls) != 1 || state.Active == nil || state.Active.State != "recovery-required" || !found || marker.State != "completed" || marker.Result == nil || marker.Result.Metadata["retryable"] == true || state.Active.NextReconcileAt == "" {
		t.Fatalf("a recovery lease must attempt a fresh guarded Release and remain blocked after an uncertain effect: active=%#v marker=%#v helper=%#v", state.Active, marker, *calls)
	}
}

func TestLeaseExpiryRetriesRecordedPreEffectReleaseFailure(t *testing.T) {
	runtime := &fakeRuntime{sessionsOK: true}
	a, calls := newTestAgent(t, "dedicated", runtime)
	now := time.Now().UTC()
	active := leaseRecord{
		LeaseID: "lease-expiry-recorded-retry", LabID: "lab-recorded-retry", ReservationKey: "reservation-recorded-retry", Kind: "reservation",
		Generation: 12, State: "recovery-required", NotBefore: now.Add(-time.Hour).Format(time.RFC3339Nano),
		ExpiresAt: now.Add(-time.Second).Format(time.RFC3339Nano), UpdatedAt: now.Add(-2 * leaseReconciliationRetryInterval).Format(time.RFC3339Nano),
	}
	retryableResult := Result{ID: "gateway-release-12", Command: "release-session", ExitCode: 2, Outcome: "failure",
		Message: "session inventory unavailable", Metadata: map[string]any{"code": "STATION_SESSION_QUERY_FAILED", "retryable": true}}
	operations := map[string]leaseOperation{
		"gateway-release-12": {State: "completed", Command: "release-session", LeaseID: active.LeaseID, Generation: active.Generation, UpdatedAt: now.Add(-2 * leaseReconciliationRetryInterval).Format(time.RFC3339Nano), Result: &retryableResult},
	}
	if err := saveLeaseState(a.Config.StateDir, leaseState{NextGeneration: 12, Active: &active, Operations: operations}); err != nil {
		t.Fatal(err)
	}

	if err := a.enforceLeaseExpiry(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := loadLeaseState(a.Config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Active != nil || state.LastReleased == nil || state.LastReleased.State != "expired" || len(*calls) != 0 {
		t.Fatalf("a persisted pre-effect Release failure should be retried with no session effect required: state=%#v helper=%#v", state, *calls)
	}
	if _, found := state.Operations["lease-expiry-12"]; !found {
		t.Fatalf("expiry retry did not record its fresh operation id: %#v", state.Operations)
	}
}

func TestDispatchV2OperationStatusReturnsPersistedResult(t *testing.T) {
	matrix := loadPortableParityMatrix(t)
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	runtime := &fakeRuntime{sessionsOK: true}
	a, calls := newTestAgent(t, "dedicated", runtime)
	a.Config = cfg
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	prepared := dispatchTestRequest(t, a, v2LeaseRequest("status-prepare", "prepare-session", "lease-status", 0, now, now.Add(time.Minute), expires))
	if prepared.ExitCode >= 2 {
		t.Fatalf("prepare operation was refused before it could be journaled: %#v", prepared)
	}
	sideEffectsAfterPrepare := len(*calls)

	status := dispatchTestRequest(t, a, `{"schemaVersion":2,"id":"status-query","operation":"operation.status","operationId":"status-prepare"}`)
	var operation map[string]any
	if err := json.Unmarshal([]byte(status.Stdout), &operation); err != nil {
		t.Fatalf("decode persisted operation: %v; result=%#v", err, status)
	}
	if matrix.OperationStatus.PersistedOperation != "return-record" || operation["state"] != "completed" {
		t.Fatalf("operation.status did not return the completed journal record: %#v", operation)
	}
	result, ok := operation["result"].(map[string]any)
	if !ok || result["exitCode"] != float64(prepared.ExitCode) || len(*calls) != sideEffectsAfterPrepare {
		t.Fatalf("operation.status omitted its durable result or repeated side effects: operation=%#v calls=%#v", operation, *calls)
	}

	missing := dispatchTestRequest(t, a, `{"schemaVersion":2,"id":"status-missing","operation":"operation.status","operationId":"missing-operation"}`)
	if matrix.OperationStatus.MissingOperation != "not-found" || missing.ExitCode != 1 || missing.Metadata["state"] != "not-found" {
		t.Fatalf("missing operation status differs from shared not-found behavior: %#v", missing)
	}
	if len(*calls) != sideEffectsAfterPrepare {
		t.Fatalf("missing status lookup caused a side effect: %#v", *calls)
	}
}

func TestDispatchRejectsDuplicateJSONKeys(t *testing.T) {
	a := New(config.Defaults())
	result := dispatchTestRequest(t, a, `{"schemaVersion":1,"schemaVersion":1,"id":"dup","operation":"execute","command":"identity","args":[]}`)
	if result.ExitCode != 2 {
		t.Fatalf("duplicate request key was accepted: %#v", result)
	}
}
