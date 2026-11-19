package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	tinyDeskXauthShimDir                    = "/usr/lib/decentralabs/lab-station/xrdp-auth-bin"
	tinyDeskXorgShimPath                    = "/usr/lib/decentralabs/lab-station/xrdp-xorg"
	defaultXrdpSessionPath                  = "/sbin:/bin:/usr/bin:/usr/local/bin"
	xrdpOriginalXorgKey                     = "LABSTATION_XORG_EXEC"
	tinyDeskDisconnectGraceSeconds          = 120
	tinyDeskDisconnectedSessionLimitSeconds = 180
)

func xrdpIDExecutable() (string, error) {
	for _, path := range []string{"/usr/bin/id", "/bin/id"} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
			continue
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid == 0 {
			return path, nil
		}
	}
	return "", fmt.Errorf("a root-owned id executable is required for XRDP account isolation")
}

func rootOwnedXrdpDirectory(path string) bool {
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return false
	}
	for {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 {
			return false
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return false
		}
		parent := filepath.Dir(path)
		if parent == path {
			return true
		}
		path = parent
	}
}

func rootOwnedXrdpExecutable(path, expected string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0111 == 0 || info.Mode().Perm()&0022 != 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return false
	}
	content, err := os.ReadFile(path)
	return err == nil && string(content) == expected
}

func xrdpSessionHasXauth(searchPath, workingDirectory string) bool {
	for _, directory := range strings.Split(searchPath, ":") {
		if directory == "" {
			directory = workingDirectory
		} else if !filepath.IsAbs(directory) {
			directory = filepath.Join(workingDirectory, directory)
		}
		info, err := os.Stat(filepath.Join(directory, "xauth"))
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0001 != 0 {
			return true
		}
	}
	return false
}

// transformXrdpSesmanConfig preserves the configured Xorg arguments and the
// existing session PATH. The shims dispatch on the actual account UID.
func transformXrdpSesmanConfig(input string) (string, string, string, error) {
	lines := strings.Split(input, "\n")
	section := ""
	xorgSections, sessionSections := 0, 0
	xorgIndex, pathIndex, originalIndex, sessionEnd := -1, -1, -1, -1
	xorgValue, pathValue, storedXorg := "", "", ""
	for index, line := range lines {
		value := strings.TrimSpace(line)
		if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
			if section == "sessionvariables" && sessionEnd == -1 {
				sessionEnd = index
			}
			section = strings.ToLower(strings.TrimSpace(value[1 : len(value)-1]))
			switch section {
			case "xorg":
				xorgSections++
			case "sessionvariables":
				sessionSections++
			}
			continue
		}
		if strings.HasPrefix(value, "#") || strings.HasPrefix(value, ";") {
			continue
		}
		key, item, ok := strings.Cut(value, "=")
		if !ok {
			continue
		}
		key, item = strings.TrimSpace(key), strings.TrimSpace(item)
		if section == "xorg" && strings.EqualFold(key, "param") && xorgIndex == -1 {
			xorgIndex, xorgValue = index, item
		}
		if section != "sessionvariables" {
			continue
		}
		switch {
		case strings.EqualFold(key, "XAUTHORITY"):
			return "", "", "", fmt.Errorf("sesman.ini has a global XAUTHORITY; review it before configuring Tiny Desk")
		case strings.EqualFold(key, "PATH"):
			if key != "PATH" {
				return "", "", "", fmt.Errorf("sesman.ini has a session path variable with nonstandard casing")
			}
			if pathIndex != -1 {
				return "", "", "", fmt.Errorf("sesman.ini has multiple session PATH entries")
			}
			pathIndex, pathValue = index, item
		case strings.EqualFold(key, xrdpOriginalXorgKey):
			if key != xrdpOriginalXorgKey {
				return "", "", "", fmt.Errorf("sesman.ini has a managed Xorg variable with nonstandard casing")
			}
			if originalIndex != -1 {
				return "", "", "", fmt.Errorf("sesman.ini has multiple managed Xorg entries")
			}
			originalIndex, storedXorg = index, item
		}
	}
	if xorgSections != 1 || xorgIndex == -1 || xorgValue == "" || sessionSections > 1 {
		return "", "", "", fmt.Errorf("sesman.ini requires one usable [Xorg] section and at most one [SessionVariables] section")
	}
	if xorgValue == tinyDeskXorgShimPath {
		if originalIndex == -1 || storedXorg == "" || storedXorg == tinyDeskXorgShimPath {
			return "", "", "", fmt.Errorf("managed Xorg path has no valid original executable")
		}
		xorgValue = storedXorg
	} else if originalIndex != -1 {
		return "", "", "", fmt.Errorf("sesman.ini has an unmanaged LABSTATION_XORG_EXEC entry")
	}
	if strings.ContainsAny(xorgValue, "\r\n\x00") {
		return "", "", "", fmt.Errorf("invalid Xorg executable path")
	}
	if pathIndex == -1 {
		pathValue = defaultXrdpSessionPath
	} else if strings.HasPrefix(pathValue, tinyDeskXauthShimDir+":") {
		pathValue = strings.TrimPrefix(pathValue, tinyDeskXauthShimDir+":")
	}
	if pathValue == "" || strings.ContainsAny(pathValue, "\r\n\x00") {
		return "", "", "", fmt.Errorf("invalid XRDP session PATH")
	}
	for _, part := range strings.Split(pathValue, ":") {
		if part == tinyDeskXauthShimDir {
			return "", "", "", fmt.Errorf("XRDP session PATH already contains the managed directory")
		}
	}
	lines[xorgIndex] = "param=" + tinyDeskXorgShimPath
	if pathIndex != -1 {
		lines[pathIndex] = "PATH=" + tinyDeskXauthShimDir + ":" + pathValue
	}
	if originalIndex != -1 {
		lines[originalIndex] = xrdpOriginalXorgKey + "=" + xorgValue
	}
	if sessionSections == 0 {
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, "[SessionVariables]", "PATH="+tinyDeskXauthShimDir+":"+pathValue, xrdpOriginalXorgKey+"="+xorgValue, "")
	} else {
		if sessionEnd == -1 {
			sessionEnd = len(lines)
			if len(lines) > 0 && lines[len(lines)-1] == "" {
				sessionEnd--
			}
		}
		var additions []string
		if pathIndex == -1 {
			additions = append(additions, "PATH="+tinyDeskXauthShimDir+":"+pathValue)
		}
		if originalIndex == -1 {
			additions = append(additions, xrdpOriginalXorgKey+"="+xorgValue)
		}
		if len(additions) != 0 {
			output := append([]string{}, lines[:sessionEnd]...)
			output = append(output, additions...)
			lines = append(output, lines[sessionEnd:]...)
		}
	}
	updated, err := transformXrdpDisconnectedSessionPolicy(strings.Join(lines, "\n"))
	if err != nil {
		return "", "", "", err
	}
	return updated, xorgValue, pathValue, nil
}

