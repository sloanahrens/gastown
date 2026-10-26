package repoguard

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// maxShellScripts is the number of entries shell-scripts.txt holds: every
// tracked .sh file on 2026-10-26, when the tree stopped adding scripts
// (gt-lpm8b). Deleting a script deletes its line AND lowers this, in the same
// change, so a new line cannot pass by adding its script too.
const maxShellScripts = 79

// TestNoNewShellScripts holds every tracked .sh file to shell-scripts.txt,
// whose allowlist only shrinks.
func TestNoNewShellScripts(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	allowed, err := ReadAllowlist("shell-scripts.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(allowed) > maxShellScripts {
		t.Errorf("shell-scripts.txt has %d entries, want at most %d: the list only shrinks — deleting a script deletes its line here AND lowers maxShellScripts", len(allowed), maxShellScripts)
	}
	findings, err := CheckShellScripts(func() ([]string, error) { return TrackedShellScripts(root) }, allowed)
	if err != nil {
		t.Fatalf("listing the tracked .sh files: %v", err)
	}
	for _, f := range findings {
		t.Error(f)
	}
}

// TestCheckShellScriptsFixtures covers the answers the guard must tell apart:
// a tracked script with no allowlist line, an allowlist line whose script is
// gone, and a tree where the two sets match. The lister is injected, so no
// case needs a working tree.
func TestCheckShellScriptsFixtures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		tracked []string
		allowed []string
		want    []string
	}{
		{
			name:    "clean",
			tracked: []string{"scripts/a.sh", "scripts/b.sh"},
			allowed: []string{"scripts/a.sh", "scripts/b.sh"},
		},
		{
			name:    "new",
			tracked: []string{"scripts/a.sh", "scripts/new.sh"},
			allowed: []string{"scripts/a.sh"},
			want:    []string{"scripts/new.sh: no new .sh files; write Go (see ~/.claude/docs/standards/test-engineering.md, Scripts are debt)"},
		},
		{
			name:    "removed",
			tracked: []string{"scripts/a.sh"},
			allowed: []string{"scripts/a.sh", "scripts/old.sh"},
			want:    []string{"scripts/old.sh: allowlisted .sh file no longer exists; delete its line so shell-scripts.txt only shrinks"},
		},
		{
			name:    "new and removed",
			tracked: []string{"scripts/a.sh", "scripts/new.sh"},
			allowed: []string{"scripts/a.sh", "scripts/old.sh"},
			want: []string{
				"scripts/new.sh: no new .sh files; write Go (see ~/.claude/docs/standards/test-engineering.md, Scripts are debt)",
				"scripts/old.sh: allowlisted .sh file no longer exists; delete its line so shell-scripts.txt only shrinks",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			findings, err := CheckShellScripts(func() ([]string, error) { return tc.tracked, nil }, toSet(tc.allowed))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(findings, tc.want) {
				t.Errorf("findings = %q, want %q", findings, tc.want)
			}
		})
	}
}

// TestCheckShellScriptsListerError: a lister that cannot see the tree makes
// the guard fail rather than pass, since an empty tree and an unread one must
// not look alike.
func TestCheckShellScriptsListerError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("git is not on PATH")
	_, err := CheckShellScripts(func() ([]string, error) { return nil, wantErr }, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

// TestReadAllowlistSkipsHeader pins that comments and blank lines are not
// entries: the header's prose would otherwise read as missing scripts.
func TestReadAllowlistSkipsHeader(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "allow.txt")
	body := "# the header\n\nscripts/a.sh\n  scripts/b.sh  \n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadAllowlist(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"scripts/a.sh": true, "scripts/b.sh": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadAllowlist = %v, want %v", got, want)
	}
}

func toSet(paths []string) map[string]bool {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return set
}
