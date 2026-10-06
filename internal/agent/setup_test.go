package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/decentralabs/lab-station-linux/internal/config"
	"github.com/decentralabs/lab-station-linux/internal/host"
)

func requireSetupRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("installer file ownership checks require root; production setup runs as root")
	}
}

func TestSetupRejectsInvalidOptionsBeforeAnyHostMutation(t *testing.T) {
	requireSetupRoot(t)

	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"--profile=desktop"}, "profile must be dedicated, hybrid, or fmu-only"},
		{[]string{"--ssh-port=65536"}, "--ssh-port must be between 1 and 65535 when supplied"},
		{[]string{"--wake-interface=invalid/interface"}, "--wake-interface must be a valid Linux interface name"},
	} {
		t.Run(test.want, func(t *testing.T) {
			err := Setup(test.args)
			if err == nil || err.Error() != test.want {
				t.Fatalf("Setup(%q) error = %v, want %q", test.args, err, test.want)
			}
		})
	}
	if err := Setup([]string{"--unknown-option"}); err == nil {
		t.Fatal("Setup accepted an unknown option")
	}
}

func TestCopyExecutorTreeCopiesRegularFilesAndRejectsSymlinks(t *testing.T) {
	requireSetupRoot(t)
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "main.py"), []byte("print('ok')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyExecutorTree(source, destination); err != nil {
		t.Fatalf("copyExecutorTree() failed on a regular tree: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "nested", "main.py"))
	if err != nil || string(data) != "print('ok')\n" {
		t.Fatalf("copied executor file = %q, %v", data, err)
	}
	if info, err := os.Stat(filepath.Join(destination, "nested", "main.py")); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("copied file mode = %v, %v; want 0644", info, err)
	}

	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(source, "outside-link")); err != nil {
		t.Fatal(err)
	}
	if err := copyExecutorTree(source, filepath.Join(root, "rejected")); err == nil {
		t.Fatal("copyExecutorTree() accepted a symbolic link")
	}
}

