package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/decentralabs/lab-station-linux/internal/config"
	"github.com/decentralabs/lab-station-linux/internal/host"
)

type fakeSupervisor struct {
	name   string
	states map[string]string
	err    error
}

func (f fakeSupervisor) Name() string    { return f.name }
func (f fakeSupervisor) Available() bool { return true }
func (f fakeSupervisor) ServiceState(unit string) (string, error) {
	if f.err != nil {
		return "unknown", f.err
	}
	if state, ok := f.states[unit]; ok {
		return state, nil
	}
	return "inactive", nil
}
func (f fakeSupervisor) ServiceAction(string, string) error { return f.err }

type fakeRuntime struct {
	supervisor           host.Supervisor
	interfaces           []map[string]string
	local                []map[string]any
	sessionSnapshots     [][]map[string]any
	sessionSnapshotIndex int
	remoteSnapshots      [][]map[string]any
	remoteSnapshotIndex  int
	remote               []map[string]any
	sessionsOK           bool
	wakeOK               bool
	wakeOn               bool
	wakeSaved            bool
	hibernate            bool
	notifyErr            error
	notices              []string
	confirmationErr      error
	confirmations        []string
	notifyStarted        chan struct{}
	notifyRelease        <-chan struct{}
}

func (f *fakeRuntime) Supervisor() host.Supervisor            { return f.supervisor }
func (f *fakeRuntime) NetworkInterfaces() []map[string]string { return f.interfaces }
func (f *fakeRuntime) Sessions() ([]map[string]any, []map[string]any, bool) {
	remote := f.remote
	if len(f.remoteSnapshots) > 0 {
		index := f.remoteSnapshotIndex
		if index >= len(f.remoteSnapshots) {
			index = len(f.remoteSnapshots) - 1
		} else if f.remoteSnapshotIndex < len(f.remoteSnapshots)-1 {
			f.remoteSnapshotIndex++
		}
		remote = f.remoteSnapshots[index]
	}
	if len(f.sessionSnapshots) > 0 {
		index := f.sessionSnapshotIndex
		if index >= len(f.sessionSnapshots) {
			index = len(f.sessionSnapshots) - 1
		} else if f.sessionSnapshotIndex < len(f.sessionSnapshots)-1 {
			f.sessionSnapshotIndex++
		}
		return append([]map[string]any(nil), f.sessionSnapshots[index]...), append([]map[string]any(nil), remote...), f.sessionsOK
	}
	return append([]map[string]any(nil), f.local...), append([]map[string]any(nil), remote...), f.sessionsOK
}
func (f *fakeRuntime) RDPClientsConnected() (bool, bool) {
	return false, false
}
func (f *fakeRuntime) WakeStatus([]map[string]string) (bool, bool) { return f.wakeOK, f.wakeOn }
func (f *fakeRuntime) WakePersistent([]map[string]string) bool     { return f.wakeSaved }
func (f *fakeRuntime) EnergyStatus() host.EnergyStatus {
	return host.EnergyStatus{SuspendStates: []string{"mem", "disk"}, ActiveSwap: true, HibernateSupported: f.hibernate, Supervisor: f.supervisor.Name(), InhibitorsAvailable: true, ActiveInhibitors: 1, AutomaticSuspendPolicy: "managed"}
}
func (f *fakeRuntime) KernelRelease() string              { return "test-kernel" }
func (f *fakeRuntime) LocalGraphicsPresent() bool         { return true }
func (f *fakeRuntime) UserPasswordConfigured(string) bool { return true }
func (f *fakeRuntime) NotifyLocalUsers(message string) error {
	f.notices = append(f.notices, message)
	if f.notifyStarted != nil {
		select {
		case f.notifyStarted <- struct{}{}:
		default:
		}
	}
	if f.notifyRelease != nil {
		<-f.notifyRelease
	}
	return f.notifyErr
}
func (f *fakeRuntime) HibernateSupported() bool { return f.hibernate }

func newTestAgent(t *testing.T, profile string, runtime *fakeRuntime) (*Agent, *[]map[string]any) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Name = "station-test"
	cfg.Profile = profile
	cfg.GuardGraceSeconds = 0
	stationRoot := t.TempDir()
	cfg.StateDir = filepath.Join(stationRoot, "data")
	if err := os.MkdirAll(cfg.StateDir, 0o750); err != nil {
		t.Fatal(err)
	}
	workHome := filepath.Join(stationRoot, "tiny-desk-home")
	if err := os.MkdirAll(workHome, 0o700); err != nil {
		t.Fatal(err)
	}
	calls := &[]map[string]any{}
	a := &Agent{Config: cfg, Runtime: runtime}
	a.cleanWorkData = func() error { return cleanTinyDeskWorkingDataAt(workHome, stationRoot) }
	a.runHelper = func(_ context.Context, raw []byte) error {
		var request map[string]any
		if err := json.Unmarshal(raw, &request); err != nil {
			return err
		}
		*calls = append(*calls, request)
		if request["operation"] == "confirm-local-session" {
			runtime.confirmations = append(runtime.confirmations, fmt.Sprint(request["sessionId"]))
			return runtime.confirmationErr
		}
		if request["operation"] == "terminate-session" {
			if sessionID, ok := request["sessionId"].(string); ok {
				runtime.local = removeSessionByID(runtime.local, sessionID)
				runtime.remote = removeSessionByID(runtime.remote, sessionID)
			}
		}
		return nil
	}
	return a, calls
}

func removeSessionByID(sessions []map[string]any, id string) []map[string]any {
	remaining := sessions[:0]
	for _, session := range sessions {
		if session["id"] != id {
			remaining = append(remaining, session)
		}
	}
	return remaining
}

