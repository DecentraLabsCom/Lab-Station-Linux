package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testFMUVersion = "0.1.0"
	testFMUCommit  = "0123456789abcdef0123456789abcdef01234567"
	testFMUTree    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func createFMUExecutorSourceFixture(t *testing.T, version, commit, treeDigest string) string {
	t.Helper()
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"app/main.py":      "print('fmu')\n",
		"app/runner.py":    "def run(): return 0\n",
		"requirements.txt": "example-dependency==1.0\n",
		"VERSION":          version + "\n",
	} {
		path := filepath.Join(source, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runtimeDigest, err := fmuExecutorRuntimePayloadDigest(source)
	if err != nil {
		t.Fatal(err)
	}
	lock := fmuExecutorSourceLock{
		Repository: "DecentraLabsCom/FMU-Executor",
		Version:    version,
		Commit:     commit,
	}
	lock.SourceTree.Format = "git-ls-tree-manifest-v1"
	lock.SourceTree.SHA256 = treeDigest
	lock.RuntimePayload.Format = "sha256-path-manifest-v1"
	lock.RuntimePayload.SHA256 = runtimeDigest
	lockBytes, err := json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "SOURCE.lock.json"), lockBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	return source
}

func TestValidateFMUExecutorSourceAcceptsPinnedContent(t *testing.T) {
	source := createFMUExecutorSourceFixture(t, testFMUVersion, testFMUCommit, testFMUTree)
	if err := validateFMUExecutorSourceWithPins(source, testFMUVersion, testFMUCommit, testFMUTree, mustRuntimeDigest(t, source)); err != nil {
		t.Fatalf("validateFMUExecutorSourceWithPins() rejected pinned source: %v", err)
	}
}

func TestPinnedFMUExecutorRuntimePayloadMatchesSharedCheckout(t *testing.T) {
	source := os.Getenv("FMU_EXECUTOR_SOURCE")
	if source == "" {
		t.Skip("set FMU_EXECUTOR_SOURCE to the pinned shared checkout for the cross-repository pin check")
	}
	version, err := os.ReadFile(filepath.Join(source, "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(version)) != strings.TrimSpace(pinnedFMUExecutorVersion) {
		t.Fatalf("shared FMU Executor version %q does not match station pin %q", strings.TrimSpace(string(version)), strings.TrimSpace(pinnedFMUExecutorVersion))
	}
	digest, err := fmuExecutorRuntimePayloadDigest(source)
	if err != nil {
		t.Fatal(err)
	}
	if digest != strings.TrimSpace(pinnedFMUExecutorRuntimePayloadSHA256) {
		t.Fatalf("shared FMU Executor runtime payload digest %s does not match station pin %s", digest, strings.TrimSpace(pinnedFMUExecutorRuntimePayloadSHA256))
	}
}

func TestValidateFMUExecutorSourceRejectsMismatchedPins(t *testing.T) {
	tests := []struct {
		name       string
		version    string
		commit     string
		treeDigest string
	}{
		{name: "version", version: "0.1.1", commit: testFMUCommit, treeDigest: testFMUTree},
		{name: "commit", version: testFMUVersion, commit: "1123456789abcdef0123456789abcdef01234567", treeDigest: testFMUTree},
		{name: "source tree", version: testFMUVersion, commit: testFMUCommit, treeDigest: strings.Repeat("a", 64)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := createFMUExecutorSourceFixture(t, testFMUVersion, testFMUCommit, testFMUTree)
			if err := validateFMUExecutorSourceWithPins(source, test.version, test.commit, test.treeDigest, mustRuntimeDigest(t, source)); err == nil {
				t.Fatal("validator accepted a source that differs from the expected pin")
			}
		})
	}
}

func TestValidateFMUExecutorSourceRejectsTamperedRuntimeFiles(t *testing.T) {
	source := createFMUExecutorSourceFixture(t, testFMUVersion, testFMUCommit, testFMUTree)
	expectedDigest := mustRuntimeDigest(t, source)
	if err := os.WriteFile(filepath.Join(source, "app", "runner.py"), []byte("def run(): return 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateFMUExecutorSourceWithPins(source, testFMUVersion, testFMUCommit, testFMUTree, expectedDigest); err == nil {
		t.Fatal("validator accepted runtime source that differs from its content pin")
	}
}

func TestValidateFMUExecutorSourceRejectsUnexpectedFilesAndLinks(t *testing.T) {
	t.Run("unexpected top-level file", func(t *testing.T) {
		source := createFMUExecutorSourceFixture(t, testFMUVersion, testFMUCommit, testFMUTree)
		if err := os.WriteFile(filepath.Join(source, "unexpected.txt"), []byte("extra"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := validateFMUExecutorSourceWithPins(source, testFMUVersion, testFMUCommit, testFMUTree, mustRuntimeDigest(t, source)); err == nil {
			t.Fatal("validator accepted an unexpected top-level file")
		}
	})

	t.Run("symbolic link", func(t *testing.T) {
		source := createFMUExecutorSourceFixture(t, testFMUVersion, testFMUCommit, testFMUTree)
		expectedDigest := mustRuntimeDigest(t, source)
		if err := os.Symlink(filepath.Join(source, "app", "main.py"), filepath.Join(source, "app", "linked.py")); err != nil {
			t.Skipf("symbolic links unavailable: %v", err)
		}
		if err := validateFMUExecutorSourceWithPins(source, testFMUVersion, testFMUCommit, testFMUTree, expectedDigest); err == nil {
			t.Fatal("validator accepted a symbolic link in app source")
		}
	})
}

func mustRuntimeDigest(t *testing.T, source string) string {
	t.Helper()
	digest, err := fmuExecutorRuntimePayloadDigest(source)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}
