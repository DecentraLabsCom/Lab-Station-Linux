package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type LeaseContext struct {
	Kind           string `json:"kind"`
	LabID          string `json:"labId,omitempty"`
	ReservationKey string `json:"reservationKey,omitempty"`
	LeaseID        string `json:"leaseId"`
	Generation     uint64 `json:"generation,omitempty"`
	NotBefore      string `json:"notBefore"`
	ExpiresAt      string `json:"expiresAt"`
}

type leaseRecord struct {
	LeaseID        string `json:"leaseId"`
	ReservationKey string `json:"reservationKey,omitempty"`
	Kind           string `json:"kind"`
	Generation     uint64 `json:"generation"`
	State          string `json:"state"`
	NotBefore      string `json:"notBefore"`
	ExpiresAt      string `json:"expiresAt"`
	UpdatedAt      string `json:"updatedAt"`
}

type leaseOperation struct {
	PayloadHash string  `json:"payloadHash"`
	State       string  `json:"state"`
	Command     string  `json:"command"`
	LeaseID     string  `json:"leaseId,omitempty"`
	Generation  uint64  `json:"generation,omitempty"`
	UpdatedAt   string  `json:"updatedAt"`
	Result      *Result `json:"result,omitempty"`
}

type leaseState struct {
	NextGeneration uint64                    `json:"nextGeneration"`
	Active         *leaseRecord              `json:"active,omitempty"`
	LastReleased   *leaseRecord              `json:"lastReleased,omitempty"`
	Operations     map[string]leaseOperation `json:"operations"`
}

type leaseDeadlineError struct{ message string }

func (e *leaseDeadlineError) Error() string { return e.message }

var leaseIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
var errLeaseOperationInProgress = errors.New("lease operation is already in progress")

func validateV2LeaseRequest(request Request, now time.Time) error {
	if request.Operation != "execute" || request.Context == nil || request.Command != "prepare-session" && request.Command != "release-session" {
		return errors.New("lease context is required")
	}
	issuedAt, err := parseUTC(request.IssuedAt)
	if err != nil {
		return &leaseDeadlineError{message: "issuedAt is invalid"}
	}
	executeBefore, err := parseUTC(request.ExecuteBefore)
	if err != nil || !executeBefore.After(issuedAt) || executeBefore.Sub(issuedAt) > 5*time.Minute || now.Before(issuedAt) || now.After(executeBefore) {
		return &leaseDeadlineError{message: "request is outside the dispatcher execution window"}
	}
	lease := request.Context
	if !oneOf(lease.Kind, "reservation", "demo") || !leaseIdentifierPattern.MatchString(lease.LeaseID) {
		return errors.New("lease identity is invalid")
	}
	if lease.Kind == "reservation" && (!leaseIdentifierPattern.MatchString(lease.LabID) || !leaseIdentifierPattern.MatchString(lease.ReservationKey)) {
		return errors.New("reservation identity is invalid")
	}
	if lease.Kind == "demo" && !strings.HasPrefix(lease.LeaseID, "demo:") {
		return errors.New("demo lease identity is invalid")
	}
	notBefore, err := parseUTC(lease.NotBefore)
	if err != nil {
		return errors.New("lease notBefore is invalid")
	}
	expiresAt, err := parseUTC(lease.ExpiresAt)
	if err != nil || !expiresAt.After(notBefore) {
		return errors.New("lease expiry is invalid")
	}
	if request.Command == "prepare-session" {
		if lease.Generation != 0 || !expiresAt.After(now) {
			return errors.New("prepare lease generation or expiry is invalid")
		}
	} else if lease.Generation == 0 {
		return errors.New("release lease generation is required")
	}
	return nil
}

// ValidateLeaseRequest exposes the protocol validation for trusted local
// clients such as labstationctl before they persist a queue item.
func ValidateLeaseRequest(request Request, now time.Time) error {
	return validateV2LeaseRequest(request, now)
}

func parseUTC(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	_, offset := parsed.Zone()
	if offset != 0 {
		return time.Time{}, errors.New("timestamp must use UTC")
	}
	return parsed, nil
}

