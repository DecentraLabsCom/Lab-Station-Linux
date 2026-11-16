package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (a *Agent) Run(ctx context.Context) error {
	if err := os.MkdirAll(a.Config.StateDir, 0750); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(a.Config.StateDir, "commands", "inbox"), 0750); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(a.Config.StateDir, "commands", "processing"), 0750); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(a.Config.StateDir, "commands", "processed"), 0750); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(a.Config.StateDir, "commands", "results"), 0750); err != nil {
		return err
	}
	if err := os.MkdirAll(a.Config.LogDir, 0750); err != nil {
		return err
	}
	if err := a.reconcileInterruptedWork(); err != nil {
		return err
	}
	statusTimer := time.NewTicker(15 * time.Second)
	defer statusTimer.Stop()
	go a.runQueue(ctx)
	watchdogTimer := time.NewTicker(time.Second)
	defer watchdogTimer.Stop()
	if err := a.publish(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-statusTimer.C:
			if err := a.publish(); err != nil {
				a.log("heartbeat-write-failed", map[string]any{"error": err.Error()})
			}
		case <-watchdogTimer.C:
			if err := a.enforceLeaseExpiry(ctx); err != nil {
				a.log("lease-watchdog-error", map[string]any{"error": err.Error()})
			}
		}
	}
}