func TestTinyDeskCloseCoordinatesConfiguredForSingleAndPrimaryMultiAppOnly(t *testing.T) {
	if tinyDeskCloseCoordinatesConfigured(config.Defaults()) {
		t.Fatal("default single-app configuration unexpectedly has close coordinates")
	}

	single := config.Defaults()
	single.Application.CloseCoordinates = []int{120, 240}
	if !tinyDeskCloseCoordinatesConfigured(single) {
		t.Fatal("single-app close coordinates were not detected")
	}

	multi := config.Defaults()
	multi.Applications = []config.ApplicationLaunch{
		{ID: "primary", Command: "/opt/lab/apps/primary", CloseCoordinates: []int{120, 240}},
		{ID: "secondary", Command: "/opt/lab/apps/secondary"},
	}
	if !tinyDeskCloseCoordinatesConfigured(multi) {
		t.Fatal("primary multi-app close coordinates were not detected")
	}
	multi.Applications[0].CloseCoordinates = nil
	multi.Applications[1].CloseCoordinates = []int{20, 30}
	if tinyDeskCloseCoordinatesConfigured(multi) {
		t.Fatal("secondary app coordinates were treated as workspace close coordinates")
	}
}

func TestValidateCommandAllowlistAndInjectionBoundary(t *testing.T) {
	allowed := []struct {
		command string
		args    []string
	}{
		{"identity", nil},
		{"status-json", nil},
		{"prepare-session", []string{"--guard-grace=0", "--guard-notify=false"}},
		{"release-session", []string{"--reboot=false"}},
		{"session guard", []string{"--user=instructor_1", "--no-guard"}},
		{"power", []string{"shutdown", "--require-wake=false"}},
		{"recovery reboot-if-needed", []string{"--force", "--timeout=10"}},
		{"local-mode", []string{"set", "--ttl=60"}},
		{"service", []string{"status"}},
		{"fmu-executor", []string{"restart"}},
	}
	for _, test := range allowed {
		t.Run(test.command+"/allowed", func(t *testing.T) {
			if err := ValidateCommand(test.command, test.args); err != nil {
				t.Fatalf("allowed command rejected: %v", err)
			}
		})
	}

	denied := []struct {
		command string
		args    []string
	}{
		{"bash", []string{"-c", "id"}},
		{"status-json", []string{"--anything"}},
		{"prepare-session", []string{"--unknown"}},
		{"prepare-session", []string{"--guard-message=hello;id"}},
		{"release-session", []string{"--reboot=true\n"}},
		{"session guard", []string{"--user=../root"}},
		{"power", []string{"reboot"}},
		{"power", []string{"shutdown", "--delay=1;id"}},
		{"local-mode", []string{"set", "--ttl=59"}},
		{"local-mode", []string{"set", "--ttl=86401"}},
		{"service", []string{"restart"}},
		{"fmu-executor", []string{"exec", "--", "id"}},
	}
	for _, test := range denied {
		t.Run(test.command+"/denied", func(t *testing.T) {
			if err := ValidateCommand(test.command, test.args); err == nil {
				t.Fatalf("unsafe command accepted: %q %#v", test.command, test.args)
			}
		})
	}
	if err := ValidateCommand("prepare-session", make([]string, 13)); err == nil {
		t.Fatal("accepted more than the global argument limit")
	}
}

func TestOperationBudgetsAreBoundedByCommandClass(t *testing.T) {
	for _, test := range []struct {
		command string
		args    []string
		want    time.Duration
	}{
		{"identity", nil, 15 * time.Second},
		{"prepare-session", nil, 150 * time.Second},
		{"session guard", nil, 150 * time.Second},
		{"release-session", nil, 90 * time.Second},
		{"fmu-executor", []string{"status"}, 15 * time.Second},
		{"fmu-executor", []string{"restart"}, 90 * time.Second},
		{"power", []string{"reboot"}, 90 * time.Second},
	} {
		if got := operationBudget(test.command, test.args); got != test.want {
			t.Errorf("operationBudget(%q, %v) = %v, want %v", test.command, test.args, got, test.want)
		}
	}
}

func TestExecutePropagatesCancellationToPrivilegedHelper(t *testing.T) {
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}}
	a, _ := newTestAgent(t, "fmu-only", runtime)
	a.runHelper = func(ctx context.Context, _ []byte) error {
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := a.Execute(ctx, "cancelled-fmu-action", "fmu-executor", []string{"restart"})
	if result.ExitCode != 2 || result.Metadata["backend"] != "systemd" {
		t.Fatalf("cancelled helper operation did not return a bounded failure: %#v", result)
	}
}

func TestSessionGuardOptionsValidateValuesAndTargeting(t *testing.T) {
	options, err := parseSessionGuardOptions([]string{"--guard-grace=5", "--guard-message=Save now", "--guard-notify=false", "--no-guard=false"}, 30, true)
	if err != nil {
		t.Fatal(err)
	}
	if options.GraceSeconds != 5 || options.Message != "Save now" || options.Notify || options.NoGuard {
		t.Fatalf("unexpected parsed options: %#v", options)
	}
	for _, args := range [][]string{
		{"--guard-grace=91"}, {"--guard-grace=-1"}, {"--guard-message="},
		{"--guard-notify=maybe"}, {"--no-guard=maybe"}, {"--user=student"},
	} {
		if _, err := parseSessionGuardOptions(args, 30, false); err == nil {
			t.Errorf("prepare-session accepted invalid options %#v", args)
		}
	}
	if _, err := parseSessionGuardOptions([]string{"--user=bad user"}, 30, true); err == nil {
		t.Fatal("accepted a username with shell whitespace")
	}
}