func (a *Agent) executeLeaseRequest(parent context.Context, request Request) Result {
	if !ValidOperationID(request.ID) {
		return leaseErrorResult(request, "STATION_COMMAND_REJECTED", "operation id is invalid")
	}
	operationLock, err := lockLeaseOperation(a.Config.StateDir, request.ID, true)
	if errors.Is(err, errLeaseOperationInProgress) {
		return leaseErrorResult(request, "STATION_OPERATION_IN_PROGRESS", "operation is still being processed")
	}
	if err != nil {
		return leaseErrorResult(request, "STATION_JOURNAL_UNAVAILABLE", "operation journal is unavailable")
	}
	defer unlockState(operationLock)
	lock, err := lockLeaseJournal(a.Config.StateDir)
	if err != nil {
		return leaseErrorResult(request, "STATION_JOURNAL_UNAVAILABLE", "operation journal is unavailable")
	}
	locked := true
	defer func() {
		if locked {
			unlockState(lock)
		}
	}()
	state, err := loadLeaseState(a.Config.StateDir)
	if err != nil {
		return leaseErrorResult(request, "STATION_JOURNAL_UNAVAILABLE", "operation journal is unavailable")
	}
	if state.Operations == nil {
		state.Operations = map[string]leaseOperation{}
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return leaseErrorResult(request, "STATION_COMMAND_REJECTED", "request payload is invalid")
	}
	digest := sha256.Sum256(payload)
	payloadHash := hex.EncodeToString(digest[:])
	if existing, found := state.Operations[request.ID]; found {
		if existing.PayloadHash != payloadHash {
			return leaseErrorResult(request, "STATION_OPERATION_ID_CONFLICT", "operation id was already used with a different request")
		}
		if existing.Result != nil {
			return *existing.Result
		}
		if existing.State == "processing" {
			result := leaseErrorResult(request, "STATION_OPERATION_RECOVERY_REQUIRED", "operation outcome is unknown; operator reconciliation is required")
			existing.State = "recovery-required"
			existing.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			existing.Result = &result
			state.Operations[request.ID] = existing
			if state.Active != nil && state.Active.LeaseID == existing.LeaseID && state.Active.Generation == existing.Generation {
				state.Active.State = "recovery-required"
				state.Active.UpdatedAt = existing.UpdatedAt
			}
			if saveErr := saveLeaseState(a.Config.StateDir, state); saveErr != nil {
				return leaseErrorResult(request, "STATION_JOURNAL_UNAVAILABLE", "operation result could not be recorded")
			}
			return result
		}
		return leaseErrorResult(request, "STATION_OPERATION_IN_PROGRESS", "operation is still being reconciled")
	}
	now := time.Now().UTC()
	if now.After(mustParseUTC(request.ExecuteBefore)) {
		return leaseErrorResult(request, "STATION_DISPATCHER_DEADLINE_EXPIRED", "dispatcher request is outside its execution window")
	}
	lease := request.Context
	var generation uint64
	if request.Command == "prepare-session" {
		if state.Active != nil {
			return leaseErrorResult(request, "STATION_LEASE_CONFLICT", "another lease is active or requires recovery")
		}
		state.NextGeneration++
		generation = state.NextGeneration
		state.Active = &leaseRecord{LeaseID: lease.LeaseID, ReservationKey: lease.ReservationKey, Kind: lease.Kind, Generation: generation, State: "preparing", NotBefore: lease.NotBefore, ExpiresAt: lease.ExpiresAt, UpdatedAt: now.Format(time.RFC3339Nano)}
	} else {
		generation = lease.Generation
		if state.Active == nil && state.LastReleased != nil && state.LastReleased.LeaseID == lease.LeaseID && state.LastReleased.Generation == generation {
			result := leaseSuccessResult(request, generation, "released")
			state.Operations[request.ID] = newLeaseOperation(request, payloadHash, result, generation)
			if saveErr := saveLeaseState(a.Config.StateDir, state); saveErr != nil {
				return leaseErrorResult(request, "STATION_JOURNAL_UNAVAILABLE", "operation result could not be recorded")
			}
			return result
		}
		if state.Active == nil || state.Active.LeaseID != lease.LeaseID || state.Active.Generation != generation || state.Active.State != "active" && state.Active.State != "recovery-required" {
			return leaseErrorResult(request, "STATION_LEASE_STALE", "lease identity or generation is no longer active")
		}
		state.Active.State = "releasing"
		state.Active.UpdatedAt = now.Format(time.RFC3339Nano)
	}
	state.Operations[request.ID] = leaseOperation{PayloadHash: payloadHash, State: "processing", Command: request.Command, LeaseID: lease.LeaseID, Generation: generation, UpdatedAt: now.Format(time.RFC3339Nano)}
	if err := saveLeaseState(a.Config.StateDir, state); err != nil {
		return leaseErrorResult(request, "STATION_JOURNAL_UNAVAILABLE", "operation could not be started")
	}
	executeBefore := mustParseUTC(request.ExecuteBefore)
	deadline := executeBefore
	if request.Command == "prepare-session" {
		if expiresAt := mustParseUTC(lease.ExpiresAt); expiresAt.Before(deadline) {
			deadline = expiresAt
		}
	}
	unlockState(lock)
	locked = false
	if parent.Err() != nil || !time.Now().UTC().Before(deadline) {
		result := leaseErrorResult(request, "STATION_DISPATCHER_DEADLINE_EXPIRED", "dispatcher request expired before execution began")
		lock, err = lockLeaseJournal(a.Config.StateDir)
		if err != nil {
			return leaseErrorResult(request, "STATION_JOURNAL_UNAVAILABLE", "operation result could not be recorded")
		}
		locked = true
		state, err = loadLeaseState(a.Config.StateDir)
		if err != nil {
			return leaseErrorResult(request, "STATION_JOURNAL_UNAVAILABLE", "operation result could not be recorded")
		}
		if state.Active != nil && state.Active.LeaseID == lease.LeaseID && state.Active.Generation == generation {
			if request.Command == "prepare-session" {
				state.Active = nil
			} else {
				state.Active.State = "recovery-required"
				state.Active.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			}
		}
		operation := state.Operations[request.ID]
		operation.State = "completed"
		operation.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		operation.Result = &result
		state.Operations[request.ID] = operation
		if err := saveLeaseState(a.Config.StateDir, state); err != nil {
			return leaseErrorResult(request, "STATION_JOURNAL_UNAVAILABLE", "operation result could not be recorded")
		}
		return result
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	result := a.Execute(ctx, request.ID, request.Command, request.Args)
	cancel()
	result.Metadata["lease"] = map[string]any{"leaseId": lease.LeaseID, "generation": generation, "state": "active", "expiresAt": lease.ExpiresAt}
	lock, err = lockLeaseJournal(a.Config.StateDir)
	if err != nil {
		return leaseErrorResult(request, "STATION_JOURNAL_UNAVAILABLE", "operation result could not be recorded")
	}
	locked = true
	state, err = loadLeaseState(a.Config.StateDir)
	if err != nil {
		return leaseErrorResult(request, "STATION_JOURNAL_UNAVAILABLE", "operation result could not be recorded")
	}
	operation := state.Operations[request.ID]
	operation.State = "completed"
	operation.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	operation.Result = &result
	if request.Command == "prepare-session" {
		if state.Active == nil || state.Active.LeaseID != lease.LeaseID || state.Active.Generation != generation {
			return leaseErrorResult(request, "STATION_JOURNAL_CONFLICT", "lease state changed during prepare")
		}
		if result.ExitCode < 2 {
			state.Active.State = "active"
		} else {
			state.Active.State = "recovery-required"
		}
		state.Active.UpdatedAt = operation.UpdatedAt
		result.Metadata["lease"] = map[string]any{"leaseId": lease.LeaseID, "generation": generation, "state": state.Active.State, "expiresAt": lease.ExpiresAt}
		operation.Result = &result
	} else if result.ExitCode < 2 {
		released := *state.Active
		released.State = "released"
		released.UpdatedAt = operation.UpdatedAt
		state.LastReleased = &released
		state.Active = nil
		result.Metadata["lease"] = map[string]any{"leaseId": lease.LeaseID, "generation": generation, "state": "released", "expiresAt": lease.ExpiresAt}
		operation.Result = &result
	} else if state.Active != nil {
		state.Active.State = "recovery-required"
		state.Active.UpdatedAt = operation.UpdatedAt
	}
	state.Operations[request.ID] = operation
	if err := saveLeaseState(a.Config.StateDir, state); err != nil {
		return leaseErrorResult(request, "STATION_JOURNAL_UNAVAILABLE", "operation result could not be recorded")
	}
	return result
}

func newLeaseOperation(request Request, hash string, result Result, generation uint64) leaseOperation {
	return leaseOperation{PayloadHash: hash, State: "completed", Command: request.Command, LeaseID: request.Context.LeaseID, Generation: generation, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano), Result: &result}
}

