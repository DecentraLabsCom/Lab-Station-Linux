package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var procfsRoot = "/proc"
var signalProcess = syscall.Kill
var lookupLabuserUID = func() (uint32, error) {
	account, err := user.Lookup("labuser")
	if err != nil {
		return 0, err
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(uid), nil
}

const maxTinyDeskCloseTimeout = 10 * time.Second
const tinyDeskSessionExitGrace = 2 * time.Second

func closeTinyDeskSession(ctx context.Context, request request) error {
	scope, _ := readSessionScope(ctx, request.SessionID)
	if uid, err := lookupLabuserUID(); err == nil {
		if supervisorPID, err := findTinyDeskSupervisor(procfsRoot, request.SessionID, scope, uid); err == nil {
			currentPID, findErr := findTinyDeskSupervisor(procfsRoot, request.SessionID, scope, uid)
			if findErr == nil && currentPID == supervisorPID && sessionStillMatches(ctx, request, scope) && signalProcess(supervisorPID, syscall.SIGUSR1) == nil {
				ended, waitErr := waitForTinyDeskSessionEnd(ctx, request, effectiveCloseTimeout(request.CloseTimeoutSeconds)+tinyDeskSessionExitGrace)
				if ended {
					return nil
				}
				if waitErr != nil {
					return waitErr
				}
			}
		}
	}
	return terminateApprovedTinyDeskSession(ctx, request, scope)
}

func sessionStillMatches(ctx context.Context, expected request, expectedScope string) bool {
	values, err := readSessionIdentity(ctx, expected.SessionID)
	if err != nil || !sameSessionIdentity(values, expected) {
		return false
	}
	if expectedScope == "" {
		return true
	}
	scope, err := readSessionScope(ctx, expected.SessionID)
	return err == nil && scope == expectedScope
}

func effectiveCloseTimeout(seconds int) time.Duration {
	if seconds < 1 || seconds > int(maxTinyDeskCloseTimeout/time.Second) {
		return maxTinyDeskCloseTimeout
	}
	return time.Duration(seconds) * time.Second
}

func readSessionScope(ctx context.Context, id string) (string, error) {
	properties, err := exec.CommandContext(ctx, loginctlBinary, "show-session", id, "-p", "Scope").Output()
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(properties), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && key == "Scope" && validSessionScope(value) {
			return value, nil
		}
	}
	return "", errors.New("logind did not return a valid session scope")
}

func validSessionScope(scope string) bool {
	if scope == "" || strings.ContainsAny(scope, "/\\\x00\r\n") || !strings.HasSuffix(scope, ".scope") {
		return false
	}
	return true
}

func findTinyDeskSupervisor(root, sessionID, scope string, uid uint32) (int, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, err
	}
	var matches []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || !entry.IsDir() {
			continue
		}
		processDir := filepath.Join(root, entry.Name())
		if !processHasUID(filepath.Join(processDir, "status"), uid) || !processIsSessionMember(processDir, sessionID, scope) || !isApplicationSupervisor(processDir) {
			continue
		}
		matches = append(matches, pid)
	}
	if len(matches) != 1 {
		return 0, fmt.Errorf("expected one Tiny Desk application supervisor in session, found %d", len(matches))
	}
	return matches[0], nil
}

func processHasUID(statusPath string, uid uint32) bool {
	status, err := os.ReadFile(statusPath)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(status), "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "Uid:"))
		if len(fields) == 0 {
			return false
		}
		actualUID, err := strconv.ParseUint(fields[0], 10, 32)
		return err == nil && uint32(actualUID) == uid
	}
	return false
}

func processIsSessionMember(processDir, sessionID, scope string) bool {
	if scope != "" {
		if cgroup, err := os.ReadFile(filepath.Join(processDir, "cgroup")); err == nil {
			for _, line := range strings.Split(string(cgroup), "\n") {
				parts := strings.SplitN(line, ":", 3)
				if len(parts) != 3 {
					continue
				}
				for _, component := range strings.Split(parts[2], "/") {
					if component == scope {
						return true
					}
				}
			}
			return false
		}
	}
	environment, err := os.ReadFile(filepath.Join(processDir, "environ"))
	if err != nil {
		return false
	}
	for _, entry := range strings.Split(string(environment), "\x00") {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == "XDG_SESSION_ID" && value == sessionID {
			return true
		}
	}
	return false
}

func isApplicationSupervisor(processDir string) bool {
	executable, err := os.Readlink(filepath.Join(processDir, "exe"))
	if err != nil || filepath.Base(executable) != "labstationctl" {
		return false
	}
	commandLine, err := os.ReadFile(filepath.Join(processDir, "cmdline"))
	if err != nil {
		return false
	}
	arguments := strings.Split(strings.TrimRight(string(commandLine), "\x00"), "\x00")
	return len(arguments) == 3 && filepath.Base(arguments[0]) == "labstationctl" && arguments[1] == "app" && arguments[2] == "launch"
}

func waitForTinyDeskSessionEnd(ctx context.Context, expected request, timeout time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		values, err := readSessionIdentity(ctx, expected.SessionID)
		if err == nil {
			if !sameSessionIdentity(values, expected) {
				return false, errors.New("session identity changed while waiting for controlled close")
			}
		} else {
			exists, listErr := sessionIDExists(ctx, expected.SessionID)
			if listErr != nil {
				return false, listErr
			}
			if !exists {
				return true, nil
			}
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
	return false, nil
}

func sessionIDExists(ctx context.Context, id string) (bool, error) {
	output, err := exec.CommandContext(ctx, loginctlBinary, "list-sessions", "--no-legend").Output()
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == id {
			return true, nil
		}
	}
	return false, nil
}

func terminateApprovedTinyDeskSession(ctx context.Context, expected request, expectedScope string) error {
	values, err := readSessionIdentity(ctx, expected.SessionID)
	if err != nil {
		exists, listErr := sessionIDExists(ctx, expected.SessionID)
		if listErr == nil && !exists {
			return nil
		}
		return errors.New("unable to revalidate Tiny Desk session before forced termination")
	}
	if !sameSessionIdentity(values, expected) || !approvedSessionIdentity(values["Name"], values["Remote"], values["Seat"], values["Type"]) {
		return errors.New("session identity changed before forced termination")
	}
	if expectedScope != "" {
		scope, err := readSessionScope(ctx, expected.SessionID)
		if err != nil || scope != expectedScope {
			return errors.New("session scope changed before forced termination")
		}
	}
	if err := exec.CommandContext(ctx, loginctlBinary, "terminate-session", expected.SessionID).Run(); err != nil {
		exists, listErr := sessionIDExists(ctx, expected.SessionID)
		if listErr == nil && !exists {
			return nil
		}
		return err
	}
	return nil
}
