package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

func TestLeaseAdmissionWindowFailsClosedAndWatchdogCleansExpiredLease(t *testing.T) {
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
	expired := leaseRecord{LeaseID: "lease-expired", Kind: "reservation", Generation: 7, State: "active", NotBefore: now.Add(-time.Hour).Format(time.RFC3339Nano), ExpiresAt: now.Add(-time.Second).Format(time.RFC3339Nano)}
	if err := saveLeaseState(a.Config.StateDir, leaseState{NextGeneration: 7, Active: &expired, Operations: map[string]leaseOperation{}}); err != nil {
		t.Fatal(err)
	}
	if err := a.enforceLeaseExpiry(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := loadLeaseState(a.Config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Active != nil || state.LastReleased == nil || state.LastReleased.Generation != 7 || state.LastReleased.State != "expired" {
		t.Fatalf("watchdog did not persist cleanup and generation: %#v", state)
	}
	if len(*calls) != 1 || (*calls)[0]["operation"] != "terminate-session" || (*calls)[0]["sessionId"] != "opaque-rdp-7" {
		t.Fatalf("watchdog did not terminate only the active Tiny Desk session: %#v", *calls)
	}
}

func TestDispatchRejectsDuplicateJSONKeys(t *testing.T) {
	a := New(config.Defaults())
	result := dispatchTestRequest(t, a, `{"schemaVersion":1,"schemaVersion":1,"id":"dup","operation":"execute","command":"identity","args":[]}`)
	if result.ExitCode != 2 {
		t.Fatalf("duplicate request key was accepted: %#v", result)
	}
}
