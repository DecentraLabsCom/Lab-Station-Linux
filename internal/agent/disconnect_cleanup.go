package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const reservationDisconnectCloseGrace = time.Duration(tinyDeskDisconnectGraceSeconds) * time.Second
const reservationDisconnectCloseRetry = 25 * time.Second
const reservationDisconnectCloseMaxAttempts = 2

type reservationDisconnectIdentity struct {
	ReservationKey string `json:"reservationKey"`
	LabID          string `json:"labId"`
	LeaseID        string `json:"leaseId"`
	Generation     uint64 `json:"generation"`
}

type reservationDisconnectWatch struct {
	reservationDisconnectIdentity
	DisconnectedAt string `json:"disconnectedAt"`
	AttemptCount   int    `json:"attemptCount,omitempty"`
	LastAttemptAt  string `json:"lastAttemptAt,omitempty"`
}

type reservationDisconnectClose struct {
	reservationDisconnectIdentity
	Success     bool   `json:"success"`
	CompletedAt string `json:"completedAt"`
	Method      string `json:"method"`
}

func (a *Agent) enforceReservationDisconnectClose(ctx context.Context) error {
	if a.Config.Profile == "fmu-only" {
		return nil
	}
	a.disconnectMu.Lock()
	defer a.disconnectMu.Unlock()

	journalLock, err := lockLeaseJournal(a.Config.StateDir)
	if err != nil {
		return err
	}
	state, err := loadLeaseState(a.Config.StateDir)
	unlockState(journalLock)
	if err != nil {
		return err
	}
	if state.Active == nil || state.Active.Kind != "reservation" || state.Active.State != "active" {
		return clearReservationDisconnectWatch(a.Config.StateDir)
	}
	active := *state.Active
	identity := reservationDisconnectIdentity{
		ReservationKey: active.ReservationKey,
		LabID:          active.LabID,
		LeaseID:        active.LeaseID,
		Generation:     active.Generation,
	}
	if identity.ReservationKey == "" || identity.LabID == "" || identity.LeaseID == "" || identity.Generation == 0 {
		return nil
	}
	expiresAt, err := parseUTC(active.ExpiresAt)
	if err != nil || !time.Now().UTC().Before(expiresAt) {
		// The scheduled reservation end path owns cleanup at or after expiry.
		return clearReservationDisconnectWatch(a.Config.StateDir)
	}

	_, remote, sessionsKnown := a.hostRuntime().Sessions()
	if !sessionsKnown {
		return errors.New("session inventory is unavailable")
	}
	tinyDeskSessions := make([]map[string]any, 0, 1)
	for _, session := range remote {
		if isTinyDeskRemoteSession(session) {
			tinyDeskSessions = append(tinyDeskSessions, session)
		}
	}
	watch, err := loadReservationDisconnectWatch(a.Config.StateDir)
	if err != nil {
		return err
	}
	if watch != nil && !sameDisconnectIdentity(watch.reservationDisconnectIdentity, identity) {
		if err := clearReservationDisconnectWatch(a.Config.StateDir); err != nil {
			return err
		}
		watch = nil
	}

	if len(tinyDeskSessions) == 0 {
		if watch == nil {
			return nil
		}
		connected, connectionKnown := a.hostRuntime().RDPClientsConnected()
		if !connectionKnown {
			return errors.New("RDP connection state is unavailable")
		}
		if connected {
			return clearReservationDisconnectWatch(a.Config.StateDir)
		}
		return a.recordReservationDisconnectClose(identity, "session-ended")
	}

	connected, connectionKnown := a.hostRuntime().RDPClientsConnected()
	if !connectionKnown {
		return errors.New("RDP connection state is unavailable")
	}
	if connected {
		return clearReservationDisconnectWatch(a.Config.StateDir)
	}

	now := time.Now().UTC()
	if watch == nil {
		watch = &reservationDisconnectWatch{
			reservationDisconnectIdentity: identity,
			DisconnectedAt:                now.Format(time.RFC3339Nano),
		}
		return saveReservationDisconnectWatch(a.Config.StateDir, watch)
	}
	disconnectedAt, err := parseUTC(watch.DisconnectedAt)
	if err != nil {
		return errors.New("reservation disconnect timestamp is invalid")
	}
	if now.Sub(disconnectedAt) < reservationDisconnectCloseGrace {
		return nil
	}
	if watch.AttemptCount >= reservationDisconnectCloseMaxAttempts {
		return nil
	}
	if watch.AttemptCount > 0 && watch.LastAttemptAt != "" {
		lastAttemptAt, parseErr := parseUTC(watch.LastAttemptAt)
		if parseErr == nil && now.Sub(lastAttemptAt) < reservationDisconnectCloseRetry {
			return nil
		}
	}
	watch.AttemptCount++
	watch.LastAttemptAt = now.Format(time.RFC3339Nano)
	if err := saveReservationDisconnectWatch(a.Config.StateDir, watch); err != nil {
		return err
	}

	closeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	closeErr := a.helper(closeCtx, helperSessionRequest(tinyDeskSessions[0], true, a.Config.Application.CloseTimeoutSeconds, false))
	cancel()
	_, remainingRemote, sessionsKnown := a.hostRuntime().Sessions()
	if !sessionsKnown {
		return errors.New("session inventory became unavailable after Tiny Desk close")
	}
	for _, session := range remainingRemote {
		if isTinyDeskRemoteSession(session) {
			if closeErr != nil {
				return fmt.Errorf("controlled Tiny Desk close failed: %w", closeErr)
			}
			return errors.New("Tiny Desk session remained after controlled close")
		}
	}
	method := "station-controlled"
	if closeErr != nil {
		method = "session-ended"
	}
	return a.recordReservationDisconnectClose(identity, method)
}