func TestLocalModeSetStatusExpiryAndClear(t *testing.T) {
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, sessionsOK: true}
	a, _ := newTestAgent(t, "hybrid", runtime)

	set := a.Execute(context.Background(), "set-1", "local-mode", []string{"set", "--ttl=60"})
	if set.ExitCode != 0 {
		t.Fatalf("set failed: %#v", set)
	}
	status := a.Execute(context.Background(), "status-1", "local-mode", []string{"status"})
	var value map[string]any
	if err := json.Unmarshal([]byte(status.Stdout), &value); err != nil {
		t.Fatal(err)
	}
	if value["enabled"] != true {
		t.Fatalf("local mode should be active: %#v", value)
	}
	expires, err := time.Parse(time.RFC3339Nano, value["expiresAt"].(string))
	if err != nil || time.Until(expires) < 55*time.Second || time.Until(expires) > 65*time.Second {
		t.Fatalf("local mode expiry outside requested TTL: %v (%v)", expires, err)
	}
	cleared := a.Execute(context.Background(), "clear-1", "local-mode", []string{"clear"})
	if cleared.ExitCode != 0 || a.localModeEnabled() {
		t.Fatalf("clear did not disable local mode: result=%#v", cleared)
	}
}

func TestLocalModeLegacyMarkerExpiresAfterEightHours(t *testing.T) {
	created := time.Now().UTC().Add(-9 * time.Hour)
	if got := localModeExpiry([]byte(created.Format(time.RFC3339Nano))); !got.Equal(created.Add(8 * time.Hour)) {
		t.Fatalf("legacy marker expires at %v, want %v", got, created.Add(8*time.Hour))
	}
	if got := localModeExpiry([]byte(`{"expiresAt":"invalid"}`)); !got.IsZero() {
		t.Fatalf("malformed marker should not manufacture an expiry: %v", got)
	}
}

func TestLocalModeMarkerPolicyMatchesSharedMatrix(t *testing.T) {
	matrix := loadPortableParityMatrix(t)
	policy := matrix.LocalMode
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, sessionsOK: true}
	a, _ := newTestAgent(t, "hybrid", runtime)
	markerPath := filepath.Join(a.Config.StateDir, "local-mode.flag")

	if err := os.WriteFile(markerPath, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if policy.MalformedLegacyMarker != "active-until-explicit-clear" || !a.localModeEnabled() {
		t.Fatal("malformed local-mode marker must remain active until explicitly cleared")
	}

	expired := time.Now().UTC().Add(-9 * time.Hour).Format(time.RFC3339Nano)
	if err := os.WriteFile(markerPath, []byte(expired), 0o600); err != nil {
		t.Fatal(err)
	}
	if policy.ExpiredLegacyTimestamp != "inactive" || a.localModeEnabled() {
		t.Fatal("expired legacy local-mode timestamp must be cleared")
	}
	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("expired marker was not removed: %v", err)
	}
}

func TestStatusSummaryTracksPrimaryCapability(t *testing.T) {
	matrix := loadPortableParityMatrix(t)
	for _, profile := range matrix.PortableStatus.Profiles {
		t.Run(profile, func(t *testing.T) {
			runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, sessionsOK: true}
			a, _ := newTestAgent(t, profile, runtime)
			raw, err := json.Marshal(a.Status())
			if err != nil {
				t.Fatal(err)
			}
			var status map[string]any
			if err := json.Unmarshal(raw, &status); err != nil {
				t.Fatal(err)
			}
			assertPortableStatusContract(t, status, "linux", matrix)
		})
	}
}

func TestStatusLocalSessionIncludesLABUSERPhysicalConsole(t *testing.T) {
	runtime := &fakeRuntime{
		supervisor: fakeSupervisor{name: "systemd"},
		local:      []map[string]any{{"id": "console-1", "user": "labuser", "kind": "local", "active": true}},
		sessionsOK: true,
	}
	a, _ := newTestAgent(t, "dedicated", runtime)
	status := a.Status()
	sessions, ok := status["sessions"].(map[string]any)
	if !ok || sessions["localSessionActive"] != true {
		t.Fatalf("a physical LABUSER console session must be reported as local: %#v", status["sessions"])
	}
}

func TestPrepareLocalSessionPolicyMatchesSharedMatrix(t *testing.T) {
	matrix := loadPortableParityMatrix(t)
	policy := matrix.SessionSafety.PrepareDecision
	cases := []struct {
		name, profile, expected string
		count                   int
		allowEviction           bool
	}{
		{name: "no-local-session", profile: "hybrid", expected: policy["noLocalSession"]},
		{name: "dedicated-warns-and-evicts-local-user", profile: "dedicated", count: 1, expected: policy["dedicatedLocalSession"]},
		{name: "hybrid-protects-unless-authorized", profile: "hybrid", count: 1, expected: policy["hybridLocalSessionWithoutAuthorization"]},
		{name: "hybrid-evicts-after-authorization", profile: "hybrid", count: 1, allowEviction: true, expected: policy["hybridLocalSessionWithAuthorization"]},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := localSessionPrepareDecision(testCase.profile, testCase.count, testCase.allowEviction); got != testCase.expected {
				t.Fatalf("prepare decision = %q, want shared matrix value %q", got, testCase.expected)
			}
		})
	}
}

func TestPrepareUsesDedicatedHandoffAndHybridAuthorizationPolicies(t *testing.T) {
	matrix := loadPortableParityMatrix(t)
	if matrix.SessionSafety.LocalSession != "profile-specific" {
		t.Fatalf("shared local-session policy = %q", matrix.SessionSafety.LocalSession)
	}
	instructor := map[string]any{"id": "local-1", "user": "instructor", "kind": "local"}
	dedicatedRuntime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, local: []map[string]any{instructor}, sessionsOK: true}
	dedicated, calls := newTestAgent(t, "dedicated", dedicatedRuntime)
	var graceSeconds int
	dedicated.waitGraceFn = func(_ context.Context, seconds int) error { graceSeconds = seconds; return nil }
	result := dedicated.Execute(context.Background(), "prepare-dedicated", "prepare-session", nil)
	if result.ExitCode != 0 || result.Metadata["releasedSessions"] != 1 || graceSeconds != 30 || len(dedicatedRuntime.notices) != 1 || len(dedicatedRuntime.confirmations) != 0 || len(*calls) != 1 || (*calls)[0]["operation"] != "terminate-session" {
		t.Fatalf("dedicated Prepare must notify, wait, and hand off without a prompt: result=%#v grace=%d notices=%#v confirmations=%#v helper=%#v", result, graceSeconds, dedicatedRuntime.notices, dedicatedRuntime.confirmations, *calls)
	}

	hybridRuntime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, local: []map[string]any{instructor}, sessionsOK: true}
	hybrid, _ := newTestAgent(t, "hybrid", hybridRuntime)
	hybrid.Config.AllowLocalSessionEviction = false
	result = hybrid.Execute(context.Background(), "prepare-hybrid", "prepare-session", nil)
	if result.ExitCode != 2 || result.Metadata["code"] != "STATION_LOCAL_SESSION_ACTIVE" {
		t.Fatalf("hybrid station should protect instructor session unless eviction was opted in: %#v", result)
	}
}

