package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/decentralabs/lab-station-linux/internal/config"
)

const (
	maxDispatcherRequestBytes  = 64 * 1024
	maxDispatcherResponseBytes = 2 * 1024 * 1024
)

var secretPattern = regexp.MustCompile(`^[\x20-\x7e]{32,512}$`)
var operationIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,95}$`)

func Dispatch(input io.Reader, output io.Writer, a *Agent) error {
	raw, err := io.ReadAll(io.LimitReader(input, maxDispatcherRequestBytes+1))
	if err != nil {
		return writeRejected(output, "", "invalid JSON request")
	}
	if len(raw) > maxDispatcherRequestBytes {
		return writeRejected(output, "", "request exceeds dispatcher limit")
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return writeRejected(output, "", "invalid JSON request")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		return writeRejected(output, "", "invalid JSON request")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return writeRequestError(output, request, "STATION_COMMAND_REJECTED", "request must contain one JSON object")
	}
	if request.SchemaVersion != 1 && request.SchemaVersion != 2 {
		return writeRequestError(output, request, "STATION_COMMAND_REJECTED", "unsupported dispatcher protocol")
	}
	if !ValidOperationID(request.ID) {
		return writeRequestError(output, request, "STATION_COMMAND_REJECTED", "operation id is invalid")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if request.SchemaVersion == 2 {
		return dispatchV2(ctx, output, a, request)
	}
	if request.Operation == "execute" && (request.Command == "prepare-session" || request.Command == "release-session") {
		return writeRequestError(output, request, "STATION_LEASE_REQUIRED", "session lifecycle requires dispatcher protocol 2")
	}
	return dispatchV1(ctx, output, a, request)
}

// ValidOperationID reports whether an operation identifier is safe to use as
// a journal key and as a queue result filename.
func ValidOperationID(id string) bool { return operationIDPattern.MatchString(id) }

func dispatchV1(ctx context.Context, output io.Writer, a *Agent, request Request) error {
	switch request.Operation {
	case "execute":
		if request.Context != nil || request.IssuedAt != "" || request.ExecuteBefore != "" || request.OperationID != "" {
			return writeRequestError(output, request, "STATION_COMMAND_REJECTED", "dispatcher protocol 1 does not accept protocol 2 fields")
		}
		return writeJSONResult(output, a.Execute(ctx, request.ID, request.Command, request.Args))
	case "artifact.read":
		data, err := a.Artifact(request.Artifact)
		if err != nil {
			return encodeResult(output, request.ID, "artifact.read", 2, "", err.Error(), map[string]any{"code": "STATION_ARTIFACT_UNAVAILABLE"})
		}
		return encodeResult(output, request.ID, "artifact.read", 0, string(data), "", map[string]any{"artifact": request.Artifact})
	case "secret.set":
		if request.SecretID != "fmu-internal-token" || !secretPattern.MatchString(request.SecretValue) {
			return encodeResult(output, request.ID, "secret.set", 2, "", "secret payload is invalid", map[string]any{"code": "STATION_SECRET_REJECTED"})
		}
		value := request.SecretValue
		err := a.helper(ctx, map[string]any{"operation": "secret-set", "secretId": request.SecretID, "secretValue": value})
		value = ""
		if err != nil {
			return encodeResult(output, request.ID, "secret.set", 2, "", "secret provisioning failed", map[string]any{"code": "STATION_SECRET_FAILED"})
		}
		return encodeResult(output, request.ID, "secret.set", 0, "", "secret configured", map[string]any{"secretId": request.SecretID})
	case "secret.clear":
		if request.SecretID != "fmu-internal-token" {
			return encodeResult(output, request.ID, "secret.clear", 2, "", "secret identifier is not allowlisted", map[string]any{"code": "STATION_SECRET_REJECTED"})
		}
		if err := a.helper(ctx, map[string]any{"operation": "secret-clear", "secretId": request.SecretID}); err != nil {
			return encodeResult(output, request.ID, "secret.clear", 2, "", "secret release failed", map[string]any{"code": "STATION_SECRET_FAILED"})
		}
		return encodeResult(output, request.ID, "secret.clear", 0, "", "secret cleared", map[string]any{"secretId": request.SecretID})
	default:
		return writeRequestError(output, request, "STATION_COMMAND_REJECTED", "dispatcher operation is not allowlisted")
	}
}

func dispatchV2(ctx context.Context, output io.Writer, a *Agent, request Request) error {
	if request.Operation == "operation.status" {
		if request.OperationID == "" || !operationIDPattern.MatchString(request.OperationID) || request.Command != "" || request.Context != nil {
			return writeRequestError(output, request, "STATION_COMMAND_REJECTED", "operation.status request is invalid")
		}
		return writeOperationStatus(output, a.Config.StateDir, request.ID, request.OperationID)
	}
	if request.Operation == "artifact.read" {
		if request.Context != nil || request.IssuedAt != "" || request.ExecuteBefore != "" || request.OperationID != "" {
			return writeRequestError(output, request, "STATION_COMMAND_REJECTED", "artifact reads do not accept lease context")
		}
		return dispatchV1(ctx, output, a, request)
	}
	if request.Operation != "execute" {
		return writeRequestError(output, request, "STATION_COMMAND_REJECTED", "dispatcher operation is not allowlisted")
	}
	if request.OperationID != "" {
		return writeRequestError(output, request, "STATION_COMMAND_REJECTED", "operationId is valid only for operation.status")
	}
	if request.Command != "prepare-session" && request.Command != "release-session" {
		if request.Context != nil || request.IssuedAt != "" || request.ExecuteBefore != "" {
			return writeRequestError(output, request, "STATION_COMMAND_REJECTED", "lease context is not valid for this operation")
		}
		if !isReadOnlyV2Command(request.Command, request.Args) {
			return writeRequestError(output, request, "STATION_LEASE_CONTEXT_REQUIRED", "mutating station command requires an authorized lease")
		}
		return writeJSONResult(output, a.Execute(ctx, request.ID, request.Command, request.Args))
	}
	if err := validateV2LeaseRequest(request, time.Now().UTC()); err != nil {
		code, message := "STATION_LEASE_CONTEXT_REQUIRED", "lease context is missing or invalid"
		var deadlineError *leaseDeadlineError
		if errors.As(err, &deadlineError) {
			code, message = "STATION_DISPATCHER_DEADLINE_EXPIRED", "dispatcher request is outside its execution window"
		}
		return writeRequestError(output, request, code, message)
	}
	result := a.executeLeaseRequest(ctx, request)
	return writeJSONResult(output, result)
}

func isReadOnlyV2Command(command string, args []string) bool {
	switch command {
	case "identity", "status-json", "energy audit":
		return len(args) == 0
	case "service", "fmu-executor", "local-mode":
		return len(args) == 1 && args[0] == "status"
	default:
		return false
	}
}

func writeOperationStatus(output io.Writer, stateDir, id, operationID string) error {
	state, err := loadLeaseState(stateDir)
	if err != nil {
		return writeRequestError(output, Request{ID: id, Operation: "operation.status"}, "STATION_JOURNAL_UNAVAILABLE", "operation journal is unavailable")
	}
	operation, found := state.Operations[operationID]
	if !found {
		return encodeResult(output, id, "operation.status", 1, `{"operationId":"`+operationID+`","state":"not-found"}`, "", map[string]any{"operationId": operationID, "state": "not-found"})
	}
	encoded, err := json.Marshal(operation)
	if err != nil {
		return writeRequestError(output, Request{ID: id, Operation: "operation.status"}, "STATION_JOURNAL_UNAVAILABLE", "operation journal is unavailable")
	}
	return encodeResult(output, id, "operation.status", 0, string(encoded), "", map[string]any{"operationId": operationID, "state": operation.State})
}

func writeRequestError(output io.Writer, request Request, code, message string) error {
	command := request.Command
	if command == "" {
		command = request.Operation
	}
	if command == "" {
		command = "dispatcher"
	}
	return encodeResult(output, request.ID, command, 2, "", message, map[string]any{"code": code})
}

func writeRejected(output io.Writer, id, message string) error {
	return encodeResult(output, id, "dispatcher", 2, "", message, map[string]any{"code": "STATION_COMMAND_REJECTED"})
}

func encodeResult(output io.Writer, id, command string, code int, stdout, stderr string, metadata map[string]any) error {
	if id == "" {
		id = fmt.Sprintf("local-%d", time.Now().UnixNano())
	}
	outcome := "success"
	if code == 1 {
		outcome = "warning"
	} else if code >= 2 {
		outcome = "failure"
	}
	message := strings.TrimSpace(stdout)
	if message == "" {
		message = outcome
	}
	if stderr != "" {
		message = stderr
	}
	return writeJSONResult(output, Result{ID: id, Command: command, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano), Success: code < 2, ExitCode: code, Outcome: outcome, Message: message, Stdout: stdout, Stderr: stderr, Metadata: metadata})
}

func writeJSONResult(output io.Writer, result Result) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if len(encoded) > maxDispatcherResponseBytes {
		result = Result{ID: result.ID, Command: result.Command, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano), Success: false, ExitCode: 2, Outcome: "failure", Message: "dispatcher result exceeds output limit", Stderr: "dispatcher result exceeds output limit", Metadata: map[string]any{"code": "STATION_OUTPUT_LIMIT"}}
		encoded, err = json.Marshal(result)
		if err != nil {
			return err
		}
	}
	_, err = output.Write(append(encoded, '\n'))
	return err
}

func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func RejectDuplicateJSONKeys(raw []byte) error { return rejectDuplicateJSONKeys(raw) }

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok || seen[key] {
				return errors.New("duplicate JSON object key")
			}
			seen[key] = true
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("invalid JSON delimiter")
	}
}

func ServeDispatcher() error {
	configPath := os.Getenv("LABSTATION_CONFIG")
	if configPath == "" {
		configPath = "/etc/decentralabs/lab-station/station.toml"
	}
	cfg, err := loadConfigWithFallback(configPath)
	if err != nil {
		return err
	}
	if err := Dispatch(bufio.NewReader(os.Stdin), os.Stdout, New(cfg)); err != nil {
		return err
	}
	return nil
}

func loadConfigWithFallback(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		cfg = config.Defaults()
		cfg.Name, _ = os.Hostname()
		return cfg, nil
	}
	return cfg, err
}