func leaseSuccessResult(request Request, generation uint64, state string) Result {
	return Result{ID: request.ID, Command: request.Command, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano), Success: true, ExitCode: 0, Outcome: "success", Message: "lease already released", Stdout: "lease already released", Stderr: "", Metadata: map[string]any{"lease": map[string]any{"leaseId": request.Context.LeaseID, "generation": generation, "state": state, "expiresAt": request.Context.ExpiresAt}}}
}

func leaseErrorResult(request Request, code, message string) Result {
	return Result{ID: request.ID, Command: request.Command, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano), Success: false, ExitCode: 2, Outcome: "failure", Message: message, Stdout: "", Stderr: message, Metadata: map[string]any{"code": code}}
}

func mustParseUTC(value string) time.Time {
	parsed, _ := parseUTC(value)
	return parsed
}

func lockLeaseJournal(stateDir string) (*os.File, error) {
	if err := os.MkdirAll(stateDir, 0750); err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, "lease-journal.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func lockLeaseOperation(stateDir, operationID string, nonblocking bool) (*os.File, error) {
	if !ValidOperationID(operationID) {
		return nil, errors.New("operation id is invalid")
	}
	if err := os.MkdirAll(stateDir, 0750); err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, "operation-"+operationID+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		_ = file.Close()
		return nil, errors.New("operation lock path is unsafe")
	}
	flags := syscall.LOCK_EX
	if nonblocking {
		flags |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(file.Fd()), flags); err != nil {
		_ = file.Close()
		if nonblocking && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)) {
			return nil, errLeaseOperationInProgress
		}
		return nil, err
	}
	return file, nil
}

