package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// crewHooksWrite writes content to p, creating its parent directories.
func crewHooksWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// crewHooksTown makes a Mac town holding one main crew clone, gastown/crew/sloan.
// A nil config leaves its .git/config absent. It returns the town root and the
// clone path.
func crewHooksTown(t *testing.T, config *string) (string, string) {
	t.Helper()
	town := t.TempDir()
	clone := filepath.Join(town, "gastown", "crew", "sloan")
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if config != nil {
		crewHooksWrite(t, filepath.Join(clone, ".git", "config"), *config)
	}
	return town, clone
}

func crewHooksRun(town string) *CheckResult {
	return NewMacCrewHooksCheck().Run(&CheckContext{TownRoot: town})
}

// TestMacCrewHooksReportsADanglingPath: an absolute and a relative hooksPath that
// names a missing directory are each an ERROR that names the clone, the value, the
// linked worktree count and the one-line repair. The repair is the .githooks one
// only when the tree has a .githooks directory; otherwise it unsets the key.
func TestMacCrewHooksReportsADanglingPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		hooksPath     string
		githooks      bool
		worktrees     []string
		wantWorktrees string
		wantRepair    string
	}{
		{
			name:          "absolute missing, no githooks, unset repair",
			hooksPath:     "/nonexistent/gt-9glye5/hooks",
			wantWorktrees: "0 linked worktree(s) share it",
			wantRepair:    "--unset core.hooksPath",
		},
		{
			name:          "relative missing with githooks, pointed repair",
			hooksPath:     "missing/hooks",
			githooks:      true,
			worktrees:     []string{"a", "b"},
			wantWorktrees: "2 linked worktree(s) share it",
			wantRepair:    "config core.hooksPath .githooks",
		},
		{
			name:          "hooksPath names a regular file",
			hooksPath:     "not-a-dir",
			worktrees:     []string{"a"},
			wantWorktrees: "1 linked worktree(s) share it",
			wantRepair:    "--unset core.hooksPath",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			config := "[core]\n\thooksPath = " + tc.hooksPath + "\n"
			town, clone := crewHooksTown(t, &config)
			if tc.hooksPath == "not-a-dir" {
				crewHooksWrite(t, filepath.Join(clone, "not-a-dir"), "file\n")
			}
			if tc.githooks {
				if err := os.MkdirAll(filepath.Join(clone, ".githooks"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			for _, wt := range tc.worktrees {
				if err := os.MkdirAll(filepath.Join(clone, ".git", "worktrees", wt), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			res := crewHooksRun(town)
			if res.Status != StatusError {
				t.Fatalf("Status = %v, want StatusError: %+v", res.Status, res)
			}
			if len(res.Details) != 1 {
				t.Fatalf("Details = %v, want one finding", res.Details)
			}
			finding := res.Details[0]
			for _, want := range []string{
				clone,
				"dangling core.hooksPath " + tc.hooksPath,
				tc.wantWorktrees,
				tc.wantRepair,
			} {
				if !strings.Contains(finding, want) {
					t.Errorf("finding %q does not contain %q", finding, want)
				}
			}
		})
	}
}

// TestMacCrewHooksReportsAnUnreadableConfig: a .git/config the reader cannot parse,
// or cannot read at all, is an ERROR. It is never read as an unset hooksPath.
func TestMacCrewHooksReportsAnUnreadableConfig(t *testing.T) {
	t.Parallel()
	absent := (*string)(nil)
	cases := []struct {
		name   string
		config *string
		why    string
	}{
		{name: "bare text", config: strPtr("not a [valid config\n"), why: "is not a key"},
		{name: "unterminated section", config: strPtr("[core\n\thooksPath = .githooks\n"), why: "unterminated section header"},
		{name: "section name missing", config: strPtr("[ core]\n\thooksPath = .githooks\n"), why: "section header has no valid name"},
		{name: "unterminated subsection", config: strPtr("[core \"x\n"), why: "unterminated quote"},
		{name: "subsection ends in a backslash", config: strPtr("[core \"x\\\n"), why: "quoted text ends in a backslash"},
		{name: "unknown escape in subsection", config: strPtr("[core \"a\\q\"]\n"), why: "unknown escape"},
		{name: "unterminated quote", config: strPtr("[core]\n\thooksPath = \"unterminated\n"), why: "unterminated quote"},
		{name: "key outside a section", config: strPtr("hooksPath = .githooks\n"), why: "outside any section"},
		{name: "unknown escape", config: strPtr("[core]\n\thooksPath = bad\\q\n"), why: "unknown escape"},
		{name: "empty value", config: strPtr("[core]\n\thooksPath =\n"), why: "has no value"},
		{name: "valueless key", config: strPtr("[core]\n\thooksPath\n"), why: "has no value"},
		{name: "line continuation", config: strPtr("[core]\n\thooksPath = .githooks \\\n"), why: "line continuation is not read"},
		{name: "text after section header", config: strPtr("[core] hooksPath = .githooks\n"), why: "text after a section header"},
		{name: "config absent", config: absent, why: "no such file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town, clone := crewHooksTown(t, tc.config)

			res := crewHooksRun(town)
			if res.Status != StatusError {
				t.Fatalf("Status = %v, want StatusError: %+v", res.Status, res)
			}
			if len(res.Details) != 1 || !strings.Contains(res.Details[0], clone) ||
				!strings.Contains(res.Details[0], "cannot read .git/config") ||
				!strings.Contains(res.Details[0], tc.why) {
				t.Errorf("Details = %v, want one finding naming %s, 'cannot read .git/config' and %q", res.Details, clone, tc.why)
			}
		})
	}
}

// TestMacCrewHooksPassesHealthyClones: an unset hooksPath, a relative or absolute
// one that exists, and each key form the reader accepts all pass. A linked
// worktree, whose .git is a file, is not scanned on its own.
func TestMacCrewHooksPassesHealthyClones(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		config   string
		absolute bool
		worktree bool
	}{
		{name: "unset", config: "[core]\n\tbare = false\n"},
		{name: "relative existing", config: "[core]\n\thooksPath = .githooks\n"},
		{name: "absolute existing", config: "[core]\n\thooksPath = @HOOKS@\n", absolute: true},
		{name: "case varied key and section", config: "[CORE]\n\tHooksPath = .githooks\n"},
		{name: "spaced key and value", config: "[core]\n  hooksPath   =   \".githooks\"  \n"},
		{name: "comment after value", config: "[core]\n\thooksPath = .githooks # hooks\n"},
		{name: "quoted subsection is not core", config: "[core \"x\"]\n\thooksPath = /nonexistent\n"},
		{name: "linked worktree is not scanned", config: "[core]\n", worktree: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town, clone := crewHooksTown(t, nil)
			hooks := filepath.Join(town, "hooks")
			if err := os.MkdirAll(hooks, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(clone, ".githooks"), 0o755); err != nil {
				t.Fatal(err)
			}
			config := strings.ReplaceAll(tc.config, "@HOOKS@", hooks)
			crewHooksWrite(t, filepath.Join(clone, ".git", "config"), config)
			if tc.worktree {
				// A linked worktree whose .git is a file has no config of its own. Scanning
				// it would read a missing .git/config and report an error.
				crewHooksWrite(t, filepath.Join(town, "gastown", "crew", "sloan-wt", ".git"), "gitdir: /nonexistent/gitdir\n")
			}

			res := crewHooksRun(town)
			if res.Status != StatusOK {
				t.Fatalf("Status = %v, want StatusOK: %+v", res.Status, res)
			}
			if len(res.Details) != 0 {
				t.Errorf("Details = %v, want none", res.Details)
			}
		})
	}
}

