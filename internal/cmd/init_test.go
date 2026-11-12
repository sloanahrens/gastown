package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCreateAgentDirsMakesNoMayor: a newly initialized rig gets polecats/ and
// crew/ and no mayor/ directory, since mayor/rig is no longer an agent
// directory (gt-3lvcjz.30).
func TestCreateAgentDirsMakesNoMayor(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	var out bytes.Buffer
	created, err := createAgentDirs(cwd, &out)
	if err != nil {
		t.Fatalf("createAgentDirs: %v", err)
	}
	if created != 2 {
		t.Errorf("created = %d, want 2", created)
	}

	entries, err := os.ReadDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if got := strings.Join(names, ","); got != "crew,polecats" {
		t.Errorf("rig root entries = %q, want %q", got, "crew,polecats")
	}
	for _, dir := range []string{"polecats", "crew"} {
		if _, err := os.Stat(filepath.Join(cwd, dir, ".gitkeep")); err != nil {
			t.Errorf("%s/.gitkeep: %v", dir, err)
		}
	}
	if strings.Contains(out.String(), "mayor") {
		t.Errorf("progress output %q names mayor", out.String())
	}
}

// TestCreateAgentDirsMkdirFailureIsAnError: a directory that cannot be made is
// an error, not a rig initialized with fewer directories.
func TestCreateAgentDirsMkdirFailureIsAnError(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	// A regular file where the polecats directory belongs blocks MkdirAll.
	if err := os.WriteFile(filepath.Join(cwd, "polecats"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	created, err := createAgentDirs(cwd, io.Discard)
	if err == nil {
		t.Fatal("createAgentDirs over a blocked polecats path = nil, want an error")
	}
	if !strings.Contains(err.Error(), "creating polecats") {
		t.Errorf("error = %q, want it to name polecats", err)
	}
	if created != 0 {
		t.Errorf("created = %d, want 0 before the first failure", created)
	}
}

// TestUpdateGitExcludeNamesNoMayor: the exclude lines a new rig gets anchor
// polecats and crew, and no /mayor/ line.
func TestUpdateGitExcludeNamesNoMayor(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	if _, err := createAgentDirs(cwd, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := updateGitExclude(cwd); err != nil {
		t.Fatalf("updateGitExclude: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(cwd, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{"/polecats/", "/crew/"} {
		if !strings.Contains(string(content), "\n"+line+"\n") {
			t.Errorf("exclude = %q, want a %s line", content, line)
		}
	}
	if strings.Contains(string(content), "/mayor/") {
		t.Errorf("exclude = %q, want no /mayor/ line", content)
	}
}

// TestUpdateGitExcludeKeepsExistingSection: a rig initialized before this
// change keeps its exclude file as it was; the Gas Town section is not
// rewritten, so its old /mayor/ line stays.
func TestUpdateGitExcludeKeepsExistingSection(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	excludeDir := filepath.Join(cwd, ".git", "info")
	if err := os.MkdirAll(excludeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := "# Gas Town agent directories\n/polecats/\n/mayor/\n"
	excludePath := filepath.Join(excludeDir, "exclude")
	if err := os.WriteFile(excludePath, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := updateGitExclude(cwd); err != nil {
		t.Fatalf("updateGitExclude: %v", err)
	}
	got, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != old {
		t.Errorf("exclude = %q, want it unchanged %q", got, old)
	}
}
