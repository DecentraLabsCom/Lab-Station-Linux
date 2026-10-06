package agent

import (
	"context"
	"encoding/json"
	"errors"
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
	supervisor host.Supervisor
	interfaces []map[string]string
	local      []map[string]any
	remote     []map[string]any
	sessionsOK bool
	wakeOK     bool
	wakeOn     bool
	wakeSaved  bool
	hibernate  bool
	notifyErr  error
	notices    []string
}

func (f *fakeRuntime) Supervisor() host.Supervisor            { return f.supervisor }
func (f *fakeRuntime) NetworkInterfaces() []map[string]string { return f.interfaces }
func (f *fakeRuntime) Sessions() ([]map[string]any, []map[string]any, bool) {
	return append([]map[string]any(nil), f.local...), append([]map[string]any(nil), f.remote...), f.sessionsOK
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
	return f.notifyErr
}
func (f *fakeRuntime) HibernateSupported() bool { return f.hibernate }

func newTestAgent(t *testing.T, profile string, runtime *fakeRuntime) (*Agent, *[]map[string]any) {
	t.Helper()
	cfg := config.Defaults()
	cfg.Name = "station-test"
	cfg.Profile = profile
	cfg.GuardGraceSeconds = 0
	cfg.StateDir = t.TempDir()
	calls := &[]map[string]any{}
	a := &Agent{Config: cfg, Runtime: runtime}
	a.runHelper = func(_ context.Context, raw []byte) error {
		var request map[string]any
		if err := json.Unmarshal(raw, &request); err != nil {
			return err
		}
		*calls = append(*calls, request)
		return nil
	}
	return a, calls
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

func TestPrepareSessionRefusesUnsafeProfilesAndReleasesHybridSessions(t *testing.T) {
	matrix := loadPortableParityMatrix(t)
	if matrix.SessionSafety.LocalSession != "protect-unless-authorized" {
		t.Fatalf("shared local-session policy = %q", matrix.SessionSafety.LocalSession)
	}
	instructor := map[string]any{"id": "local-1", "user": "instructor", "kind": "local"}
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, local: []map[string]any{instructor}, sessionsOK: true}
	a, calls := newTestAgent(t, "dedicated", runtime)
	result := a.Execute(context.Background(), "prepare-1", "prepare-session", nil)
	if result.ExitCode != 2 || result.Metadata["code"] != "STATION_LOCAL_SESSION_ACTIVE" || len(*calls) != 0 {
		t.Fatalf("dedicated station should fail closed on an active local user: %#v, helper=%#v", result, *calls)
	}

	a.Config.Profile = "hybrid"
	a.Config.AllowLocalSessionEviction = false
	result = a.Execute(context.Background(), "prepare-2", "prepare-session", nil)
	if result.ExitCode != 2 || result.Metadata["code"] != "STATION_LOCAL_SESSION_ACTIVE" {
		t.Fatalf("hybrid station should protect instructor session unless eviction was opted in: %#v", result)
	}

	a.Config.AllowLocalSessionEviction = true
	result = a.Execute(context.Background(), "prepare-3", "prepare-session", []string{"--guard-grace=0", "--guard-notify=false"})
	if result.ExitCode != 0 || len(*calls) != 1 || (*calls)[0]["operation"] != "terminate-session" || (*calls)[0]["sessionId"] != "local-1" {
		t.Fatalf("authorized hybrid preparation should notify helper exactly once: result=%#v helper=%#v", result, *calls)
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
			{"id": "rdp-1", "user": "labuser", "type": "x11"},
			{"id": "ssh-1", "user": "labstation-ops", "type": "tty"},
		},
		sessionsOK: true,
	}
	a, calls := newTestAgent(t, "hybrid", runtime)
	result := a.Execute(context.Background(), "release-1", "release-session", []string{"--reboot", "--reboot-timeout=0"})
	if result.ExitCode != 1 || result.Metadata["rebootRequested"] != false || result.Metadata["reason"] != "active user sessions" {
		t.Fatalf("release must defer reboot while a local user remains: %#v", result)
	}
	if len(*calls) != 1 || (*calls)[0]["sessionId"] != "rdp-1" || (*calls)[0]["operation"] != "terminate-session" {
		t.Fatalf("only the labuser X11 session may be terminated: %#v", *calls)
	}
	if _, ok := (*calls)[0]["action"]; ok {
		t.Fatal("management SSH or local user session reached privileged action")
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
	if result.ExitCode != 0 || result.Metadata["rebootRequested"] != true {
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
