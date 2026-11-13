package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (a *Agent) cleanFMUExecutorState(ctx context.Context) error {
	if !fileExists("/opt/decentralabs/fmu-executor/app/main.py") {
		return nil
	}
	supervisor := a.hostRuntime().Supervisor()
	if supervisor == nil {
		return errors.New("service supervisor is unavailable")
	}
	unit := serviceUnit(supervisor.Name(), "decentralabs-labstation-fmu.service")
	state, err := supervisor.ServiceState(unit)
	if err != nil || state == "unknown" {
		return errors.New("FMU Executor service state is unavailable")
	}
	if state == "active" {
		if err := a.helper(ctx, map[string]any{"operation": "service-action", "unit": unit, "action": "restart"}); err != nil {
			return errors.New("FMU Executor could not be restarted to close active sessions")
		}
		state, err = supervisor.ServiceState(unit)
		if err != nil || state != "active" {
			return errors.New("FMU Executor did not return to the active state")
		}
	}
	if err := a.helper(ctx, map[string]any{"operation": "fmu-clean-temp"}); err != nil {
		return errors.New("FMU Executor temporary state could not be removed")
	}
	return nil
}

// cleanTinyDeskWorkingData removes user work products between reservations
// while preserving the managed application and desktop configuration.
func cleanTinyDeskWorkingData() error {
	managedRoot := filepath.Clean(filepath.Dir(tinyDeskSessionHome))
	return cleanTinyDeskWorkingDataAt(tinyDeskWorkHome, managedRoot)
}

func cleanTinyDeskWorkingDataAt(workHome, managedRoot string) error {
	root := filepath.Clean(workHome)
	managedRoot = filepath.Clean(managedRoot)
	expectedRoot := filepath.Join(managedRoot, "tiny-desk-home")
	if !filepath.IsAbs(root) || root != expectedRoot {
		return errors.New("Tiny Desk working home is outside the managed Station workspace")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return fmt.Errorf("inspect Tiny Desk working home: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("Tiny Desk working home is not a managed directory")
	}

	for _, relative := range []string{
		".cache",
		"tmp",
		"Downloads",
		"Documents/LabStationScratch",
		".local/share/Trash/files",
		".local/share/Trash/info",
	} {
		if err := clearManagedDirectory(root, relative); err != nil {
			return err
		}
	}
	for _, relative := range []string{".local/share/recently-used.xbel"} {
		if err := removeManagedFile(root, relative); err != nil {
			return err
		}
	}
	return nil
}

func clearManagedDirectory(root, relative string) error {
	path, exists, err := resolveManagedPath(root, relative)
	if err != nil || !exists {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", relative, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s is not a managed directory", relative)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", relative, err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(path, entry.Name())); err != nil {
			return fmt.Errorf("remove %s/%s: %w", relative, entry.Name(), err)
		}
	}
	return nil
}

func removeManagedFile(root, relative string) error {
	path, exists, err := resolveManagedPath(root, relative)
	if err != nil || !exists {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", relative, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a managed regular file", relative)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", relative, err)
	}
	return nil
}

func resolveManagedPath(root, relative string) (string, bool, error) {
	cleanRelative := filepath.Clean(relative)
	if cleanRelative == "." || filepath.IsAbs(cleanRelative) || cleanRelative == ".." || strings.HasPrefix(cleanRelative, ".."+string(filepath.Separator)) {
		return "", false, errors.New("cleanup target escapes the Tiny Desk working home")
	}
	current := root
	for _, component := range strings.Split(cleanRelative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return "", false, nil
		}
		if err != nil {
			return "", false, fmt.Errorf("inspect cleanup path %s: %w", component, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", false, fmt.Errorf("cleanup path %s contains a symbolic link", component)
		}
		if current != filepath.Join(root, cleanRelative) && !info.IsDir() {
			return "", false, fmt.Errorf("cleanup parent %s is not a directory", component)
		}
	}
	return current, true, nil
}