func loadLeaseState(stateDir string) (leaseState, error) {
	state := leaseState{Operations: map[string]leaseOperation{}}
	raw, err := os.ReadFile(filepath.Join(stateDir, "lease-state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return leaseState{}, err
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return leaseState{}, fmt.Errorf("lease journal is invalid: %w", err)
	}
	if state.Operations == nil {
		state.Operations = map[string]leaseOperation{}
	}
	return state, nil
}

func saveLeaseState(stateDir string, state leaseState) error {
	if state.Operations == nil {
		state.Operations = map[string]leaseOperation{}
	}
	return atomicJSON(filepath.Join(stateDir, "lease-state.json"), state)
}

// CheckLeaseAdmission authorizes only a currently active Tiny Desk lease.
// The helper calls this as root after validating the untrusted stateDir value.
func CheckLeaseAdmission(stateDir string, now time.Time) error {
	clean := filepath.Clean(stateDir)
	root := "/var/lib/decentralabs/lab-station"
	relative, err := filepath.Rel(root, clean)
	if err != nil || !filepath.IsAbs(clean) || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("lease state directory is outside the managed station directory")
	}
	if err := validateLeaseStatePath(root, clean); err != nil {
		return err
	}
	state, err := loadLeaseState(clean)
	if err != nil {
		return errors.New("lease state is unavailable")
	}
	if state.Active == nil || state.Active.State != "active" {
		return errors.New("no active lease is available")
	}
	if err := checkLeaseWindow(*state.Active, now); err != nil {
		return err
	}
	return nil
}