// transformXrdpDisconnectedSessionPolicy keeps XRDP sessions available for a
// short reconnect window, then lets sesman terminate the disconnected Xorg
// session. The policy is host-wide for xorgxrdp sessions.
func transformXrdpDisconnectedSessionPolicy(input string) (string, error) {
	lines := strings.Split(input, "\n")
	section := ""
	sessionsSections := 0
	sessionsEnd := -1
	killIndex, limitIndex := -1, -1
	for index, line := range lines {
		value := strings.TrimSpace(line)
		if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
			if section == "sessions" && sessionsEnd == -1 {
				sessionsEnd = index
			}
			section = strings.ToLower(strings.TrimSpace(value[1 : len(value)-1]))
			if section == "sessions" {
				sessionsSections++
			}
			continue
		}
		if section != "sessions" || strings.HasPrefix(value, "#") || strings.HasPrefix(value, ";") {
			continue
		}
		key, _, ok := strings.Cut(value, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "killdisconnected":
			if killIndex != -1 {
				return "", fmt.Errorf("sesman.ini has multiple KillDisconnected entries")
			}
			killIndex = index
		case "disconnectedtimelimit":
			if limitIndex != -1 {
				return "", fmt.Errorf("sesman.ini has multiple DisconnectedTimeLimit entries")
			}
			limitIndex = index
		}
	}
	if sessionsSections > 1 {
		return "", fmt.Errorf("sesman.ini has multiple [Sessions] sections")
	}
	if sessionsSections == 0 {
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, "[Sessions]", "KillDisconnected=true", fmt.Sprintf("DisconnectedTimeLimit=%d", tinyDeskDisconnectedSessionLimitSeconds), "")
		return strings.Join(lines, "\n"), nil
	}

	if killIndex != -1 {
		lines[killIndex] = "KillDisconnected=true"
	}
	if limitIndex != -1 {
		lines[limitIndex] = fmt.Sprintf("DisconnectedTimeLimit=%d", tinyDeskDisconnectedSessionLimitSeconds)
	}
	if sessionsEnd == -1 {
		sessionsEnd = len(lines)
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			sessionsEnd--
		}
	}
	var additions []string
	if killIndex == -1 {
		additions = append(additions, "KillDisconnected=true")
	}
	if limitIndex == -1 {
		additions = append(additions, fmt.Sprintf("DisconnectedTimeLimit=%d", tinyDeskDisconnectedSessionLimitSeconds))
	}
	if len(additions) > 0 {
		updated := append([]string{}, lines[:sessionsEnd]...)
		updated = append(updated, additions...)
		lines = append(updated, lines[sessionsEnd:]...)
	}
	return strings.Join(lines, "\n"), nil
}

