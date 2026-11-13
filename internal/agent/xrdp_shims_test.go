package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeXrdpTestCommand(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+content+"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func runXrdpTestShim(t *testing.T, shim, uid, capture string, args ...string) ([]string, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{shim}, args...)...)
	cmd.Env = append(os.Environ(), "TEST_XRDP_UID="+uid, "TEST_XRDP_CAPTURE="+capture)
	err := cmd.Run()
	content, readErr := os.ReadFile(capture)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(content) == 0 {
		return nil, err
	}
	return strings.Split(strings.TrimSuffix(string(content), "\n"), "\n"), err
}

func TestXrdpXauthShimScopesLegacyRedirectToLabuser(t *testing.T) {
	dir := t.TempDir()
	id := writeXrdpTestCommand(t, dir, "fake-id", `printf '%s\n' "$TEST_XRDP_UID"`)
	originalDir := filepath.Join(dir, "original")
	if err := os.Mkdir(originalDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeXrdpTestCommand(t, originalDir, "xauth", `printf '%s\n' "$@" > "$TEST_XRDP_CAPTURE"`)
	shim := filepath.Join(dir, "xauth")
	if err := os.WriteFile(shim, []byte(renderXrdpXauthShim(1002, id, originalDir, "/work space/.Xauthority")), 0755); err != nil {
		t.Fatal(err)
	}
	args := []string{"-q", "-f", ".Xauthority", "add", ":10", ".", "cookie"}
	for _, tc := range []struct{ name, uid, want string }{
		{"labuser", "1002", "/work space/.Xauthority"},
		{"another user", "1003", ".Xauthority"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runXrdpTestShim(t, shim, tc.uid, filepath.Join(t.TempDir(), "args"), args...)
			if err != nil || len(got) != len(args) || got[2] != tc.want {
				t.Fatalf("unexpected xauth arguments %q, error %v", got, err)
			}
			for i := range args {
				if i != 2 && got[i] != args[i] {
					t.Fatalf("argument %d changed: %q", i, got)
				}
			}
		})
	}
	got, err := runXrdpTestShim(t, shim, "1002", filepath.Join(t.TempDir(), "args"), "-f", "/other/auth", "list")
	if err != nil || strings.Join(got, "|") != "-f|/other/auth|list" {
		t.Fatalf("unrelated xauth invocation changed: %q, %v", got, err)
	}
}

func TestXrdpXorgShimScopesLogRedirectToLabuser(t *testing.T) {
	dir := t.TempDir()
	id := writeXrdpTestCommand(t, dir, "fake-id", `printf '%s\n' "$TEST_XRDP_UID"`)
	real := writeXrdpTestCommand(t, dir, "fake-xorg", `printf '%s\n' "$@" > "$TEST_XRDP_CAPTURE"`)
	shim := filepath.Join(dir, "xorg-shim")
	if err := os.WriteFile(shim, []byte(renderXrdpXorgShim(1002, id, real, "/work space/.xorgxrdp.%s.log")), 0755); err != nil {
		t.Fatal(err)
	}
	args := []string{":10", "-auth", ".Xauthority", "-logfile", ".xorgxrdp.%s.log", "-nolisten", "tcp"}
	for _, tc := range []struct{ name, uid, want string }{
		{"labuser", "1002", "/work space/.xorgxrdp.%s.log"},
		{"another user", "1003", ".xorgxrdp.%s.log"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runXrdpTestShim(t, shim, tc.uid, filepath.Join(t.TempDir(), "args"), args...)
			if err != nil || len(got) != len(args) || got[4] != tc.want {
				t.Fatalf("unexpected Xorg arguments %q, error %v", got, err)
			}
			for i := range args {
				if i != 4 && got[i] != args[i] {
					t.Fatalf("argument %d changed: %q", i, got)
				}
			}
		})
	}
	got, err := runXrdpTestShim(t, shim, "1002", filepath.Join(t.TempDir(), "args"), ":10", "-auth", ".Xauthority")
	if err != nil || strings.Join(got, "|") != ":10|-auth|.Xauthority|-logfile|/work space/.xorgxrdp.%s.log" {
		t.Fatalf("missing logfile was not added: %q, %v", got, err)
	}
	if _, err := runXrdpTestShim(t, shim, "1002", filepath.Join(t.TempDir(), "args"), ":10", "-logfile"); err == nil {
		t.Fatal("incomplete logfile argument was accepted")
	}
}

