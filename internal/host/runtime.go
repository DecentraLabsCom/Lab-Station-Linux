package host

import (
	"bufio"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

type Runtime interface {
	Supervisor() Supervisor
	NetworkInterfaces() []map[string]string
	Sessions() (local []map[string]any, remote []map[string]any, queryOK bool)
	WakeStatus(interfaces []map[string]string) (supported bool, enabled bool)
	WakePersistent(interfaces []map[string]string) bool
	EnergyStatus() EnergyStatus
	KernelRelease() string
	LocalGraphicsPresent() bool
	UserPasswordConfigured(username string) bool
	NotifyLocalUsers(message string) error
	HibernateSupported() bool
}

type EnergyStatus struct {
	SuspendStates          []string
	ActiveSwap             bool
	HibernateSupported     bool
	Supervisor             string
	InhibitorsAvailable    bool
	ActiveInhibitors       int
	AutomaticSuspendPolicy string
	Issues                 []string
}

type LinuxRuntime struct{}

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)
var interfaceNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,32}$`)
var loginctlCommand = "/usr/bin/loginctl"

func NewRuntime() Runtime { return LinuxRuntime{} }

func (LinuxRuntime) Supervisor() Supervisor                 { return DetectSupervisor() }
func (LinuxRuntime) NetworkInterfaces() []map[string]string { return NetworkInterfaces() }

func (LinuxRuntime) Sessions() ([]map[string]any, []map[string]any, bool) {
	out, err := exec.Command(loginctlCommand, "list-sessions", "--no-legend", "--no-pager").Output()
	if err != nil {
		return nil, nil, false
	}
	local := []map[string]any{}
	remote := []map[string]any{}
	ok := true
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 || !sessionIDPattern.MatchString(fields[0]) {
			continue
		}
		id := fields[0]
		props, err := exec.Command(loginctlCommand, "show-session", id, "-p", "Name", "-p", "Remote", "-p", "Seat", "-p", "Type").Output()
		if err != nil {
			ok = false
			continue
		}
		values := map[string]string{}
		for _, line := range strings.Split(string(props), "\n") {
			key, value, found := strings.Cut(line, "=")
			if found {
				values[key] = value
			}
		}
		if values["Name"] == "" || (values["Remote"] != "yes" && values["Remote"] != "no") {
			ok = false
			continue
		}
		remoteSession := values["Remote"] == "yes"
		user := values["Name"]
		kind := "local"
		evictable := false
		if remoteSession {
			kind = "remote"
			if user == "labstation-ops" {
				kind = "management"
			}
			if user == "labuser" && values["Type"] == "x11" {
				evictable = true
			}
		} else if values["Seat"] == "" {
			kind = "service"
		} else {
			evictable = true
		}
		entry := map[string]any{"id": id, "user": user, "remote": remoteSession, "seat": values["Seat"], "type": values["Type"], "kind": kind, "active": true, "evictable": evictable}
		if remoteSession || kind == "service" {
			remote = append(remote, entry)
		} else {
			local = append(local, entry)
		}
	}
	return local, remote, ok && scanner.Err() == nil
}

func (LinuxRuntime) WakeStatus(interfaces []map[string]string) (bool, bool) {
	supported, enabled := false, false
	for _, item := range interfaces {
		name := item["name"]
		if !interfaceNamePattern.MatchString(name) {
			continue
		}
		out, err := exec.Command("ethtool", "--show-wol", name).CombinedOutput()
		if err != nil {
			continue
		}
		text := string(out)
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Supports Wake-on:") && strings.Contains(strings.TrimPrefix(line, "Supports Wake-on:"), "g") {
				supported = true
			}
			if strings.HasPrefix(line, "Wake-on:") && strings.Contains(strings.TrimPrefix(line, "Wake-on:"), "g") {
				enabled = true
			}
		}
	}
	return supported, enabled
}

func (LinuxRuntime) WakePersistent(interfaces []map[string]string) bool {
	raw, err := os.ReadFile("/etc/decentralabs/lab-station/wake-interfaces")
	if err != nil {
		return false
	}
	configured := strings.Fields(string(raw))
	if len(configured) != 1 || !interfaceNamePattern.MatchString(configured[0]) {
		return false
	}
	found := false
	for _, item := range interfaces {
		if item["name"] == configured[0] {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	if supervisor := DetectSupervisor(); supervisor == nil {
		return false
	} else if supervisor.Name() == "systemd" {
		return exec.Command("systemctl", "is-enabled", "--quiet", "decentralabs-labstation-wol.service").Run() == nil
	} else if supervisor.Name() == "openrc" {
		out, err := exec.Command("rc-update", "show", "default").CombinedOutput()
		return err == nil && strings.Contains(string(out), "decentralabs-labstation-wol")
	}
	return false
}

func (runtime LinuxRuntime) EnergyStatus() EnergyStatus {
	states, _ := os.ReadFile("/sys/power/state")
	inhibitorsAvailable, count := inhibitorCount()
	activeSwap := activeSwap()
	hibernate := runtime.HibernateSupported()
	issues := []string{}
	if !inhibitorsAvailable {
		issues = append(issues, "power inhibitors could not be inspected")
	}
	if !activeSwap {
		issues = append(issues, "no active swap target is configured")
	}
	return EnergyStatus{
		SuspendStates: strings.Fields(string(states)), ActiveSwap: activeSwap,
		HibernateSupported: hibernate, Supervisor: supervisorName(),
		InhibitorsAvailable: inhibitorsAvailable, ActiveInhibitors: count,
		AutomaticSuspendPolicy: "not reported by the active host power manager", Issues: issues,
	}
}

func (LinuxRuntime) KernelRelease() string {
	out, err := exec.Command("uname", "-r").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func (LinuxRuntime) LocalGraphicsPresent() bool {
	return fileExists("/usr/bin/Xorg") || fileExists("/usr/lib/Xorg")
}

func (LinuxRuntime) UserPasswordConfigured(username string) bool {
	if username == "" || strings.ContainsAny(username, "\r\n\x00") {
		return false
	}
	out, err := exec.Command("passwd", "-S", username).Output()
	if err != nil {
		return false
	}
	fields := strings.Fields(string(out))
	return len(fields) >= 2 && fields[1] == "P"
}

func (LinuxRuntime) NotifyLocalUsers(message string) error {
	if strings.ContainsAny(message, "\r\n\x00") {
		return os.ErrInvalid
	}
	return exec.Command("wall", message).Run()
}

func (LinuxRuntime) HibernateSupported() bool {
	states, err := os.ReadFile("/sys/power/state")
	return err == nil && strings.Contains(string(states), "disk") && activeSwap()
}

func activeSwap() bool {
	raw, err := os.ReadFile("/proc/swaps")
	if err != nil {
		return false
	}
	return len(strings.Split(strings.TrimSpace(string(raw)), "\n")) > 1
}

func inhibitorCount() (bool, int) {
	out, err := exec.Command("systemd-inhibit", "--list", "--no-pager").Output()
	if err != nil {
		return false, 0
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) <= 1 {
		return true, 0
	}
	return true, len(lines) - 1
}

func supervisorName() string {
	if supervisor := DetectSupervisor(); supervisor != nil {
		return supervisor.Name()
	}
	return "unsupported"
}
