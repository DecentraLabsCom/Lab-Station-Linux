package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/decentralabs/lab-station-linux/internal/config"
)

func writeQueueRequest(t *testing.T, path string, request Request) {
	t.Helper()
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestQueueProcessesOnlyMatchingAllowlistedStructuredRequests(t *testing.T) {
	cfg := config.Defaults()
	cfg.Name = "queue-test"
	cfg.StateDir = t.TempDir()
	cfg.LogDir = filepath.Join(cfg.StateDir, "logs")
	a := &Agent{Config: cfg, Runtime: &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, sessionsOK: true}}
	for _, directory := range []string{"inbox", "processing", "processed", "results"} {
		if err := os.MkdirAll(filepath.Join(cfg.StateDir, "commands", directory), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	inbox := filepath.Join(cfg.StateDir, "commands", "inbox")
	writeQueueRequest(t, filepath.Join(inbox, "job-1.json"), Request{SchemaVersion: 1, ID: "job-1", Operation: "execute", Command: "local-mode", Args: []string{"status"}})
	writeQueueRequest(t, filepath.Join(inbox, "job-2.json"), Request{SchemaVersion: 1, ID: "different-id", Operation: "execute", Command: "identity"})
	writeQueueRequest(t, filepath.Join(inbox, "job-3.json"), Request{SchemaVersion: 1, ID: "job-3", Operation: "execute", Command: "identity", SecretID: "fmu-internal-token", SecretValue: "never-queue-secrets"})

	if err := a.processQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	for id, exitCode := range map[string]float64{"job-1": 0, "job-2": 2, "job-3": 2} {
		path := filepath.Join(cfg.StateDir, "commands", "results", id+".json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		if result["exitCode"] != exitCode {
			t.Errorf("%s exitCode = %v, want %v", id, result["exitCode"], exitCode)
		}
		if id != "job-1" && result["metadata"].(map[string]any)["code"] != "STATION_COMMAND_REJECTED" {
			t.Errorf("%s should be rejected at the queue boundary: %#v", id, result)
		}
	}
	processed, err := os.ReadDir(filepath.Join(cfg.StateDir, "commands", "processed"))
	if err != nil {
		t.Fatal(err)
	}
	if len(processed) != 3 {
		t.Fatalf("processed queue entries = %d, want 3", len(processed))
	}
}

func TestQueueRejectsTruncatedJSONAndUnsafeIdsWithoutExecution(t *testing.T) {
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	for _, directory := range []string{"inbox", "processing", "processed", "results"} {
		if err := os.MkdirAll(filepath.Join(cfg.StateDir, "commands", directory), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	inbox := filepath.Join(cfg.StateDir, "commands", "inbox")
	if err := os.WriteFile(filepath.Join(inbox, "invalid.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Agent{Config: cfg}
	if err := a.processQueue(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := os.ReadFile(filepath.Join(cfg.StateDir, "commands", "results", "invalid.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rejected Result
	if err := json.Unmarshal(result, &rejected); err != nil {
		t.Fatal(err)
	}
	if rejected.ExitCode != 2 || rejected.Metadata["code"] != "STATION_COMMAND_REJECTED" {
		t.Fatalf("truncated request was not rejected: %#v", rejected)
	}
}

func TestReconcileInterruptedWorkRequiresRecoveryAndNeverReplays(t *testing.T) {
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	cfg.LogDir = filepath.Join(cfg.StateDir, "logs")
	commands := filepath.Join(cfg.StateDir, "commands")
	for _, directory := range []string{"inbox", "processing", "processed", "results"} {
		if err := os.MkdirAll(filepath.Join(commands, directory), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	lease := &leaseRecord{LeaseID: "lease-after-crash", Kind: "reservation", Generation: 4, State: "preparing", NotBefore: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), ExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)}
	state := leaseState{NextGeneration: 4, Active: lease, Operations: map[string]leaseOperation{
		"prepare-after-crash": {PayloadHash: "payload-hash", State: "processing", Command: "prepare-session", LeaseID: lease.LeaseID, Generation: lease.Generation},
	}}
	if err := saveLeaseState(cfg.StateDir, state); err != nil {
		t.Fatal(err)
	}
	writeQueueRequest(t, filepath.Join(commands, "processing", "prepare-after-crash.json"), Request{SchemaVersion: 2, ID: "prepare-after-crash", Operation: "execute", Command: "prepare-session"})
	writeQueueRequest(t, filepath.Join(commands, "processing", "plain-after-crash.json"), Request{SchemaVersion: 1, ID: "plain-after-crash", Operation: "execute", Command: "identity"})
	a, calls := newTestAgent(t, "dedicated", &fakeRuntime{sessionsOK: true})
	a.Config = cfg

	if err := a.reconcileInterruptedWork(); err != nil {
		t.Fatal(err)
	}
	recovered, err := loadLeaseState(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Active == nil || recovered.Active.State != "recovery-required" {
		t.Fatalf("crashed lease did not fail closed: %#v", recovered.Active)
	}
	operation := recovered.Operations["prepare-after-crash"]
	if operation.State != "recovery-required" || operation.Result == nil || operation.Result.Metadata["code"] != "STATION_OPERATION_RECOVERY_REQUIRED" {
		t.Fatalf("crashed operation was not durably classified: %#v", operation)
	}
	for _, id := range []string{"prepare-after-crash", "plain-after-crash"} {
		var result Result
		raw, err := os.ReadFile(filepath.Join(commands, "results", id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		if result.ExitCode != 2 || result.Metadata["code"] != "STATION_OPERATION_RECOVERY_REQUIRED" {
			t.Fatalf("queue result %q did not fail closed: %#v", id, result)
		}
		if _, err := os.Stat(filepath.Join(commands, "processed", id+".json")); err != nil {
			t.Fatalf("queue record %q was not preserved: %v", id, err)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("recovery replayed potentially completed operations: %#v", *calls)
	}
	if err := a.reconcileInterruptedWork(); err != nil {
		t.Fatalf("reconciliation was not idempotent: %v", err)
	}
}

func TestReconcileLeavesLiveLeaseOperationAlone(t *testing.T) {
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	commands := filepath.Join(cfg.StateDir, "commands")
	for _, directory := range []string{"inbox", "processing", "processed", "results"} {
		if err := os.MkdirAll(filepath.Join(commands, directory), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	lease := &leaseRecord{LeaseID: "lease-live", Kind: "reservation", Generation: 2, State: "preparing", NotBefore: time.Now().UTC().Format(time.RFC3339Nano), ExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)}
	if err := saveLeaseState(cfg.StateDir, leaseState{NextGeneration: 2, Active: lease, Operations: map[string]leaseOperation{
		"prepare-live": {PayloadHash: "payload-hash", State: "processing", Command: "prepare-session", LeaseID: lease.LeaseID, Generation: lease.Generation},
	}}); err != nil {
		t.Fatal(err)
	}
	processingPath := filepath.Join(commands, "processing", "prepare-live.json")
	writeQueueRequest(t, processingPath, Request{SchemaVersion: 2, ID: "prepare-live", Operation: "execute", Command: "prepare-session"})
	active, err := lockLeaseOperation(cfg.StateDir, "prepare-live", false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlockState(active)
	a := &Agent{Config: cfg}
	if err := a.reconcileInterruptedWork(); err != nil {
		t.Fatal(err)
	}
	state, err := loadLeaseState(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if state.Active == nil || state.Active.State != "preparing" || state.Operations["prepare-live"].State != "processing" {
		t.Fatalf("startup recovery modified a live operation: %#v", state)
	}
	if _, err := os.Stat(processingPath); err != nil {
		t.Fatalf("startup recovery moved a live queue request: %v", err)
	}
	if _, err := os.Stat(filepath.Join(commands, "results", "prepare-live.json")); !os.IsNotExist(err) {
		t.Fatalf("startup recovery created a result for a live operation: %v", err)
	}
}

func TestLeaseOperationLockRejectsConcurrentAndSymlinkedLocks(t *testing.T) {
	stateDir := t.TempDir()
	first, err := lockLeaseOperation(stateDir, "operation-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockLeaseOperation(stateDir, "operation-1", true); err != errLeaseOperationInProgress {
		t.Fatalf("concurrent lock error = %v, want in-progress sentinel", err)
	}
	unlockState(first)
	second, err := lockLeaseOperation(stateDir, "operation-1", true)
	if err != nil {
		t.Fatalf("released operation lock could not be reacquired: %v", err)
	}
	unlockState(second)
	if _, err := lockLeaseOperation(stateDir, "../outside", true); err == nil {
		t.Fatal("operation lock accepted a path-traversal identifier")
	}

	linkDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(linkDir, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(linkDir, "operation-linked.lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := lockLeaseOperation(linkDir, "linked", true); err == nil {
		t.Fatal("operation lock followed a symbolic link")
	}
}

func TestDaemonPublishesBothContractSnapshotsAndStopsOnContextCancellation(t *testing.T) {
	matrix := loadPortableParityMatrix(t)
	cfg := config.Defaults()
	cfg.Name = "daemon-test"
	cfg.StateDir = t.TempDir()
	cfg.LogDir = filepath.Join(cfg.StateDir, "logs")
	runtime := &fakeRuntime{
		supervisor: fakeSupervisor{name: "systemd"},
		local:      []map[string]any{{"id": "seat-1", "user": "teacher", "kind": "local", "active": true, "evictable": false}},
		sessionsOK: true,
	}
	a := &Agent{Config: cfg, Runtime: runtime}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := a.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"status.json", "heartbeat.json"} {
		raw, err := os.ReadFile(filepath.Join(cfg.StateDir, name))
		if err != nil {
			t.Fatal(err)
		}
		var status map[string]any
		if err := json.Unmarshal(raw, &status); err != nil {
			t.Fatal(err)
		}
		assertPortableStatusContract(t, status, "linux", matrix)
	}
	if _, err := os.Stat(filepath.Join(cfg.LogDir, "labstation.log")); err != nil {
		t.Fatal("daemon did not produce its file-backed audit log")
	}
}
