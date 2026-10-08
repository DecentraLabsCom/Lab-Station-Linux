package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/decentralabs/lab-station-linux/internal/agent"
	"github.com/decentralabs/lab-station-linux/internal/config"
)

func TestLoadApplicationProfileRequiresRootOwnedNonWritableFileAndManagedStateDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app-profile.json")
	valid := `{"stateDir":"/var/lib/decentralabs/lab-station/data","application":{"id":"lab-app","command":"/opt/lab/apps/demo/run","args":["--safe"],"user":"labuser","closeTimeoutSeconds":15}}`
	if err := os.WriteFile(path, []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	profile, err := loadApplicationProfile(path)
	if err != nil {
		t.Fatal(err)
	}
	if profile.Application.ID != "lab-app" || profile.Application.Command != "/opt/lab/apps/demo/run" || profile.StateDir != "/var/lib/decentralabs/lab-station/data" {
		t.Fatalf("loaded profile = %#v", profile)
	}

	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := loadApplicationProfile(path); err == nil {
		t.Fatal("world-writable application profile was accepted")
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"stateDir":"/tmp/elsewhere","application":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadApplicationProfile(path); err == nil {
		t.Fatal("application profile outside the managed station state was accepted")
	}
}

func TestLoadApplicationProfileRejectsSymlinks(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.json")
	if err := os.WriteFile(target, []byte(`{"stateDir":"/var/lib/decentralabs/lab-station/data"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "app-profile.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadApplicationProfile(link); err == nil {
		t.Fatal("symbolic link application profile was accepted")
	}
}

func TestSubmitQueuePersistsProtocolOneAndValidatedLeaseRequests(t *testing.T) {
	stateDir := t.TempDir()
	cfg := config.Defaults()
	cfg.StateDir = stateDir
	if err := submitQueue(cfg, []string{"--id", "status-1", "--command", "identity"}); err != nil {
		t.Fatal(err)
	}
	var legacy agent.Request
	readQueueSubmission(t, stateDir, "status-1", &legacy)
	if legacy.SchemaVersion != 1 || legacy.Command != "identity" || legacy.Context != nil {
		t.Fatalf("legacy queue request changed contract: %#v", legacy)
	}

	now := time.Now().UTC().Add(-time.Second)
	args := []string{
		"--id", "prepare-1", "--command", "prepare-session", "--schema-version", "2",
		"--kind", "reservation", "--lab-id", "lab-7", "--reservation-key", "reservation-19", "--lease-id", "lease-19",
		"--issued-at", now.Format(time.RFC3339Nano), "--execute-before", now.Add(4 * time.Minute).Format(time.RFC3339Nano),
		"--not-before", now.Format(time.RFC3339Nano), "--expires-at", now.Add(time.Hour).Format(time.RFC3339Nano),
	}
	if err := submitQueue(cfg, args); err != nil {
		t.Fatal(err)
	}
	var lifecycle agent.Request
	readQueueSubmission(t, stateDir, "prepare-1", &lifecycle)
	if lifecycle.SchemaVersion != 2 || lifecycle.Context == nil || lifecycle.Context.LeaseID != "lease-19" || lifecycle.Context.Generation != 0 || lifecycle.IssuedAt == "" || lifecycle.ExecuteBefore == "" {
		t.Fatalf("protocol 2 lease envelope was not persisted: %#v", lifecycle)
	}

	original, err := os.ReadFile(filepath.Join(stateDir, "commands", "inbox", "status-1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := submitQueue(cfg, []string{"--id", "status-1", "--command", "power", "--", "--reboot"}); err == nil {
		t.Fatal("duplicate queue operation id replaced an existing request")
	}
	current, err := os.ReadFile(filepath.Join(stateDir, "commands", "inbox", "status-1.json"))
	if err != nil || string(current) != string(original) {
		t.Fatalf("duplicate queue operation changed existing payload: err=%v", err)
	}
}

func TestSubmitQueueRejectsInvalidProtocolAndLeaseInputs(t *testing.T) {
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	for _, args := range [][]string{
		{"--id", "bad/id", "--command", "identity"},
		{"--id", "unknown-schema", "--command", "identity", "--schema-version", "3"},
		{"--id", "mutating-v2", "--command", "power", "--schema-version", "2"},
		{"--id", "missing-lease", "--command", "prepare-session", "--schema-version", "2"},
	} {
		if err := submitQueue(cfg, args); err == nil {
			t.Errorf("submitQueue accepted invalid arguments %#v", args)
		}
	}
}

func readQueueSubmission(t *testing.T, stateDir, id string, target any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(stateDir, "commands", "inbox", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatal(err)
	}
}
