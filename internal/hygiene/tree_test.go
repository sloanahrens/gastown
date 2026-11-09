package hygiene

import (
	"bytes"
	_ "embed"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// allowJSON is the excuse list. It is embedded, so an edit to the file changes
// the test binary and the test cache reruns the tests that read it.
//
//go:embed allow.json
var allowJSON []byte

// allowRowCeiling is the most rows the excuse list may hold. It only shrinks.
const allowRowCeiling = 140

// allowReasonShareLimit is how many rows may share one reason before the
// reason has to name the directory its rows sit under.
const allowReasonShareLimit = 25

// TestTreeScanWorksInLinkedWorktree builds its own repository in a temporary
// directory, adds a linked worktree of it, and checks that the scan of the
// worktree reports what the scan of the main checkout reports. It never reads
// the repository the test runs in.
//
// Tier: unit, on purpose: one git init, one commit, one worktree add, and no
// network. It fails when git is missing.
func TestTreeScanWorksInLinkedWorktree(t *testing.T) {
	t.Parallel()
	requireGit(t)
	base := t.TempDir()
	mainDir := filepath.Join(base, "main")
	linkedDir := filepath.Join(base, "linked")
	if err := os.Mkdir(mainDir, 0o755); err != nil {
		t.Fatalf("mkdir main checkout: %v", err)
	}
	fixtureGit(t, mainDir, "init", "-q")
	// The planted line holds a home path whose account name is not a
	// placeholder, so the scan has one finding to compare.
	homeLine := planted("notes: see /", "home/", "jane", "/work")
	if err := os.WriteFile(filepath.Join(mainDir, "notes.txt"), []byte(homeLine+"\n"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	fixtureGit(t, mainDir, "add", "notes.txt")
	fixtureGit(t, mainDir, "commit", "-q", "-m", "fixture")
	fixtureGit(t, mainDir, "worktree", "add", "-q", "--detach", linkedDir)

	// A linked worktree names its repository in a .git file, not a directory.
	info, err := os.Lstat(filepath.Join(linkedDir, ".git"))
	if err != nil {
		t.Fatalf("stat linked worktree .git: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("linked worktree .git is %s, want a regular file", info.Mode())
	}

	mainFindings := scanRepo(t, mainDir)
	linkedFindings := scanRepo(t, linkedDir)
	want := []Finding{{Path: "notes.txt", Rule: RuleHomePath, Line: 1, LineHash: HashLine(homeLine)}}
	assertFindings(t, mainFindings, want...)
	assertFindings(t, linkedFindings, want...)
}

// TestAllowFileHasNoStaleOrEmptyRows checks the refusals ValidateAllow owes the
// allow list, on in-memory files. An empty path or reason, a path git does not
// track, a rule that does not fire and a line hash no longer in the file are
// each refused.
func TestAllowFileHasNoStaleOrEmptyRows(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"kept.txt":  {Data: []byte("kept line\n")},
		"other.txt": {Data: []byte("nothing here\n")},
	}
	paths := sortedPaths(fsys)
	keptLine := Allow{Path: "kept.txt", Rule: RuleBlockedWord, Line: HashLine("kept line"), Reason: "fixture"}
	cases := []struct {
		name string
		row  Allow
		ok   bool
	}{
		{"valid line row", keptLine, true},
		{"empty path", Allow{Rule: RuleBlockedWord, Line: HashLine("kept line"), Reason: "fixture"}, false},
		{"empty reason", Allow{Path: "kept.txt", Rule: RuleBlockedWord, Line: HashLine("kept line")}, false},
		{"untracked path", Allow{Path: "gone.txt", Rule: RuleBlockedWord, Line: HashLine("kept line"), Reason: "fixture"}, false},
		{"vanished line hash", Allow{Path: "kept.txt", Rule: RuleBlockedWord, Line: HashLine("edited line"), Reason: "fixture"}, false},
		{"rule that does not fire", Allow{Path: "other.txt", Rule: RuleBinaryFile, Reason: "fixture"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateAllow(fsys, paths, rulesThatNeverMatch(), blockedFrom(t, "kept"), []Allow{tc.row})
			if tc.ok && err != nil {
				t.Fatalf("ValidateAllow: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("ValidateAllow accepted a stale or empty row")
			}
		})
	}
}

// TestAllowFileIsSmallAndJustified checks the shape of the shipped allow list:
// its size, its line hashes, its reasons and its duplicates.
func TestAllowFileIsSmallAndJustified(t *testing.T) {
	t.Parallel()
	if err := checkAllowFile(loadAllowFile(t)); err != nil {
		t.Fatalf("allow.json: %v", err)
	}
}

// TestAllowFileIsSmallAndJustifiedRefusesBadShapes drives each refusal of
// checkAllowFile with a fixture, so the check is red without each branch.
func TestAllowFileIsSmallAndJustifiedRefusesBadShapes(t *testing.T) {
	t.Parallel()
	many := func(n int, reason string, path func(i int) string) []Allow {
		rows := make([]Allow, 0, n)
		for i := 0; i < n; i++ {
			rows = append(rows, Allow{Path: path(i), Rule: RuleBlockedWord, Reason: reason})
		}
		return rows
	}
	flat := func(i int) string { return fmt.Sprintf("file%03d.txt", i) }
	under := func(i int) string { return fmt.Sprintf("internal/cmd/file%03d.go", i) }
	// Each row has its own reason, so only the ceiling can refuse this fixture.
	ceiling := make([]Allow, 0, allowRowCeiling+1)
	for i := 0; i <= allowRowCeiling; i++ {
		ceiling = append(ceiling, Allow{Path: fmt.Sprintf("d%03d/f.txt", i), Rule: RuleBlockedWord, Reason: fmt.Sprintf("reason %d", i)})
	}
	cases := []struct {
		name string
		rows []Allow
	}{
		{"over the ceiling", ceiling},
		{"shared reason with no path pattern", many(allowReasonShareLimit+1, "fixture", flat)},
		{"identical rows", []Allow{
			{Path: "a.txt", Rule: RuleBlockedWord, Reason: "fixture"},
			{Path: "a.txt", Rule: RuleBlockedWord, Reason: "fixture"},
		}},
		{"line hash of the wrong shape", []Allow{{Path: "a.txt", Rule: RuleBlockedWord, Line: "xyz", Reason: "fixture"}}},
		{"empty reason", []Allow{{Path: "a.txt", Rule: RuleBlockedWord}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := checkAllowFile(tc.rows); err == nil {
				t.Fatal("checkAllowFile accepted a bad shape")
			}
		})
	}
	t.Run("shared reason that names its directory", func(t *testing.T) {
		t.Parallel()
		rows := many(allowReasonShareLimit+1, "kept under internal/cmd/ for the fixture", under)
		if err := checkAllowFile(rows); err != nil {
			t.Fatalf("checkAllowFile: %v", err)
		}
	})
}

// requireGit fails the test when git is not on the PATH. It never skips: the
// scan is only worth running where it can read the tree.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required by this test: %v", err)
	}
}

// loadAllowFile decodes the embedded allow list, refusing unknown fields.
func loadAllowFile(t *testing.T) []Allow {
	t.Helper()
	rows, err := LoadAllow(bytes.NewReader(allowJSON))
	if err != nil {
		t.Fatalf("LoadAllow: %v", err)
	}
	return rows
}

// checkAllowFile is the shape check TestAllowFileIsSmallAndJustified applies to
// the shipped rows: the ceiling, the line hashes, the reasons, and duplicates.
// A reason shared by more than allowReasonShareLimit rows must name the
// directory those rows sit under.
func checkAllowFile(rows []Allow) error {
	if len(rows) > allowRowCeiling {
		return fmt.Errorf("%d rows, the ceiling is %d", len(rows), allowRowCeiling)
	}
	seen := make(map[Allow]struct{}, len(rows))
	byReason := make(map[string][]Allow)
	for _, row := range rows {
		if row.Path == "" || row.Rule == "" {
			return fmt.Errorf("row for %q names no path or no rule", row.Path)
		}
		if strings.TrimSpace(row.Reason) == "" {
			return fmt.Errorf("row for %s names no reason", row.Path)
		}
		if row.Line != "" && (len(row.Line) != 16 || !isHex(row.Line)) {
			return fmt.Errorf("row for %s holds a line hash of the wrong shape", row.Path)
		}
		if _, dup := seen[row]; dup {
			return fmt.Errorf("row for %s appears twice", row.Path)
		}
		seen[row] = struct{}{}
		byReason[row.Reason] = append(byReason[row.Reason], row)
	}
	for reason, sharing := range byReason {
		if len(sharing) > allowReasonShareLimit && !reasonNamesDirectory(reason, sharing) {
			return fmt.Errorf("%d rows share the reason %q and it names no directory", len(sharing), reason)
		}
	}
	return nil
}

// reasonNamesDirectory reports whether the reason names a directory, ending in a
// slash, that every one of the rows sits under.
func reasonNamesDirectory(reason string, rows []Allow) bool {
	for _, field := range strings.Fields(reason) {
		if !strings.HasSuffix(field, "/") {
			continue
		}
		all := true
		for _, row := range rows {
			if !strings.HasPrefix(row.Path, field) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// loadRepoTree lists the tracked files of the repository that holds dir and
// returns them as a file system rooted at the repository's top. The git index
// is read first, so a checkout whose index cannot be read fails loudly instead
// of scanning a tracked set git no longer reports. Each file is then read by
// the scanner.
func loadRepoTree(t *testing.T, dir string) (fs.FS, []string) {
	t.Helper()
	requireGit(t)
	root, err := gitOutput(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatalf("%v", err)
	}
	gitDir, err := gitOutput(dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatalf("%v", err)
	}
	// A linked worktree keeps its index under the common git directory, and
	// its .git entry is a file: the path comes from git, never from a guess.
	index := filepath.Join(strings.TrimSpace(string(gitDir)), "index")
	if _, err := os.ReadFile(index); err != nil {
		t.Fatalf("hygiene: read git index: %v", err)
	}
	top := strings.TrimSpace(string(root))
	paths, err := listTracked(top)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return os.DirFS(top), paths
}

// listTracked lists the tracked files of the repository at root, relative to
// root, NUL-separated from git so no file name is split. Only
// TestTreeScanWorksInLinkedWorktree reaches it; every other test in the package
// uses an in-memory file set and runs no process.
func listTracked(root string) ([]string, error) {
	out, err := gitOutput(root, "ls-files", "-z")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// scanRepo scans the tracked files of the repository that holds dir with the
// shipped rules and blocklist and no allow rows.
func scanRepo(t *testing.T, dir string) []Finding {
	t.Helper()
	fsys, paths := loadRepoTree(t, dir)
	findings, _, err := Scan(fsys, paths, DefaultRules(), shippedBlocked(t), nil)
	if err != nil {
		t.Fatalf("Scan %s: %v", filepath.Base(dir), err)
	}
	return findings
}

// gitOutput runs git in dir with the GIT_ variables removed, so an inherited
// variable cannot point the command at another repository. The error names the
// subcommand only: captured output can hold a path or a name.
func gitOutput(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //testpolicy:allow no-git — the guard reads the repository's tracked set, so it cannot run on canned output
	cmd.Env = withoutGitEnv(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("hygiene: git %s: %w", args[0], err)
	}
	return out, nil
}

// fixtureGit runs git in a fixture repository with a fixed identity and no
// signing, so a commit does not depend on the host's configuration.
func fixtureGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := []string{"-C", dir, "-c", "user.name=fixture", "-c", "user.email=" + planted("fixture", "@", "example", ".com"), "-c", "commit.gpgsign=false"}
	cmd := exec.Command("git", append(full, args...)...) //testpolicy:allow no-git — the linked-worktree test builds its own repository under t.TempDir
	cmd.Env = withoutGitEnv(os.Environ())
	if err := cmd.Run(); err != nil {
		t.Fatalf("hygiene: git %s in fixture: %v", args[0], err)
	}
}

func withoutGitEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		if !strings.HasPrefix(kv, "GIT_") {
			out = append(out, kv)
		}
	}
	return out
}
