package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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

// TestRigHelpNamesTheRegistrysRealHome: the rig commands an operator reads
// help for must name where the registry lives — the "registry" section of
// mayor/town.json on a two-file town — and not the retired mayor/rigs.json,
// which a two-file town does not have (gt-51bdr).
func TestRigHelpNamesTheRegistrysRealHome(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cmd  *cobra.Command
	}{
		{"add", rigAddCmd},
		{"remove", rigRemoveCmd},
		{"park", rigParkCmd},
		{"unpark", rigUnparkCmd},
	} {
		help := strings.Join([]string{tc.cmd.Short, tc.cmd.Long, tc.cmd.Flags().FlagUsages()}, "\n")
		if strings.Contains(help, "mayor/rigs.json") {
			t.Errorf("gt rig %s help still names the retired mayor/rigs.json:\n%s", tc.name, help)
		}
		if !strings.Contains(help, "mayor/town.json") || !strings.Contains(help, "registry") {
			t.Errorf("gt rig %s help does not name the registry in mayor/town.json:\n%s", tc.name, help)
		}
	}
}

// TestMigrateParkedRigsNamesTheRegistryItWroteTo: the --migrate line names the
// file the records landed in, so a two-file town is told the registry section
// of mayor/town.json and never mayor/rigs.json, which is not there (gt-51bdr).
func TestMigrateParkedRigsNamesTheRegistryItWroteTo(t *testing.T) {
	t.Parallel()
	entry := `{"git_url":"x","added_at":"2026-09-07T00:00:00Z","beads":{"repo":"","prefix":"tr"}}`
	town := writeTownFiles(t, map[string]string{
		"mayor/town.json":                 `{"type":"town","version":2,"name":"t","registry":{"version":1,"rigs":{"testrig":` + entry + `}}}`,
		".beads-wisp/config/testrig.json": `{"rig":"testrig","values":{"status":"parked"}}`,
	})
	var out bytes.Buffer
	if err := migrateParkedRigs(&out, town, "sloan"); err != nil {
		t.Fatalf("migrateParkedRigs = %v\n%s", err, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "testrig") {
		t.Fatalf("migrate output does not name the migrated rig: %q", got)
	}
	if strings.Contains(got, "mayor/rigs.json") {
		t.Errorf("migrate output names the retired mayor/rigs.json: %q", got)
	}
	if want := `mayor/town.json (section "registry")`; !strings.Contains(got, want) {
		t.Errorf("migrate output = %q, want it to name %q", got, want)
	}
}
