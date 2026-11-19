//go:build acceptance

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/decentralabs/lab-station-linux/internal/config"
	"github.com/decentralabs/lab-station-linux/internal/host"
)

// Run on a dedicated XRDP Lab Station with exactly one connected LABUSER
// session. Set LABSTATION_XRDP_ACCEPTANCE=1 and run:
// go test -tags acceptance -v ./internal/agent -run '^TestAcceptanceXRDPReconnectCancelsGrace$'
// Disconnect the RDP client only after the test prints its prompt, then
// reconnect before the 120-second grace expires. This test never invokes the
// privileged session-close helper.
func TestAcceptanceXRDPReconnectCancelsGrace(t *testing.T) {
	if configAcceptanceEnabled() != "1" {
		t.Skip("set LABSTATION_XRDP_ACCEPTANCE=1 on the dedicated XRDP station to run acceptance")
	}

	runtime := host.NewRuntime()
	_, remote, sessionsKnown := runtime.Sessions()
	if !sessionsKnown {
		t.Fatal("XRDP acceptance requires a readable loginctl session inventory")
	}
	initialSessionID := acceptanceTinyDeskSessionID(remote)
	if initialSessionID == "" {
		t.Fatal("XRDP acceptance requires exactly one remote LABUSER X11 session")
	}
	connected, connectionKnown := runtime.RDPClientsConnected()
	if !connectionKnown || !connected {
		t.Fatal("XRDP acceptance requires one active XRDP TCP client connection")
	}

	cfg := config.Defaults()
	cfg.Profile = "dedicated"
	cfg.StateDir = filepath.Join(t.TempDir(), "station-state")
	if err := os.MkdirAll(cfg.StateDir, 0o750); err != nil {
		t.Fatalf("create isolated acceptance state: %v", err)
	}
	testAgent := &Agent{Config: cfg, Runtime: runtime}
	closeHelperCalls := 0
	testAgent.runHelper = func(context.Context, []byte) error {
		closeHelperCalls++
		return errors.New("acceptance test deliberately blocks real session termination")
	}

	now := time.Now().UTC()
	if err := saveLeaseState(cfg.StateDir, leaseState{
		Active: &leaseRecord{
			LeaseID: "acceptance-lease", LabID: "acceptance-lab", ReservationKey: "acceptance-reservation",
			Kind: "reservation", Generation: 1, State: "active",
			NotBefore: now.Add(-time.Minute).Format(time.RFC3339Nano),
			ExpiresAt: now.Add(30 * time.Minute).Format(time.RFC3339Nano),
			UpdatedAt: now.Format(time.RFC3339Nano),
		},
	}); err != nil {
		t.Fatalf("prepare isolated acceptance lease: %v", err)
	}
	if err := testAgent.enforceReservationDisconnectClose(context.Background()); err != nil {
		t.Fatalf("connected XRDP baseline failed: %v", err)
	}

	t.Log("Disconnect the XRDP client now, then reconnect before the 120-second grace expires.")
	var disconnectDeadline time.Time
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	timeout := time.NewTimer(15 * time.Minute)
	defer timeout.Stop()
	for {
		select {
		case <-ticker.C:
			connected, connectionKnown = runtime.RDPClientsConnected()
			if !connectionKnown {
				t.Fatal("XRDP client connection state became unknown during acceptance")
			}
			if err := testAgent.enforceReservationDisconnectClose(context.Background()); err != nil {
				t.Fatalf("XRDP disconnect lifecycle poll failed: %v", err)
			}
			watch, err := loadReservationDisconnectWatch(cfg.StateDir)
			if err != nil {
				t.Fatalf("read isolated disconnect watch: %v", err)
			}
			if watch != nil && disconnectDeadline.IsZero() {
				disconnectedAt, parseErr := parseUTC(watch.DisconnectedAt)
				if parseErr != nil {
					t.Fatalf("invalid disconnect watch timestamp: %v", parseErr)
				}
				disconnectDeadline = disconnectedAt.Add(reservationDisconnectCloseGrace - time.Second)
				t.Logf("XRDP disconnect detected; reconnect by %s", disconnectDeadline.Format(time.RFC3339))
			}
			if !disconnectDeadline.IsZero() && connected {
				_, currentRemote, known := runtime.Sessions()
				if !known || !containsAcceptanceTinyDeskSession(currentRemote, initialSessionID) {
					t.Fatal("reconnect did not retain the original Tiny Desk XRDP session")
				}
				if watch != nil {
					t.Fatal("reconnected XRDP session still has a pending disconnect watch")
				}
				if closeHelperCalls != 0 {
					t.Fatal("disconnect grace invoked the real-session-close path before reconnect")
				}
				t.Log("PASS: the same LABUSER XRDP session survived reconnect and its pending close was cancelled")
				return
			}
			if !disconnectDeadline.IsZero() && !time.Now().Before(disconnectDeadline) {
				t.Fatal("LABUSER did not reconnect before the guarded 120-second deadline")
			}
		case <-timeout.C:
			t.Fatal("acceptance timed out before an XRDP disconnect and reconnect were observed")
		}
	}
}

func configAcceptanceEnabled() string {
	return os.Getenv("LABSTATION_XRDP_ACCEPTANCE")
}

func acceptanceTinyDeskSessionID(sessions []map[string]any) string {
	var found string
	for _, session := range sessions {
		if !isTinyDeskRemoteSession(session) {
			continue
		}
		if found != "" {
			return ""
		}
		found = fmt.Sprint(session["id"])
	}
	return found
}

func containsAcceptanceTinyDeskSession(sessions []map[string]any, sessionID string) bool {
	for _, session := range sessions {
		if isTinyDeskRemoteSession(session) && fmt.Sprint(session["id"]) == sessionID {
			return true
		}
	}
	return false
}