func checkLeaseWindow(lease leaseRecord, now time.Time) error {
	notBefore, err := parseUTC(lease.NotBefore)
	if err != nil || now.Before(notBefore) {
		return errors.New("lease admission has not started")
	}
	expiresAt, err := parseUTC(lease.ExpiresAt)
	if err != nil || !now.Before(expiresAt) {
		return errors.New("lease has expired")
	}
	return nil
}

func validateLeaseStatePath(root, stateDir string) error {
	for current := root; ; {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0002 != 0 {
			return errors.New("lease state directory permissions are unsafe")
		}
		if current == stateDir {
			break
		}
		relative, err := filepath.Rel(current, stateDir)
		if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == ".." {
			return errors.New("lease state directory is invalid")
		}
		part := strings.Split(relative, string(filepath.Separator))[0]
		current = filepath.Join(current, part)
	}
	path := filepath.Join(stateDir, "lease-state.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return errors.New("lease state file permissions are unsafe")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	daemon, daemonErr := user.Lookup("labstationd")
	if daemonErr != nil {
		return errors.New("lease state owner is unavailable")
	}
	daemonUID, parseErr := strconv.ParseUint(daemon.Uid, 10, 32)
	group, groupErr := user.LookupGroup("labstation")
	groupID, groupParseErr := strconv.ParseUint(func() string {
		if group == nil {
			return "-1"
		}
		return group.Gid
	}(), 10, 32)
	if !ok || parseErr != nil || groupErr != nil || groupParseErr != nil || stat.Uid != 0 && stat.Uid != uint32(daemonUID) || stat.Gid != uint32(groupID) {
		return errors.New("lease state file owner is invalid")
	}
	return nil
}

func (a *Agent) enforceLeaseExpiry(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	lock, err := lockLeaseJournal(a.Config.StateDir)
	if err != nil {
		return err
	}
	state, err := loadLeaseState(a.Config.StateDir)
	if err != nil {
		unlockState(lock)
		return err
	}
	if state.Active == nil {
		unlockState(lock)
		return nil
	}
	expiresAt, parseErr := parseUTC(state.Active.ExpiresAt)
	if parseErr != nil {
		state.Active.State = "recovery-required"
		state.Active.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		saveErr := saveLeaseState(a.Config.StateDir, state)
		unlockState(lock)
		if saveErr != nil {
			return saveErr
		}
		return errors.New("active lease expiry is invalid; admission is closed")
	}
	if time.Now().UTC().Before(expiresAt) {
		unlockState(lock)
		return nil
	}
	for _, operation := range state.Operations {
		if operation.LeaseID == state.Active.LeaseID && operation.Generation == state.Active.Generation && operation.State == "processing" {
			unlockState(lock)
			return nil
		}
	}
	active := *state.Active
	state.Active.State = "expiring"
	state.Active.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := saveLeaseState(a.Config.StateDir, state); err != nil {
		unlockState(lock)
		return err
	}
	unlockState(lock)

	result := a.Execute(ctx, fmt.Sprintf("lease-expiry-%d", active.Generation), "release-session", nil)
	lock, err = lockLeaseJournal(a.Config.StateDir)
	if err != nil {
		return err
	}
	defer unlockState(lock)
	state, err = loadLeaseState(a.Config.StateDir)
	if err != nil {
		return err
	}
	if state.Active == nil || state.Active.LeaseID != active.LeaseID || state.Active.Generation != active.Generation {
		return nil
	}
	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	if result.ExitCode < 2 {
		active.State = "expired"
		active.UpdatedAt = updatedAt
		state.LastReleased = &active
		state.Active = nil
	} else {
		state.Active.State = "recovery-required"
		state.Active.UpdatedAt = updatedAt
	}
	if err := saveLeaseState(a.Config.StateDir, state); err != nil {
		return err
	}
	a.appendEvent(map[string]any{"kind": "lease-expired", "timestamp": updatedAt, "leaseId": active.LeaseID, "generation": active.Generation, "cleanupExitCode": result.ExitCode})
	return nil
}
