package host

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Capability struct {
	Available bool `json:"available"`
	Ready bool `json:"ready"`
	Backend string `json:"backend,omitempty"`
	Issues []string `json:"issues"`
}

type Supervisor interface {
	Name() string
	Available() bool
	ServiceState(string) (string, error)
	ServiceAction(string, string) error
}

type Systemd struct{}
func (Systemd) Name() string { return "systemd" }
func (Systemd) Available() bool { return fileExists("/run/systemd/system") && commandExists("systemctl") }
func (Systemd) ServiceState(unit string) (string, error) {
	out, err := exec.Command("systemctl", "is-active", unit).Output()
	state := strings.TrimSpace(string(out))
	if state == "" && err != nil { return "unknown", err }
	return state, nil
}
func (Systemd) ServiceAction(unit, action string) error {
	if action != "start" && action != "stop" && action != "restart" { return os.ErrPermission }
	return exec.Command("systemctl", action, unit).Run()
}

type OpenRC struct{}
func (OpenRC) Name() string { return "openrc" }
func (OpenRC) Available() bool { return fileExists("/run/openrc") && commandExists("rc-service") }
func (OpenRC) ServiceState(unit string) (string, error) {
	out, err := exec.Command("rc-service", unit, "status").CombinedOutput()
	text := strings.ToLower(strings.TrimSpace(string(out)))
	if err != nil {
		if strings.Contains(text, "stopped") || strings.Contains(text, "crashed") { return "inactive", nil }
		return "unknown", err
	}
	if strings.Contains(text, "started") || strings.Contains(text, "running") { return "active", nil }
	return "inactive", nil
}
func (OpenRC) ServiceAction(unit, action string) error {
	if action != "start" && action != "stop" && action != "restart" { return os.ErrPermission }
	return exec.Command("rc-service", unit, action).Run()
}

func DetectSupervisor() Supervisor {
	for _, candidate := range []Supervisor{Systemd{}, OpenRC{}} { if candidate.Available() { return candidate } }
	return nil
}

func DetectPackageManager() (string, []string) {
	checks := []struct { command string; name string; packages []string }{
		{"apt-get", "apt", []string{"openssh-server", "ethtool"}},
		{"dnf", "dnf", []string{"openssh-server", "ethtool"}},
		{"yum", "yum", []string{"openssh-server", "ethtool"}},
		{"zypper", "zypper", []string{"openssh-server", "ethtool"}},
		{"pacman", "pacman", []string{"openssh", "ethtool"}},
	}
	for _, check := range checks { if commandExists(check.command) { return check.name, check.packages } }
	return "", nil
}

func DetectDistro() (string, string) {
	file, err := os.Open("/etc/os-release")
	if err != nil { return "unknown", "" }
	defer file.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() { key, value, ok := strings.Cut(scanner.Text(), "="); if !ok { continue }; values[key] = strings.Trim(value, "\"") }
	return strings.ToLower(values["ID"]), values["VERSION_ID"]
}

func NetworkInterfaces() []map[string]string {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil { return nil }
	result := make([]map[string]string, 0, len(entries))
	for _, entry := range entries {
		base := filepath.Join("/sys/class/net", entry.Name())
		mac, _ := os.ReadFile(filepath.Join(base, "address"))
		state, _ := os.ReadFile(filepath.Join(base, "operstate"))
		driver := ""
		if target, err := filepath.EvalSymlinks(filepath.Join(base, "device", "driver")); err == nil { driver = filepath.Base(target) }
		result = append(result, map[string]string{"name": entry.Name(), "mac": strings.TrimSpace(string(mac)), "state": strings.TrimSpace(string(state)), "driver": driver})
	}
	return result
}

func commandExists(name string) bool { _, err := exec.LookPath(name); return err == nil }
func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }
