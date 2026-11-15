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
)

const maxSessionPromptSeconds = 90

var zenityBinary = "/usr/bin/zenity"
var lookupSessionAccount = user.Lookup
var sessionRuntimeDirectory = func(uid uint32) string { return filepath.Join("/run/user", strconv.FormatUint(uint64(uid), 10)) }
var runSessionPrompt = runZenityPrompt
var inspectLocalGraphicalSession = inspectLocalGraphicalEnvironment
var validateSessionRuntimeDirectory = validateUserRuntimeDirectory
var readConfirmationSessionIdentity = readSessionIdentity

func confirmLocalSession(ctx context.Context, request request) error {
	if !sessionID.MatchString(request.SessionID) || request.TimeoutSeconds < 1 || request.TimeoutSeconds > maxSessionPromptSeconds {
		return errors.New("local session confirmation request is invalid")
	}
	if len(request.Message) == 0 || len(request.Message) > 256 || strings.ContainsRune(request.Message, '\x00') {
		return errors.New("local session confirmation message is invalid")
	}
	if !approvedLocalSeatIdentity(request.SessionUser, request.SessionRemote, request.SessionSeat, request.SessionType) {
		return errors.New("confirmation is limited to an interactive local user session")
	}
	identity, err := readConfirmationSessionIdentity(ctx, request.SessionID)
	if err != nil || !sameSessionIdentity(identity, request) || !approvedLocalSeatIdentity(identity["Name"], identity["Remote"], identity["Seat"], identity["Type"]) {
		return errors.New("local session identity changed before confirmation")
	}

	account, err := lookupSessionAccount(request.SessionUser)
	if err != nil {
		return errors.New("local session account is unavailable")
	}
	uid, uidErr := strconv.ParseUint(account.Uid, 10, 32)
	gid, gidErr := strconv.ParseUint(account.Gid, 10, 32)
	if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 {
		return errors.New("local session account identity is invalid")
	}

	environment, err := inspectLocalGraphicalSession(ctx, request.SessionID, uint32(uid))
	if err != nil {
		return errors.New("local graphical session environment is unavailable")
	}
	if environment["DISPLAY"] == "" && environment["WAYLAND_DISPLAY"] == "" {
		return errors.New("local graphical display is unavailable")
	}

	runtimeDir := sessionRuntimeDirectory(uint32(uid))
	if err := validateSessionRuntimeDirectory(runtimeDir, uint32(uid)); err != nil {
		return errors.New("local user runtime directory is unavailable or has the wrong owner")
	}

	if environment["XDG_RUNTIME_DIR"] == "" {
		environment["XDG_RUNTIME_DIR"] = runtimeDir
	}
	if environment["DBUS_SESSION_BUS_ADDRESS"] == "" {
		environment["DBUS_SESSION_BUS_ADDRESS"] = "unix:path=" + filepath.Join(runtimeDir, "bus")
	}
	if environment["XDG_SESSION_TYPE"] == "" {
		environment["XDG_SESSION_TYPE"] = request.SessionType
	}
	if environment["XAUTHORITY"] == "" && request.SessionType == "x11" {
		authority := filepath.Join(account.HomeDir, ".Xauthority")
		if _, err := os.Stat(authority); err == nil {
			environment["XAUTHORITY"] = authority
		}
	}

	groups := []uint32{uint32(gid)}
	if groupIDs, groupErr := account.GroupIds(); groupErr == nil {
		for _, value := range groupIDs {
			parsed, parseErr := strconv.ParseUint(value, 10, 32)
			if parseErr != nil {
				continue
			}
			group := uint32(parsed)
			if !containsGroup(groups, group) {
				groups = append(groups, group)
			}
		}
	}

	body := request.Message + "\n\nSave your work, then choose “I have saved my work” to let Prepare close this session after the remaining grace period. Choose “Keep my session open” or do not respond before the timeout to cancel Prepare."
	args := []string{
		"--question",
		"--no-markup",
		"--title=Lab Station reservation",
		"--text=" + body,
		"--ok-label=I have saved my work",
		"--cancel-label=Keep my session open",
		"--timeout=" + strconv.Itoa(request.TimeoutSeconds),
	}
	if err := runSessionPrompt(ctx, args, sessionEnvironment(account, environment), uint32(uid), uint32(gid), groups); err != nil {
		return errors.New("local session did not acknowledge the Prepare warning")
	}
	return nil
}

