package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateFMUExecutorSourceAcceptsPinnedSharedVersion(t *testing.T) {
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"app/main.py":      "print('fmu')\n",
		"requirements.txt": "example-dependency==1.0\n",
		"VERSION":          strings.TrimSpace(pinnedFMUExecutorVersion) + "\n",
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateFMUExecutorSource(source); err != nil {
		t.Fatalf("validateFMUExecutorSource() rejected the pinned source: %v", err)
	}
}

func TestValidateFMUExecutorSourceRejectsVersionMismatch(t *testing.T) {
	source := t.TempDir()
	wrongVersion := "0.1.1"
	if wrongVersion == strings.TrimSpace(pinnedFMUExecutorVersion) {
		wrongVersion = "0.1.2"
	}
	if err := os.Mkdir(filepath.Join(source, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"app/main.py":      "print('fmu')\n",
		"requirements.txt": "example-dependency==1.0\n",
		"VERSION":          wrongVersion + "\n",
	} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateFMUExecutorSource(source); err == nil {
		t.Fatal("validateFMUExecutorSource() accepted a source version different from the station pin")
	}
}
