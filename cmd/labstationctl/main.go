package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
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
	if command == "app" && len(os.Args) > 2 {
		switch os.Args[2] {
		case "launch":
			profile, err := loadApplicationProfile("/usr/share/decentralabs/lab-station/app-profile.json")
			if err != nil || launchApplication(profile) != nil {
				fmt.Fprintln(os.Stderr, "application launch denied")
				os.Exit(2)
			}
			return
		case "apply":
			cfgPath := os.Getenv("LABSTATION_CONFIG")
			if cfgPath == "" {
				cfgPath = "/etc/decentralabs/lab-station/station.toml"
			}
			if err := agent.ApplyApplicationProfile(cfgPath); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
			fmt.Println("Tiny Desk application profile updated")
			return
		}
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
	StateDir     string                     `json:"stateDir"`
	Application  config.Application         `json:"application"`
	Applications []config.ApplicationLaunch `json:"applications,omitempty"`
}

func loadApplicationProfile(path string) (applicationProfile, error) {
	return loadApplicationProfileOwnedBy(path, 0)
}

func loadApplicationProfileOwnedBy(path string, expectedUID uint32) (applicationProfile, error) {
	var profile applicationProfile
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
		return profile, errors.New("application profile is not a protected regular file")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != expectedUID {
		return profile, fmt.Errorf("application profile must be owned by uid %d", expectedUID)
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
	// Provider-managed RDP may launch Tiny Desk without a reservation lease.
	// Dispatcher leases continue to authorize prepare-session and release-session.
	applications := profile.Applications
	legacyApplication := len(applications) == 0
	if legacyApplication {
		if profile.Application.Command == "" {
			return errors.New("no application is configured")
		}
		applications = []config.ApplicationLaunch{{ID: profile.Application.ID, Command: profile.Application.Command, Args: profile.Application.Args}}
	}
	if !legacyApplication {
		if err := config.ValidateApplicationLaunches(applications); err != nil {
			return err
		}
	}
	root, err := filepath.EvalSymlinks("/opt/lab/apps")
	if err != nil {
		return fmt.Errorf("application catalog is unavailable")
	}
	type launch struct {
		command string
		args    []string
	}
	launches := make([]launch, 0, len(applications))
	for _, application := range applications {
		command := application.Command
		if command == "" || !filepath.IsAbs(command) || !strings.HasPrefix(filepath.Clean(command), "/opt/lab/apps/") {
			return fmt.Errorf("application command must be inside /opt/lab/apps")
		}
		resolved, resolveErr := filepath.EvalSymlinks(command)
		if resolveErr != nil {
			return fmt.Errorf("application %s is unavailable", application.ID)
		}
		relative, relErr := filepath.Rel(root, resolved)
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("application symlink escapes /opt/lab/apps")
		}
		base := strings.ToLower(filepath.Base(resolved))
		if base == "sh" || base == "bash" || base == "dash" || base == "zsh" || base == "fish" || base == "env" {
			return fmt.Errorf("shell launchers are not permitted as an application")
		}
		info, statErr := os.Stat(resolved)
		if statErr != nil || info.IsDir() || info.Mode()&0111 == 0 {
			return fmt.Errorf("application %s is not executable", application.ID)
		}
		launches = append(launches, launch{command: resolved, args: application.Args})
	}
	children := make([]*exec.Cmd, 0, len(launches))
	for _, item := range launches {
		child := exec.Command(item.command, item.args...)
		child.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "HOME=" + os.Getenv("HOME"), "DISPLAY=" + os.Getenv("DISPLAY"), "XAUTHORITY=" + os.Getenv("XAUTHORITY")}
		child.Stdin = os.Stdin
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := child.Start(); err != nil {
			for _, started := range children {
				terminateProcessGroup(started.Process.Pid, applicationCloseTimeout(profile.Application.CloseTimeoutSeconds))
				_ = started.Wait()
			}
			return err
		}
		children = append(children, child)
	}
	type waitResult struct {
		child *exec.Cmd
		err   error
	}
	results := make(chan waitResult, len(children))
	for _, child := range children {
		go func(child *exec.Cmd) { results <- waitResult{child: child, err: child.Wait()} }(child)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	remaining := len(children)
	var firstErr error
	for remaining > 0 {
		select {
		case result := <-results:
			remaining--
			terminateProcessGroup(result.child.Process.Pid, applicationCloseTimeout(profile.Application.CloseTimeoutSeconds))
			if result.err != nil && firstErr == nil {
				firstErr = result.err
			}
		case <-signals:
			for _, child := range children {
				terminateProcessGroup(child.Process.Pid, applicationCloseTimeout(profile.Application.CloseTimeoutSeconds))
			}
			for remaining > 0 {
				<-results
				remaining--
			}
			return nil
		}
	}
	err = firstErr
	return err
}

func applicationCloseTimeout(seconds int) time.Duration {
	timeout := time.Duration(seconds) * time.Second
	if timeout < time.Second || timeout > 5*time.Minute {
		timeout = 15 * time.Second
	}
	return timeout
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
	fmt.Fprintln(os.Stderr, "usage: labstationctl <status-json|identity|heartbeat|local-mode|prepare-session|release-session|power|setup|config-check|app apply>")
}
