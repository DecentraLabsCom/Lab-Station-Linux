package agent

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
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

func TestEnsureManagedDirectoryRejectsSymlinkedComponents(t *testing.T) {
	requireSetupRoot(t)
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "linked")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureManagedDirectory(filepath.Join(link, "created"), 0, 0, 0o700); err == nil {
		t.Fatal("managed directory creation followed a symlinked path component")
	}
	plain := filepath.Join(base, "plain", "nested")
	if err := ensureManagedDirectory(plain, 0, 0, 0o750); err != nil {
		t.Fatalf("managed directory creation failed for a plain path: %v", err)
	}
	if info, err := os.Stat(plain); err != nil || info.Mode().Perm() != 0o750 || info.Sys().(*syscall.Stat_t).Uid != 0 {
		t.Fatalf("managed directory ownership or mode = %v, %v", info, err)
	}
}

func TestMissingAccountLookupsFailClosed(t *testing.T) {
	const missing = "labstation-account-that-does-not-exist"
	if _, err := lookupUserIDs(missing); err == nil {
		t.Fatal("uid/gid lookup accepted a missing account")
	}
	if lookupGroup(missing) != 0 {
		t.Fatal("missing group lookup returned an id")
	}
	if uid, gid := lookupUser(missing); uid != 0 || gid != 0 {
		t.Fatalf("missing user lookup returned uid/gid %d/%d", uid, gid)
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

	upgradePath := filepath.Join(t.TempDir(), "managed-upgrade.conf")
	if err := writeManagedConfig(upgradePath, "first=true\n", 0o640); err != nil {
		t.Fatal(err)
	}
	if err := writeManagedConfig(upgradePath, "first=false\nsecond=true\n", 0o640); err != nil {
		t.Fatalf("valid managed update did not migrate to the new content: %v", err)
	}
	if upgraded, err := os.ReadFile(upgradePath); err != nil || string(upgraded) != "first=false\nsecond=true\n" {
		t.Fatalf("managed update content = %q, %v", upgraded, err)
	}

	link := filepath.Join(t.TempDir(), "managed-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := writeManagedConfig(link, content, 0o640); err == nil {
		t.Fatal("managed config symlink was accepted")
	}
}

func TestWriteApplicationProfileOmitsGuiProfileForFmuOnly(t *testing.T) {
	if err := writeApplicationProfile(config.Config{Profile: "fmu-only"}); err != nil {
		t.Fatalf("FMU-only profile should not install a graphical app profile: %v", err)
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
	previousPins := []string{pinnedFMUExecutorVersion, pinnedFMUExecutorCommit, pinnedFMUExecutorSourceTreeSHA256, pinnedFMUExecutorRuntimePayloadSHA256}
	pinnedFMUExecutorVersion = testFMUVersion
	pinnedFMUExecutorCommit = testFMUCommit
	pinnedFMUExecutorSourceTreeSHA256 = testFMUTree
	defer func() {
		pinnedFMUExecutorVersion = previousPins[0]
		pinnedFMUExecutorCommit = previousPins[1]
		pinnedFMUExecutorSourceTreeSHA256 = previousPins[2]
		pinnedFMUExecutorRuntimePayloadSHA256 = previousPins[3]
	}()
	if err := os.MkdirAll("/etc/xrdp", 0o755); err != nil {
		t.Fatal(err)
	}
	fmuSource := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fmuSource, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	for relativePath, content := range map[string]string{
		"app/main.py":      "print('fmu')\n",
		"requirements.txt": "example-dependency==1.0\n",
		"VERSION":          testFMUVersion + "\n",
	} {
		if err := os.WriteFile(filepath.Join(fmuSource, relativePath), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runtimeDigest := mustRuntimeDigest(t, fmuSource)
	pinnedFMUExecutorRuntimePayloadSHA256 = runtimeDigest
	lock := fmuExecutorSourceLock{Repository: "DecentraLabsCom/FMU-Executor", Version: testFMUVersion, Commit: testFMUCommit}
	lock.SourceTree.Format, lock.SourceTree.SHA256 = "git-ls-tree-manifest-v1", testFMUTree
	lock.RuntimePayload.Format, lock.RuntimePayload.SHA256 = "sha256-path-manifest-v1", runtimeDigest
	lockBytes, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fmuSource, "SOURCE.lock.json"), lockBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("/etc/xrdp/xrdp.ini", []byte("[Globals]\nallow_channels=true\nallow_multimon=true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("/etc/xrdp/sesman.ini", []byte("[Globals]\nXAuthorityInSystemDir=no\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := "/etc/decentralabs/lab-station/e2e.toml"
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB labstation-test"
	args := []string{
		"--profile=hybrid",
		"--management-public-key=" + key,
		"--ssh-port=2222",
		"--config=" + configPath,
		"--fmu-executor-source=" + fmuSource,
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
	if _, err := os.Stat(filepath.Join(tinyDeskSessionHome, ".xsession")); err != nil {
		t.Errorf("Tiny Desk session file was not installed: %v", err)
	}
	if info, err := os.Stat(tinyDeskSessionHome); err != nil || info.Mode().Perm() != 0o755 || info.Sys().(*syscall.Stat_t).Uid != 0 {
		t.Errorf("Tiny Desk session home must be root-owned and non-writable: %v, %v", info, err)
	}
	if info, err := os.Stat(filepath.Join(tinyDeskSessionHome, ".xsession")); err != nil || info.Mode().Perm() != 0o755 || info.Sys().(*syscall.Stat_t).Uid != 0 {
		t.Errorf("Tiny Desk session entry must be root-owned and immutable to labuser: %v, %v", info, err)
	}
	if target, err := os.Readlink(filepath.Join(tinyDeskSessionHome, ".xsession-errors")); err != nil || target != filepath.Join(tinyDeskWorkHome, ".xsession-errors") {
		t.Errorf("Xsession log path is not redirected to the writable workspace: %q, %v", target, err)
	}
	if account, err := user.Lookup("labuser"); err != nil || account.HomeDir != tinyDeskSessionHome {
		t.Errorf("labuser account home is not the protected Tiny Desk session directory: %#v, %v", account, err)
	}
	if ids, err := lookupUserIDs("labuser"); err != nil {
		t.Errorf("labuser uid/gid are unavailable for the writable application workspace: %v", err)
	} else if info, statErr := os.Stat(tinyDeskWorkHome); statErr != nil || uint32(info.Sys().(*syscall.Stat_t).Uid) != ids.uid || info.Mode().Perm() != 0o700 {
		t.Errorf("application workspace is not private to labuser: %v, %v", info, statErr)
	}
	if info, err := os.Stat("/usr/share/decentralabs/lab-station/app-profile.json"); err != nil || info.Mode().Perm() != 0o644 || info.Sys().(*syscall.Stat_t).Uid != 0 {
		t.Errorf("application-only profile must be root-owned and readable: %v, %v", info, err)
	}
	sudoers, err := os.ReadFile("/etc/sudoers.d/decentralabs-lab-station")
	if err != nil || !strings.Contains(string(sudoers), "labstation-ops ALL=(labstationd)") || !strings.Contains(string(sudoers), "labstationd ALL=(root)") || !strings.Contains(string(sudoers), "labuser ALL=(root)") {
		t.Errorf("sudoers policy did not separate management, daemon, and admission helper access: %q, %v", sudoers, err)
	}
	ini, err := os.ReadFile("/etc/xrdp/xrdp.ini")
	if err != nil || !strings.Contains(string(ini), "allow_channels=false") || !strings.Contains(string(ini), "allow_multimon=false") {
		t.Errorf("RDP channel policy was not restricted: %q, %v", ini, err)
	}
	if _, err := os.Stat("/etc/xrdp/xrdp.ini.decentralabs-lab-station.bak"); err != nil {
		t.Errorf("original RDP config was not backed up: %v", err)
	}
	sesman, err := os.ReadFile("/etc/xrdp/sesman.ini")
	if err != nil || !strings.Contains(string(sesman), "XAuthorityInSystemDir=yes") {
		t.Errorf("xrdp-sesman did not move Xauthority outside the protected home: %q, %v", sesman, err)
	}
	if _, err := os.Stat("/etc/xrdp/sesman.ini.decentralabs-lab-station.bak"); err != nil {
		t.Errorf("original xrdp-sesman config was not backed up: %v", err)
	}
	if _, err := os.Stat("/etc/systemd/system/decentralabs-labstation.service"); err != nil {
		t.Errorf("systemd service unit was not installed: %v", err)
	}
	if data, err := os.ReadFile("/opt/decentralabs/fmu-executor/app/main.py"); err != nil || string(data) != "print('fmu')\n" {
		t.Errorf("FMU source was not copied into the managed installation: %q, %v", data, err)
	}
	if version, err := os.ReadFile("/opt/decentralabs/fmu-executor/VERSION"); err != nil || strings.TrimSpace(string(version)) != strings.TrimSpace(pinnedFMUExecutorVersion) {
		t.Errorf("FMU version was not installed: %q, %v", version, err)
	}
	if lock, err := os.ReadFile("/opt/decentralabs/fmu-executor/SOURCE.lock.json"); err != nil || !strings.Contains(string(lock), testFMUCommit) || !strings.Contains(string(lock), runtimeDigest) {
		t.Errorf("FMU source lock was not installed with its verified identity: %q, %v", lock, err)
	}
	if _, err := os.Stat("/etc/systemd/system/decentralabs-labstation-fmu.service"); err != nil {
		t.Errorf("FMU service unit was not installed for the supplied source: %v", err)
	}
	if info, err := os.Stat("/var/lib/decentralabs/fmu-executor/fmu-data"); err != nil || info.Mode().Perm() != 0o750 {
		t.Errorf("FMU state directory is not private to the FMU account: %v, %v", info, err)
	}
	if err := os.WriteFile(filepath.Join(tinyDeskSessionHome, ".xsessionrc"), []byte("touch /tmp/untrusted-session-hook\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installTinyDeskSession(config.Config{Profile: "hybrid"}); err == nil {
		t.Error("Tiny Desk setup accepted an untrusted Xsession startup hook")
	}
	if err := os.Remove(filepath.Join(tinyDeskSessionHome, ".xsessionrc")); err != nil {
		t.Fatal(err)
	}
	errorLog := filepath.Join(tinyDeskSessionHome, ".xsession-errors")
	if err := os.Remove(errorLog); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(errorLog, []byte("unexpected log object"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := installTinyDeskSession(config.Config{Profile: "hybrid"}); err == nil {
		t.Error("Tiny Desk setup accepted an unprotected Xsession error log")
	}
	if err := os.Remove(errorLog); err != nil {
		t.Fatal(err)
	}
	if err := installTinyDeskSession(config.Config{Profile: "hybrid"}); err != nil {
		t.Fatalf("Tiny Desk could not restore the managed session log link after rejection: %v", err)
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
	if _, err := os.Stat(tinyDeskSessionHome); err != nil {
		t.Errorf("hybrid profile did not create the protected Tiny Desk home: %v", err)
	}
	serviceLog, err := os.ReadFile("/tmp/labstation-test-systemctl.log")
	if err != nil || !strings.Contains(string(serviceLog), "enable --now xrdp.service") {
		t.Errorf("xrdp was not enabled for the graphical profile: %q, %v", serviceLog, err)
	}
	if err := Setup(args); err != nil {
		t.Fatalf("Setup() was not idempotent: %v", err)
	}
	sesmanBeforeDrift, err := os.ReadFile("/etc/xrdp/sesman.ini")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("/etc/xrdp/sesman.ini", []byte("[Globals]\nXAuthorityInSystemDir=no\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := configureTinyDeskSesman(); err == nil {
		t.Error("xrdp-sesman drift was silently overwritten")
	}
	if err := os.WriteFile("/etc/xrdp/sesman.ini", sesmanBeforeDrift, 0o644); err != nil {
		t.Fatal(err)
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
		"/etc/init.d/decentralabs-labstation-fmu": "command_user=labstation-fmu:labstation-fmu",
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
