package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func TestFindTinyDeskSupervisorMatchesSessionScopeAndCommand(t *testing.T) {
	procRoot := t.TempDir()
	writeSessionProcess(t, procRoot, 101, 1002, "session-rdp-7.scope", "/usr/lib/decentralabs/lab-station/labstationctl", "app", "launch")
	writeSessionProcess(t, procRoot, 102, 1002, "session-rdp-8.scope", "/usr/lib/decentralabs/lab-station/labstationctl", "app", "launch")
	writeSessionProcess(t, procRoot, 103, 1002, "session-rdp-7.scope", "/usr/lib/decentralabs/lab-station/labstationctl", "status")
	writeSessionProcess(t, procRoot, 104, 1003, "session-rdp-7.scope", "/usr/lib/decentralabs/lab-station/labstationctl", "app", "launch")
	writeSessionProcess(t, procRoot, 105, 1002, "session-rdp-7.scope", "/opt/lab/apps/demo", "app", "launch")

	pid, err := findTinyDeskSupervisor(procRoot, "rdp-7", "session-rdp-7.scope", 1002)
	if err != nil {
		t.Fatal(err)
	}
	if pid != 101 {
		t.Fatalf("supervisor PID = %d, want 101", pid)
	}
}

func TestFindTinyDeskSupervisorRejectsAmbiguousSession(t *testing.T) {
	procRoot := t.TempDir()
	writeSessionProcess(t, procRoot, 101, 1002, "session-rdp-7.scope", "/usr/bin/labstationctl", "app", "launch")
	writeSessionProcess(t, procRoot, 102, 1002, "session-rdp-7.scope", "/usr/bin/labstationctl", "app", "launch")
	if _, err := findTinyDeskSupervisor(procRoot, "rdp-7", "session-rdp-7.scope", 1002); err == nil {
		t.Fatal("ambiguous session supervisor was accepted")
	}
}

func TestFindTinyDeskSupervisorFallsBackToLogindSessionEnvironment(t *testing.T) {
	procRoot := t.TempDir()
	writeSessionProcess(t, procRoot, 101, 1002, "another-session.scope", "/usr/bin/labstationctl", "app", "launch")
	pid, err := findTinyDeskSupervisor(procRoot, "rdp-7", "", 1002)
	if err != nil {
		t.Fatal(err)
	}
	if pid != 101 {
		t.Fatalf("supervisor PID = %d, want the process identified by XDG_SESSION_ID", pid)
	}
}

