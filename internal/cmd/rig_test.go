package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestIsGitRemoteURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  bool
	}{
		// Remote URLs — should return true
		{"https://github.com/org/repo.git", true},
		{"http://github.com/org/repo.git", true},
		{"git@github.com:org/repo.git", true},
		{"ssh://git@github.com/org/repo.git", true},
		{"git://github.com/org/repo.git", true},
		{"deploy@private-host.internal:repos/app.git", true},

		// Custom git remote helper schemes — should return true
		{"s3://my-bucket/rigs/my-project", true},
		{"codecommit://my-repo", true},
		{"gs://my-bucket/repos/foo", true},

		// Local paths — should return false
		{"/Users/scott/projects/foo", false},
		{"/tmp/repo", false},
		{"./foo", false},
		{"../foo", false},
		{"~/projects/foo", false},
		{"C:\\Users\\scott\\projects\\foo", false},
		{"C:/Users/scott/projects/foo", false},

		// Bare directory name — should return false
		{"foo", false},

		// file:// URIs — explicit local git remotes are allowed
		{"file:///tmp/local-repo.git", true},
		{"file:///Users/scott/projects/foo", true},
		{"file://user@localhost:/tmp/local-repo.git", true},

		// Argument injection — should return false
		{"-oProxyCommand=evil", false},
		{"--upload-pack=touch /tmp/pwned", false},
		{"-c", false},

		// Malformed SCP-style — should return false
		{"@host:path", false},     // empty user
		{"user@:/path", false},    // empty host
		{"localhost:path", false}, // no user (not SCP-style)
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isGitRemoteURL(tt.input)
			if got != tt.want {
				t.Errorf("isGitRemoteURL(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// townConfigGitRecorder records what commitTownConfigChanges stages and commits.
type townConfigGitRecorder struct {
	added     []string
	msg       string
	committed []string
}

func (g *townConfigGitRecorder) Add(paths ...string) error {
	g.added = append(g.added, paths...)
	return nil
}

func (g *townConfigGitRecorder) CommitPaths(message string, paths ...string) error {
	g.msg = message
	g.committed = append([]string(nil), paths...)
	return nil
}

// TestCommitTownConfigChangesNamesOnlyItsPaths: the town repo is shared, so the
// config commit must name the four town files it owns rather than committing
// whatever else is staged in that repo (gt-2czgm).
func TestCommitTownConfigChangesNamesOnlyItsPaths(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	for _, p := range []string{"mayor/rigs.json", "mayor/town.json", ".beads/routes.jsonl"} {
		path := filepath.Join(townRoot, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// mayor/daemon.json is intentionally absent: adopt need not create it.

	rec := &townConfigGitRecorder{}

	commitTownConfigChangesWith(rec, townRoot, "gastown")

	want := []string{"mayor/rigs.json", "mayor/town.json", ".beads/routes.jsonl"}
	if !slices.Equal(rec.committed, want) {
		t.Errorf("commit named %v, want %v", rec.committed, want)
	}
	if !slices.Equal(rec.added, want) {
		t.Errorf("staged %v, want %v", rec.added, want)
	}
	if !strings.Contains(rec.msg, "gastown") {
		t.Errorf("commit message %q does not name the rig", rec.msg)
	}
}
