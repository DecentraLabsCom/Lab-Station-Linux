package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func installCommand(t *testing.T, directory, name, body string) {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func addCommandDirectoryToPath(t *testing.T, directory string) {
	t.Helper()
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func setLoginctlCommand(t *testing.T, directory string) {
	t.Helper()
	previous := loginctlCommand
	loginctlCommand = filepath.Join(directory, "loginctl")
	t.Cleanup(func() { loginctlCommand = previous })
}

func TestSessionInventoryClassifiesLocalRemoteManagementAndServiceSessions(t *testing.T) {
	bin := t.TempDir()
	installCommand(t, bin, "loginctl", `
if [ "$1" = "list-sessions" ]; then
  printf '1 1000 teacher seat0\nxrdp-ses_a-9 1001 labuser -\n3 1002 labstation-ops -\n4 1003 daemon -\nbad;id 1004 bad seat0\n'
  exit 0
fi
if [ "$1" = "show-session" ]; then
  case "$2" in
    1) printf 'Name=teacher\nRemote=no\nSeat=seat0\nType=wayland\n' ;;
    xrdp-ses_a-9) printf 'Name=labuser\nRemote=yes\nSeat=\nType=x11\n' ;;
    3) printf 'Name=labstation-ops\nRemote=yes\nSeat=\nType=tty\n' ;;
    4) printf 'Name=daemon\nRemote=no\nSeat=\nType=unspecified\n' ;;
    *) exit 1 ;;
  esac
  exit 0
fi
exit 2`)
	addCommandDirectoryToPath(t, bin)
	setLoginctlCommand(t, bin)

	local, remote, ok := (LinuxRuntime{}).Sessions()
	if !ok {
		t.Fatalf("complete loginctl responses should be queryable: local=%#v remote=%#v", local, remote)
	}
	if len(local) != 1 || local[0]["id"] != "1" || local[0]["kind"] != "local" || local[0]["evictable"] != true {
		t.Fatalf("unexpected local session projection: %#v", local)
	}
	if len(remote) != 3 {
		t.Fatalf("expected labuser, management, and service sessions, got %#v", remote)
	}
	if remote[0]["kind"] != "remote" || remote[0]["evictable"] != true {
		t.Errorf("Tiny Desk session should be evictable: %#v", remote[0])
	}
	if remote[1]["kind"] != "management" || remote[1]["evictable"] != false {
		t.Errorf("SSH management session must never be evictable: %#v", remote[1])
	}
	if remote[2]["kind"] != "service" || remote[2]["evictable"] != false {
		t.Errorf("headless service session must be retained: %#v", remote[2])
	}
}

func TestSessionInventoryFailsClosedWhenLoginctlCannotRunOrDescribeSession(t *testing.T) {
	bin := t.TempDir()
	installCommand(t, bin, "loginctl", `
if [ "$1" = "list-sessions" ]; then printf '1 1000 teacher seat0\n'; exit 0; fi
exit 1`)
	addCommandDirectoryToPath(t, bin)
	setLoginctlCommand(t, bin)
	local, remote, ok := (LinuxRuntime{}).Sessions()
	if ok || len(local) != 0 || len(remote) != 0 {
		t.Fatalf("a partial session inventory must not be reported as complete: local=%#v remote=%#v ok=%v", local, remote, ok)
	}
}

func TestWakeStatusUsesOnlyValidatedInterfacesAndDetectsSupportedAndEnabled(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "ethtool.log")
	installCommand(t, bin, "ethtool", `
printf '%s\n' "$2" >> "$ETHTOOL_TEST_LOG"
case "$2" in
  eth0) printf 'Supports Wake-on: pumbg\nWake-on: g\n' ;;
  eth1) printf 'Supports Wake-on: g\nWake-on: d\n' ;;
  *) exit 1 ;;
esac`)
	t.Setenv("ETHTOOL_TEST_LOG", log)
	addCommandDirectoryToPath(t, bin)
	interfaces := []map[string]string{{"name": "eth0"}, {"name": "eth1"}, {"name": "../../tmp"}, {"name": "name with spaces"}}

	supported, enabled := (LinuxRuntime{}).WakeStatus(interfaces)

	if !supported || !enabled {
		t.Fatalf("WoL support/enabled state was not detected: supported=%v enabled=%v", supported, enabled)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(strings.TrimSpace(string(calls)), "\n"); len(got) != 2 || got[0] != "eth0" || got[1] != "eth1" {
		t.Fatalf("untrusted interface reached ethtool: %q", calls)
	}
}

func TestPackageManagerDetectionUsesAnExplicitSupportedOrder(t *testing.T) {
	bin := t.TempDir()
	for _, name := range []string{"apt-get", "dnf", "pacman"} {
		installCommand(t, bin, name, "exit 0")
	}
	t.Setenv("PATH", bin)
	manager, packages := DetectPackageManager()
	if manager != "apt" || strings.Join(packages, ",") != "openssh-server,ethtool" {
		t.Fatalf("unexpected priority or package contract: manager=%q packages=%#v", manager, packages)
	}

	if err := os.Remove(filepath.Join(bin, "apt-get")); err != nil {
		t.Fatal(err)
	}
	manager, _ = DetectPackageManager()
	if manager != "dnf" {
		t.Fatalf("expected dnf fallback after apt, got %q", manager)
	}
}

func TestServiceActionsPermitOnlyStartStopAndRestart(t *testing.T) {
	for _, supervisor := range []Supervisor{Systemd{}, OpenRC{}} {
		t.Run(supervisor.Name(), func(t *testing.T) {
			if err := supervisor.ServiceAction("station.service", "shell"); err == nil {
				t.Fatal("unsupported service action was accepted")
			}
			if err := supervisor.ServiceAction("station.service", "start;id"); err == nil {
				t.Fatal("shell syntax was accepted as a service action")
			}
		})
	}
}

func TestNetworkInterfaceInventoryUsesKernelNamesAndFields(t *testing.T) {
	interfaces := NetworkInterfaces()
	if len(interfaces) == 0 {
		t.Skip("this test host does not expose /sys/class/net")
	}
	for _, networkInterface := range interfaces {
		if networkInterface["name"] == "" {
			t.Fatal("kernel interface inventory included an empty name")
		}
		if _, ok := networkInterface["mac"]; !ok {
			t.Fatalf("interface is missing a MAC field: %#v", networkInterface)
		}
		if _, ok := networkInterface["state"]; !ok {
			t.Fatalf("interface is missing an operational state: %#v", networkInterface)
		}
	}
}