func TestXrdpSesmanConfigPreservesOtherUsers(t *testing.T) {
	input := "[Globals]\nEnableUserWindowManager=true\n\n[Xorg]\nparam=/usr/lib/xorg/Xorg\nparam=-config\nparam=xrdp/xorg.conf\nparam=-logfile\nparam=.xorgxrdp.%s.log\n\n[SessionVariables]\nPULSE_SCRIPT=/etc/xrdp/pulse/default.pa\n"
	updated, realXorg, originalPath, err := transformXrdpSesmanConfig(input)
	if err != nil {
		t.Fatal(err)
	}
	if realXorg != "/usr/lib/xorg/Xorg" || originalPath != defaultXrdpSessionPath {
		t.Fatalf("unexpected original programs: %q, %q", realXorg, originalPath)
	}
	for _, want := range []string{
		"param=" + tinyDeskXorgShimPath,
		"param=-logfile\nparam=.xorgxrdp.%s.log",
		"PULSE_SCRIPT=/etc/xrdp/pulse/default.pa",
		"PATH=" + tinyDeskXauthShimDir + ":" + defaultXrdpSessionPath,
		"LABSTATION_XORG_EXEC=/usr/lib/xorg/Xorg",
	} {
		if !strings.Contains(updated, want) {
			t.Fatalf("missing %q in updated config:\n%s", want, updated)
		}
	}
	again, repeatedXorg, repeatedPath, err := transformXrdpSesmanConfig(updated)
	if err != nil || again != updated || repeatedXorg != realXorg || repeatedPath != originalPath {
		t.Fatalf("config transformation is not idempotent: %v\n%s", err, again)
	}
}

func TestXrdpSesmanConfigPreservesCustomPathAndRejectsConflicts(t *testing.T) {
	input := "[Xorg]\nparam=/usr/lib/xorg/Xorg\nparam=-logfile\nparam=custom.%s.log\n[SessionVariables]\nPATH=/custom/bin:/usr/bin\n"
	updated, _, originalPath, err := transformXrdpSesmanConfig(input)
	if err != nil || originalPath != "/custom/bin:/usr/bin" || !strings.Contains(updated, "PATH="+tinyDeskXauthShimDir+":/custom/bin:/usr/bin") {
		t.Fatalf("custom session PATH was not preserved: %q, %v\n%s", originalPath, err, updated)
	}
	for _, bad := range []string{
		"[Xorg]\nparam=/usr/lib/xorg/Xorg\n[SessionVariables]\nXAUTHORITY=/custom/file\n",
		"[Xorg]\nparam=/usr/lib/xorg/Xorg\n[SessionVariables]\nPATH=/a\nPATH=/b\n",
		"[Xorg]\nparam=" + tinyDeskXorgShimPath + "\n[SessionVariables]\n",
		"[SessionVariables]\nPULSE_SCRIPT=/tmp/pulse\n",
	} {
		if _, _, _, err := transformXrdpSesmanConfig(bad); err == nil {
			t.Fatalf("conflicting configuration was accepted:\n%s", bad)
		}
	}
}

func TestXrdpSessionHasXauth(t *testing.T) {
	dir := t.TempDir()
	if xrdpSessionHasXauth(dir, dir) {
		t.Fatal("missing xauth was reported as available")
	}
	path := filepath.Join(dir, "xauth")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if !xrdpSessionHasXauth(dir, dir) {
		t.Fatal("executable xauth was not detected")
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	if xrdpSessionHasXauth(dir, dir) {
		t.Fatal("xauth unavailable to the managed account was reported as available")
	}
}
