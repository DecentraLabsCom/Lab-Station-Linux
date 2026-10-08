package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecodeRequestAcceptsOneBoundedStrictObject(t *testing.T) {
	got, err := decodeRequest(strings.NewReader(`{"operation":"service-action","unit":"decentralabs-labstation.service","action":"restart"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.Operation != "service-action" || got.Unit != "decentralabs-labstation.service" || got.Action != "restart" {
		t.Fatalf("decoded request = %#v", got)
	}
}

func TestDecodeRequestRejectsMalformedUnknownTrailingAndOversizedInput(t *testing.T) {
	tests := map[string][]byte{
		"malformed": []byte(`{"operation":`),
		"unknown":   []byte(`{"operation":"identity","shell":"id"}`),
		"duplicate": []byte(`{"operation":"identity","operation":"power-reboot"}`),
		"trailing":  []byte(`{"operation":"identity"}{"operation":"power-reboot"}`),
		"oversized": bytes.Repeat([]byte(" "), 8193),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeRequest(bytes.NewReader(input)); err == nil {
				t.Fatal("decodeRequest accepted invalid input")
			}
		})
	}
}

func TestHelperCallerMatrixSeparatesAdmissionControlAndSecretOperations(t *testing.T) {
	for _, tc := range []struct {
		user      string
		operation string
		allowed   bool
	}{
		{"labuser", "lease-admission", true},
		{"labuser", "secret-status", false},
		{"labuser", "terminate-session", false},
		{"labstationd", "terminate-session", true},
		{"labstationd", "secret-status", true},
		{"labstation-ops", "secret-set", false},
		{"labstation-fmu", "service-action", false},
		{"root", "power-reboot", true},
	} {
		t.Run(tc.user+"/"+tc.operation, func(t *testing.T) {
			if got := callerCanRunOperation(tc.user, tc.operation); got != tc.allowed {
				t.Fatalf("callerCanRunOperation() = %t, want %t", got, tc.allowed)
			}
		})
	}
}

func TestExecuteRejectsOperationsAndPrivilegedArgumentsOutsideAllowlist(t *testing.T) {
	tests := []struct {
		name string
		req  request
	}{
		{"unknown operation", request{Operation: "shell", Action: "id"}},
		{"invalid session id", request{Operation: "terminate-session", SessionID: "1;id"}},
		{"unapproved service", request{Operation: "service-action", Unit: "ssh.service", Action: "stop"}},
		{"unapproved action", request{Operation: "service-action", Unit: "decentralabs-labstation.service", Action: "enable"}},
		{"unapproved secret id", request{Operation: "secret-clear", SecretID: "other-token"}},
		{"short secret", request{Operation: "secret-set", SecretID: "fmu-internal-token", SecretValue: "too-short"}},
		{"secret with newline", request{Operation: "secret-set", SecretID: "fmu-internal-token", SecretValue: strings.Repeat("a", 32) + "\n"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := execute(tc.req); err == nil {
				t.Fatal("execute accepted a request outside the allowlist")
			}
		})
	}
}

func TestExecuteServiceActionUsesOnlyAllowlistedSystemctlArguments(t *testing.T) {
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "systemctl.log")
	systemctl := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$TEST_SYSTEMCTL_LOG\"\n"
	writeExecutable(t, filepath.Join(binDir, "systemctl"), systemctl)
	oldSystemctl := systemctlBinary
	systemctlBinary = filepath.Join(binDir, "systemctl")
	t.Cleanup(func() { systemctlBinary = oldSystemctl })
	t.Setenv("TEST_SYSTEMCTL_LOG", logPath)

	if err := execute(request{Operation: "service-action", Unit: "decentralabs-labstation-fmu.service", Action: "restart"}); err != nil {
		t.Fatal(err)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(logged) != "restart decentralabs-labstation-fmu.service\n" {
		t.Fatalf("systemctl arguments = %q", logged)
	}
}

func TestHelperCommandsAreBoundedByCallerDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loginctl")
	writeExecutable(t, path, "#!/bin/sh\nexec sleep 2\n")
	oldLoginctl := loginctlBinary
	loginctlBinary = path
	t.Cleanup(func() { loginctlBinary = oldLoginctl })
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := readSessionIdentity(ctx, "opaque-session-1")
	if err == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("loginctl command ignored its deadline: err=%v ctx=%v", err, ctx.Err())
	}
	if time.Since(started) > time.Second {
		t.Fatalf("deadline-bound helper command took too long: %s", time.Since(started))
	}
}

func TestTerminateSessionOnlyKillsApprovedLocalOrTinyDeskSessions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		properties string
		wantErr    bool
		sessionID  string
	}{
		{"local seat", "Name=alice\nRemote=no\nSeat=seat0\nType=wayland\n", false, "42"},
		{"tiny desk", "Name=labuser\nRemote=yes\nSeat=\nType=x11\n", false, "42"},
		{"opaque Tiny Desk id", "Name=labuser\nRemote=yes\nSeat=\nType=x11\n", false, "xrdp-ses_a:12"},
		{"unapproved remote", "Name=alice\nRemote=yes\nSeat=\nType=x11\n", true, "42"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			binDir := filepath.Join(root, "bin")
			if err := os.MkdirAll(binDir, 0o700); err != nil {
				t.Fatal(err)
			}
			fakeLoginctl := filepath.Join(binDir, "loginctl")
			writeExecutable(t, fakeLoginctl, `#!/bin/sh
if [ "$1" = show-session ]; then
  if [ -f "$TEST_SESSION_KILLED" ]; then exit 1; fi
  printf '%s' "$TEST_SESSION_PROPERTIES"
  exit 0
fi
if [ "$1" = kill-session ]; then touch "$TEST_SESSION_KILLED"; exit 0; fi
exit 4
`)
			oldLoginctl := loginctlBinary
			loginctlBinary = fakeLoginctl
			t.Cleanup(func() { loginctlBinary = oldLoginctl })
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TEST_SESSION_PROPERTIES", tc.properties)
			t.Setenv("TEST_SESSION_KILLED", filepath.Join(root, "killed"))
			values := map[string]string{}
			for _, line := range strings.Split(tc.properties, "\n") {
				key, value, ok := strings.Cut(line, "=")
				if ok {
					values[key] = value
				}
			}
			err := execute(request{Operation: "terminate-session", SessionID: tc.sessionID, SessionUser: values["Name"], SessionRemote: values["Remote"], SessionSeat: values["Seat"], SessionType: values["Type"]})
			if tc.wantErr && err == nil {
				t.Fatal("unapproved session was terminated")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("approved session was rejected: %v", err)
			}
			_, killedErr := os.Stat(filepath.Join(root, "killed"))
			if tc.wantErr && killedErr == nil {
				t.Fatal("unapproved session reached loginctl kill-session")
			}
			if !tc.wantErr && killedErr != nil {
				t.Fatal("approved session did not reach loginctl kill-session")
			}
		})
	}
}

func TestTerminateSessionRevalidatesIdentityImmediatelyBeforeEffect(t *testing.T) {
	root := t.TempDir()
	loginctl := filepath.Join(root, "loginctl")
	writeExecutable(t, loginctl, `#!/bin/sh
if [ "$1" = show-session ]; then
  count=0
  [ -f "$TEST_SESSION_READS" ] && count=$(cat "$TEST_SESSION_READS")
  count=$((count + 1))
  echo "$count" > "$TEST_SESSION_READS"
  if [ "$count" -ge 2 ]; then echo "Name=alice"; else echo "Name=labuser"; fi
  echo "Remote=yes"
  echo "Seat="
  echo "Type=x11"
  exit 0
fi
if [ "$1" = kill-session ]; then touch "$TEST_SESSION_KILLED"; exit 0; fi
exit 4
`)
	oldLoginctl := loginctlBinary
	loginctlBinary = loginctl
	t.Cleanup(func() { loginctlBinary = oldLoginctl })
	killed := filepath.Join(root, "killed")
	t.Setenv("TEST_SESSION_READS", filepath.Join(root, "reads"))
	t.Setenv("TEST_SESSION_KILLED", killed)
	err := execute(request{Operation: "terminate-session", SessionID: "xrdp-9", SessionUser: "labuser", SessionRemote: "yes", SessionType: "x11"})
	if err == nil {
		t.Fatal("session ID reuse was not rejected")
	}
	if _, err := os.Stat(killed); err == nil {
		t.Fatal("changed session identity reached loginctl kill-session")
	}
}

func TestWriteSecretFileIsAtomicEncodedAndOwnerOnly(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "secrets")
	secret := "test-token-with-more-than-thirty-two-characters"
	if err := writeSecretFile(directory, secret); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("secret directory mode = %04o, want 0700", info.Mode().Perm())
	}
	secretPath := filepath.Join(directory, "fmu-internal-token.env")
	info, err = os.Stat(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("secret file mode = %04o, want 0600", info.Mode().Perm())
	}
	contents, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "FMU_INTERNAL_TOKEN_B64=" + base64.RawURLEncoding.EncodeToString([]byte(secret)) + "\n"
	if string(contents) != want {
		t.Fatalf("secret file contents = %q, want encoded value", contents)
	}
	if strings.Contains(string(contents), secret) {
		t.Fatal("secret was persisted in plaintext")
	}
	if _, err := os.Stat(secretPath + ".new"); !os.IsNotExist(err) {
		t.Fatalf("temporary secret file remained after successful write: %v", err)
	}
}

func TestSecretStatusProbeChecksMetadataWithoutReadingSecret(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("secret ownership probe requires root")
	}
	directory := filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "fmu-internal-token.env"), []byte("FMU_INTERNAL_TOKEN_B64=not-read\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !secretConfiguredAt(directory) {
		t.Fatal("valid root-only token metadata was not reported as configured")
	}
	if err := os.Chmod(filepath.Join(directory, "fmu-internal-token.env"), 0o640); err != nil {
		t.Fatal(err)
	}
	if secretConfiguredAt(directory) {
		t.Fatal("insecure token file permissions were reported as configured")
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}