// reconcileInterruptedWork never replays a command whose process may have
// performed effects before the daemon stopped. It records an explicit
// recovery-required result and keeps lease admission closed.
func (a *Agent) reconcileInterruptedWork() error {
	lock, err := lockLeaseJournal(a.Config.StateDir)
	if err != nil {
		return err
	}
	state, err := loadLeaseState(a.Config.StateDir)
	if err != nil {
		unlockState(lock)
		return err
	}
	operationIDs := make([]string, 0, len(state.Operations))
	for id, operation := range state.Operations {
		if operation.State == "processing" && ValidOperationID(id) {
			operationIDs = append(operationIDs, id)
		}
	}
	unlockState(lock)
	for _, id := range operationIDs {
		err = func() error {
			operationLock, err := lockLeaseOperation(a.Config.StateDir, id, true)
			if errors.Is(err, errLeaseOperationInProgress) {
				return nil
			}
			if err != nil {
				return err
			}
			defer unlockState(operationLock)
			journalLock, err := lockLeaseJournal(a.Config.StateDir)
			if err != nil {
				return err
			}
			defer unlockState(journalLock)
			current, err := loadLeaseState(a.Config.StateDir)
			if err != nil {
				return err
			}
			operation, found := current.Operations[id]
			if !found || operation.State != "processing" {
				return nil
			}
			updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
			operation.State = "recovery-required"
			operation.UpdatedAt = updatedAt
			result := Result{ID: id, Command: operation.Command, CompletedAt: updatedAt, Success: false, ExitCode: 2, Outcome: "failure", Message: "operation outcome is unknown after daemon restart; operator reconciliation is required", Stderr: "operation outcome is unknown after daemon restart; operator reconciliation is required", Metadata: map[string]any{"code": "STATION_OPERATION_RECOVERY_REQUIRED"}}
			operation.Result = &result
			current.Operations[id] = operation
			if current.Active != nil && current.Active.LeaseID == operation.LeaseID && current.Active.Generation == operation.Generation {
				current.Active.State = "recovery-required"
				current.Active.UpdatedAt = updatedAt
			}
			return saveLeaseState(a.Config.StateDir, current)
		}()
		if err != nil {
			return err
		}
	}

	processingDir := filepath.Join(a.Config.StateDir, "commands", "processing")
	entries, err := os.ReadDir(processingDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !ValidOperationID(id) {
			continue
		}
		path := filepath.Join(processingDir, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		err = func() error {
			operationLock, err := lockLeaseOperation(a.Config.StateDir, id, true)
			if err != nil {
				return err
			}
			defer unlockState(operationLock)
			journalLock, err := lockLeaseJournal(a.Config.StateDir)
			if err != nil {
				return err
			}
			state, err := loadLeaseState(a.Config.StateDir)
			unlockState(journalLock)
			if err != nil {
				return err
			}
			resultPath := filepath.Join(a.Config.StateDir, "commands", "results", id+".json")
			if _, statErr := os.Lstat(resultPath); errors.Is(statErr, os.ErrNotExist) {
				result := Result{ID: id, Command: "queue", CompletedAt: time.Now().UTC().Format(time.RFC3339Nano), Success: false, ExitCode: 2, Outcome: "failure", Message: "queued operation outcome is unknown after daemon restart; operator reconciliation is required", Stderr: "queued operation outcome is unknown after daemon restart; operator reconciliation is required", Metadata: map[string]any{"code": "STATION_OPERATION_RECOVERY_REQUIRED"}}
				if operation, found := state.Operations[id]; found && operation.Result != nil {
					result = *operation.Result
				}
				if err := atomicJSON(resultPath, result); err != nil {
					return err
				}
			} else if statErr != nil {
				return statErr
			}
			processedPath := filepath.Join(a.Config.StateDir, "commands", "processed", entry.Name())
			if _, err := os.Lstat(processedPath); err == nil {
				return fmt.Errorf("cannot reconcile interrupted queue item %q: processed record already exists", id)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return os.Rename(path, processedPath)
		}()
		if errors.Is(err, errLeaseOperationInProgress) {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) runQueue(ctx context.Context) {
	timer := time.NewTicker(750 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if err := a.processQueue(ctx); err != nil {
				a.log("queue-error", map[string]any{"error": err.Error()})
			}
		}
	}
}

func (a *Agent) publish() error {
	status := a.Status()
	if err := atomicJSON(filepath.Join(a.Config.StateDir, "status.json"), status); err != nil {
		return err
	}
	if err := atomicJSON(filepath.Join(a.Config.StateDir, "heartbeat.json"), status); err != nil {
		return err
	}
	a.log("heartbeat-published", map[string]any{"profile": a.Config.Profile, "ready": status["summary"].(map[string]any)["ready"]})
	return nil
}

func (a *Agent) processQueue(ctx context.Context) error {
	inbox := filepath.Join(a.Config.StateDir, "commands", "inbox")
	entries, err := os.ReadDir(inbox)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !ValidOperationID(id) {
			continue
		}
		claim := filepath.Join(a.Config.StateDir, "commands", "processing", entry.Name())
		if err := os.Rename(filepath.Join(inbox, entry.Name()), claim); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return err
		}
		result := a.processQueueRequest(ctx, claim, id)
		resultPath := filepath.Join(a.Config.StateDir, "commands", "results", id+".json")
		if err := atomicJSON(resultPath, result); err != nil {
			return err
		}
		_ = os.Rename(claim, filepath.Join(a.Config.StateDir, "commands", "processed", entry.Name()))
		a.log("queue-command-complete", map[string]any{"operationId": id, "command": result.Command, "exitCode": result.ExitCode, "outcome": result.Outcome})
	}
	return nil
}

func (a *Agent) processQueueRequest(ctx context.Context, path, id string) Result {
	file, err := os.Open(path)
	if err != nil {
		return a.rejected(id, "unable to read queued command")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxDispatcherRequestBytes+1))
	if err != nil || len(data) > maxDispatcherRequestBytes {
		return a.rejected(id, "queued command exceeds dispatcher limit")
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return a.rejected(id, "queued command is invalid")
	}
	var request Request
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&request); err != nil {
		return a.rejected(id, "queued command is invalid")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || request.SchemaVersion != 1 && request.SchemaVersion != 2 || request.Operation != "execute" || request.ID != id {
		return a.rejected(id, "queued command identity is invalid")
	}
	if request.SecretValue != "" || request.SecretID != "" {
		return a.rejected(id, "secrets are not accepted through the queue")
	}
	if request.SchemaVersion == 1 && (request.Command == "prepare-session" || request.Command == "release-session") {
		return leaseErrorResult(request, "STATION_LEASE_REQUIRED", "session lifecycle requires dispatcher protocol 2")
	}
	if request.SchemaVersion == 2 {
		if request.Command == "prepare-session" || request.Command == "release-session" {
			if err := validateV2LeaseRequest(request, time.Now().UTC()); err != nil {
				code := "STATION_LEASE_CONTEXT_REQUIRED"
				var deadlineError *leaseDeadlineError
				if errors.As(err, &deadlineError) {
					code = "STATION_DISPATCHER_DEADLINE_EXPIRED"
				}
				return leaseErrorResult(request, code, "queued lease request is invalid or expired")
			}
			return a.executeLeaseRequest(ctx, request)
		}
		if !isReadOnlyV2Command(request.Command, request.Args) {
			return leaseErrorResult(request, "STATION_LEASE_CONTEXT_REQUIRED", "mutating station command requires an authorized lease")
		}
	}
	return a.Execute(ctx, request.ID, request.Command, request.Args)
}

func (a *Agent) rejected(id, message string) Result {
	return Result{ID: id, Command: "queue", CompletedAt: time.Now().UTC().Format(time.RFC3339Nano), Success: false, ExitCode: 2, Outcome: "failure", Message: message, Stderr: message, Metadata: map[string]any{"code": "STATION_COMMAND_REJECTED"}}
}

func (a *Agent) log(event string, fields map[string]any) {
	fields["event"] = event
	fields["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	fields["host"] = a.Config.Name
	fields["profile"] = a.Config.Profile
	file, err := os.OpenFile(filepath.Join(a.Config.LogDir, "labstation.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		return
	}
	defer file.Close()
	_ = json.NewEncoder(file).Encode(fields)
}
