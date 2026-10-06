package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
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

func TestTerminateSessionOnlyKillsApprovedLocalOrTinyDeskSessions(t *testing.T) {
	for _, tc := range []struct {
		name       string
		properties string
		wantErr    bool
	}{
		{"local seat", "Name=alice\nRemote=no\nSeat=seat0\nType=wayland\n", false},
		{"tiny desk", "Name=labuser\nRemote=yes\nSeat=\nType=x11\n", false},
		{"unapproved remote", "Name=alice\nRemote=yes\nSeat=\nType=x11\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			binDir := filepath.Join(root, "bin")
			if err := os.MkdirAll(binDir, 0o700); err != nil {
				t.Fatal(err)
			}
			writeExecutable(t, filepath.Join(binDir, "loginctl"), `#!/bin/sh
if [ "$1" = show-session ]; then
  if [ -f "$TEST_SESSION_KILLED" ]; then exit 1; fi
  case "$*" in *"-p Id"*) echo "Id=$3";; *) printf '%s' "$TEST_SESSION_PROPERTIES";; esac
  exit 0
fi
if [ "$1" = kill-session ]; then touch "$TEST_SESSION_KILLED"; exit 0; fi
exit 4
`)
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TEST_SESSION_PROPERTIES", tc.properties)
			t.Setenv("TEST_SESSION_KILLED", filepath.Join(root, "killed"))
			err := execute(request{Operation: "terminate-session", SessionID: "42"})
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

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}
