package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/decentralabs/lab-station-linux/internal/agent"
)

type request struct {
	Operation           string `json:"operation"`
	SessionID           string `json:"sessionId"`
	Unit                string `json:"unit"`
	Action              string `json:"action"`
	SecretID            string `json:"secretId"`
	SecretValue         string `json:"secretValue"`
	SessionUser         string `json:"sessionUser"`
	SessionRemote       string `json:"sessionRemote"`
	SessionSeat         string `json:"sessionSeat"`
	SessionType         string `json:"sessionType"`
	ForceSession        bool   `json:"forceSession"`
	CloseTimeoutSeconds int    `json:"closeTimeoutSeconds,omitempty"`
	StateDir            string `json:"stateDir"`
}

var sessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)
var secretValue = regexp.MustCompile(`^[\x20-\x7e]{32,512}$`)
var loginctlBinary = "/usr/bin/loginctl"
var systemctlBinary = "/usr/bin/systemctl"
var rcServiceBinary = "/usr/sbin/rc-service"

const (
	internalTinyDeskCleanup = "--internal-clean-tiny-desk"
	internalFMUCleanup      = "--internal-clean-fmu-temp"
)

func main() {
	if len(os.Args) == 2 && strings.HasPrefix(os.Args[1], "--internal-clean-") {
		if err := executeInternalCleanup(os.Args[1]); err != nil {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 1 {
		reject("helper accepts JSON input only")
	}
	request, err := decodeRequest(os.Stdin)
	if err != nil {
		reject("invalid helper request")
	}
	if err := authorizeCaller(request.Operation); err != nil {
		reject("helper caller is not authorized for this operation")
	}
	if request.Operation == "lease-admission" {
		if err := agent.CheckLeaseAdmission(request.StateDir, time.Now().UTC()); err != nil {
			reject("no active reservation permits application access")
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]bool{"allowed": true})
		return
	}
	if request.Operation == "secret-status" {
		configured := secretConfigured()
		_ = json.NewEncoder(os.Stdout).Encode(map[string]bool{"configured": configured})
		return
	}
	if request.Operation == "labuser-password-status" {
		configured, err := labuserPasswordConfigured()
		if err != nil {
			reject("unable to read labuser password status")
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]bool{"configured": configured})
		return
	}
	if err := execute(request); err != nil {
		reject("privileged operation rejected")
	}
}

func authorizeCaller(operation string) error {
	if os.Geteuid() != 0 {
		return errors.New("helper must run as root")
	}
	callerUID := strings.TrimSpace(os.Getenv("SUDO_UID"))
	if callerUID == "" {
		if os.Getuid() == 0 {
			return nil
		}
		return errors.New("sudo caller identity is missing")
	}
	caller, err := user.LookupId(callerUID)
	if err != nil {
		return errors.New("sudo caller identity is invalid")
	}
	if !callerCanRunOperation(caller.Username, operation) {
		return errors.New("caller is not allowed to use this helper operation")
	}
	return nil
}

func callerCanRunOperation(username, operation string) bool {
	if username == "root" {
		return true
	}
	switch requestOperationClass(operation) {
	case "admission":
		return username == "labuser"
	case "control":
		return username == "labstationd"
	}
	return false
}