func TestPrepareRequiresLocalSessionAcknowledgementBeforeEviction(t *testing.T) {
	if policy := loadPortableParityMatrix(t).SessionSafety.LocalEvictionAcknowledgement; policy != "required-for-hybrid-only" {
		t.Fatalf("shared local-session acknowledgement policy = %q", policy)
	}
	teacher := map[string]any{"id": "seat-7", "user": "teacher", "remote": false, "seat": "seat0", "type": "wayland", "kind": "local"}
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, local: []map[string]any{teacher}, sessionsOK: true, confirmationErr: errors.New("instructor declined")}
	a, calls := newTestAgent(t, "hybrid", runtime)
	a.Config.AllowLocalSessionEviction = true
	cleaned := false
	a.cleanWorkData = func() error { cleaned = true; return nil }

	result := a.Execute(context.Background(), "prepare-needs-ack", "prepare-session", nil)
	if result.ExitCode != 2 || result.Metadata["code"] != "STATION_SESSION_ACKNOWLEDGEMENT_REQUIRED" || result.Metadata["retryable"] != true {
		t.Fatalf("Prepare must fail retryably when the instructor does not acknowledge: %#v", result)
	}
	if len(runtime.confirmations) != 1 || runtime.confirmations[0] != "seat-7" || len(*calls) != 1 || (*calls)[0]["operation"] != "confirm-local-session" {
		t.Fatalf("Prepare must request acknowledgement for the local session and stop before logoff: confirmation=%#v helper=%#v", runtime.confirmations, *calls)
	}
	if cleaned || len(runtime.local) != 1 {
		t.Fatalf("Prepare changed session/workspace state without acknowledgement: cleaned=%v local=%#v", cleaned, runtime.local)
	}
}

func TestPrepareEvictsOnlyAfterLocalSessionAcknowledgementAndGrace(t *testing.T) {
	teacher := map[string]any{"id": "seat-8", "user": "teacher", "remote": false, "seat": "seat0", "type": "x11", "kind": "local"}
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, local: []map[string]any{teacher}, sessionsOK: true}
	a, calls := newTestAgent(t, "hybrid", runtime)
	a.Config.AllowLocalSessionEviction = true
	var graceSeconds int
	a.waitGraceFn = func(_ context.Context, seconds int) error { graceSeconds = seconds; return nil }

	result := a.Execute(context.Background(), "prepare-after-ack", "prepare-session", nil)
	if result.ExitCode != 0 || result.Metadata["acknowledgedSessions"] != 1 {
		t.Fatalf("Prepare did not record the instructor acknowledgement: %#v", result)
	}
	if graceSeconds != 30 || len(runtime.confirmations) != 1 || len(*calls) != 2 || (*calls)[0]["operation"] != "confirm-local-session" || (*calls)[1]["operation"] != "terminate-session" {
		t.Fatalf("Prepare must acknowledge, wait the minimum grace, then evict: grace=%d confirmations=%#v helper=%#v", graceSeconds, runtime.confirmations, *calls)
	}
}

func TestSameSessionSnapshotAllowsVoluntaryLogoutButRejectsNewOrChangedSessions(t *testing.T) {
	teacher := map[string]any{"id": "local-1", "user": "teacher", "remote": false, "seat": "seat0", "type": "wayland"}
	student := map[string]any{"id": "local-2", "user": "student", "remote": false, "seat": "seat0", "type": "wayland"}
	newUser := map[string]any{"id": "local-3", "user": "new-user", "remote": false, "seat": "seat0", "type": "wayland"}
	changedTeacher := map[string]any{"id": "local-1", "user": "teacher", "remote": true, "seat": "seat0", "type": "wayland"}

	if !sameSessionSnapshot([]map[string]any{teacher, student}, []map[string]any{student}) {
		t.Fatal("a user voluntarily signing out during the warning period should be accepted")
	}
	if sameSessionSnapshot([]map[string]any{teacher}, []map[string]any{teacher, newUser}) {
		t.Fatal("a new session during the warning period should be rejected")
	}
	if sameSessionSnapshot([]map[string]any{teacher}, []map[string]any{changedTeacher}) {
		t.Fatal("a changed session identity should be rejected")
	}
}

func TestPrepareContinuesWhenInstructorSignsOutDuringGuard(t *testing.T) {
	teacher := map[string]any{"id": "local-1", "user": "teacher", "kind": "local"}
	runtime := &fakeRuntime{
		supervisor:       fakeSupervisor{name: "systemd"},
		sessionSnapshots: [][]map[string]any{{teacher}, {}, {}},
		sessionsOK:       true,
	}
	a, calls := newTestAgent(t, "hybrid", runtime)
	a.Config.AllowLocalSessionEviction = true

	result := a.Execute(context.Background(), "prepare-voluntary-signout", "prepare-session", []string{"--no-guard"})

	if result.ExitCode != 0 || result.Metadata["releasedSessions"] != 0 || len(*calls) != 0 {
		t.Fatalf("a voluntary instructor sign-out should complete without a forced termination: result=%#v helper=%#v", result, *calls)
	}
}