func (a *Agent) recordReservationDisconnectClose(identity reservationDisconnectIdentity, method string) error {
	record := reservationDisconnectClose{
		reservationDisconnectIdentity: identity,
		Success:                       true,
		CompletedAt:                   time.Now().UTC().Format(time.RFC3339Nano),
		Method:                        method,
	}
	if err := atomicJSON(filepath.Join(a.Config.StateDir, "reservation-disconnect-close.json"), record); err != nil {
		return err
	}
	return clearReservationDisconnectWatch(a.Config.StateDir)
}

func (a *Agent) reservationDisconnectCloseStatus() map[string]any {
	raw, err := os.ReadFile(filepath.Join(a.Config.StateDir, "reservation-disconnect-close.json"))
	if err != nil {
		return map[string]any{}
	}
	var record reservationDisconnectClose
	if json.Unmarshal(raw, &record) != nil || !record.Success || record.ReservationKey == "" || record.LabID == "" || record.LeaseID == "" || record.Generation == 0 || record.Method == "" {
		return map[string]any{}
	}
	if _, err := parseUTC(record.CompletedAt); err != nil {
		return map[string]any{}
	}
	return map[string]any{
		"reservationKey": record.ReservationKey,
		"labId":          record.LabID,
		"leaseId":        record.LeaseID,
		"generation":     record.Generation,
		"success":        record.Success,
		"completedAt":    record.CompletedAt,
		"method":         record.Method,
	}
}

func sameDisconnectIdentity(left, right reservationDisconnectIdentity) bool {
	return left.ReservationKey == right.ReservationKey && left.LabID == right.LabID && left.LeaseID == right.LeaseID && left.Generation == right.Generation
}

func loadReservationDisconnectWatch(stateDir string) (*reservationDisconnectWatch, error) {
	raw, err := os.ReadFile(filepath.Join(stateDir, "reservation-disconnect-watch.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var watch reservationDisconnectWatch
	if err := json.Unmarshal(raw, &watch); err != nil {
		return nil, errors.New("reservation disconnect watch state is invalid")
	}
	if watch.ReservationKey == "" || watch.LabID == "" || watch.LeaseID == "" || watch.Generation == 0 || watch.DisconnectedAt == "" {
		return nil, errors.New("reservation disconnect watch identity is invalid")
	}
	// Older daemon versions persisted the first attempt timestamp without a
	// counter. Treat that record as an already-started attempt so an upgrade
	// cannot restart the local retry sequence from zero.
	if watch.AttemptCount == 0 && watch.LastAttemptAt != "" {
		watch.AttemptCount = 1
	}
	if watch.AttemptCount < 0 || watch.AttemptCount > reservationDisconnectCloseMaxAttempts {
		return nil, errors.New("reservation disconnect watch attempt count is invalid")
	}
	if watch.AttemptCount > 0 {
		if watch.LastAttemptAt == "" {
			return nil, errors.New("reservation disconnect watch attempt timestamp is missing")
		}
		if _, err := parseUTC(watch.LastAttemptAt); err != nil {
			return nil, errors.New("reservation disconnect watch attempt timestamp is invalid")
		}
	}
	return &watch, nil
}

func saveReservationDisconnectWatch(stateDir string, watch *reservationDisconnectWatch) error {
	return atomicJSON(filepath.Join(stateDir, "reservation-disconnect-watch.json"), watch)
}

func clearReservationDisconnectWatch(stateDir string) error {
	err := os.Remove(filepath.Join(stateDir, "reservation-disconnect-watch.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