func requestOperationClass(operation string) string {
	switch operation {
	case "lease-admission":
		return "admission"
	case "terminate-session", "power-shutdown", "power-hibernate", "power-reboot", "service-action", "fmu-clean-temp", "tiny-desk-clean", "secret-set", "secret-clear", "secret-status", "labuser-password-status":
		return "control"
	default:
		return ""
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
	if err := agent.RejectDuplicateJSONKeys(raw); err != nil {
		return request, err
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
	ctx, cancel := context.WithTimeout(context.Background(), helperOperationBudget(request.Operation))
	defer cancel()
	return executeWithContext(ctx, request)
}

func helperOperationBudget(operation string) time.Duration {
	switch operation {
	case "terminate-session":
		return 25 * time.Second
	case "service-action", "fmu-clean-temp", "tiny-desk-clean", "secret-set", "secret-clear":
		return 90 * time.Second
	case "power-shutdown", "power-hibernate", "power-reboot":
		return 30 * time.Second
	default:
		return 15 * time.Second
	}
}

func executeWithContext(ctx context.Context, request request) error {
	switch request.Operation {
	case "terminate-session":
		if !sessionID.MatchString(request.SessionID) {
			return errors.New("invalid session id")
		}
		if !approvedSessionIdentity(request.SessionUser, request.SessionRemote, request.SessionSeat, request.SessionType) {
			return errors.New("expected session identity is invalid")
		}
		values, err := readSessionIdentity(ctx, request.SessionID)
		if err != nil || !sameSessionIdentity(values, request) || !approvedSessionIdentity(values["Name"], values["Remote"], values["Seat"], values["Type"]) {
			return errors.New("session is not an approved local or Tiny Desk session")
		}
		values, err = readSessionIdentity(ctx, request.SessionID)
		if err != nil || !sameSessionIdentity(values, request) {
			return errors.New("session identity changed before termination")
		}
		if request.ForceSession {
			if request.SessionUser != "labuser" || request.SessionRemote != "yes" || request.SessionType != "x11" {
				return errors.New("forced termination is limited to remote Tiny Desk sessions")
			}
			return closeTinyDeskSession(ctx, request)
		}
		if err := exec.CommandContext(ctx, loginctlBinary, "kill-session", "--signal=TERM", request.SessionID).Run(); err != nil {
			return err
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if _, err := readSessionIdentity(ctx, request.SessionID); err != nil {
				return nil
			}
			timer := time.NewTimer(250 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		values, err = readSessionIdentity(ctx, request.SessionID)
		if err != nil || !sameSessionIdentity(values, request) {
			return errors.New("session identity changed before forced termination")
		}
		return exec.CommandContext(ctx, loginctlBinary, "terminate-session", request.SessionID).Run()
	case "power-shutdown":
		if !fileExists("/run/openrc") && commandExists("systemctl") {
			return runSystemctl(ctx, "poweroff")
		}
		return runApprovedCommand(ctx, []string{"/sbin/poweroff", "/usr/sbin/poweroff"})
	case "power-hibernate":
		if !fileExists("/run/openrc") && commandExists("systemctl") {
			return runSystemctl(ctx, "hibernate")
		}
		return runApprovedCommand(ctx, []string{loginctlBinary, "/bin/loginctl"}, "hibernate")
	case "power-reboot":
		if !fileExists("/run/openrc") && commandExists("systemctl") {
			return runSystemctl(ctx, "reboot")
		}
		return runApprovedCommand(ctx, []string{"/sbin/reboot", "/usr/sbin/reboot"})
	case "service-action":
		if request.Unit != "decentralabs-labstation.service" && request.Unit != "decentralabs-labstation-fmu.service" && request.Unit != "decentralabs-labstation" && request.Unit != "decentralabs-labstation-fmu" {
			return errors.New("service is not allowlisted")
		}
		if request.Action != "start" && request.Action != "stop" && request.Action != "restart" {
			return errors.New("service action is not allowlisted")
		}
		if fileExists("/run/openrc") {
			return runApprovedCommand(ctx, []string{rcServiceBinary, "/sbin/rc-service", "/usr/bin/rc-service"}, strings.TrimSuffix(request.Unit, ".service"), request.Action)
		}
		return runSystemctl(ctx, request.Action, request.Unit)
	case "fmu-clean-temp":
		return runManagedCleanup(ctx, "labstation-fmu", internalFMUCleanup)
	case "tiny-desk-clean":
		return runManagedCleanup(ctx, "labuser", internalTinyDeskCleanup)
	case "secret-set":
		if request.SecretID != "fmu-internal-token" || !secretValue.MatchString(request.SecretValue) {
			return errors.New("secret is not valid")
		}
		return writeSecret(ctx, request.SecretValue)
	case "secret-clear":
		if request.SecretID != "fmu-internal-token" {
			return errors.New("secret is not allowlisted")
		}
		path := filepath.Join("/etc/decentralabs/lab-station/secrets", "fmu-internal-token.env")
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return stopFMU(ctx)
	case "secret-status":
		return nil
	case "lease-admission":
		return errors.New("lease admission is handled by the helper entrypoint")
	default:
		return errors.New("operation is not allowlisted")
	}
}

func executeInternalCleanup(operation string) error {
	username, uid, gid, err := internalCleanupIdentity(operation)
	if err != nil {
		return err
	}
	if os.Geteuid() != int(uid) || os.Getegid() != int(gid) {
		return errors.New("internal cleanup must run as its workspace owner")
	}
	switch operation {
	case internalTinyDeskCleanup:
		if username != "labuser" {
			return errors.New("Tiny Desk cleanup identity is invalid")
		}
		return agent.CleanTinyDeskWorkingData()
	case internalFMUCleanup:
		if username != "labstation-fmu" {
			return errors.New("FMU cleanup identity is invalid")
		}
		return cleanFMUTemporaryState(context.Background())
	default:
		return errors.New("internal cleanup operation is not allowlisted")
	}
}

func internalCleanupIdentity(operation string) (string, uint32, uint32, error) {
	username := ""
	switch operation {
	case internalTinyDeskCleanup:
		username = "labuser"
	case internalFMUCleanup:
		username = "labstation-fmu"
	default:
		return "", 0, 0, errors.New("internal cleanup operation is not allowlisted")
	}
	account, err := user.Lookup(username)
	if err != nil {
		return "", 0, 0, err
	}
	uid, uidErr := strconv.ParseUint(account.Uid, 10, 32)
	gid, gidErr := strconv.ParseUint(account.Gid, 10, 32)
	if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 {
		return "", 0, 0, errors.New("managed cleanup account is invalid")
	}
	return username, uint32(uid), uint32(gid), nil
}

func managedCleanupCommand(ctx context.Context, username, operation string) (*exec.Cmd, error) {
	expectedUsername, uid, gid, err := internalCleanupIdentity(operation)
	if err != nil || username != expectedUsername {
		return nil, errors.New("managed cleanup identity is not allowlisted")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return commandWithCredentials(ctx, executable, []string{operation}, uid, gid), nil
}

func commandWithCredentials(ctx context.Context, executable string, args []string, uid, gid uint32) *exec.Cmd {
	command := exec.CommandContext(ctx, executable, args...)
	command.Env = []string{"PATH=/usr/bin:/bin"}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: []uint32{gid}}}
	return command
}

func runManagedCleanup(ctx context.Context, username, operation string) error {
	if os.Geteuid() != 0 {
		return errors.New("managed cleanup launcher must run as root")
	}
	command, err := managedCleanupCommand(ctx, username, operation)
	if err != nil {
		return err
	}
	return command.Run()
}

func cleanFMUTemporaryState(ctx context.Context) error {
	root := "/var/lib/decentralabs/fmu-executor/fmu-data"
	for _, path := range []string{
		"/var/lib",
		"/var/lib/decentralabs",
		"/var/lib/decentralabs/fmu-executor",
		root,
	} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("FMU data path is not a managed directory")
		}
	}
	return cleanFMUTemporaryDirectory(ctx, root)
}

func cleanFMUTemporaryDirectory(ctx context.Context, root string) error {
	info, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("FMU data path is not a managed directory")
	}
	temporary := filepath.Join(root, ".tmp")
	info, err = os.Lstat(temporary)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("FMU temporary path is not a managed directory")
	}
	entries, err := os.ReadDir(temporary)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(temporary, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func readSessionIdentity(ctx context.Context, id string) (map[string]string, error) {
	properties, err := exec.CommandContext(ctx, loginctlBinary, "show-session", id, "-p", "Name", "-p", "Remote", "-p", "Seat", "-p", "Type").Output()
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(properties), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	if values["Name"] == "" || values["Remote"] != "yes" && values["Remote"] != "no" {
		return nil, errors.New("session identity is incomplete")
	}
	return values, nil
}

func sameSessionIdentity(actual map[string]string, expected request) bool {
	return actual["Name"] == expected.SessionUser && actual["Remote"] == expected.SessionRemote && actual["Seat"] == expected.SessionSeat && actual["Type"] == expected.SessionType
}

func approvedSessionIdentity(name, remote, seat, sessionType string) bool {
	tinyDesk := remote == "yes" && name == "labuser" && sessionType == "x11"
	localSeat := remote == "no" && seat != "" && name != "root" && name != "labstationd" && name != "labstation-ops" && name != "labstation-fmu"
	return tinyDesk || localSeat
}

func secretConfigured() bool {
	return secretConfiguredAt("/etc/decentralabs/lab-station/secrets")
}

func labuserPasswordConfigured() (bool, error) {
	out, err := exec.Command("/usr/bin/passwd", "-S", "labuser").Output()
	if err != nil {
		return false, err
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 || fields[0] != "labuser" {
		return false, errors.New("passwd returned an invalid labuser status")
	}
	return fields[1] == "P", nil
}

func secretConfiguredAt(directory string) bool {
	dirInfo, err := os.Lstat(directory)
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode()&os.ModeSymlink != 0 || dirInfo.Mode().Perm() != 0o700 {
		return false
	}
	fileInfo, err := os.Lstat(filepath.Join(directory, "fmu-internal-token.env"))
	if err != nil || !fileInfo.Mode().IsRegular() || fileInfo.Mode()&os.ModeSymlink != 0 || fileInfo.Mode().Perm() != 0o600 {
		return false
	}
	stat, ok := fileInfo.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0
}

func writeSecret(ctx context.Context, value string) error {
	directory := "/etc/decentralabs/lab-station/secrets"
	if err := writeSecretFile(directory, value); err != nil {
		return err
	}
	return startOrRestartFMU(ctx)
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

func startOrRestartFMU(ctx context.Context) error {
	if fileExists("/run/openrc") {
		if runApprovedCommand(ctx, []string{rcServiceBinary, "/sbin/rc-service", "/usr/bin/rc-service"}, "decentralabs-labstation-fmu", "status") == nil {
			return runApprovedCommand(ctx, []string{rcServiceBinary, "/sbin/rc-service", "/usr/bin/rc-service"}, "decentralabs-labstation-fmu", "restart")
		}
		return runApprovedCommand(ctx, []string{rcServiceBinary, "/sbin/rc-service", "/usr/bin/rc-service"}, "decentralabs-labstation-fmu", "start")
	}
	unit := "decentralabs-labstation-fmu.service"
	if runSystemctl(ctx, "is-active", "--quiet", unit) == nil {
		return runSystemctl(ctx, "restart", unit)
	}
	return runSystemctl(ctx, "start", unit)
}

func stopFMU(ctx context.Context) error {
	if fileExists("/run/openrc") {
		if runApprovedCommand(ctx, []string{rcServiceBinary, "/sbin/rc-service", "/usr/bin/rc-service"}, "decentralabs-labstation-fmu", "status") != nil {
			return nil
		}
		return runApprovedCommand(ctx, []string{rcServiceBinary, "/sbin/rc-service", "/usr/bin/rc-service"}, "decentralabs-labstation-fmu", "stop")
	}
	unit := "decentralabs-labstation-fmu.service"
	if runSystemctl(ctx, "is-active", "--quiet", unit) != nil {
		return nil
	}
	return runSystemctl(ctx, "stop", unit)
}

func runSystemctl(ctx context.Context, args ...string) error {
	return runApprovedCommand(ctx, []string{systemctlBinary, "/bin/systemctl", "/usr/sbin/systemctl", "/sbin/systemctl"}, args...)
}

func runApprovedCommand(ctx context.Context, candidates []string, args ...string) error {
	for _, path := range candidates {
		if path == "" || !filepath.IsAbs(path) {
			continue
		}
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return exec.CommandContext(ctx, path, args...).Run()
		}
	}
	return errors.New("approved host command is unavailable")
}

func reject(message string)          { fmt.Fprintln(os.Stderr, message); os.Exit(2) }
func commandExists(name string) bool { _, err := exec.LookPath(name); return err == nil }
func fileExists(path string) bool    { _, err := os.Stat(path); return err == nil }
