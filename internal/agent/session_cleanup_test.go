package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareCleanupClearsSharedEphemeralCategories(t *testing.T) {
	matrix := loadPortableParityMatrix(t)
	cleanup := matrix.PrepareCleanup
	managedRoot := t.TempDir()
	workHome := filepath.Join(managedRoot, "tiny-desk-home")
	if err := os.MkdirAll(workHome, 0o700); err != nil {
		t.Fatal(err)
	}

	categoryPaths := map[string]string{
		"temporary":         "tmp",
		"application-cache": ".cache",
		"downloads":         "Downloads",
		"scratch":           filepath.Join("Documents", "LabStationScratch"),
		"recent-items":      filepath.Join(".local", "share", "recently-used.xbel"),
	}
	for _, category := range cleanup.RequiredCategories {
		relativePath, ok := categoryPaths[category]
		if !ok {
			t.Fatalf("Linux cleanup has no workspace mapping for shared category %q", category)
		}
		path := filepath.Join(workHome, relativePath)
		if category == "recent-items" {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("recent item"), 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "session-data.tmp"), []byte("reservation data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	managedConfig := filepath.Join(workHome, ".config", "labstation", "application.conf")
	if err := os.MkdirAll(filepath.Dir(managedConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(managedConfig, []byte("managed application settings"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := cleanTinyDeskWorkingDataAt(workHome, managedRoot); err != nil {
		t.Fatalf("clean reservation workspaces: %v", err)
	}
	for _, category := range cleanup.RequiredCategories {
		path := filepath.Join(workHome, categoryPaths[category])
		if category == "recent-items" {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("recent-items data remains after cleanup: %v", err)
			}
			continue
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			t.Errorf("inspect %s data: %v", category, err)
			continue
		}
		if len(entries) != 0 {
			t.Errorf("%s data remains after cleanup: %v", category, entries)
		}
	}
	if _, err := os.Stat(managedConfig); err != nil {
		t.Errorf("cleanup removed managed application configuration: %v", err)
	}
	if !containsParityValue(cleanup.PreserveCategories, "managed-application-configuration") {
		t.Fatal("shared cleanup matrix does not protect managed application configuration")
	}
}

func TestTinyDeskCleanupRejectsSymlinkedWorkDirectoryWithoutTouchingTarget(t *testing.T) {
	managedRoot := t.TempDir()
	workHome := filepath.Join(managedRoot, "tiny-desk-home")
	outside := t.TempDir()
	protected := filepath.Join(outside, "keep.txt")
	if err := os.Mkdir(workHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(protected, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workHome, ".cache")); err != nil {
		t.Fatal(err)
	}
	if err := cleanTinyDeskWorkingDataAt(workHome, managedRoot); err == nil {
		t.Fatal("Tiny Desk cleaner accepted a symlinked work directory")
	}
	if contents, err := os.ReadFile(protected); err != nil || string(contents) != "keep" {
		t.Fatalf("Tiny Desk cleanup changed data outside its work directory: contents=%q err=%v", contents, err)
	}
}
