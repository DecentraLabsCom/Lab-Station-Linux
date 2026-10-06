package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
