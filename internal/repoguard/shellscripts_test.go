package repoguard

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The literal alarm text is pinned here, not read back from the guard: a test
// that reuses the production string passes when the message loses the rule or
// the offending path.
const (
	newScriptAlarm     = "no new .sh files; write Go (scripts are debt: see .om.json, scripts-are-debt)"
	removedScriptAlarm = "allowlisted .sh file is no longer tracked; the shell-scripts.txt line is stale"
)

// TestCheckShellScriptsFixtures drives each branch of the guard through an
// injected lister: a tracked script with no allowlist line, an allowlist line
// whose script is gone, both at once, and a tree where the two sets match.
// Each failure asserts the alarm names the offending path and the rule.
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
			name:    "new file not in the allowlist",
			tracked: []string{"scripts/a.sh", "scripts/new.sh"},
			allowed: []string{"scripts/a.sh"},
			want:    []string{"scripts/new.sh: " + newScriptAlarm},
		},
		{
			name:    "allowlist line whose file is gone",
			tracked: []string{"scripts/a.sh"},
			allowed: []string{"scripts/a.sh", "scripts/old.sh"},
			want:    []string{"scripts/old.sh: " + removedScriptAlarm},
		},
		{
			name:    "new file and a gone line together",
			tracked: []string{"scripts/a.sh", "scripts/new.sh"},
			allowed: []string{"scripts/a.sh", "scripts/old.sh"},
			want: []string{
				"scripts/new.sh: " + newScriptAlarm,
				"scripts/old.sh: " + removedScriptAlarm,
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
	findings, err := CheckShellScripts(func() ([]string, error) { return nil, wantErr }, map[string]bool{"scripts/a.sh": true})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if findings != nil {
		t.Errorf("findings = %q with a lister error, want nil", findings)
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