func inspectLocalGraphicalEnvironment(ctx context.Context, sessionID string, uid uint32) (map[string]string, error) {
	leader, display, err := readSessionPromptProperties(ctx, sessionID)
	if err != nil || !processHasUID(filepath.Join(procfsRoot, strconv.Itoa(leader), "status"), uid) {
		return nil, errors.New("local graphical session leader is unavailable")
	}
	environment, err := readSessionProcessEnvironment(filepath.Join(procfsRoot, strconv.Itoa(leader), "environ"))
	if err != nil {
		return nil, err
	}
	if display != "" && environment["DISPLAY"] == "" {
		environment["DISPLAY"] = display
	}
	return environment, nil
}

func validateUserRuntimeDirectory(path string, uid uint32) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("runtime directory is unavailable")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uid {
		return errors.New("runtime directory owner is invalid")
	}
	return nil
}

func approvedLocalSeatIdentity(name, remote, seat, sessionType string) bool {
	if name == "" || name == "root" || name == "labstationd" || name == "labstation-ops" || name == "labstation-fmu" {
		return false
	}
	if remote != "no" || seat == "" || !sessionID.MatchString(seat) {
		return false
	}
	return sessionType == "x11" || sessionType == "wayland"
}

func readSessionPromptProperties(ctx context.Context, id string) (int, string, error) {
	output, err := exec.CommandContext(ctx, loginctlBinary, "show-session", id, "-p", "Leader", "-p", "Display").Output()
	if err != nil {
		return 0, "", err
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	leader, err := strconv.Atoi(values["Leader"])
	if err != nil || leader <= 0 {
		return 0, "", errors.New("logind did not return a valid session leader")
	}
	return leader, values["Display"], nil
}

func readSessionProcessEnvironment(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{
		"DISPLAY": true, "WAYLAND_DISPLAY": true, "XAUTHORITY": true,
		"DBUS_SESSION_BUS_ADDRESS": true, "XDG_RUNTIME_DIR": true,
		"XDG_SESSION_TYPE": true,
	}
	environment := map[string]string{}
	for _, entry := range strings.Split(string(data), "\x00") {
		key, value, ok := strings.Cut(entry, "=")
		if ok && allowed[key] && !strings.ContainsAny(value, "\r\n\x00") {
			environment[key] = value
		}
	}
	return environment, nil
}

func sessionEnvironment(account *user.User, session map[string]string) []string {
	values := map[string]string{
		"PATH": "/usr/bin:/bin", "HOME": account.HomeDir,
		"USER": account.Username, "LOGNAME": account.Username,
	}
	for key, value := range session {
		if value != "" {
			values[key] = value
		}
	}
	keys := []string{"PATH", "HOME", "USER", "LOGNAME", "DISPLAY", "WAYLAND_DISPLAY", "XAUTHORITY", "DBUS_SESSION_BUS_ADDRESS", "XDG_RUNTIME_DIR", "XDG_SESSION_TYPE"}
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		if value, ok := values[key]; ok {
			environment = append(environment, key+"="+value)
		}
	}
	return environment
}

func runZenityPrompt(ctx context.Context, args, environment []string, uid, gid uint32, groups []uint32) error {
	if os.Geteuid() != 0 {
		return errors.New("graphical session prompt must be launched by the privileged helper")
	}
	info, err := os.Stat(zenityBinary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return errors.New("zenity is unavailable")
	}
	command := exec.CommandContext(ctx, zenityBinary, args...)
	command.Env = environment
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid, Groups: groups}}
	if err := command.Run(); err != nil {
		return fmt.Errorf("zenity was not affirmatively acknowledged: %w", err)
	}
	return nil
}

func containsGroup(groups []uint32, expected uint32) bool {
	for _, group := range groups {
		if group == expected {
			return true
		}
	}
	return false
}