func TestPrepareSessionFailsClosedWhenSessionInventoryUnavailable(t *testing.T) {
	matrix := loadPortableParityMatrix(t)
	if matrix.SessionSafety.UnavailableInventory != "fail-closed" {
		t.Fatalf("shared session-inventory policy = %q", matrix.SessionSafety.UnavailableInventory)
	}
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, sessionsOK: false}
	a, _ := newTestAgent(t, "hybrid", runtime)
	result := a.Execute(context.Background(), "prepare", "prepare-session", nil)
	if result.ExitCode != 2 || result.Metadata["code"] != "STATION_SESSION_QUERY_FAILED" {
		t.Fatalf("preparation should fail closed: %#v", result)
	}
}

func TestManualPrepareRefusesRemoteLabUserSessionBeforeCleanup(t *testing.T) {
	runtime := &fakeRuntime{
		supervisor: fakeSupervisor{name: "systemd"},
		remote: []map[string]any{{
			"id":     "xrdp-session-1",
			"user":   "labuser",
			"type":   "x11",
			"seat":   "",
			"remote": false,
			"kind":   "service",
		}},
		sessionsOK: true,
	}
	a, calls := newTestAgent(t, "hybrid", runtime)

	result := a.ExecuteOperator(context.Background(), "operator-prepare", "prepare-session", nil)

	if result.ExitCode != 2 || result.Metadata["code"] != "STATION_REMOTE_SESSION_ACTIVE" {
		t.Fatalf("manual prepare did not refuse the active Tiny Desk session: %#v", result)
	}
	if !strings.Contains(result.Message, "Downloads") || !strings.Contains(result.Message, "no cleanup ran") {
		t.Fatalf("manual prepare did not explain its cleanup effect: %#v", result)
	}
	if len(*calls) != 0 {
		t.Fatalf("manual prepare changed files or closed Tiny Desk while the session remained active: %#v", *calls)
	}
}

func TestPrepareRunsTinyDeskCleanupThroughPrivilegedHelper(t *testing.T) {
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, sessionsOK: true}
	a, calls := newTestAgent(t, "hybrid", runtime)
	a.cleanWorkData = nil

	result := a.Execute(context.Background(), "prepare-cleanup", "prepare-session", nil)
	if result.ExitCode != 0 {
		t.Fatalf("prepare failed while exercising Tiny Desk cleanup routing: %#v", result)
	}
	if len(*calls) != 1 || (*calls)[0]["operation"] != "tiny-desk-clean" {
		t.Fatalf("Tiny Desk cleanup did not use the privileged helper: %#v", *calls)
	}
}

func TestManualPrepareRechecksRemoteLabUserBeforeCleanup(t *testing.T) {
	remoteLabUser := map[string]any{
		"id":     "xrdp-session-1",
		"user":   "labuser",
		"type":   "x11",
		"seat":   "",
		"remote": false,
		"kind":   "service",
	}
	runtime := &fakeRuntime{
		supervisor:      fakeSupervisor{name: "systemd"},
		remoteSnapshots: [][]map[string]any{{}, {remoteLabUser}},
		sessionsOK:      true,
	}
	a, calls := newTestAgent(t, "hybrid", runtime)

	result := a.ExecuteOperator(context.Background(), "operator-prepare-race", "prepare-session", nil)

	if result.ExitCode != 2 || result.Metadata["code"] != "STATION_REMOTE_SESSION_ACTIVE" {
		t.Fatalf("manual prepare did not detect the session before cleanup: %#v", result)
	}
	if !result.Metadata["cleanupSkipped"].(bool) || len(*calls) != 0 {
		t.Fatalf("the newly connected LABUSER session must prevent all cleanup: result=%#v helper=%#v", result, *calls)
	}
}

func TestLeasePrepareRequiresGatewayAuthorizationForRemoteLabUserHandoff(t *testing.T) {
	runtime := &fakeRuntime{
		supervisor: fakeSupervisor{name: "systemd"},
		remote: []map[string]any{{
			"id":     "xrdp-session-1",
			"user":   "labuser",
			"type":   "x11",
			"seat":   "",
			"remote": false,
			"kind":   "service",
		}},
		sessionsOK: true,
	}
	a, calls := newTestAgent(t, "dedicated", runtime)
	var graceSeconds int
	a.waitGraceFn = func(_ context.Context, seconds int) error { graceSeconds = seconds; return nil }
	now := time.Now().UTC()
	request := v2LeaseRequest("reservation-prepare", "prepare-session", "lease-prepare-remote", 0, now, now.Add(time.Minute), now.Add(time.Hour))
	refused := dispatchTestRequest(t, a, request)
	if refused.ExitCode != 2 || refused.Metadata["code"] != "STATION_REMOTE_SESSION_ACTIVE" || len(*calls) != 0 {
		t.Fatalf("lease Prepare must fail closed without Gateway proof that the remote session is unreserved: %#v helper=%#v", refused, *calls)
	}

	authorized := strings.Replace(request, `"args":[]`, `"args":["--unreserved-remote-labuser-sessions=xrdp-session-1"]`, 1)
	result := dispatchTestRequest(t, a, authorized)
	if result.ExitCode != 0 || graceSeconds != 30 || len(runtime.notices) != 1 || len(runtime.confirmations) != 0 || len(*calls) != 1 || (*calls)[0]["operation"] != "terminate-session" || (*calls)[0]["sessionId"] != "xrdp-session-1" {
		t.Fatalf("authorized Dedicated reservation Prepare must warn and hand off the exact unreserved session without a prompt: result=%#v grace=%d notices=%#v confirmations=%#v helper=%#v", result, graceSeconds, runtime.notices, runtime.confirmations, *calls)
	}
}