func TestForcedTinyDeskSessionSignalsSupervisorBeforeLogindFallback(t *testing.T) {
	root := t.TempDir()
	procRoot := filepath.Join(root, "proc")
	if err := os.Mkdir(procRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	const supervisorPID = 101
	writeSessionProcess(t, procRoot, supervisorPID, 1002, "session-rdp-7.scope", "/usr/bin/labstationctl", "app", "launch")
	ended := filepath.Join(root, "session-ended")
	forced := filepath.Join(root, "session-forced")
	loginctl := filepath.Join(root, "loginctl")
	script := `#!/bin/sh
if [ "$1" = "show-session" ]; then
  [ -f "$TEST_SESSION_ENDED" ] && exit 1
  printf 'Name=labuser\nRemote=yes\nSeat=\nType=x11\nScope=session-rdp-7.scope\n'
  exit 0
fi
if [ "$1" = "list-sessions" ]; then
  [ -f "$TEST_SESSION_ENDED" ] && exit 0
  printf 'rdp-7 1002 labuser\n'
  exit 0
fi
if [ "$1" = "terminate-session" ]; then touch "$TEST_SESSION_FORCED"; exit 0; fi
exit 4
`
	writeExecutable(t, loginctl, script)
	oldLoginctl, oldProcRoot, oldLookup, oldSignal := loginctlBinary, procfsRoot, lookupLabuserUID, signalProcess
	loginctlBinary = loginctl
	procfsRoot = procRoot
	lookupLabuserUID = func() (uint32, error) { return 1002, nil }
	signalProcess = func(pid int, signal syscall.Signal) error {
		if pid != supervisorPID || signal != syscall.SIGUSR1 {
			return fmt.Errorf("unexpected supervisor signal: pid=%d signal=%d", pid, signal)
		}
		return os.WriteFile(ended, []byte("closed"), 0o600)
	}
	t.Cleanup(func() {
		loginctlBinary, procfsRoot, lookupLabuserUID, signalProcess = oldLoginctl, oldProcRoot, oldLookup, oldSignal
	})
	t.Setenv("TEST_SESSION_ENDED", ended)
	t.Setenv("TEST_SESSION_FORCED", forced)

	err := execute(request{
		Operation: "terminate-session", SessionID: "rdp-7", SessionUser: "labuser",
		SessionRemote: "yes", SessionType: "x11", ForceSession: true,
		CloseTimeoutSeconds: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(forced); err == nil {
		t.Fatal("logind force termination ran even though the controlled close ended the session")
	}
}

func TestForcedTinyDeskSessionUsesLogindWhenSupervisorIsMissing(t *testing.T) {
	root := t.TempDir()
	procRoot := filepath.Join(root, "proc")
	if err := os.Mkdir(procRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	forced := filepath.Join(root, "session-forced")
	loginctl := filepath.Join(root, "loginctl")
	script := `#!/bin/sh
if [ "$1" = "show-session" ]; then
  printf 'Name=labuser\nRemote=yes\nSeat=\nType=x11\nScope=session-rdp-7.scope\n'
  exit 0
fi
if [ "$1" = "terminate-session" ]; then touch "$TEST_SESSION_FORCED"; exit 0; fi
exit 4
`
	writeExecutable(t, loginctl, script)
	oldLoginctl, oldProcRoot, oldLookup := loginctlBinary, procfsRoot, lookupLabuserUID
	loginctlBinary = loginctl
	procfsRoot = procRoot
	lookupLabuserUID = func() (uint32, error) { return 1002, nil }
	t.Cleanup(func() { loginctlBinary, procfsRoot, lookupLabuserUID = oldLoginctl, oldProcRoot, oldLookup })
	t.Setenv("TEST_SESSION_FORCED", forced)

	err := execute(request{
		Operation: "terminate-session", SessionID: "rdp-7", SessionUser: "labuser",
		SessionRemote: "yes", SessionType: "x11", ForceSession: true,
		CloseTimeoutSeconds: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(forced); err != nil {
		t.Fatalf("missing supervisor did not reach logind cleanup: %v", err)
	}
}

func TestForcedTinyDeskSessionAcceptsLogindRemoteNoWithoutSeat(t *testing.T) {
	root := t.TempDir()
	procRoot := filepath.Join(root, "proc")
	if err := os.Mkdir(procRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	forced := filepath.Join(root, "session-forced")
	loginctl := filepath.Join(root, "loginctl")
	script := `#!/bin/sh
if [ "$1" = "show-session" ]; then
  printf 'Name=labuser\nRemote=no\nSeat=\nType=x11\nScope=session-rdp-7.scope\n'
  exit 0
fi
if [ "$1" = "terminate-session" ]; then touch "$TEST_SESSION_FORCED"; exit 0; fi
exit 4
`
	writeExecutable(t, loginctl, script)
	oldLoginctl, oldProcRoot, oldLookup := loginctlBinary, procfsRoot, lookupLabuserUID
	loginctlBinary = loginctl
	procfsRoot = procRoot
	lookupLabuserUID = func() (uint32, error) { return 1002, nil }
	t.Cleanup(func() { loginctlBinary, procfsRoot, lookupLabuserUID = oldLoginctl, oldProcRoot, oldLookup })
	t.Setenv("TEST_SESSION_FORCED", forced)

	err := execute(request{
		Operation: "terminate-session", SessionID: "rdp-7", SessionUser: "labuser",
		SessionRemote: "no", SessionSeat: "", SessionType: "x11", ForceSession: true,
		CloseTimeoutSeconds: 1,
	})
	if err != nil {
		t.Fatalf("seatless Tiny Desk session was rejected when logind reported Remote=no: %v", err)
	}
	if _, err := os.Stat(forced); err != nil {
		t.Fatalf("seatless Tiny Desk session did not reach logind cleanup: %v", err)
	}
}

func TestForcedTinyDeskSessionRejectsLocalSeatEvenWhenOwnedByLabuser(t *testing.T) {
	root := t.TempDir()
	procRoot := filepath.Join(root, "proc")
	if err := os.Mkdir(procRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	forced := filepath.Join(root, "session-forced")
	loginctl := filepath.Join(root, "loginctl")
	script := `#!/bin/sh
if [ "$1" = "show-session" ]; then
  printf 'Name=labuser\nRemote=no\nSeat=seat0\nType=x11\nScope=session-seat0.scope\n'
  exit 0
fi
if [ "$1" = "terminate-session" ]; then touch "$TEST_SESSION_FORCED"; exit 0; fi
exit 4
`
	writeExecutable(t, loginctl, script)
	oldLoginctl, oldProcRoot, oldLookup := loginctlBinary, procfsRoot, lookupLabuserUID
	loginctlBinary = loginctl
	procfsRoot = procRoot
	lookupLabuserUID = func() (uint32, error) { return 1002, nil }
	t.Cleanup(func() { loginctlBinary, procfsRoot, lookupLabuserUID = oldLoginctl, oldProcRoot, oldLookup })
	t.Setenv("TEST_SESSION_FORCED", forced)

	err := execute(request{
		Operation: "terminate-session", SessionID: "rdp-seat0", SessionUser: "labuser",
		SessionRemote: "no", SessionSeat: "seat0", SessionType: "x11", ForceSession: true,
		CloseTimeoutSeconds: 1,
	})
	if err == nil {
		t.Fatal("forced termination accepted labuser's local seat session")
	}
	if _, err := os.Stat(forced); err == nil {
		t.Fatal("local seat session reached logind termination")
	}
}

func writeSessionProcess(t *testing.T, procRoot string, pid int, uid uint32, scope, executable string, arguments ...string) {
	t.Helper()
	directory := filepath.Join(procRoot, strconv.Itoa(pid))
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	status := fmt.Sprintf("Name:\tlabstationctl\nUid:\t%d %d %d %d\n", uid, uid, uid, uid)
	if err := os.WriteFile(filepath.Join(directory, "status"), []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}
	argv := append([]string{executable}, arguments...)
	if err := os.WriteFile(filepath.Join(directory, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "cgroup"), []byte("0::/user.slice/"+scope+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "environ"), []byte("XDG_SESSION_ID=rdp-7\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(directory, "exe")); err != nil {
		t.Fatal(err)
	}
}
