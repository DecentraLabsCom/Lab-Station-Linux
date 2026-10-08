package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/decentralabs/lab-station-linux/internal/agent"
	"github.com/decentralabs/lab-station-linux/internal/config"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	command := os.Args[1]
	if command == "setup" {
		if err := agent.Setup(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		return
	}
	if command == "app" && len(os.Args) > 2 && os.Args[2] == "launch" {
		profile, err := loadApplicationProfile("/usr/share/decentralabs/lab-station/app-profile.json")
		if err != nil || launchApplication(profile) != nil {
			fmt.Fprintln(os.Stderr, "application launch denied")
			os.Exit(2)
		}
		return
	}
	cfgPath := os.Getenv("LABSTATION_CONFIG")
	if cfgPath == "" {
		cfgPath = "/etc/decentralabs/lab-station/station.toml"
	}
	cfg, err := load(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unable to load station configuration")
		os.Exit(2)
	}
	instance := agent.New(cfg)
	switch command {
	case "config-check":
		fmt.Println("station configuration valid")
		return
	case "heartbeat":
		data, e := json.Marshal(instance.Status())
		if e != nil {
			os.Exit(2)
		}
		fmt.Println(string(data))
		return
	case "queue":
		if len(os.Args) > 2 && os.Args[2] == "submit" {
			if err := submitQueue(cfg, os.Args[3:]); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			return
		}
	}
	args := []string{}
	if len(os.Args) > 2 {
		args = os.Args[2:]
	}
	result := instance.Execute(context.Background(), "", command, args)
	if result.Stdout != "" {
		fmt.Println(result.Stdout)
	}
	if result.Stderr != "" {
		fmt.Fprintln(os.Stderr, result.Stderr)
	}
	if result.ExitCode > 0 {
		os.Exit(result.ExitCode)
	}
}

func load(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	if os.IsNotExist(err) {
		fallback := config.Defaults()
		fallback.Name, _ = os.Hostname()
		return fallback, nil
	}
	return cfg, err
}

type applicationProfile struct {
	StateDir    string             `json:"stateDir"`
	Application config.Application `json:"application"`
}

func loadApplicationProfile(path string) (applicationProfile, error) {
	var profile applicationProfile
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return profile, errors.New("application profile is not a protected regular file")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != 0 {
		return profile, errors.New("application profile must be owned by root")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return profile, errors.New("application profile path is invalid")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return profile, err
	}
	if err := json.Unmarshal(data, &profile); err != nil {
		return profile, err
	}
	if !filepath.IsAbs(profile.StateDir) || filepath.Clean(profile.StateDir) != profile.StateDir || !strings.HasPrefix(profile.StateDir, "/var/lib/decentralabs/lab-station/") {
		return profile, errors.New("application profile state directory is invalid")
	}
	return profile, nil
}

func launchApplication(profile applicationProfile) error {
	if err := requireLeaseAdmission(profile.StateDir); err != nil {
		return err
	}
	application := profile.Application
	command := application.Command
	if command == "" || !filepath.IsAbs(command) || !strings.HasPrefix(filepath.Clean(command), "/opt/lab/apps/") {
		return fmt.Errorf("application command must be inside /opt/lab/apps")
	}
	root, err := filepath.EvalSymlinks("/opt/lab/apps")
	if err != nil {
		return fmt.Errorf("application catalog is unavailable")
	}
	resolved, err := filepath.EvalSymlinks(command)
	if err != nil {
		return fmt.Errorf("application command is unavailable")
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("application symlink escapes /opt/lab/apps")
	}
	base := strings.ToLower(filepath.Base(resolved))
	if base == "sh" || base == "bash" || base == "dash" || base == "zsh" || base == "fish" || base == "env" {
		return fmt.Errorf("shell launchers are not permitted as the application")
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() || info.Mode()&0111 == 0 {
		return fmt.Errorf("application is not executable")
	}
	child := exec.Command(resolved, application.Args...)
	child.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "HOME=" + os.Getenv("HOME"), "DISPLAY=" + os.Getenv("DISPLAY"), "XAUTHORITY=" + os.Getenv("XAUTHORITY")}
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		return err
	}
	err = child.Wait()
	timeout := time.Duration(application.CloseTimeoutSeconds) * time.Second
	if timeout < time.Second || timeout > 5*time.Minute {
		timeout = 15 * time.Second
	}
	terminateProcessGroup(child.Process.Pid, timeout)
	return err
}