func TestWriteManagedConfigIsIdempotentAndDetectsChanges(t *testing.T) {
	requireSetupRoot(t)
	path := filepath.Join(t.TempDir(), "owned.conf")
	content := "managed=true\n"
	if err := writeManagedConfig(path, content, 0o640); err != nil {
		t.Fatalf("initial managed write failed: %v", err)
	}
	if err := writeManagedConfig(path, content, 0o640); err != nil {
		t.Fatalf("idempotent managed write failed: %v", err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("managed file mode = %v, %v; want 0640", info, err)
	}
	if marker, err := os.ReadFile(path + ".sha256"); err != nil || len(marker) != 65 {
		t.Fatalf("managed file digest marker = %q, %v", marker, err)
	}

	if err := os.WriteFile(path, []byte("manual edit\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := writeManagedConfig(path, content, 0o640); err == nil {
		t.Fatal("managed file edit was silently overwritten")
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".sha256", []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeManagedConfig(path, content, 0o640); err == nil {
		t.Fatal("tampered ownership digest was accepted")
	}

	link := filepath.Join(t.TempDir(), "managed-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := writeManagedConfig(link, content, 0o640); err == nil {
		t.Fatal("managed config symlink was accepted")
	}
}

func TestConfiguredSshPortsParsesAndDeduplicatesEffectiveListeners(t *testing.T) {
	bin := t.TempDir()
	sshd := filepath.Join(bin, "sshd")
	if err := os.WriteFile(sshd, []byte("#!/bin/sh\n[ \"$1\" = -T ] || exit 2\nprintf 'port 22\\nport 2222\\nport 22\\nport 0\\nport 65536\\nPort 24\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ports, err := configuredSshPorts()
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{22, 2222}; !reflect.DeepEqual(ports, want) {
		t.Fatalf("configuredSshPorts() = %v, want %v", ports, want)
	}
	if !containsPort(ports, 2222) || containsPort(ports, 5986) || containsPort(nil, 22) {
		t.Fatalf("containsPort() did not match configured listener ports: %v", ports)
	}
}

func TestSetupEndToEnd(t *testing.T) {
	if os.Getenv("LABSTATION_SETUP_E2E") != "1" {
		t.Skip("runs only inside the disposable setup integration container")
	}
	requireSetupRoot(t)
	if err := os.MkdirAll("/etc/xrdp", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("/tmp/labstation-test-fmu-source/app", 0o755); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		"/tmp/labstation-test-fmu-source/app/main.py":      "print('fmu')\n",
		"/tmp/labstation-test-fmu-source/requirements.txt": "example-dependency==1.0\n",
		"/tmp/labstation-test-fmu-source/VERSION":          "test-version\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile("/etc/xrdp/xrdp.ini", []byte("[Globals]\nallow_channels=true\nallow_multimon=true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := "/etc/decentralabs/lab-station/e2e.toml"
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB labstation-test"
	args := []string{
		"--profile=hybrid",
		"--management-public-key=" + key,
		"--ssh-port=2222",
		"--config=" + configPath,
		"--fmu-executor-source=/tmp/labstation-test-fmu-source",
		"--wake-interface=eth0",
		"--no-install-deps",
	}
	if err := Setup(args); err != nil {
		t.Fatalf("Setup() failed in isolated systemd container: %v", err)
	}

	configData, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	configText := string(configData)
	for _, expected := range []string{"profile = \"hybrid\"", "management_port = 2222", "management_public_key = \"" + key + "\""} {
		if !strings.Contains(configText, expected) {
			t.Errorf("generated station config missing %q", expected)
		}
	}
	if mode, err := os.Stat(filepath.Join(filepath.Dir(configPath), "ssh")); err != nil || mode.Mode().Perm() != 0o700 {
		t.Errorf("SSH config directory mode = %v, %v; want 0700", mode, err)
	}
	if mode, err := os.Stat("/var/lib/decentralabs/lab-station/data"); err != nil || mode.Mode()&os.ModeSetgid == 0 {
		t.Errorf("shared station state directory lacks setgid mode: %v, %v", mode, err)
	}
	if _, err := os.Stat("/home/labuser/.xsession"); err != nil {
		t.Errorf("Tiny Desk session file was not installed: %v", err)
	}
	ini, err := os.ReadFile("/etc/xrdp/xrdp.ini")
	if err != nil || !strings.Contains(string(ini), "allow_channels=false") || !strings.Contains(string(ini), "allow_multimon=false") {
		t.Errorf("RDP channel policy was not restricted: %q, %v", ini, err)
	}
	if _, err := os.Stat("/etc/xrdp/xrdp.ini.decentralabs-lab-station.bak"); err != nil {
		t.Errorf("original RDP config was not backed up: %v", err)
	}
	if _, err := os.Stat("/etc/systemd/system/decentralabs-labstation.service"); err != nil {
		t.Errorf("systemd service unit was not installed: %v", err)
	}
	if data, err := os.ReadFile("/opt/decentralabs/fmu-executor/app/main.py"); err != nil || string(data) != "print('fmu')\n" {
		t.Errorf("FMU source was not copied into the managed installation: %q, %v", data, err)
	}
	if version, err := os.ReadFile("/opt/decentralabs/fmu-executor/VERSION"); err != nil || string(version) != "test-version\n" {
		t.Errorf("FMU version was not installed: %q, %v", version, err)
	}
	if _, err := os.Stat("/etc/systemd/system/decentralabs-labstation-fmu.service"); err != nil {
		t.Errorf("FMU service unit was not installed for the supplied source: %v", err)
	}
	if info, err := os.Stat("/var/lib/decentralabs/fmu-executor/fmu-data"); err != nil || info.Mode()&os.ModeSetgid == 0 {
		t.Errorf("FMU state directory lacks setgid mode: %v, %v", info, err)
	}
	if mode, err := os.ReadFile("/tmp/labstation-test-wol-mode"); err != nil || strings.TrimSpace(string(mode)) != "g" {
		t.Errorf("Wake-on-LAN did not remain enabled: %q, %v", mode, err)
	}
	if _, err := os.Stat("/etc/systemd/system/decentralabs-labstation-wol.service"); err != nil {
		t.Errorf("Wake-on-LAN persistence unit was not installed: %v", err)
	}
	if _, err := os.Stat("/var/lib/labstation-ops/.ssh/authorized_keys"); err != nil {
		t.Errorf("management key was not installed: %v", err)
	}
	sshConfig, err := os.ReadFile("/etc/ssh/sshd_config")
	if err != nil || !strings.Contains(string(sshConfig), "AuthenticationMethods publickey") ||
		!strings.Contains(string(sshConfig), "PasswordAuthentication no") {
		t.Errorf("management SSH policy was not installed: %q, %v", sshConfig, err)
	}
	if _, err := os.Stat("/etc/ssh/sshd_config.decentralabs-lab-station.bak"); err != nil {
		t.Errorf("original SSH policy was not backed up: %v", err)
	}
	if _, err := os.Stat("/home/labuser"); err != nil {
		t.Errorf("hybrid profile did not create the labuser home: %v", err)
	}
	serviceLog, err := os.ReadFile("/tmp/labstation-test-systemctl.log")
	if err != nil || !strings.Contains(string(serviceLog), "enable --now xrdp.service") {
		t.Errorf("xrdp was not enabled for the graphical profile: %q, %v", serviceLog, err)
	}
	if err := Setup(args); err != nil {
		t.Fatalf("Setup() was not idempotent: %v", err)
	}

	sshdConfig := "/etc/ssh/sshd_config"
	if err := os.WriteFile(sshdConfig, []byte("PermitRootLogin no\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{sshdConfig + ".decentralabs-lab-station.bak", sshdConfig + ".decentralabs-lab-station.sha256"} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("LABSTATION_TEST_SSHD_FAIL", "1")
	err = Setup(args)
	if err == nil || !strings.Contains(err.Error(), "OpenSSH rejected the Lab Station user policy") {
		t.Fatalf("Setup() did not report invalid SSH policy validation: %v", err)
	}
	if restored, readErr := os.ReadFile(sshdConfig); readErr != nil || string(restored) != "PermitRootLogin no\n" {
		t.Fatalf("failed SSH policy validation did not restore the original config: %q, %v", restored, readErr)
	}
	if _, err := os.Stat(sshdConfig + ".decentralabs-lab-station.sha256"); !os.IsNotExist(err) {
		t.Fatalf("failed SSH policy validation left an ownership marker: %v", err)
	}

	if err := os.Remove("/run/systemd/system"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("/run/openrc", 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := host.DetectSupervisor().(host.OpenRC); !ok {
		t.Fatalf("test did not select the OpenRC adapter: %T", host.DetectSupervisor())
	}
	openRCCfg := config.Defaults()
	openRCCfg.Profile = "hybrid"
	if err := installSupervisor(openRCCfg, configPath, true); err != nil {
		t.Fatalf("OpenRC service installation failed: %v", err)
	}
	for path, want := range map[string]string{
		"/etc/init.d/decentralabs-labstation":     "command_user=labstationd:labstation",
		"/etc/init.d/decentralabs-labstation-fmu": "FMU_INTERNAL_TOKEN_B64",
		"/etc/init.d/decentralabs-labstation-wol": "/usr/lib/decentralabs/lab-station/labstation-wol",
	} {
		data, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(data), want) {
			t.Errorf("OpenRC script %s omitted %q: %q, %v", path, want, data, err)
		}
	}
	if err := startManagementServices(openRCCfg); err != nil {
		t.Fatalf("OpenRC service activation failed: %v", err)
	}
	openRCLog, err := os.ReadFile("/tmp/labstation-test-openrc.log")
	if err != nil || !strings.Contains(string(openRCLog), "rc-update add sshd default") ||
		!strings.Contains(string(openRCLog), "rc-service decentralabs-labstation start") ||
		!strings.Contains(string(openRCLog), "rc-service xrdp restart") {
		t.Errorf("OpenRC service lifecycle was incomplete: %q, %v", openRCLog, err)
	}
}
