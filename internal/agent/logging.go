package agent

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/decentralabs/lab-station-linux/internal/config"
	"github.com/decentralabs/lab-station-linux/internal/host"
)

const (
	stationLogFilename      = "labstation.log"
	stationLogSchemaVersion = 1
	stationLogRetention     = 180 * 24 * time.Hour
	stationLogSummaryPeriod = 5 * time.Minute
	stationLogWarningPeriod = 5 * time.Minute
)

type logWarningState struct {
	lastLogged time.Time
	suppressed uint64
}

func (a *Agent) log(event string, fields map[string]any) {
	a.logWithLevel("INFO", event, fields)
}

func (a *Agent) logWarning(event string, fields map[string]any) {
	a.logWithLevel("WARNING", event, fields)
}

func (a *Agent) logError(event string, fields map[string]any) {
	a.logWithLevel("ERROR", event, fields)
}

func (a *Agent) logDebug(event string, fields map[string]any) {
	a.logWithLevel("DEBUG", event, fields)
}

func (a *Agent) logWithLevel(level, event string, fields map[string]any) {
	if !stationLogLevelEnabled(level) {
		return
	}
	if event == "" {
		event = "station-log"
	}

	now := time.Now()
	details := make(map[string]any, len(fields))
	for key, value := range fields {
		switch key {
		case "event", "timestamp", "host", "profile", "schemaVersion", "level", "message":
			continue
		default:
			details[key] = value
		}
	}
	message := strings.ReplaceAll(event, "-", " ")
	if value, ok := fields["message"].(string); ok && value != "" {
		message = value
	}
	originalMessage := message
	logEvent := event

	a.logMu.Lock()
	defer a.logMu.Unlock()

	if level == "WARNING" {
		keyPayload, _ := json.Marshal(struct {
			Event   string `json:"event"`
			Message string `json:"message"`
		}{Event: event, Message: message})
		key := string(keyPayload)
		if a.logWarnings == nil {
			a.logWarnings = make(map[string]logWarningState)
		}
		state, found := a.logWarnings[key]
		if found && now.Sub(state.lastLogged) < stationLogWarningPeriod {
			state.suppressed++
			a.logWarnings[key] = state
			return
		}
		if found && state.suppressed > 0 {
			logEvent = "warning-repeat-summary"
			message = fmt.Sprintf("Repeated warning %s %d times in the last five minutes", event, state.suppressed+1)
			details["repeatedEvent"] = event
			details["originalMessage"] = originalMessage
			details["repeatCount"] = state.suppressed + 1
		}
		a.logWarnings[key] = logWarningState{lastLogged: now}
	}

	hostName := stationLogHostName(a.Config.Name)
	record := map[string]any{
		"schemaVersion": stationLogSchemaVersion,
		"timestamp":     now.UTC().Format(time.RFC3339),
		"level":         level,
		"event":         logEvent,
		"message":       message,
		"platform":      "linux",
		"host":          hostName,
		"profile":       a.Config.Profile,
		"details":       details,
	}

	if err := a.maintainStationLogLocked(now); err != nil {
		return
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return
	}
	encoded = append(encoded, '\n')
	if a.usesOpenRCLogOutput() {
		_, _ = os.Stdout.Write(encoded)
		return
	}
	file, err := os.OpenFile(filepath.Join(a.Config.LogDir, stationLogFilename), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0640)
	if err != nil {
		return
	}
	_, _ = file.Write(encoded)
	_ = file.Close()
}

func (a *Agent) usesOpenRCLogOutput() bool {
	a.logSupervisorOnce.Do(func() {
		if supervisor := host.DetectSupervisor(); supervisor != nil {
			a.logToSupervisor = supervisor.Name() == "openrc"
			return
		}
		a.logToSupervisor = strings.EqualFold(a.Config.Supervisor, "openrc")
	})
	return a.logToSupervisor
}

func (a *Agent) LogProcessFailure(event string, err error) {
	fields := map[string]any{}
	if err != nil {
		fields["error"] = err.Error()
	}
	a.logError(event, fields)
	if a.usesOpenRCLogOutput() {
		return
	}
	record := map[string]any{
		"schemaVersion": stationLogSchemaVersion,
		"timestamp":     time.Now().UTC().Format(time.RFC3339),
		"level":         "ERROR",
		"event":         event,
		"message":       strings.ReplaceAll(event, "-", " "),
		"platform":      "linux",
		"host":          stationLogHostName(a.Config.Name),
		"profile":       a.Config.Profile,
		"details":       fields,
	}
	if encoded, marshalErr := json.Marshal(record); marshalErr == nil {
		_, _ = fmt.Fprintln(os.Stderr, string(encoded))
	}
}

func LogStartupFailure(cfg config.Config, event string, err error) {
	New(cfg).LogProcessFailure(event, err)
}

func stationLogHostName(configured string) string {
	if hostName := strings.TrimSpace(configured); hostName != "" {
		return hostName
	}
	hostName, _ := os.Hostname()
	return hostName
}

func stationLogLevelEnabled(level string) bool {
	configured := strings.ToUpper(strings.TrimSpace(os.Getenv("LABSTATION_LOG_LEVEL")))
	switch configured {
	case "", "DEBUG", "INFO", "WARNING", "ERROR":
	default:
		configured = "INFO"
	}
	if configured == "DEBUG" {
		return true
	}
	switch level {
	case "INFO":
		return configured == "" || configured == "INFO"
	case "WARNING":
		return configured == "" || configured == "INFO" || configured == "WARNING"
	case "ERROR":
		return true
	default:
		return false
	}
}

