package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Application struct {
	ID string
	Command string
	Args []string
	User string
	CloseTimeoutSeconds int
}

type Config struct {
	SourcePath string
	Name string
	Profile string
	Version string
	StateDir string
	ConfigDir string
	LogDir string
	ManagementUser string
	ManagementPort int
	ManagementPublicKey string
	Transport string
	Supervisor string
	GuardGraceSeconds int
	AllowLocalSessionEviction bool
	Application Application
}

func Defaults() Config {
	return Config{
		Name: "", Profile: "dedicated", Version: "0.1.0",
		StateDir: "/var/lib/decentralabs/lab-station/data",
		ConfigDir: "/etc/decentralabs/lab-station",
		LogDir: "/var/log/decentralabs/lab-station",
		ManagementUser: "labstation-ops", ManagementPort: 22, Transport: "ssh", Supervisor: "auto",
		GuardGraceSeconds: 30,
		Application: Application{ID: "lab-app", User: "labuser", CloseTimeoutSeconds: 15},
	}
}

func Load(path string) (Config, error) {
	cfg := Defaults()
	file, err := os.Open(path)
	if err != nil { return cfg, err }
	defer file.Close()
	cfg.SourcePath = filepath.Clean(path)

	section := ""
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for lineNo := 1; scanner.Scan(); lineNo++ {
		line := strings.TrimSpace(stripComment(scanner.Text()))
		if line == "" { continue }
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1:len(line)-1])
			continue
		}
		key, raw, ok := strings.Cut(line, "=")
		if !ok { return cfg, fmt.Errorf("station.toml:%d: expected key = value", lineNo) }
		value, err := parseValue(strings.TrimSpace(raw))
		if err != nil { return cfg, fmt.Errorf("station.toml:%d: %w", lineNo, err) }
		if err := cfg.assign(section, strings.TrimSpace(key), value); err != nil {
			return cfg, fmt.Errorf("station.toml:%d: %w", lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil { return cfg, err }
	if cfg.Name == "" { cfg.Name, _ = os.Hostname() }
	if cfg.Profile != "dedicated" && cfg.Profile != "hybrid" && cfg.Profile != "fmu-only" {
		return cfg, errors.New("profile must be dedicated, hybrid, or fmu-only")
	}
	if cfg.ManagementUser != "labstation-ops" { return cfg, errors.New("management user must be labstation-ops") }
	if cfg.ManagementPort < 1 || cfg.ManagementPort > 65535 { return cfg, errors.New("management_port must be between 1 and 65535") }
	if cfg.Application.User != "labuser" { return cfg, errors.New("application user must be labuser") }
	if !filepath.IsAbs(cfg.StateDir) || !pathWithin(cfg.StateDir, "/var/lib/decentralabs/lab-station") {
		return cfg, errors.New("state_dir must stay under /var/lib/decentralabs/lab-station")
	}
	if cfg.ConfigDir != "/etc/decentralabs/lab-station" {
		return cfg, errors.New("config_dir must be /etc/decentralabs/lab-station")
	}
	if !filepath.IsAbs(cfg.LogDir) || !pathWithin(cfg.LogDir, "/var/log/decentralabs/lab-station") {
		return cfg, errors.New("log_dir must stay under /var/log/decentralabs/lab-station")
	}
	if cfg.Application.Command != "" && !strings.HasPrefix(cfg.Application.Command, "/") {
		return cfg, errors.New("application command must be an absolute path")
	}
	if cfg.Application.Command != "" {
		name := strings.ToLower(filepath.Base(cfg.Application.Command))
		if name == "sh" || name == "bash" || name == "dash" || name == "zsh" || name == "fish" || name == "env" {
			return cfg, errors.New("application command must name a catalogued executable, not a shell")
		}
	}
	if cfg.GuardGraceSeconds < 0 || cfg.GuardGraceSeconds > 90 {
		return cfg, errors.New("guard_grace_seconds must be between 0 and 90")
	}
	if cfg.Application.CloseTimeoutSeconds < 1 || cfg.Application.CloseTimeoutSeconds > 300 {
		return cfg, errors.New("close_timeout_seconds must be between 1 and 300")
	}
	if len(cfg.Application.Args) > 32 {
		return cfg, errors.New("application.args may contain at most 32 values")
	}
	for _, arg := range cfg.Application.Args {
		if len(arg) > 1024 || strings.ContainsAny(arg, "\r\n\x00`$|;&><") {
			return cfg, errors.New("application.args contains unsupported shell or control characters")
		}
	}
	return cfg, nil
}

func pathWithin(path, root string) bool {
	cleanPath := filepath.Clean(path)
	cleanRoot := filepath.Clean(root)
	relative, err := filepath.Rel(cleanRoot, cleanPath)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (c *Config) assign(section, key string, value any) error {
	text, isString := value.(string)
	boolean, isBool := value.(bool)
	integer, isInt := value.(int)
	switch section + "." + key {
	case "station.name": if !isString { return typeError(key, "string") }; c.Name = text
	case "station.profile": if !isString { return typeError(key, "string") }; c.Profile = text
	case "station.version": if !isString { return typeError(key, "string") }; c.Version = text
	case "station.state_dir": if !isString { return typeError(key, "string") }; c.StateDir = text
	case "station.config_dir": if !isString { return typeError(key, "string") }; c.ConfigDir = text
	case "station.log_dir": if !isString { return typeError(key, "string") }; c.LogDir = text
	case "station.management_user": if !isString { return typeError(key, "string") }; c.ManagementUser = text
	case "station.management_port": if !isInt { return typeError(key, "integer") }; c.ManagementPort = integer
	case "station.management_public_key": if !isString { return typeError(key, "string") }; c.ManagementPublicKey = text
	case "station.transport": if !isString { return typeError(key, "string") }; c.Transport = text
	case "station.supervisor": if !isString { return typeError(key, "string") }; c.Supervisor = text
	case "station.guard_grace_seconds": if !isInt { return typeError(key, "integer") }; c.GuardGraceSeconds = integer
	case "station.allow_local_session_eviction": if !isBool { return typeError(key, "boolean") }; c.AllowLocalSessionEviction = boolean
	case "application.id": if !isString { return typeError(key, "string") }; c.Application.ID = text
	case "application.command": if !isString { return typeError(key, "string") }; c.Application.Command = text
	case "application.args":
		items, ok := value.([]string); if !ok { return typeError(key, "string array") }; c.Application.Args = items
	case "application.user": if !isString { return typeError(key, "string") }; c.Application.User = text
	case "application.close_timeout_seconds": if !isInt { return typeError(key, "integer") }; c.Application.CloseTimeoutSeconds = integer
	default: return fmt.Errorf("unknown setting %s.%s", section, key)
	}
	return nil
}

func typeError(key, expected string) error { return fmt.Errorf("%s must be a %s", key, expected) }

func stripComment(line string) string {
	quoted, escaped := false, false
	for i, r := range line {
		if escaped { escaped = false; continue }
		if r == '\\' && quoted { escaped = true; continue }
		if r == '"' { quoted = !quoted; continue }
		if r == '#' && !quoted { return line[:i] }
	}
	return line
}

func parseValue(raw string) (any, error) {
	if strings.HasPrefix(raw, "\"") { return strconv.Unquote(raw) }
	if raw == "true" { return true, nil }
	if raw == "false" { return false, nil }
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		body := strings.TrimSpace(raw[1:len(raw)-1])
		if body == "" { return []string{}, nil }
		parts := make([]string, 0, 4)
		start := 0
		quoted := false
		escaped := false
		for i := 0; i < len(body); i++ {
			if escaped { escaped = false; continue }
			if quoted && body[i] == '\\' { escaped = true; continue }
			if body[i] == '"' { quoted = !quoted; continue }
			if body[i] == ',' && !quoted { parts = append(parts, body[start:i]); start = i + 1 }
		}
		if quoted || escaped { return nil, errors.New("invalid syntax") }
		parts = append(parts, body[start:])
		values := make([]string, 0, len(parts))
		for index, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" && index == len(parts)-1 { continue }
			item, err := strconv.Unquote(part); if err != nil { return nil, err }; values = append(values, item)
		}
		return values, nil
	}
	return strconv.Atoi(raw)
}
