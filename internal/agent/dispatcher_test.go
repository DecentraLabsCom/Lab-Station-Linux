package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/decentralabs/lab-station-linux/internal/config"
)

func decodeResult(t *testing.T, raw []byte) Result {
	t.Helper()
	var result Result
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("invalid dispatcher JSON %q: %v", raw, err)
	}
	return result
}

func TestDispatchRejectsMalformedUnknownAndMultiObjectRequests(t *testing.T) {
	a := New(config.Defaults())
	cases := []struct {
		name  string
		input string
		code  string
	}{
		{"malformed JSON", "{not-json", "STATION_COMMAND_REJECTED"},
		{"unknown field", `{"schemaVersion":1,"operation":"execute","command":"status-json","args":[],"shell":"id"}`, "STATION_COMMAND_REJECTED"},
		{"multiple objects", `{"schemaVersion":1,"operation":"execute","command":"identity","args":[]}{}`, "STATION_COMMAND_REJECTED"},
		{"unsupported protocol", `{"schemaVersion":2,"operation":"execute","command":"identity","args":[]}`, "STATION_COMMAND_REJECTED"},
		{"unknown operation", `{"schemaVersion":1,"operation":"shell","command":"id","args":[]}`, "STATION_COMMAND_REJECTED"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := Dispatch(strings.NewReader(test.input), &output, a); err != nil {
				t.Fatal(err)
			}
			result := decodeResult(t, output.Bytes())
			if result.ExitCode != 2 || result.Metadata["code"] != test.code {
				t.Fatalf("request was not rejected: %#v", result)
			}
		})
	}
}

func TestDispatchRunsIdentityAndReturnsOneContractEnvelope(t *testing.T) {
	cfg := config.Defaults()
	cfg.Name = "station-dispatch-test"
	cfg.StateDir = t.TempDir()
	runtime := &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, sessionsOK: true}
	a := &Agent{Config: cfg, Runtime: runtime}
	request := `{"schemaVersion":1,"id":"request-9","operation":"execute","command":"identity","args":[]}`
	var output bytes.Buffer
	if err := Dispatch(strings.NewReader(request), &output, a); err != nil {
		t.Fatal(err)
	}
	result := decodeResult(t, output.Bytes())
	if result.ID != "request-9" || result.ExitCode != 0 || result.Command != "identity" {
		t.Fatalf("unexpected dispatcher result: %#v", result)
	}
	var identity map[string]any
	if err := json.Unmarshal([]byte(result.Stdout), &identity); err != nil {
		t.Fatal(err)
	}
	if identity["contractVersion"] != "3.0.0" || identity["host"] != cfg.Name || identity["platform"].(map[string]any)["os"] != "linux" {
		t.Fatalf("identity contract is incomplete: %#v", identity)
	}
}

func TestDispatchSecretSetIsAllowlistedAndDoesNotReturnSecret(t *testing.T) {
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	calls := 0
	const secret = "0123456789abcdef0123456789abcdef"
	a := &Agent{Config: cfg, Runtime: &fakeRuntime{supervisor: fakeSupervisor{name: "systemd"}, sessionsOK: true}}
	a.runHelper = func(_ context.Context, raw []byte) error {
		calls++
		var helperRequest map[string]any
		if err := json.Unmarshal(raw, &helperRequest); err != nil {
			return err
		}
		if helperRequest["operation"] != "secret-set" || helperRequest["secretValue"] != secret {
			t.Fatalf("wrong helper request: %#v", helperRequest)
		}
		return nil
	}
	input, _ := json.Marshal(map[string]any{"schemaVersion": 1, "id": "secret-1", "operation": "secret.set", "secretId": "fmu-internal-token", "secretValue": secret})
	var output bytes.Buffer
	if err := Dispatch(bytes.NewReader(input), &output, a); err != nil {
		t.Fatal(err)
	}
	result := decodeResult(t, output.Bytes())
	if result.ExitCode != 0 || calls != 1 || strings.Contains(output.String(), secret) {
		t.Fatalf("secret result leaked or was not provisioned: result=%#v output=%s", result, output.String())
	}
	if strings.Contains(result.Message, secret) || strings.Contains(result.Stdout, secret) || strings.Contains(result.Stderr, secret) {
		t.Fatal("secret appeared in dispatcher result")
	}
}

func TestDispatchRejectsSecretReuseAndInvalidPayloadWithoutCallingHelper(t *testing.T) {
	a := New(config.Defaults())
	calls := 0
	a.runHelper = func(context.Context, []byte) error { calls++; return nil }
	for _, request := range []string{
		`{"schemaVersion":1,"operation":"secret.set","secretId":"root-password","secretValue":"0123456789abcdef0123456789abcdef"}`,
		`{"schemaVersion":1,"operation":"secret.set","secretId":"fmu-internal-token","secretValue":"short"}`,
		`{"schemaVersion":1,"operation":"secret.clear","secretId":"root-password"}`,
	} {
		var output bytes.Buffer
		if err := Dispatch(strings.NewReader(request), &output, a); err != nil {
			t.Fatal(err)
		}
		if decodeResult(t, output.Bytes()).ExitCode != 2 {
			t.Errorf("invalid secret operation was accepted: %s", request)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid secret requests reached the helper %d times", calls)
	}
}

func TestDispatchSecretHelperFailureReturnsGenericError(t *testing.T) {
	a := New(config.Defaults())
	const secret = "0123456789abcdef0123456789abcdef"
	a.runHelper = func(context.Context, []byte) error { return errors.New(secret) }
	request := `{"schemaVersion":1,"operation":"secret.set","secretId":"fmu-internal-token","secretValue":"` + secret + `"}`
	var output bytes.Buffer
	if err := Dispatch(strings.NewReader(request), &output, a); err != nil {
		t.Fatal(err)
	}
	result := decodeResult(t, output.Bytes())
	if result.ExitCode != 2 || strings.Contains(output.String(), secret) || !strings.Contains(result.Stderr, "secret provisioning failed") {
		t.Fatalf("helper error exposed a secret: %#v", result)
	}
}

func TestDispatchRejectsInputLargerThanProtocolLimit(t *testing.T) {
	a := New(config.Defaults())
	request := `{"schemaVersion":1,"operation":"execute","command":"identity","args":[],"x":"` + strings.Repeat("x", 70*1024) + `"}`
	var output bytes.Buffer
	if err := Dispatch(strings.NewReader(request), &output, a); err != nil {
		t.Fatal(err)
	}
	result := decodeResult(t, output.Bytes())
	if result.ExitCode != 2 {
		t.Fatalf("oversized request was accepted: %#v", result)
	}
}