func TestPrepareCleanupFailureIsARecoveryRequiredFailure(t *testing.T) {
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, sessionsOK: true}
	a, _ := newTestAgent(t, "hybrid", runtime)
	a.cleanWorkData = func() error { return errors.New("permission denied") }

	result := a.Execute(context.Background(), "prepare-cleanup-fails", "prepare-session", nil)

	if result.ExitCode != 2 || result.Metadata["code"] != "STATION_PREPARE_CLEANUP_FAILED" {
		t.Fatalf("incomplete workspace cleanup must not be reported as a successful warning: %#v", result)
	}
}

func TestPrepareCancellationDuringCleanupCannotSucceed(t *testing.T) {
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, sessionsOK: true}
	a, _ := newTestAgent(t, "hybrid", runtime)
	ctx, cancel := context.WithCancel(context.Background())
	a.cleanWorkData = func() error {
		cancel()
		return context.Canceled
	}

	result := a.Execute(ctx, "prepare-cancelled", "prepare-session", nil)
	if result.ExitCode != 2 || result.Metadata["code"] != "STATION_OPERATION_CANCELLED" {
		t.Fatalf("cancelled preparation was reported as successful or warning-only: %#v", result)
	}
}

func TestGuardTargetsOnlyRequestedLocalUserAndRecordsOutcome(t *testing.T) {
	runtime := &fakeRuntime{
		supervisor: fakeSupervisor{name: "systemd"},
		local:      []map[string]any{{"id": "1", "user": "teacher"}, {"id": "2", "user": "student"}},
		sessionsOK: true,
		notifyErr:  errors.New("wall unavailable"),
	}
	a, _ := newTestAgent(t, "hybrid", runtime)
	result := a.Execute(context.Background(), "guard-1", "session guard", []string{"--user=teacher", "--guard-grace=0"})
	if result.ExitCode != 1 || result.Outcome != "warning" {
		t.Fatalf("notification failure should be visible as a warning: %#v", result)
	}
	var metadata map[string]any
	if err := json.Unmarshal(mustJSON(t, result.Metadata), &metadata); err != nil {
		t.Fatal(err)
	}
	sessions := metadata["localSessions"].([]any)
	if len(sessions) != 1 || sessions[0].(map[string]any)["user"] != "teacher" {
		t.Fatalf("guard did not filter to selected user: %#v", metadata)
	}
	if len(runtime.notices) != 1 || !strings.Contains(runtime.notices[0], "reservation") {
		t.Fatalf("expected one default local warning, got %#v", runtime.notices)
	}
}

func TestGuardCanSkipNotificationAndReportsQueryFailure(t *testing.T) {
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, local: []map[string]any{{"user": "teacher"}}, sessionsOK: true}
	a, _ := newTestAgent(t, "hybrid", runtime)
	result := a.Execute(context.Background(), "guard-2", "session guard", []string{"--no-guard", "--guard-notify=false"})
	if result.ExitCode != 1 || len(runtime.notices) != 0 || result.Metadata["noGuard"] != true {
		t.Fatalf("no-guard must avoid notifications and waits while warning that a session remains: %#v", result)
	}
	runtime.sessionsOK = false
	result = a.Execute(context.Background(), "guard-3", "session guard", nil)
	if result.ExitCode != 1 || result.Metadata["queryOk"] != false {
		t.Fatalf("guard must surface unavailable session inventory: %#v", result)
	}
}

func TestReleaseTerminatesOnlyTinyDeskAndNeverRebootsWhileLocalUsersRemain(t *testing.T) {
	runtime := &fakeRuntime{
		supervisor: fakeSupervisor{name: "openrc"},
		local:      []map[string]any{{"id": "seat-1", "user": "teacher"}},
		remote: []map[string]any{
			{"id": "rdp-1", "user": "labuser", "remote": false, "seat": "", "type": "x11"},
			{"id": "ssh-1", "user": "labstation-ops", "type": "tty"},
		},
		sessionsOK: true,
	}
	a, calls := newTestAgent(t, "hybrid", runtime)
	result := a.Execute(context.Background(), "release-1", "release-session", []string{"--reboot", "--reboot-timeout=0"})
	if result.ExitCode != 1 || result.Metadata["rebootRequested"] != true || result.Metadata["rebootScheduled"] != false || result.Metadata["rebootDeferred"] != true || result.Metadata["rebootReason"] != "active user sessions" {
		t.Fatalf("release must defer reboot while a local user remains: %#v", result)
	}
	if len(*calls) != 1 || (*calls)[0]["sessionId"] != "rdp-1" || (*calls)[0]["operation"] != "terminate-session" {
		t.Fatalf("only the labuser X11 session may be terminated: %#v", *calls)
	}
	if (*calls)[0]["sessionRemote"] != "no" || (*calls)[0]["sessionSeat"] != "" || (*calls)[0]["forceSession"] != true || (*calls)[0]["terminateOnCloseFailure"] != true {
		t.Fatalf("release did not preserve the seatless XRDP identity reported by logind: %#v", *calls)
	}
	if (*calls)[0]["closeTimeoutSeconds"] != float64(10) {
		t.Fatalf("release did not pass the shared 10 second close deadline: %#v", *calls)
	}
	if _, ok := (*calls)[0]["action"]; ok {
		t.Fatal("management SSH or local user session reached privileged action")
	}
}

func TestReleaseCleansFMUExecutorAfterTerminatingTinyDesk(t *testing.T) {
	runtime := &fakeRuntime{
		supervisor: fakeSupervisor{name: "systemd"},
		remote: []map[string]any{{
			"id": "rdp-1", "user": "labuser", "type": "x11", "remote": false, "seat": "",
		}},
		sessionsOK: true,
	}
	a, calls := newTestAgent(t, "dedicated", runtime)
	cleanupFollowedSessionTermination := false
	a.cleanFMUExecutorStateOverride = func(context.Context) error {
		cleanupFollowedSessionTermination = len(*calls) == 1 && (*calls)[0]["operation"] == "terminate-session"
		return nil
	}

	result := a.Execute(context.Background(), "release-fmu-cleanup", "release-session", nil)

	if result.ExitCode != 0 || result.Metadata["fmuCleanupCompleted"] != true {
		t.Fatalf("Release should complete after cleaning FMU sessions and temporary state: %#v", result)
	}
	if !cleanupFollowedSessionTermination {
		t.Fatalf("FMU cleanup should run after the remote Tiny Desk session ends: %#v", *calls)
	}
}

