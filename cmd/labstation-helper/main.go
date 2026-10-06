package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type request struct {
	Operation   string `json:"operation"`
	SessionID   string `json:"sessionId"`
	Unit        string `json:"unit"`
	Action      string `json:"action"`
	SecretID    string `json:"secretId"`
	SecretValue string `json:"secretValue"`
}

var numericID = regexp.MustCompile(`^[0-9]{1,12}$`)
var secretValue = regexp.MustCompile(`^[\x20-\x7e]{32,512}$`)

func main() {
	if len(os.Args) != 1 {
		reject("helper accepts JSON input only")
	}
	request, err := decodeRequest(os.Stdin)
	if err != nil {
		reject("invalid helper request")
	}
	if err := execute(request); err != nil {
		reject("privileged operation rejected")
	}
}

func decodeRequest(input io.Reader) (request, error) {
	var request request
	raw, err := io.ReadAll(io.LimitReader(input, 8193))
	if err != nil {
		return request, err
	}
	if len(raw) > 8192 {
		return request, errors.New("helper request exceeds 8192 bytes")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return request, errors.New("helper request must contain one JSON value")
	}
	return request, nil
}

func execute(request request) error {
	switch request.Operation {
	case "terminate-session":
		if !numericID.MatchString(request.SessionID) {
			return errors.New("invalid session id")
		}
		properties, err := exec.Command("loginctl", "show-session", request.SessionID, "-p", "Name", "-p", "Remote", "-p", "Seat", "-p", "Type").Output()
		if err != nil {
			return err
		}
		values := map[string]string{}
		for _, line := range strings.Split(string(properties), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if ok {
				values[key] = value
			}
		}
		localSeat := values["Remote"] == "no" && values["Seat"] != ""
		tinyDesk := values["Remote"] == "yes" && values["Name"] == "labuser" && values["Type"] == "x11"
		if !localSeat && !tinyDesk {
			return errors.New("session is not an approved local or Tiny Desk session")
		}
		if err := exec.Command("loginctl", "kill-session", "--signal=TERM", request.SessionID).Run(); err != nil {
			return err
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if exec.Command("loginctl", "show-session", request.SessionID, "-p", "Id").Run() != nil {
				return nil
			}
			time.Sleep(250 * time.Millisecond)
		}
		return exec.Command("loginctl", "terminate-session", request.SessionID).Run()
	case "power-shutdown":
		if !fileExists("/run/openrc") && commandExists("systemctl") {
			return exec.Command("systemctl", "poweroff").Run()
		}
		return exec.Command("/sbin/poweroff").Run()
	case "power-hibernate":
		if !fileExists("/run/openrc") && commandExists("systemctl") {
			return exec.Command("systemctl", "hibernate").Run()
		}
		return exec.Command("loginctl", "hibernate").Run()
	case "power-reboot":
		if !fileExists("/run/openrc") && commandExists("systemctl") {
			return exec.Command("systemctl", "reboot").Run()
		}
		return exec.Command("/sbin/reboot").Run()
	case "service-action":
		if request.Unit != "decentralabs-labstation.service" && request.Unit != "decentralabs-labstation-fmu.service" && request.Unit != "decentralabs-labstation" && request.Unit != "decentralabs-labstation-fmu" {
			return errors.New("service is not allowlisted")
		}
		if request.Action != "start" && request.Action != "stop" && request.Action != "restart" {
			return errors.New("service action is not allowlisted")
		}
		if fileExists("/run/openrc") {
			return exec.Command("rc-service", strings.TrimSuffix(request.Unit, ".service"), request.Action).Run()
		}
		return exec.Command("systemctl", request.Action, request.Unit).Run()
	case "secret-set":
		if request.SecretID != "fmu-internal-token" || !secretValue.MatchString(request.SecretValue) {
			return errors.New("secret is not valid")
		}
		return writeSecret(request.SecretValue)
	case "secret-clear":
		if request.SecretID != "fmu-internal-token" {
			return errors.New("secret is not allowlisted")
		}
		path := filepath.Join("/etc/decentralabs/lab-station/secrets", "fmu-internal-token.env")
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return stopFMU()
	default:
		return errors.New("operation is not allowlisted")
	}
}

func writeSecret(value string) error {
	directory := "/etc/decentralabs/lab-station/secrets"
	if err := writeSecretFile(directory, value); err != nil {
		return err
	}
	return startOrRestartFMU()
}

func writeSecretFile(directory, value string) error {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0700); err != nil {
		return err
	}
	path := filepath.Join(directory, "fmu-internal-token.env")
	temporary := path + ".new"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	encoded := base64.RawURLEncoding.EncodeToString([]byte(value))
	if _, err = fmt.Fprintf(file, "FMU_INTERNAL_TOKEN_B64=%s\n", encoded); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return err
	}
	if err = file.Close(); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err = os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	if err = os.Chmod(path, 0600); err != nil {
		return err
	}
	return nil
}

func startOrRestartFMU() error {
	if fileExists("/run/openrc") {
		if exec.Command("rc-service", "decentralabs-labstation-fmu", "status").Run() == nil {
			return exec.Command("rc-service", "decentralabs-labstation-fmu", "restart").Run()
		}
		return exec.Command("rc-service", "decentralabs-labstation-fmu", "start").Run()
	}
	unit := "decentralabs-labstation-fmu.service"
	if exec.Command("systemctl", "is-active", "--quiet", unit).Run() == nil {
		return exec.Command("systemctl", "restart", unit).Run()
	}
	return exec.Command("systemctl", "start", unit).Run()
}

func stopFMU() error {
	if fileExists("/run/openrc") {
		if exec.Command("rc-service", "decentralabs-labstation-fmu", "status").Run() != nil {
			return nil
		}
		return exec.Command("rc-service", "decentralabs-labstation-fmu", "stop").Run()
	}
	unit := "decentralabs-labstation-fmu.service"
	if exec.Command("systemctl", "is-active", "--quiet", unit).Run() != nil {
		return nil
	}
	return exec.Command("systemctl", "stop", unit).Run()
}

func reject(message string)          { fmt.Fprintln(os.Stderr, message); os.Exit(2) }
func commandExists(name string) bool { _, err := exec.LookPath(name); return err == nil }
func fileExists(path string) bool    { _, err := os.Stat(path); return err == nil }
