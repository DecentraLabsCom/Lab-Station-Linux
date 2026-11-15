package main

import (
	"context"
	"errors"
	"os/user"
	"strings"
	"testing"
)

func TestConfirmLocalSessionRequiresAnAffirmativeGraphicalResponse(t *testing.T) {
	oldLookup := lookupSessionAccount
	oldRuntimeDir := sessionRuntimeDirectory
	oldInspect := inspectLocalGraphicalSession
	oldValidateRuntimeDir := validateSessionRuntimeDirectory
	oldRunPrompt := runSessionPrompt
	oldReadIdentity := readConfirmationSessionIdentity
	t.Cleanup(func() {
		lookupSessionAccount = oldLookup
		sessionRuntimeDirectory = oldRuntimeDir
		inspectLocalGraphicalSession = oldInspect
		validateSessionRuntimeDirectory = oldValidateRuntimeDir
		runSessionPrompt = oldRunPrompt
		readConfirmationSessionIdentity = oldReadIdentity
	})

	lookupSessionAccount = func(username string) (*user.User, error) {
		if username != "instructor" {
			t.Fatalf("looked up unexpected account %q", username)
		}
		return &user.User{Username: username, Uid: "1500", Gid: "1500", HomeDir: "/home/instructor"}, nil
	}
	sessionRuntimeDirectory = func(uid uint32) string {
		if uid != 1500 {
			t.Fatalf("runtime directory requested for unexpected uid %d", uid)
		}
		return "/run/user/1500"
	}
	inspectLocalGraphicalSession = func(_ context.Context, id string, uid uint32) (map[string]string, error) {
		if id != "seat-3" || uid != 1500 {
			t.Fatalf("unexpected session inspection: id=%q uid=%d", id, uid)
		}
		return map[string]string{"DISPLAY": ":0", "DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1500/bus"}, nil
	}
	validateSessionRuntimeDirectory = func(path string, uid uint32) error {
		if path != "/run/user/1500" || uid != 1500 {
			t.Fatalf("unexpected runtime-directory validation: path=%q uid=%d", path, uid)
		}
		return nil
	}
	readConfirmationSessionIdentity = func(_ context.Context, id string) (map[string]string, error) {
		return map[string]string{"Name": "instructor", "Remote": "no", "Seat": "seat0", "Type": "x11"}, nil
	}

	var promptArgs, promptEnv []string
	var promptUID, promptGID uint32
	runSessionPrompt = func(_ context.Context, args, environment []string, uid, gid uint32, groups []uint32) error {
		promptArgs = append([]string(nil), args...)
		promptEnv = append([]string(nil), environment...)
		promptUID, promptGID = uid, gid
		if len(groups) == 0 || groups[0] != gid {
			t.Fatalf("prompt did not receive the account's primary group: %#v", groups)
		}
		return nil
	}

	req := request{
		Operation: "confirm-local-session", SessionID: "seat-3", SessionUser: "instructor",
		SessionRemote: "no", SessionSeat: "seat0", SessionType: "x11",
		Message: "A reservation is starting", TimeoutSeconds: 30,
	}
	if err := confirmLocalSession(context.Background(), req); err != nil {
		t.Fatalf("affirmative graphical acknowledgement failed: %v", err)
	}
	if promptUID != 1500 || promptGID != 1500 || !containsArgument(promptArgs, "--question") || !containsArgument(promptArgs, "--timeout=30") {
		t.Fatalf("confirmation prompt was not targeted or bounded: uid=%d gid=%d args=%#v", promptUID, promptGID, promptArgs)
	}
	if !strings.Contains(joinArguments(promptArgs), "Keep my session open") || !strings.Contains(joinArguments(promptArgs), "I have saved my work") {
		t.Fatalf("prompt does not explain acknowledgement and refusal: %#v", promptArgs)
	}
	if containsArgumentPrefix(promptEnv, "LD_PRELOAD=") || !containsArgumentPrefix(promptEnv, "DISPLAY=:0") {
		t.Fatalf("prompt did not receive a restricted graphical environment: %#v", promptEnv)
	}

	runSessionPrompt = func(context.Context, []string, []string, uint32, uint32, []uint32) error {
		return errors.New("zenity returned timeout or cancel")
	}
	if err := confirmLocalSession(context.Background(), req); err == nil {
		t.Fatal("cancelled or timed-out dialog was accepted as acknowledgement")
	}
}

func TestConfirmLocalSessionRejectsNonLocalOrNonGraphicalTargets(t *testing.T) {
	oldRunPrompt := runSessionPrompt
	t.Cleanup(func() { runSessionPrompt = oldRunPrompt })
	called := false
	runSessionPrompt = func(context.Context, []string, []string, uint32, uint32, []uint32) error {
		called = true
		return nil
	}
	base := request{Operation: "confirm-local-session", SessionID: "seat-4", SessionUser: "instructor", SessionRemote: "no", SessionSeat: "seat0", SessionType: "wayland", Message: "Save", TimeoutSeconds: 30}
	for _, mutate := range []func(*request){
		func(value *request) { value.SessionRemote = "yes" },
		func(value *request) { value.SessionSeat = "" },
		func(value *request) { value.SessionType = "tty" },
		func(value *request) { value.TimeoutSeconds = 0 },
	} {
		value := base
		mutate(&value)
		if err := confirmLocalSession(context.Background(), value); err == nil {
			t.Fatalf("unsafe confirmation request was accepted: %#v", value)
		}
	}
	if called {
		t.Fatal("invalid session request reached the graphical prompt")
	}
}

func containsArgument(arguments []string, expected string) bool {
	return containsArgumentPrefix(arguments, expected)
}

func containsArgumentPrefix(arguments []string, prefix string) bool {
	for _, argument := range arguments {
		if argument == prefix || strings.HasPrefix(argument, prefix) {
			return true
		}
	}
	return false
}

func joinArguments(arguments []string) string { return strings.Join(arguments, "\n") }