func TestReleaseFMUCleanupFailureBlocksRebootAndRequiresRecovery(t *testing.T) {
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, sessionsOK: true}
	a, calls := newTestAgent(t, "hybrid", runtime)
	a.cleanFMUExecutorStateOverride = func(context.Context) error { return errors.New("synthetic FMU cleanup failure") }

	result := a.Execute(context.Background(), "release-fmu-cleanup-fails", "release-session", []string{"--reboot"})

	if result.ExitCode != 2 || result.Metadata["code"] != "STATION_FMU_CLEANUP_FAILED" || result.Metadata["fmuCleanupCompleted"] != false {
		t.Fatalf("incomplete FMU cleanup must make Release a hard failure: %#v", result)
	}
	for _, call := range *calls {
		if call["operation"] == "power-reboot" {
			t.Fatalf("Release must not reboot before FMU cleanup completes: %#v", *calls)
		}
	}
}

func TestPowerHonorsWakeRequirementAndHibernateCapability(t *testing.T) {
	vector := loadPortableParityMatrix(t).Power
	if vector.Action != "shutdown" || !vector.RequireWake || vector.Expected != "blocked" {
		t.Fatalf("unexpected shared power vector: %#v", vector)
	}
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, sessionsOK: true}
	a, calls := newTestAgent(t, "dedicated", runtime)
	args := []string{vector.Action}
	if vector.RequireWake {
		args = append(args, "--require-wake")
	}
	result := a.Execute(context.Background(), "power-1", "power", args)
	if result.ExitCode != 2 || result.Metadata["code"] != "STATION_WAKE_NOT_READY" || len(*calls) != 0 {
		t.Fatalf("required WoL failure must block shutdown: %#v", result)
	}
	runtime.wakeOK, runtime.wakeOn, runtime.wakeSaved = true, true, true
	result = a.Execute(context.Background(), "power-2", "power", []string{"shutdown"})
	if result.ExitCode != 0 || len(*calls) != 1 || (*calls)[0]["operation"] != "power-shutdown" {
		t.Fatalf("ready shutdown should dispatch only an allowlisted power operation: %#v", result)
	}
	result = a.Execute(context.Background(), "power-3", "power", []string{"hibernate"})
	if result.ExitCode != 2 || result.Metadata["code"] != "STATION_POWER_UNSUPPORTED" {
		t.Fatalf("unsupported hibernation must be rejected: %#v", result)
	}
}

func TestPowerWarningsWhenWakeIsNotReadyButDoesNotRequireIt(t *testing.T) {
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, sessionsOK: true}
	a, calls := newTestAgent(t, "dedicated", runtime)
	result := a.Execute(context.Background(), "power-warning", "power", []string{"shutdown", "--reason=maintenance"})
	if result.ExitCode != 1 || result.Outcome != "warning" || result.Metadata["wakeReady"] != false {
		t.Fatalf("shutdown without a configured WoL policy should report a warning: %#v", result)
	}
	if len(*calls) != 1 || (*calls)[0]["operation"] != "power-shutdown" {
		t.Fatalf("non-required WoL warning should still dispatch the shutdown action: %#v", *calls)
	}
}

func TestReleaseRequestsRebootOnlyAfterAllUserSessionsAreGone(t *testing.T) {
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, sessionsOK: true}
	a, calls := newTestAgent(t, "hybrid", runtime)
	result := a.Execute(context.Background(), "release-reboot", "release-session", []string{"--reboot"})
	if result.ExitCode != 0 || result.Metadata["rebootRequested"] != true || result.Metadata["rebootScheduled"] != true {
		t.Fatalf("empty station should request a release reboot: %#v", result)
	}
	if len(*calls) != 1 || (*calls)[0]["operation"] != "power-reboot" {
		t.Fatalf("release reboot did not use the privileged allowlist: %#v", *calls)
	}
}

func TestRecoveryRequiresMarkerAndHonorsSessionLocalModeAndCooldownGates(t *testing.T) {
	vector := loadPortableParityMatrix(t).Recovery
	if vector.SafeStationExpected != "no-reboot" {
		t.Fatalf("unexpected shared safe-station recovery result: %#v", vector)
	}
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, sessionsOK: true}
	a, calls := newTestAgent(t, "hybrid", runtime)
	marked := false
	a.rebootMarker = func() bool { return marked }
	result := a.Execute(context.Background(), "recovery-empty", "recovery reboot-if-needed", nil)
	if result.ExitCode != 1 || result.Metadata["reason"] != "no supported reboot-required marker is present" {
		t.Fatalf("recovery must do nothing without an OS marker: %#v", result)
	}
	marked = true
	result = a.Execute(context.Background(), "recovery-1", "recovery reboot-if-needed", []string{"--reason=kernel update"})
	if result.ExitCode != 0 || result.Metadata["rebootRequested"] != true || len(*calls) != 1 {
		t.Fatalf("marked station without active users should reboot once: %#v", result)
	}
	result = a.Execute(context.Background(), "recovery-2", "recovery reboot-if-needed", []string{"--force"})
	if result.ExitCode != 1 || result.Metadata["reason"] != "reboot cooldown is active" || len(*calls) != 1 {
		t.Fatalf("24-hour recovery cooldown was not enforced: %#v", result)
	}
	if err := os.Remove(filepath.Join(a.Config.StateDir, "recovery-last.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.Config.StateDir, "local-mode.flag"), []byte(`{"enabled":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	result = a.Execute(context.Background(), "recovery-local", "recovery reboot-if-needed", nil)
	if result.ExitCode != 1 || result.Metadata["reason"] != "local mode is active" || len(*calls) != 1 {
		t.Fatalf("recovery must defer while local mode is active: %#v", result)
	}
}

func TestRecoveryDefersForActiveUsersAndRemovesCooldownRecordOnHelperFailure(t *testing.T) {
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, local: []map[string]any{{"user": "teacher"}}, sessionsOK: true}
	a, calls := newTestAgent(t, "hybrid", runtime)
	a.rebootMarker = func() bool { return true }
	result := a.Execute(context.Background(), "recovery-user", "recovery reboot-if-needed", nil)
	if result.ExitCode != 1 || result.Metadata["reason"] != "active user sessions" || len(*calls) != 0 {
		t.Fatalf("recovery must not interrupt an active instructor: %#v", result)
	}
	runtime.local = nil
	a.runHelper = func(context.Context, []byte) error { return errors.New("helper unavailable") }
	result = a.Execute(context.Background(), "recovery-failure", "recovery reboot-if-needed", nil)
	if result.ExitCode != 2 || fileExists(filepath.Join(a.Config.StateDir, "recovery-last.json")) {
		t.Fatalf("failed reboot dispatch must be retryable and remove the cooldown marker: %#v", result)
	}
}

