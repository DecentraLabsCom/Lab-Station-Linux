package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

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
	case "app":
		if len(os.Args) > 2 && os.Args[2] == "launch" {
			if err := launchApplication(cfg); err != nil {
				fmt.Fprintln(os.Stderr, "application launch failed")
				os.Exit(2)
			}
			return
		}
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

func launchApplication(cfg config.Config) error {
	command := cfg.Application.Command
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
	child := exec.Command(resolved, cfg.Application.Args...)
	child.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "HOME=" + os.Getenv("HOME"), "DISPLAY=" + os.Getenv("DISPLAY"), "XAUTHORITY=" + os.Getenv("XAUTHORITY")}
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := child.Start(); err != nil {
		return err
	}
	return child.Wait()
}

func submitQueue(cfg config.Config, args []string) error {
	flags := flag.NewFlagSet("queue submit", flag.ContinueOnError)
	id := flags.String("id", "", "operation id")
	command := flags.String("command", "", "allowlisted command")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cmdArgs := flags.Args()
	if *id == "" || strings.ContainsAny(*id, "/\\\r\n") || len(*id) > 96 {
		return fmt.Errorf("queue operation id is invalid")
	}
	if err := agent.ValidateCommand(*command, cmdArgs); err != nil {
		return err
	}
	request := agent.Request{SchemaVersion: 1, ID: *id, Operation: "execute", Command: *command, Args: cmdArgs}
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
	if err = os.WriteFile(temp, append(data, '\n'), 0640); err != nil {
		return err
	}
	return os.Rename(temp, final)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: labstationctl <status-json|identity|heartbeat|local-mode|prepare-session|release-session|power|setup|config-check>")
}
