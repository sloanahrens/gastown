//go:build integration

package rig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The unit tier reads repo files through gitfake. These read them through real
// git, which is where "origin/main of the rig repository" is a real ref.

// rigWithRepoAndClone makes a rig with a real .repo.git clone of upstream and a
// real mayor/rig clone of it, and returns their paths.
func rigWithRepoAndClone(t *testing.T, townRoot, rigName, upstream string) (bare, clone string) {
	t.Helper()
	rigPath := filepath.Join(townRoot, rigName)
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	bare = filepath.Join(rigPath, ".repo.git")
	gitT(t, townRoot, "clone", "-q", "--bare", upstream, bare)
	clone = filepath.Join(rigPath, "mayor", "rig")
	gitT(t, rigPath, "clone", "-q", upstream, clone)
	return bare, clone
}

// sourceWithFiles makes a repository with one commit on main holding files.
func sourceWithFiles(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	gitT(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	commitFiles(t, dir, "v1", files)
	return dir
}

// commitFiles writes files into repo and commits them.
func commitFiles(t *testing.T, repo, message string, files map[string]string) string {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-q", "-m", message)
	return gitT(t, repo, "rev-parse", "HEAD")
}

// TestIntegrationReadRepoFilePrefersRigRepoMain is the wiring guard for
// ReadRepoFile's production opener and the gt-yj6g1 freshness rule at once: a
// gate command and a rubric that changed on main take effect while the clone
// still holds what it was cloned with.
func TestIntegrationReadRepoFilePrefersRigRepoMain(t *testing.T) {
	townRoot := t.TempDir()
	rigName := "testrig"
	upstream := sourceWithFiles(t, "upstream", map[string]string{
		".om.json":               `{"rubric":["v1"]}`,
		".gastown/settings.json": `{"type":"rig-settings","version":1,"merge_queue":{"test_command":"make test-v1"}}`,
	})
	bare, clone := rigWithRepoAndClone(t, townRoot, rigName, upstream)

	commitFiles(t, upstream, "v2", map[string]string{
		".om.json":               `{"rubric":["v2"]}`,
		".gastown/settings.json": `{"type":"rig-settings","version":1,"merge_queue":{"test_command":"make test-v2"}}`,
	})
	gitT(t, bare, "fetch", "-q", "origin", "+refs/heads/*:refs/remotes/origin/*")

	behind, err := os.ReadFile(filepath.Join(clone, ".om.json"))
	if err != nil || !strings.Contains(string(behind), "v1") {
		t.Fatalf("fixture: clone holds %q, %v; want the v1 it was cloned with", behind, err)
	}

	got, err := ReadRepoFile(openRepoFile, townRoot, rigName, ".om.json")
	if err != nil {
		t.Fatalf("ReadRepoFile: %v", err)
	}
	if !strings.Contains(string(got.Data), "v2") {
		t.Errorf("Data = %q, want main's copy", got.Data)
	}
	if !strings.Contains(got.Source, "origin/main") {
		t.Errorf("Source = %q, want the rig repo's origin/main", got.Source)
	}

	mq := ResolveMergeQueueConfig(townRoot, rigName)
	if mq == nil || mq.TestCommand != "make test-v2" {
		t.Errorf("TestCommand = %v, want %q (main's copy)", mq, "make test-v2")
	}
}

// TestIntegrationReadRepoFileFallsBackToTheClone keeps a file the repo does not
// commit readable: a clone-local .gastown/settings.json is what every rig read
// before gt-yj6g1, and it still resolves.
func TestIntegrationReadRepoFileFallsBackToTheClone(t *testing.T) {
	townRoot := t.TempDir()
	rigName := "testrig"
	upstream := sourceWithFiles(t, "upstream", map[string]string{"README.md": "# upstream\n"})
	_, clone := rigWithRepoAndClone(t, townRoot, rigName, upstream)
	settings := `{"type":"rig-settings","version":1,"merge_queue":{"test_command":"make test-clone"}}`
	commitFiles(t, clone, "clone-local settings", map[string]string{".gastown/settings.json": settings})

	got, err := ReadRepoFile(openRepoFile, townRoot, rigName, ".gastown/settings.json")
	if err != nil {
		t.Fatalf("ReadRepoFile: %v", err)
	}
	if !got.Found || string(got.Data) != settings {
		t.Errorf("read = %+v, want the clone's copy", got)
	}
}

// TestIntegrationReadRepoFileUnreadableRepoFailsClosed: real git on a .repo.git
// that is no repository reports, and does not fall back to the clone's copy.
func TestIntegrationReadRepoFileUnreadableRepoFailsClosed(t *testing.T) {
	townRoot := t.TempDir()
	rigName := "testrig"
	if err := os.MkdirAll(filepath.Join(townRoot, rigName, ".repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	clone := filepath.Join(townRoot, rigName, "mayor", "rig")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(clone, ".om.json"), []byte(`{"stale":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := ReadRepoFile(openRepoFile, townRoot, rigName, ".om.json")
	if err == nil || !strings.Contains(err.Error(), rigName) {
		t.Fatalf("ReadRepoFile = %v, want an error naming rig %s", err, rigName)
	}
}