func TestServiceAndFmuActionsMapSupervisorNamesAndFailClosed(t *testing.T) {
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "openrc", states: map[string]string{"decentralabs-labstation": "started", "decentralabs-labstation-fmu": "stopped"}}, sessionsOK: true}
	a, calls := newTestAgent(t, "fmu-only", runtime)
	status := a.Execute(context.Background(), "service-status", "service", []string{"status"})
	if status.ExitCode != 0 || status.Stdout != "started" || status.Metadata["backend"] != "openrc" {
		t.Fatalf("OpenRC unit normalization failed: %#v", status)
	}
	started := a.Execute(context.Background(), "fmu-start", "fmu-executor", []string{"start"})
	if started.ExitCode != 0 || len(*calls) != 1 || (*calls)[0]["unit"] != "decentralabs-labstation-fmu" {
		t.Fatalf("FMU service action was not allowlisted or normalized: %#v helper=%#v", started, *calls)
	}
	runtime.supervisor = nil
	unavailable := a.Execute(context.Background(), "service-start", "service", []string{"start"})
	if unavailable.ExitCode != 2 || unavailable.Metadata["code"] != "STATION_SERVICE_UNAVAILABLE" {
		t.Fatalf("missing service manager must be an error: %#v", unavailable)
	}
}

func TestArtifactsAreAllowlistedAndHeartbeatsCanBeSynthesized(t *testing.T) {
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, sessionsOK: true}
	a, _ := newTestAgent(t, "hybrid", runtime)
	data, err := a.Artifact("status")
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatal(err)
	}
	if status["schemaVersion"] != "3.0.0" || status["profile"] != "hybrid" || status["localModeEnabled"] != false {
		t.Fatalf("unexpected synthetic status: %#v", status)
	}
	if _, err := a.Artifact("../../etc/shadow"); err == nil {
		t.Fatal("arbitrary artifact path was accepted")
	}
}

func TestEnergyAuditReturnsRuntimeCapabilities(t *testing.T) {
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "openrc"}, hibernate: true}
	a, _ := newTestAgent(t, "dedicated", runtime)
	result := a.Execute(context.Background(), "energy-audit", "energy audit", nil)
	if result.ExitCode != 0 {
		t.Fatalf("energy audit failed: %#v", result)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(result.Stdout), &report); err != nil {
		t.Fatal(err)
	}
	if report["supervisor"] != "openrc" || report["hibernateSupported"] != true || report["activeSwap"] != true {
		t.Fatalf("energy audit omitted runtime capabilities: %#v", report)
	}
	inhibitors := report["inhibitors"].(map[string]any)
	if inhibitors["available"] != true || inhibitors["activeCount"] != float64(1) {
		t.Fatalf("inhibitor state was not projected: %#v", inhibitors)
	}
}

func TestWaitGraceReturnsOnTimerOrContextCancellation(t *testing.T) {
	if err := waitGrace(context.Background(), 0); err != nil {
		t.Fatalf("zero grace period should return immediately: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := waitGrace(ctx, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled grace period error = %v, want context cancellation", err)
	}
}

func TestHelperReceivesJSONOnlyAndReturnsFailureWithoutShellingOut(t *testing.T) {
	a, _ := newTestAgent(t, "hybrid", &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, sessionsOK: true})
	a.runHelper = func(_ context.Context, raw []byte) error {
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			return err
		}
		if _, exists := value["command"]; exists {
			t.Fatal("helper request unexpectedly contains an arbitrary command field")
		}
		if value["operation"] != "service-action" || value["action"] != "start" {
			t.Fatalf("unexpected helper request: %#v", value)
		}
		return errors.New("injected failure")
	}
	result := a.Execute(context.Background(), "service-start", "service", []string{"start"})
	if result.ExitCode != 2 || result.Outcome != "failure" {
		t.Fatalf("helper failure should be a hard failure: %#v", result)
	}
}

func TestOperationRecordsAndAtomicArtifactsStayInsideStateDirectory(t *testing.T) {
	a, _ := newTestAgent(t, "hybrid", &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, sessionsOK: true})
	result := a.Execute(context.Background(), "operation-1", "service", []string{"start"})
	if result.ExitCode != 0 {
		t.Fatal(result)
	}
	path := filepath.Join(a.Config.StateDir, "service-state.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record["operationId"] != "operation-1" || record["profile"] != "hybrid" {
		t.Fatalf("operation audit record is incomplete: %#v", record)
	}
	if filepath.Dir(path) != a.Config.StateDir {
		t.Fatal("audit data escaped the configured state directory")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
