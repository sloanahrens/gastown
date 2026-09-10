package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initGitRepoWithOrigin(t *testing.T, dir, originURL string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("remote", "add", "origin", originURL)
}

func writeRoutesAndConfig(t *testing.T, townRoot, rigName, syncRemote string) string {
	t.Helper()
	rigPath := filepath.Join(townRoot, rigName)
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}

	townBeadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(townBeadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	routesPath := filepath.Join(townBeadsDir, "routes.jsonl")
	routeLine := `{"prefix":"` + rigName + `-","path":"` + rigName + `"}` + "\n"
	if err := os.WriteFile(routesPath, []byte(routeLine), 0644); err != nil {
		t.Fatal(err)
	}

	if syncRemote != "" {
		content := "sync.remote: \"" + syncRemote + "\"\n"
		if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	return rigPath
}

func TestSyncRemoteOwnerCheck_MatchingOwner(t *testing.T) {
	townRoot := t.TempDir()
	rigPath := writeRoutesAndConfig(t, townRoot, "myrig", "git+https://github.com/sloanahrens/gastown.git")
	initGitRepoWithOrigin(t, rigPath, "https://github.com/sloanahrens/gastown.git")

	check := NewSyncRemoteOwnerCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK, got %v: %s (%v)", result.Status, result.Message, result.Details)
	}
}

func TestSyncRemoteOwnerCheck_MismatchedOwner(t *testing.T) {
	townRoot := t.TempDir()
	rigPath := writeRoutesAndConfig(t, townRoot, "myrig", "git+https://github.com/steveyegge/gastown.git")
	initGitRepoWithOrigin(t, rigPath, "https://github.com/sloanahrens/gastown.git")

	check := NewSyncRemoteOwnerCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusError {
		t.Fatalf("expected StatusError, got %v: %s", result.Status, result.Message)
	}
	if len(result.Details) != 1 {
		t.Fatalf("expected 1 mismatch detail, got %d: %v", len(result.Details), result.Details)
	}
}

func TestSyncRemoteOwnerCheck_NoSyncRemoteConfigured(t *testing.T) {
	townRoot := t.TempDir()
	rigPath := writeRoutesAndConfig(t, townRoot, "myrig", "")
	initGitRepoWithOrigin(t, rigPath, "https://github.com/sloanahrens/gastown.git")

	check := NewSyncRemoteOwnerCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK when sync.remote unset, got %v: %s", result.Status, result.Message)
	}
}

func TestSyncRemoteOwnerCheck_NoRoutes(t *testing.T) {
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatal(err)
	}

	check := NewSyncRemoteOwnerCheck()
	result := check.Run(&CheckContext{TownRoot: townRoot})

	if result.Status != StatusOK {
		t.Fatalf("expected StatusOK with no rigs, got %v: %s", result.Status, result.Message)
	}
}

func TestGithubOwnerFromURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		want    string
		wantErr bool
	}{
		{"git+https with .git suffix", "git+https://github.com/sloanahrens/gastown.git", "sloanahrens", false},
		{"plain https", "https://github.com/steveyegge/gastown", "steveyegge", false},
		{"ssh shorthand", "git@github.com:sloanahrens/gastown.git", "sloanahrens", false},
		{"non-github", "https://gitlab.com/someone/repo.git", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := githubOwnerFromURL(tc.url)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got owner %q", tc.url, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.url, err)
			}
			if got != tc.want {
				t.Fatalf("githubOwnerFromURL(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}
