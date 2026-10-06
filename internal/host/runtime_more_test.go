package host

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemdAndOpenRCStateAndActionAdapters(t *testing.T) {
	bin := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "service-actions.log")
	installCommand(t, bin, "systemctl", `
case "$1" in
  is-active) if [ "$2" = "running.service" ]; then echo active; else echo inactive; exit 3; fi ;;
  start|stop|restart) printf '%s\n' "$*" >> "$SERVICE_ACTIONS_LOG" ;;
  *) exit 4 ;;
esac`)
	installCommand(t, bin, "rc-service", `
case "$1/$2" in
  running/status) echo "* service started" ;;
  stopped/status) echo "* service is stopped"; exit 1 ;;
  *) printf '%s\n' "$*" >> "$SERVICE_ACTIONS_LOG" ;;
esac`)
	t.Setenv("SERVICE_ACTIONS_LOG", logPath)
	addCommandDirectoryToPath(t, bin)

	if state, err := (Systemd{}).ServiceState("running.service"); err != nil || state != "active" {
		t.Fatalf("systemd active state = %q, %v", state, err)
	}
	if state, err := (Systemd{}).ServiceState("stopped.service"); err != nil || state != "inactive" {
		t.Fatalf("systemd inactive state = %q, %v", state, err)
	}
	if state, err := (OpenRC{}).ServiceState("running"); err != nil || state != "active" {
		t.Fatalf("OpenRC active state = %q, %v", state, err)
	}
	if state, err := (OpenRC{}).ServiceState("stopped"); err != nil || state != "inactive" {
		t.Fatalf("OpenRC stopped state = %q, %v", state, err)
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, supervisor := range []Supervisor{Systemd{}, OpenRC{}} {
		if err := supervisor.ServiceAction("station", "restart"); err != nil {
			t.Fatalf("%s restart failed: %v", supervisor.Name(), err)
		}
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logged), "restart station") || !strings.Contains(string(logged), "station restart") {
		t.Fatalf("approved service actions did not reach their adapters: %q", logged)
	}
}

func TestRuntimeQueriesUseBoundedHostCommandInputs(t *testing.T) {
	bin := t.TempDir()
	installCommand(t, bin, "uname", `echo test-kernel`)
	installCommand(t, bin, "passwd", `
case "$2" in
  alice) echo "alice P 2026-01-01 0 99999 7 -1" ;;
  locked) echo "locked L 2026-01-01 0 99999 7 -1" ;;
  *) exit 1 ;;
esac`)
	installCommand(t, bin, "wall", `cat > "$WALL_MESSAGE_LOG"`)
	t.Setenv("WALL_MESSAGE_LOG", filepath.Join(t.TempDir(), "wall-message.txt"))
	addCommandDirectoryToPath(t, bin)
	runtime := LinuxRuntime{}

	if got := runtime.KernelRelease(); got != "test-kernel" {
		t.Fatalf("KernelRelease() = %q", got)
	}
	if !runtime.UserPasswordConfigured("alice") || runtime.UserPasswordConfigured("locked") {
		t.Fatal("password status did not distinguish configured and locked accounts")
	}
	if runtime.UserPasswordConfigured("bad;name") || runtime.UserPasswordConfigured("") {
		t.Fatal("invalid username was accepted")
	}
	if err := runtime.NotifyLocalUsers("maintenance window"); err != nil {
		t.Fatalf("wall notification failed: %v", err)
	}
	if err := runtime.NotifyLocalUsers("maintenance\nreboot"); err != os.ErrInvalid {
		t.Fatalf("newline notification was not rejected: %v", err)
	}
	if err := runtime.NotifyLocalUsers("maintenance\x00reboot"); err != os.ErrInvalid {
		t.Fatalf("NUL notification was not rejected: %v", err)
	}
	if runtime.LocalGraphicsPresent() != (fileExists("/usr/bin/Xorg") || fileExists("/usr/lib/Xorg")) {
		t.Fatal("graphics capability projection changed")
	}
}

func TestEnergyAuditAndSupervisorDetectionExposeTypedState(t *testing.T) {
	bin := t.TempDir()
	installCommand(t, bin, "systemd-inhibit", `printf 'WHO UID USER PID COMM WHAT WHY MODE\na 1 user 2 cmd sleep test block\nb 1 user 3 cmd sleep test delay\n'`)
	addCommandDirectoryToPath(t, bin)

	state := (LinuxRuntime{}).EnergyStatus()
	if !state.InhibitorsAvailable || state.ActiveInhibitors != 2 {
		t.Fatalf("inhibitor inventory = %#v", state)
	}
	if state.AutomaticSuspendPolicy == "" || state.Supervisor == "" {
		t.Fatalf("energy capability status is incomplete: %#v", state)
	}
	if state.HibernateSupported != (strings.Contains(strings.Join(state.SuspendStates, " "), "disk") && state.ActiveSwap) {
		t.Fatalf("hibernate capability is inconsistent with reported kernel/swap state: %#v", state)
	}
	if _, ok := DetectSupervisor().(Supervisor); ok {
		// The current host may provide systemd or OpenRC; both satisfy the runtime contract.
	} else if DetectSupervisor() != nil {
		t.Fatal("detected supervisor has an unexpected type")
	}
	if name, version := DetectDistro(); name == "" || version == "" {
		t.Fatal("Linux distro metadata is missing")
	}
}

func TestRuntimeWrappersExposeTheLinuxImplementation(t *testing.T) {
	runtime := NewRuntime()
	if _, ok := runtime.(LinuxRuntime); !ok {
		t.Fatalf("NewRuntime returned %T", runtime)
	}
	if supervisor := runtime.Supervisor(); supervisor != nil {
		if _, ok := supervisor.(Supervisor); !ok {
			t.Fatalf("unexpected supervisor type %T", supervisor)
		}
	}
	_ = runtime.NetworkInterfaces()
}