func (a *Agent) logHeartbeatSummary(ready bool) {
	now := time.Now()
	a.heartbeatLogMu.Lock()
	shouldLog := !a.heartbeatLogInitialized || ready != a.lastHeartbeatReady || now.Sub(a.lastHeartbeatLog) >= stationLogSummaryPeriod
	if shouldLog {
		a.heartbeatLogInitialized = true
		a.lastHeartbeatLog = now
		a.lastHeartbeatReady = ready
	}
	a.heartbeatLogMu.Unlock()
	if shouldLog {
		a.log("station-status-summary", map[string]any{"ready": ready})
	}
}

// maintainStationLogLocked rotates the active file at the first write on a
// new local day. Copy-and-truncate keeps the inode used by OpenRC's output_log
// and error_log descriptors valid while the daemon remains running.
func (a *Agent) maintainStationLogLocked(now time.Time) error {
	if err := os.MkdirAll(a.Config.LogDir, 0750); err != nil {
		return err
	}
	currentPath := filepath.Join(a.Config.LogDir, stationLogFilename)
	info, err := os.Stat(currentPath)
	if err == nil && info.Size() > 0 {
		fileDate := info.ModTime().In(time.Local).Format("20060102")
		currentDate := now.In(time.Local).Format("20060102")
		currentSchema := stationLogHasCurrentSchema(currentPath)
		if fileDate != currentDate || !currentSchema {
			prefix := "labstation"
			if !currentSchema {
				prefix = "labstation-legacy"
			}
			archivePath := uniqueStationLogArchivePath(a.Config.LogDir, prefix, fileDate)
			if err := gzipAndTruncate(currentPath, archivePath); err != nil {
				plainArchive := strings.TrimSuffix(archivePath, ".gz")
				if copyErr := copyAndTruncate(currentPath, plainArchive); copyErr != nil {
					return errors.Join(err, copyErr)
				}
			}
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}

	if a.lastLogPrune.IsZero() || now.Sub(a.lastLogPrune) >= time.Hour {
		if err := pruneStationLogArchives(a.Config.LogDir, now); err != nil {
			return err
		}
		a.lastLogPrune = now
	}
	return nil
}

func stationLogHasCurrentSchema(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()

	line, err := bufio.NewReader(file).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return false
	}
	line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
	var header struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	return json.Unmarshal(line, &header) == nil && header.SchemaVersion == stationLogSchemaVersion
}

func gzipAndTruncate(currentPath, archivePath string) error {
	input, err := os.Open(currentPath)
	if err != nil {
		return err
	}
	defer input.Close()

	temporary, err := os.CreateTemp(filepath.Dir(archivePath), ".labstation-log-*.gz.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0640); err != nil {
		_ = temporary.Close()
		return err
	}
	gzipWriter := gzip.NewWriter(temporary)
	if _, err := io.Copy(gzipWriter, input); err != nil {
		_ = gzipWriter.Close()
		_ = temporary.Close()
		return err
	}
	if err := gzipWriter.Close(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, archivePath); err != nil {
		return err
	}
	if err := os.Truncate(currentPath, 0); err != nil {
		_ = os.Remove(archivePath)
		return err
	}
	return nil
}

func copyAndTruncate(currentPath, archivePath string) error {
	input, err := os.Open(currentPath)
	if err != nil {
		return err
	}
	defer input.Close()

	temporary, err := os.CreateTemp(filepath.Dir(archivePath), ".labstation-log-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0640); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := io.Copy(temporary, input); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, archivePath); err != nil {
		return err
	}
	if err := os.Truncate(currentPath, 0); err != nil {
		_ = os.Remove(archivePath)
		return err
	}
	return nil
}

func uniqueStationLogArchivePath(logDir, prefix, date string) string {
	base := filepath.Join(logDir, prefix+"-"+date)
	for suffix := 1; ; suffix++ {
		candidate := base + ".log.gz"
		if suffix > 1 {
			candidate = base + "-" + strconv.Itoa(suffix) + ".log.gz"
		}
		_, gzipErr := os.Stat(candidate)
		_, plainErr := os.Stat(strings.TrimSuffix(candidate, ".gz"))
		if os.IsNotExist(gzipErr) && os.IsNotExist(plainErr) {
			return candidate
		}
	}
}

func pruneStationLogArchives(logDir string, now time.Time) error {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return err
	}
	cutoff := now.In(time.Local).AddDate(0, 0, -int(stationLogRetention/(24*time.Hour)))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		archiveDate, ok := stationLogArchiveDate(entry.Name())
		if !ok || !archiveDate.Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(logDir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func stationLogArchiveDate(name string) (time.Time, bool) {
	if !strings.HasPrefix(name, "labstation-") {
		return time.Time{}, false
	}
	if !strings.HasSuffix(name, ".log.gz") && !strings.HasSuffix(name, ".log") {
		return time.Time{}, false
	}
	base := strings.TrimSuffix(strings.TrimSuffix(name, ".gz"), ".log")
	base = strings.TrimPrefix(base, "labstation-")
	if strings.HasPrefix(base, "legacy-") {
		base = strings.TrimPrefix(base, "legacy-")
	}
	date := base
	if len(base) > 8 && base[8] == '-' {
		date = base[:8]
	}
	if len(date) != 8 {
		return time.Time{}, false
	}
	if _, err := strconv.Atoi(date); err != nil {
		return time.Time{}, false
	}
	parsed, err := time.ParseInLocation("20060102", date, time.Local)
	return parsed, err == nil
}
