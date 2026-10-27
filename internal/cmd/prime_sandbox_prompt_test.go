package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/session"
)

// sandboxEnv is the environment of a prime that runs inside an OpenShell
// sandbox: the pod contract sets GT_SESSION_BACKEND to openshell (gt-a94xe).
func sandboxEnv(key string) string {
	if key == "GT_SESSION_BACKEND" {
		return session.BackendOpenShell
	}
	return ""
}

// In a sandbox a prime leaves the uploaded file byte-for-byte, so the sandbox
// note the runtime appended survives and the logged hash stays the file's hash
// (gt-a94xe). The file's mtime is pinned, so a rewrite shows even when the
// content happens to match.
func TestRefreshSystemPromptFileLeavesAnUploadedFileInASandbox(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "system-prompt-goose.md")
	const uploaded = "directives\n\n(sandbox note)\n"
	if err := os.WriteFile(path, []byte(uploaded), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	if err := refreshSystemPromptFile(sandboxEnv, path, "directives without the note\n"); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != uploaded {
		t.Errorf("sandbox prime rewrote the uploaded file: %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Errorf("sandbox prime touched the uploaded file: mtime %v, want %v", info.ModTime(), old)
	}
}

// Outside a sandbox the file is rewritten when it differs, as before
// (gt-a94xe). The env here names a host session, not openshell.
func TestRefreshSystemPromptFileRewritesOutsideASandbox(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "system-prompt-goose.md")
	if err := os.WriteFile(path, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	host := func(key string) string {
		if key == "GT_SESSION_BACKEND" {
			return "tmux"
		}
		return ""
	}

	if err := refreshSystemPromptFile(host, path, "current\n"); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "current\n" {
		t.Errorf("host prime left the differing file: %q, want the current text", data)
	}
}