func parseXrdpDisconnectedSessionPolicy(input string) (killDisconnected, hasKillDisconnected bool, limitSeconds int, hasLimitSeconds bool, err error) {
	section := ""
	sessionsSections := 0
	killEntries, limitEntries := 0, 0
	for _, line := range strings.Split(input, "\n") {
		value := strings.TrimSpace(line)
		if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
			section = strings.ToLower(strings.TrimSpace(value[1 : len(value)-1]))
			if section == "sessions" {
				sessionsSections++
			}
			continue
		}
		if section != "sessions" || strings.HasPrefix(value, "#") || strings.HasPrefix(value, ";") {
			continue
		}
		key, item, ok := strings.Cut(value, "=")
		if !ok {
			continue
		}
		item = strings.TrimSpace(item)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "killdisconnected":
			killEntries++
			if killEntries > 1 {
				return false, false, 0, false, fmt.Errorf("sesman.ini has multiple KillDisconnected entries")
			}
			switch strings.ToLower(item) {
			case "true", "yes", "1", "on":
				killDisconnected = true
			case "false", "no", "0", "off":
				killDisconnected = false
			default:
				return false, false, 0, false, fmt.Errorf("sesman.ini has an invalid KillDisconnected value")
			}
			hasKillDisconnected = true
		case "disconnectedtimelimit":
			limitEntries++
			if limitEntries > 1 {
				return false, false, 0, false, fmt.Errorf("sesman.ini has multiple DisconnectedTimeLimit entries")
			}
			limitSeconds, err = strconv.Atoi(item)
			if err != nil {
				return false, false, 0, false, fmt.Errorf("sesman.ini has an invalid DisconnectedTimeLimit value")
			}
			hasLimitSeconds = true
		}
	}
	if sessionsSections > 1 {
		return false, false, 0, false, fmt.Errorf("sesman.ini has multiple [Sessions] sections")
	}
	return killDisconnected, hasKillDisconnected, limitSeconds, hasLimitSeconds, nil
}

func xrdpDisconnectedSessionPolicyStatus(path string) map[string]any {
	status := map[string]any{
		"source":     "xrdp-sesman",
		"scope":      "all-xorgxrdp-sessions",
		"available":  false,
		"configured": false,
		"ready":      false,
	}
	data, err := os.ReadFile(path)
	if err != nil {
		status["message"] = "XRDP sesman configuration is unavailable"
		return status
	}
	status["available"] = true
	killDisconnected, hasKillDisconnected, limitSeconds, hasLimitSeconds, err := parseXrdpDisconnectedSessionPolicy(string(data))
	if err != nil {
		status["message"] = err.Error()
		return status
	}
	if hasKillDisconnected {
		status["killDisconnected"] = killDisconnected
	}
	if hasLimitSeconds {
		status["limitSeconds"] = limitSeconds
	}
	configured := hasKillDisconnected && killDisconnected && hasLimitSeconds && limitSeconds == tinyDeskDisconnectedSessionLimitSeconds
	status["configured"] = configured
	status["ready"] = configured
	if !configured {
		status["message"] = fmt.Sprintf("expected KillDisconnected=true and DisconnectedTimeLimit=%d; Lab Station closes Tiny Desk after %d seconds", tinyDeskDisconnectedSessionLimitSeconds, tinyDeskDisconnectGraceSeconds)
	}
	return status
}

func xrdpShellLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// renderXrdpXauthShim redirects the legacy sesman xauth call for labuser.
// Other users and other xauth invocations go to the original executable.
func renderXrdpXauthShim(uid uint32, idPath, originalPath, authorityPath string) string {
	return fmt.Sprintf(`#!/bin/sh
PATH=%s
export PATH
if [ "$(%s -u)" = '%d' ] && [ "${1-}" = '-q' ] && [ "${2-}" = '-f' ] && [ "${3-}" = '.Xauthority' ]; then
    shift 3
    exec xauth -q -f %s "$@"
fi
exec xauth "$@"
`, xrdpShellLiteral(originalPath), xrdpShellLiteral(idPath), uid, xrdpShellLiteral(authorityPath))
}

// renderXrdpXorgShim redirects the Xorg log only for the managed account.
func renderXrdpXorgShim(uid uint32, idPath, xorgPath, logPath string) string {
	return fmt.Sprintf(`#!/bin/sh
if [ "$(%s -u)" != '%d' ]; then
    exec %s "$@"
fi
remaining=$#
replace_next=0
found_log=0
while [ "$remaining" -gt 0 ]; do
    argument=$1
    shift
    remaining=$((remaining - 1))
    if [ "$replace_next" -eq 1 ]; then
        set -- "$@" %s
        replace_next=0
        found_log=1
    else
        set -- "$@" "$argument"
        if [ "$argument" = '-logfile' ]; then
            replace_next=1
        fi
    fi
done
if [ "$replace_next" -ne 0 ]; then
    echo 'Xorg -logfile is missing its path' >&2
    exit 2
fi
if [ "$found_log" -eq 0 ]; then
    set -- "$@" -logfile %s
fi
exec %s "$@"
`, xrdpShellLiteral(idPath), uid, xrdpShellLiteral(xorgPath), xrdpShellLiteral(logPath), xrdpShellLiteral(logPath), xrdpShellLiteral(xorgPath))
}
