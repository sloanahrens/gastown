package wrappers

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// expectedWrappers is the canonical list of wrapper scripts.
// Keep in sync with Install() and Remove() in wrappers.go.
var expectedWrappers = []string{"gt-codex", "gt-gemini", "gt-opencode"}

func TestEmbeddedScripts_Exist(t *testing.T) {
	t.Parallel()
	for _, name := range expectedWrappers {
		t.Run(name, func(t *testing.T) {
			content, err := scriptsFS.ReadFile("scripts/" + name)
			if err != nil {
				t.Fatalf("Embedded script %s not found: %v", name, err)
			}
			if len(content) == 0 {
				t.Fatalf("Embedded script %s is empty", name)
			}
		})
	}
}

func TestEmbeddedScripts_HaveShebang(t *testing.T) {
	t.Parallel()
	for _, name := range expectedWrappers {
		t.Run(name, func(t *testing.T) {
			content, err := scriptsFS.ReadFile("scripts/" + name)
			if err != nil {
				t.Fatalf("Failed to read %s: %v", name, err)
			}
			if !strings.HasPrefix(string(content), "#!/") {
				t.Errorf("Script %s missing shebang line", name)
			}
		})
	}
}

func TestEmbeddedScripts_HaveExecLine(t *testing.T) {
	t.Parallel()
	// Each wrapper should exec its target binary.
	// gt-codex → exec codex, gt-gemini → exec gemini, gt-opencode → exec opencode
	for _, name := range expectedWrappers {
		t.Run(name, func(t *testing.T) {
			content, err := scriptsFS.ReadFile("scripts/" + name)
			if err != nil {
				t.Fatalf("Failed to read %s: %v", name, err)
			}

			// Extract expected binary name: gt-codex → codex
			binary := strings.TrimPrefix(name, "gt-")
			expectedExec := "exec " + binary

			if !strings.Contains(string(content), expectedExec) {
				t.Errorf("Script %s missing expected exec line %q", name, expectedExec)
			}
		})
	}
}

func TestEmbeddedScripts_HaveGtPrime(t *testing.T) {
	t.Parallel()
	for _, name := range expectedWrappers {
		t.Run(name, func(t *testing.T) {
			content, err := scriptsFS.ReadFile("scripts/" + name)
			if err != nil {
				t.Fatalf("Failed to read %s: %v", name, err)
			}

			if !strings.Contains(string(content), "gt prime") {
				t.Errorf("Script %s should run 'gt prime' before launching agent", name)
			}
		})
	}
}

func TestInstall_CreatesAllWrappers(t *testing.T) {
	t.Parallel()
	binDir := filepath.Join(t.TempDir(), "bin")

	if err := installTo(binDir); err != nil {
		t.Fatalf("installTo() error = %v", err)
	}

	for _, name := range expectedWrappers {
		path := filepath.Join(binDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("Wrapper %s not created: %v", name, err)
			continue
		}
		// Check executable permissions (Windows has no execute bit).
		if runtime.GOOS != "windows" && info.Mode()&0111 == 0 {
			t.Errorf("Wrapper %s is not executable: mode=%v", name, info.Mode())
		}
	}
}

func TestInstall_FailsWhenBinDirIsAFile(t *testing.T) {
	t.Parallel()
	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(binDir, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := installTo(binDir); err == nil {
		t.Error("installTo() into a regular file = nil error, want a mkdir error")
	}
}

func TestRemove_CleansUp(t *testing.T) {
	t.Parallel()
	binDir := filepath.Join(t.TempDir(), "bin")

	if err := installTo(binDir); err != nil {
		t.Fatalf("installTo() error = %v", err)
	}
	for _, name := range expectedWrappers {
		if _, err := os.Stat(filepath.Join(binDir, name)); err != nil {
			t.Fatalf("Precondition: wrapper %s should exist after install", name)
		}
	}

	if err := removeFrom(binDir); err != nil {
		t.Fatalf("removeFrom() error = %v", err)
	}

	for _, name := range expectedWrappers {
		if _, err := os.Stat(filepath.Join(binDir, name)); err == nil {
			t.Errorf("Wrapper %s still exists after removeFrom()", name)
		}
	}
}

func TestRemove_NoErrorWhenMissing(t *testing.T) {
	t.Parallel()
	// No wrappers installed: removal must not error.
	if err := removeFrom(filepath.Join(t.TempDir(), "bin")); err != nil {
		t.Errorf("removeFrom() should not error when wrappers don't exist: %v", err)
	}
}

func TestInstall_Idempotent(t *testing.T) {
	t.Parallel()
	binDir := filepath.Join(t.TempDir(), "bin")

	if err := installTo(binDir); err != nil {
		t.Fatalf("First installTo() error = %v", err)
	}
	if err := installTo(binDir); err != nil {
		t.Fatalf("Second installTo() error = %v", err)
	}

	for _, name := range expectedWrappers {
		content, err := os.ReadFile(filepath.Join(binDir, name))
		if err != nil {
			t.Errorf("Wrapper %s missing after double install: %v", name, err)
			continue
		}
		embedded, _ := scriptsFS.ReadFile("scripts/" + name)
		if string(content) != string(embedded) {
			t.Errorf("Wrapper %s content doesn't match embedded script after double install", name)
		}
	}
}

// TestBinDirIsHomeBin checks the exported entry points target ~/bin,
// reading HOME without changing it.
func TestBinDirIsHomeBin(t *testing.T) {
	t.Parallel()
	want := ""
	if home, err := os.UserHomeDir(); err == nil {
		want = filepath.Join(home, "bin")
	}
	if got := BinDir(); got != want {
		t.Errorf("BinDir() = %q, want %q", got, want)
	}
}