// TestMacCrewHooksWithNoCrewDirectoriesWarns: a town with no crew clones gives a
// WARNING that says to run the check on a native gt. It is not a pass, so a run in
// the pod cannot report clean.
func TestMacCrewHooksWithNoCrewDirectoriesWarns(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(t *testing.T, town string)
	}{
		{name: "empty town", setup: func(t *testing.T, town string) {}},
		{name: "rig without crew", setup: func(t *testing.T, town string) {
			if err := os.MkdirAll(filepath.Join(town, "gastown", "polecats"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			tc.setup(t, town)

			res := crewHooksRun(town)
			if res.Status != StatusWarning {
				t.Fatalf("Status = %v, want StatusWarning: %+v", res.Status, res)
			}
			if !strings.Contains(res.Message, "run this check on a native gt on the Mac") {
				t.Errorf("Message = %q, want the native-gt instruction", res.Message)
			}
		})
	}
}

// TestMacCrewHooksRefusesUnreadableLayout: each layout the check cannot read is an
// ERROR that names what it could not read. None is passed as an empty or clean town.
func TestMacCrewHooksRefusesUnreadableLayout(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		setup   func(t *testing.T) string
		wantMsg string
	}{
		{
			name: "town root missing",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "absent")
			},
			wantMsg: "Cannot list the town root",
		},
		{
			name: "crew is a regular file",
			setup: func(t *testing.T) string {
				town := t.TempDir()
				crewHooksWrite(t, filepath.Join(town, "gastown", "crew"), "not a dir\n")
				return town
			},
			wantMsg: "Cannot list the crew directory of rig gastown",
		},
		{
			name: "dot git is a symlink loop",
			setup: func(t *testing.T) string {
				town, clone := crewHooksTown(t, nil)
				if err := os.RemoveAll(filepath.Join(clone, ".git")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(".git", filepath.Join(clone, ".git")); err != nil {
					t.Fatal(err)
				}
				return town
			},
			wantMsg: "Cannot read the .git entry of crew clone",
		},
		{
			name: "githooks is a symlink loop",
			setup: func(t *testing.T) string {
				config := "[core]\n\thooksPath = missing/hooks\n"
				town, clone := crewHooksTown(t, &config)
				if err := os.Symlink(".githooks", filepath.Join(clone, ".githooks")); err != nil {
					t.Fatal(err)
				}
				return town
			},
			wantMsg: "cannot read .githooks",
		},
		{
			name: "hooksPath beneath a regular file",
			setup: func(t *testing.T) string {
				config := "[core]\n\thooksPath = afile/sub\n"
				town, clone := crewHooksTown(t, &config)
				crewHooksWrite(t, filepath.Join(clone, "afile"), "x\n")
				return town
			},
			wantMsg: "cannot read core.hooksPath",
		},
		{
			name: "worktrees is a regular file",
			setup: func(t *testing.T) string {
				config := "[core]\n\thooksPath = /nonexistent/gt-9glye5\n"
				town, clone := crewHooksTown(t, &config)
				crewHooksWrite(t, filepath.Join(clone, ".git", "worktrees"), "x\n")
				return town
			},
			wantMsg: "linked worktree count unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := tc.setup(t)

			res := crewHooksRun(town)
			if res.Status != StatusError {
				t.Fatalf("Status = %v, want StatusError: %+v", res.Status, res)
			}
			found := strings.Contains(res.Message, tc.wantMsg)
			for _, d := range res.Details {
				found = found || strings.Contains(d, tc.wantMsg)
			}
			if !found {
				t.Errorf("result %+v does not name %q", res, tc.wantMsg)
			}
		})
	}
}

func strPtr(s string) *string { return &s }
