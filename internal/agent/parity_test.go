package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"
)

type parityTestReference struct {
	File string `json:"file"`
	Test string `json:"test"`
}

type parityTestScenario struct {
	ID      string              `json:"id"`
	Windows parityTestReference `json:"windows"`
	Linux   parityTestReference `json:"linux"`
}

type portableStatusVector struct {
	SchemaVersion         string   `json:"schemaVersion"`
	RequiredFields        []string `json:"requiredFields"`
	Profiles              []string `json:"profiles"`
	SummaryStates         []string `json:"summaryStates"`
	ReadinessCapabilities []string `json:"readinessCapabilities"`
	SessionKinds          []string `json:"sessionKinds"`
	SessionRequiredFields []string `json:"sessionRequiredFields"`
}

type parityPlatform struct {
	OS                  string `json:"os"`
	ManagementTransport string `json:"managementTransport"`
}

type portableParityMatrix struct {
	Source         string                    `json:"source"`
	Version        int                       `json:"version"`
	PortableStatus portableStatusVector      `json:"portableStatus"`
	Platforms      map[string]parityPlatform `json:"platforms"`
	SessionSafety  struct {
		UnavailableInventory string `json:"unavailableInventory"`
		LocalSession         string `json:"localSession"`
	} `json:"sessionSafety"`
	Power struct {
		Action      string `json:"action"`
		RequireWake bool   `json:"requireWake"`
		WakeReady   bool   `json:"wakeReady"`
		Expected    string `json:"expected"`
	} `json:"power"`
	Recovery struct {
		SafeStationExpected string `json:"safeStationExpected"`
	} `json:"recovery"`
	SharedScenarios []parityTestScenario `json:"sharedScenarios"`
}

func loadPortableParityMatrix(t *testing.T) portableParityMatrix {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate the parity test source")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	raw, err := os.ReadFile(filepath.Join(repositoryRoot, "contracts", "station", "v3", "test-parity.json"))
	if err != nil {
		t.Fatalf("read shared parity vectors: %v", err)
	}
	var matrix portableParityMatrix
	if err := json.Unmarshal(raw, &matrix); err != nil {
		t.Fatalf("decode shared parity vectors: %v", err)
	}
	return matrix
}

func containsParityValue(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func parityObject(t *testing.T, parent map[string]any, field string) map[string]any {
	t.Helper()
	value, ok := parent[field].(map[string]any)
	if !ok {
		t.Errorf("%s has type %T, want object", field, parent[field])
		return map[string]any{}
	}
	return value
}

func parityString(t *testing.T, parent map[string]any, field string) string {
	t.Helper()
	value, ok := parent[field].(string)
	if !ok {
		t.Errorf("%s has type %T, want string", field, parent[field])
		return ""
	}
	return value
}

func assertPortableStatusContract(t *testing.T, status map[string]any, platformName string, matrix portableParityMatrix) {
	t.Helper()
	contract := matrix.PortableStatus
	for _, field := range contract.RequiredFields {
		if _, ok := status[field]; !ok {
			t.Errorf("status is missing portable contract field %q", field)
		}
	}
	if status["schemaVersion"] != contract.SchemaVersion {
		t.Errorf("schemaVersion = %v, want %s", status["schemaVersion"], contract.SchemaVersion)
	}
	if _, err := time.Parse(time.RFC3339Nano, parityString(t, status, "timestamp")); err != nil {
		t.Errorf("timestamp is not RFC3339: %v", err)
	}
	profile := parityString(t, status, "profile")
	if !containsParityValue(contract.Profiles, profile) {
		t.Errorf("profile %v is outside the portable values %v", status["profile"], contract.Profiles)
	}
	platform := parityObject(t, status, "platform")
	management := parityObject(t, status, "management")
	wantPlatform := matrix.Platforms[platformName]
	if platform["os"] != wantPlatform.OS {
		t.Errorf("platform.os = %v, want %s", platform["os"], wantPlatform.OS)
	}
	if management["transport"] != wantPlatform.ManagementTransport {
		t.Errorf("management.transport = %v, want %s", management["transport"], wantPlatform.ManagementTransport)
	}
	summary := parityObject(t, status, "summary")
	if !containsParityValue(contract.SummaryStates, parityString(t, summary, "state")) {
		t.Errorf("summary.state %v is outside the portable values %v", summary["state"], contract.SummaryStates)
	}
	readiness := parityObject(t, status, "readiness")
	for _, capability := range contract.ReadinessCapabilities {
		if _, ok := readiness[capability]; !ok {
			t.Errorf("readiness is missing portable capability %q", capability)
		}
	}
	sessions := parityObject(t, status, "sessions")
	active, ok := sessions["active"].([]any)
	if !ok {
		t.Errorf("sessions.active has type %T, want array", sessions["active"])
		return
	}
	if _, ok := sessions["localSessionActive"].(bool); !ok {
		t.Errorf("sessions.localSessionActive has type %T, want boolean", sessions["localSessionActive"])
	}
	for index, entry := range active {
		session, ok := entry.(map[string]any)
		if !ok {
			t.Errorf("sessions.active[%d] has type %T, want object", index, entry)
			continue
		}
		for _, field := range contract.SessionRequiredFields {
			if _, ok := session[field]; !ok {
				t.Errorf("sessions.active[%d] is missing portable field %q", index, field)
			}
		}
		if !containsParityValue(contract.SessionKinds, parityString(t, session, "kind")) {
			t.Errorf("sessions.active[%d].kind %v is outside the portable values %v", index, session["kind"], contract.SessionKinds)
		}
		if _, ok := session["active"].(bool); !ok {
			t.Errorf("sessions.active[%d].active has type %T, want boolean", index, session["active"])
		}
		if _, ok := session["evictable"].(bool); !ok {
			t.Errorf("sessions.active[%d].evictable has type %T, want boolean", index, session["evictable"])
		}
	}
}

func TestPortableParityManifestReferencesImplementedTests(t *testing.T) {
	matrix := loadPortableParityMatrix(t)
	if matrix.Version != 1 || matrix.PortableStatus.SchemaVersion != "3.0.0" {
		t.Fatalf("unexpected portable matrix version/contract: %#v", matrix)
	}
	if len(matrix.SharedScenarios) == 0 {
		t.Fatal("portable matrix has no cross-platform scenarios")
	}
	_, sourceFile, _, _ := runtime.Caller(0)
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
	seen := make(map[string]bool)
	for _, scenario := range matrix.SharedScenarios {
		if scenario.ID == "" || seen[scenario.ID] {
			t.Errorf("scenario ids must be present and unique: %q", scenario.ID)
			continue
		}
		seen[scenario.ID] = true
		for platform, ref := range map[string]parityTestReference{"windows": scenario.Windows, "linux": scenario.Linux} {
			if ref.File == "" || ref.Test == "" {
				t.Errorf("%s: %s mapping is incomplete", scenario.ID, platform)
				continue
			}
		}
		path := filepath.Join(repositoryRoot, filepath.FromSlash(scenario.Linux.File))
		source, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: read Linux test file %q: %v", scenario.ID, scenario.Linux.File, err)
			continue
		}
		declaration := regexp.MustCompile(`(?m)^func\s+` + regexp.QuoteMeta(scenario.Linux.Test) + `\s*\(`)
		if !declaration.Match(source) {
			t.Errorf("%s: Linux test %s is missing from %s", scenario.ID, scenario.Linux.Test, scenario.Linux.File)
		}
	}
}
