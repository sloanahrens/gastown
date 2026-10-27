package repoguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

// The literal alarm text is pinned here, not read back from the guard: a test
// that reuses the production string passes when the message loses the rule or
// the offending path.
const (
	newScriptAlarm     = "no new .sh files; write Go (scripts are debt: see .om.json, scripts-are-debt)"
	removedScriptAlarm = "allowlisted .sh file is no longer tracked; the shell-scripts.txt line is stale"

	reasonMenu             = `say "platform: container entrypoint", "platform: git hook" or "port: <bead id>"`
	allowlistNoReasonAlarm = "no reason; " + reasonMenu
	badPortAlarm           = `%q is not a bead id; a port reason names the bead that deletes this line, as "port: gt-abc12"`
	badFitsAlarm           = `%q does not fit this path; each platform reason is bound to the file it may exempt`
	badReasonAlarm         = `%q is not a reason the guard accepts; ` + reasonMenu
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
			allowed := Allowlist{Paths: toSet(tc.allowed)}
			findings, err := CheckShellScripts(func() ([]string, error) { return tc.tracked, nil }, allowed)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(findings, tc.want) {
				t.Errorf("findings = %q, want %q", findings, tc.want)
			}
		})
	}
}

// TestCheckShellScriptsCarriesAllowlistFindings: a malformed allowlist line is
// the tree guard's finding too, or a clean run would hide the line that lost
// its reason. The rejected path admits nothing, so the script it named is
// reported as unlisted as well.
func TestCheckShellScriptsCarriesAllowlistFindings(t *testing.T) {
	t.Parallel()
	parseFinding := "shell-scripts.txt:1: scripts/a.sh: " + allowlistNoReasonAlarm
	allowed := Allowlist{Paths: map[string]bool{}, Findings: []string{parseFinding}}
	tracked := []string{"scripts/a.sh", "scripts/b.sh"}
	want := []string{
		"scripts/a.sh: " + newScriptAlarm,
		"scripts/b.sh: " + newScriptAlarm,
		parseFinding,
	}
	findings, err := CheckShellScripts(func() ([]string, error) { return tracked, nil }, allowed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(findings, want) {
		t.Errorf("findings = %q, want %q", findings, want)
	}
}

// TestCheckShellScriptsListerError: a lister that cannot see the tree makes
// the guard fail rather than pass, since an empty tree and an unread one must
// not look alike.
func TestCheckShellScriptsListerError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("git is not on PATH")
	findings, err := CheckShellScripts(func() ([]string, error) { return nil, wantErr }, Allowlist{Paths: map[string]bool{"scripts/a.sh": true}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if findings != nil {
		t.Errorf("findings = %q with a lister error, want nil", findings)
	}
}

// TestParseAllowlistRows pins the acceptance of each line shape: the three
// reasons that admit a path, and one row for every way a line can fail. A
// failed line admits nothing, so the path is absent from Paths as well as
// named in the finding.
func TestParseAllowlistRows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		body     string
		want     []string
		findings []string
	}{
		{
			name: "each accepted reason",
			body: "docker-entrypoint.sh\tplatform: container entrypoint\n" +
				".githooks/pre-push\tplatform: git hook\n" +
				"scripts/a.sh\tport: gt-tpg0p.15\n",
			want: []string{".githooks/pre-push", "docker-entrypoint.sh", "scripts/a.sh"},
		},
		{
			name: "the header and blank lines are not entries",
			body: "# one line per script\n\nscripts/a.sh\tport: gt-tpg0p\n",
			want: []string{"scripts/a.sh"},
		},
		{
			name: "whitespace around the columns is trimmed",
			body: "  scripts/a.sh  \t  port: gt-tpg0p  \n",
			want: []string{"scripts/a.sh"},
		},
		{
			name:     "a line with no reason",
			body:     "scripts/a.sh\t\n",
			findings: []string{"shell-scripts.txt:1: scripts/a.sh: " + allowlistNoReasonAlarm},
		},
		{
			name:     "a line with no tab",
			body:     "scripts/a.sh port: gt-tpg0p\n",
			findings: []string{`shell-scripts.txt:1: "scripts/a.sh port: gt-tpg0p" has no tab; write "path<TAB>reason"`},
		},
		{
			name:     "a reason outside the closed set",
			body:     "scripts/a.sh\tbecause it is old\n",
			findings: []string{"shell-scripts.txt:1: scripts/a.sh: " + fmt.Sprintf(badReasonAlarm, "because it is old")},
		},
		{
			name:     "the entrypoint reason on another path",
			body:     "scripts/a.sh\tplatform: container entrypoint\n",
			findings: []string{"shell-scripts.txt:1: scripts/a.sh: " + fmt.Sprintf(badFitsAlarm, "platform: container entrypoint")},
		},
		{
			name:     "the git-hook reason on a .sh file",
			body:     ".githooks/pre-push_test.sh\tplatform: git hook\n",
			findings: []string{"shell-scripts.txt:1: .githooks/pre-push_test.sh: " + fmt.Sprintf(badFitsAlarm, "platform: git hook")},
		},
		{
			name:     "the git-hook reason below .githooks",
			body:     ".githooks/lib/pre-push\tplatform: git hook\n",
			findings: []string{"shell-scripts.txt:1: .githooks/lib/pre-push: " + fmt.Sprintf(badFitsAlarm, "platform: git hook")},
		},
		{
			name:     "the git-hook reason outside .githooks",
			body:     "hooks/pre-push\tplatform: git hook\n",
			findings: []string{"shell-scripts.txt:1: hooks/pre-push: " + fmt.Sprintf(badFitsAlarm, "platform: git hook")},
		},
		{
			name:     "a port reason naming no bead",
			body:     "scripts/a.sh\tport: \n",
			findings: []string{"shell-scripts.txt:1: scripts/a.sh: " + fmt.Sprintf(badPortAlarm, "")},
		},
		{
			name:     "a port reason that is not a bead id",
			body:     "scripts/a.sh\tport: not a bead\n",
			findings: []string{"shell-scripts.txt:1: scripts/a.sh: " + fmt.Sprintf(badPortAlarm, "not a bead")},
		},
		{
			name:     "a port reason carrying shell syntax",
			body:     "scripts/a.sh\tport: gt-abc;rm -rf\n",
			findings: []string{"shell-scripts.txt:1: scripts/a.sh: " + fmt.Sprintf(badPortAlarm, "gt-abc;rm -rf")},
		},
		{
			name:     "a line with a reason and no path",
			body:     "\tport: gt-tpg0p\n",
			findings: []string{"shell-scripts.txt:1: the line names no path before its tab"},
		},
		{
			name:     "a duplicate path",
			body:     "scripts/a.sh\tport: gt-tpg0p\nscripts/a.sh\tport: gt-tpg0p.2\n",
			want:     []string{"scripts/a.sh"},
			findings: []string{"shell-scripts.txt:2: scripts/a.sh already has a line at line 1"},
		},
		{
			name: "a duplicate path whose first line was rejected",
			body: "scripts/a.sh\tbecause it is old\nscripts/a.sh\tport: gt-tpg0p\n",
			findings: []string{
				"shell-scripts.txt:1: scripts/a.sh: " + fmt.Sprintf(badReasonAlarm, "because it is old"),
				"shell-scripts.txt:2: scripts/a.sh already has a line at line 1",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseAllowlist(strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if paths, want := sortedPaths(got.Paths), append([]string{}, tc.want...); !reflect.DeepEqual(paths, want) {
				t.Errorf("Paths = %q, want %q", paths, want)
			}
			if !reflect.DeepEqual(got.Findings, tc.findings) {
				t.Errorf("Findings = %q, want %q", got.Findings, tc.findings)
			}
		})
	}
}

// TestParseAllowlistReadError: an unreadable allowlist is an error, never an
// empty one, since the guard reads no paths as a clean tree.
func TestParseAllowlistReadError(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("the disk went away")
	got, err := ParseAllowlist(iotest.ErrReader(wantErr))
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if len(got.Paths) != 0 || got.Findings != nil {
		t.Errorf("ParseAllowlist = %+v with a read error, want the zero allowlist", got)
	}
}

// TestParseAllowlistHeaderAndComments: ReadAllowlist is the file-backed door
// to ParseAllowlist, and a well-formed file yields no findings.
func TestParseAllowlistHeaderAndComments(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "allow.txt")
	body := "# the header\n\nscripts/a.sh\tport: gt-tpg0p\n  scripts/b.sh\tport: gt-tpg0p.1  \n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadAllowlist(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"scripts/a.sh", "scripts/b.sh"}
	if !reflect.DeepEqual(sortedPaths(got.Paths), want) {
		t.Errorf("ReadAllowlist = %q, want %q", sortedPaths(got.Paths), want)
	}
	if got.Findings != nil {
		t.Errorf("Findings = %q from a well-formed file, want none", got.Findings)
	}
}

// TestReadAllowlistMissingFile: the caller must be able to tell an unreadable
// allowlist from a clean one.
func TestReadAllowlistMissingFile(t *testing.T) {
	t.Parallel()
	if _, err := ReadAllowlist(filepath.Join(t.TempDir(), "absent.txt")); err == nil {
		t.Fatal("ReadAllowlist of a missing file = nil error, want one")
	}
}

func toSet(paths []string) map[string]bool {
	set := make(map[string]bool, len(paths))
	for _, p := range paths {
		set[p] = true
	}
	return set
}