func requireLeaseAdmission(stateDir string) error {
	request, err := json.Marshal(map[string]string{"operation": "lease-admission", "stateDir": stateDir})
	if err != nil {
		return err
	}
	cmd := exec.Command("/usr/bin/sudo", "-n", "/usr/lib/decentralabs/lab-station/labstation-helper")
	cmd.Stdin = bytes.NewReader(append(request, '\n'))
	output, err := cmd.Output()
	if err != nil {
		return errors.New("no active reservation permits application access")
	}
	var response struct {
		Allowed bool `json:"allowed"`
	}
	if json.Unmarshal(output, &response) != nil || !response.Allowed {
		return errors.New("no active reservation permits application access")
	}
	return nil
}

func terminateProcessGroup(pid int, grace time.Duration) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if syscall.Kill(-pid, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

func submitQueue(cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("queue submit", flag.ContinueOnError)
	id := flags.String("id", "", "operation id")
	command := flags.String("command", "", "allowlisted command")
	schemaVersion := flags.Int("schema-version", 1, "dispatcher schema version (1 or 2)")
	kind := flags.String("kind", "reservation", "lease kind for protocol 2")
	labID := flags.String("lab-id", "", "lab identifier for a reservation lease")
	reservationKey := flags.String("reservation-key", "", "reservation identifier for a reservation lease")
	leaseID := flags.String("lease-id", "", "lease identifier")
	generation := flags.Uint64("generation", 0, "active lease generation (release only)")
	issuedAt := flags.String("issued-at", "", "UTC request issue time")
	executeBefore := flags.String("execute-before", "", "UTC dispatcher execution deadline")
	notBefore := flags.String("not-before", "", "UTC lease admission start")
	expiresAt := flags.String("expires-at", "", "UTC lease expiry")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cmdArgs := flags.Args()
	if !agent.ValidOperationID(*id) {
		return fmt.Errorf("queue operation id is invalid")
	}
	if err := agent.ValidateCommand(*command, cmdArgs); err != nil {
		return err
	}
	if *schemaVersion != 1 && *schemaVersion != 2 {
		return fmt.Errorf("queue schema version must be 1 or 2")
	}
	request := agent.Request{SchemaVersion: *schemaVersion, ID: *id, Operation: "execute", Command: *command, Args: cmdArgs}
	if *schemaVersion == 2 {
		if *command != "prepare-session" && *command != "release-session" {
			return fmt.Errorf("queue schema version 2 is reserved for lease lifecycle operations")
		}
		request.IssuedAt = *issuedAt
		request.ExecuteBefore = *executeBefore
		request.Context = &agent.LeaseContext{Kind: *kind, LabID: *labID, ReservationKey: *reservationKey, LeaseID: *leaseID, Generation: *generation, NotBefore: *notBefore, ExpiresAt: *expiresAt}
		if err := agent.ValidateLeaseRequest(request, time.Now().UTC()); err != nil {
			return fmt.Errorf("queue lease request is invalid: %w", err)
		}
	}
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	inbox := filepath.Join(cfg.StateDir, "commands", "inbox")
	if err = os.MkdirAll(inbox, 0750); err != nil {
		return err
	}
	temp := filepath.Join(inbox, *id+".tmp")
	final := filepath.Join(inbox, *id+".json")
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
	if err != nil {
		return err
	}
	defer os.Remove(temp)
	if _, err = file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Link(temp, final); err != nil {
		return err
	}
	return os.Remove(temp)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: labstationctl <status-json|identity|heartbeat|local-mode|prepare-session|release-session|power|setup|config-check>")
}
