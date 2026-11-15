package hygiene

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
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

// GitOutput returns one git subcommand's standard output, run in dir. The
// loader takes it as a parameter so a unit test can hand the loader a linked
// worktree's answers: a test file in the unit tier does not start a process,
// and a same-package helper that runs git would be a realgit.txt entry, a
// list that only shrinks (internal/testpolicy).
type GitOutput func(dir string, args ...string) ([]byte, error)

// TestTreeScanWorksInLinkedWorktree builds a linked worktree's layout in a
// temporary directory and checks that the scan of it reports the finding the
// scan of its main checkout reports.
//
// The layout is real — the linked worktree's .git entry is a regular file, and
// its index sits under the repository's git dir — while git's answers are
// canned (linkedWorktreeGit): the unit tier starts no process, and the
// enforcement test that needs real git is gt-xl8mb8.
func TestTreeScanWorksInLinkedWorktree(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	mainDir := filepath.Join(base, "main")
	linkedDir := filepath.Join(base, "linked")
	linkedSub := filepath.Join(linkedDir, "sub")
	// The planted line holds a home path whose account name is not a
	// placeholder, so the scan has one finding to compare.
	homeLine := planted("notes: see /", "home/", "jane", "/work")
	writeFixture(t, filepath.Join(mainDir, "notes.txt"), homeLine+"\n")
	writeFixture(t, filepath.Join(linkedDir, "notes.txt"), homeLine+"\n")
	if err := os.MkdirAll(linkedSub, 0o755); err != nil {
		t.Fatalf("mkdir linked worktree subdirectory: %v", err)
	}

	mainGitDir := filepath.Join(mainDir, ".git")
	// A linked worktree's git dir lives under its repository's git dir and
	// holds the worktree's own index, a different file from the repository's:
	// the loader reads the one git names for the worktree it scans.
	worktreeGitDir := filepath.Join(mainGitDir, "worktrees", "linked")
	writeFixture(t, filepath.Join(mainGitDir, "index"), "main index\n")
	writeFixture(t, filepath.Join(worktreeGitDir, "index"), "linked index\n")
	writeFixture(t, filepath.Join(worktreeGitDir, "commondir"), "../..\n")
	// A linked worktree names its git dir in a .git file, not a directory.
	gitFile := filepath.Join(linkedDir, ".git")
	writeFixture(t, gitFile, "gitdir: "+worktreeGitDir+"\n")
	info, err := os.Lstat(gitFile)
	if err != nil {
		t.Fatalf("stat linked worktree .git: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("linked worktree .git is %s, want a regular file", info.Mode())
	}

	git := linkedWorktreeGit{
		t:        t,
		toplevel: map[string]string{mainDir: mainDir, linkedSub: linkedDir},
		gitDirs:  map[string]string{mainDir: mainGitDir, linkedSub: worktreeGitDir},
		tracked:  []string{"notes.txt"},
	}
	want := Finding{Path: "notes.txt", Rule: RuleHomePath, Line: 1, LineHash: HashLine(homeLine)}
	assertFindings(t, scanRepo(t, git.run, mainDir), want)
	// A scan started inside the linked worktree reads the worktree's tree
	// through git's top level, not the directory it was handed: the file is
	// at the worktree's root, not under the subdirectory.
	assertFindings(t, scanRepo(t, git.run, linkedSub), want)
}

// TestLoadRepoTreeRefusesAnUnusableGit holds the loader to failing whenever
// git's answer or the index it names cannot be used. A silent fall back to the
// directory at hand would scan a tree nobody asked for.
func TestLoadRepoTreeRefusesAnUnusableGit(t *testing.T) {
	t.Parallel()
	repoDir := func(t *testing.T) string {
		t.Helper()
		repo := filepath.Join(t.TempDir(), "repo")
		writeFixture(t, filepath.Join(repo, ".git", "index"), "index\n")
		return repo
	}
	cases := []struct {
		name string
		git  func(t *testing.T, repo string) linkedWorktreeGit
		want string // the step the error must name
	}{
		{"git prints no top level", func(t *testing.T, repo string) linkedWorktreeGit {
			return linkedWorktreeGit{
				t:        t,
				toplevel: map[string]string{repo: ""},
				gitDirs:  map[string]string{repo: filepath.Join(repo, ".git")},
				tracked:  []string{"notes.txt"},
			}
		}, "rev-parse --show-toplevel in"},
		{"git prints no git dir", func(t *testing.T, repo string) linkedWorktreeGit {
			return linkedWorktreeGit{
				t:        t,
				toplevel: map[string]string{repo: repo},
				gitDirs:  map[string]string{repo: ""},
				tracked:  []string{"notes.txt"},
			}
		}, "rev-parse --absolute-git-dir in"},
		{"the git dir names no index", func(t *testing.T, repo string) linkedWorktreeGit {
			return linkedWorktreeGit{
				t:        t,
				toplevel: map[string]string{repo: repo},
				gitDirs:  map[string]string{repo: filepath.Join(repo, "elsewhere")},
				tracked:  []string{"notes.txt"},
			}
		}, "read git index"},
		{"ls-files fails", func(t *testing.T, repo string) linkedWorktreeGit {
			return linkedWorktreeGit{
				t:        t,
				toplevel: map[string]string{repo: repo},
				gitDirs:  map[string]string{repo: filepath.Join(repo, ".git")},
				fail:     map[string]error{"ls-files": errors.New("exit status 128")},
			}
		}, "ls-files -z in"},
		{"ls-files lists no file", func(t *testing.T, repo string) linkedWorktreeGit {
			return linkedWorktreeGit{
				t:        t,
				toplevel: map[string]string{repo: repo},
				gitDirs:  map[string]string{repo: filepath.Join(repo, ".git")},
			}
		}, "listed no file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := repoDir(t)
			_, _, err := loadRepoTree(tc.git(t, repo).run, repo)
			if err == nil {
				t.Fatal("loadRepoTree read an unusable git answer as a tree")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
		})
	}
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
		{"a line rule's row with no line hash", []Allow{{Path: "a.txt", Rule: RuleHomePath, Reason: "fixture"}}},
		{"a path-only rule's row with a line hash", []Allow{{Path: "bin.dat", Rule: RuleBinaryFile, Line: HashLine("x"), Reason: "fixture"}}},
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
	t.Run("path-only rows carry no line hash", func(t *testing.T) {
		t.Parallel()
		rows := []Allow{
			{Path: ".claude/commands/done.md", Rule: RuleForbiddenDir, Reason: "installed by a crew workspace, not tracked"},
			{Path: "testdata/blob.bin", Rule: RuleBinaryFile, Reason: "canned daemon response, not text"},
			{Path: "docs/blocked-in-the-path.txt", Rule: RuleBlockedWord, Reason: "the blocked word is in the path, not a line"},
		}
		if err := checkAllowFile(rows); err != nil {
			t.Fatalf("checkAllowFile: %v", err)
		}
	})
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
// the shipped rows: the ceiling, the scope of each row, the line hashes, the
// reasons, and duplicates. A reason shared by more than allowReasonShareLimit
// rows must name the directory those rows sit under.
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
		if err := checkRowScope(row); err != nil {
			return err
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

// pathScopedRules report a path without a line: a file under a forbidden
// directory, a binary file, and a blocked word in a path. Every other rule
// reports a line, so its row carries that line's hash.
var pathScopedRules = map[string]bool{
	RuleForbiddenDir: true,
	RuleBinaryFile:   true,
	RuleBlockedWord:  true,
}

// checkRowScope refuses a row whose line hash does not fit the shape of the
// finding it excuses. A line rule's finding always carries its line's hash, so
// a row without one is dead weight that reads as a file-wide excuse; a
// path-only finding never carries a hash, so a row that names one excuses
// nothing.
func checkRowScope(row Allow) error {
	if row.Line == "" {
		if !pathScopedRules[row.Rule] {
			return fmt.Errorf("row for %s rule %q names no line hash: the rule reports a line, so the row excuses nothing", row.Path, row.Rule)
		}
		return nil
	}
	if row.Rule == RuleForbiddenDir || row.Rule == RuleBinaryFile {
		return fmt.Errorf("row for %s rule %q names a line hash, but the rule reports the path, so the row excuses nothing", row.Path, row.Rule)
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

// loadRepoTree returns the tracked files of the git work tree that holds dir,
// as a file system rooted at the work tree's top plus the paths relative to
// it. The git answers come from the runner, so a test supplies them: the
// tracked set from git ls-files, so a checkout's ignored local files never
// enter the scan, and the top from git rev-parse, so a linked worktree reads
// the same tree as its main checkout.
//
// It also reads the work tree's index. The tracked set changes when a file is
// staged while no tracked file's content changes, and a cached test result
// only reruns on a file it opened.
func loadRepoTree(git GitOutput, dir string) (fs.FS, []string, error) {
	top, err := gitOutput(git, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, nil, err
	}
	gitDir, err := gitOutput(git, dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, nil, err
	}
	// A linked worktree keeps its index under the git dir it names, and its
	// .git entry is a file: the path comes from git, never from a guess.
	index := filepath.Join(gitDir, "index")
	if _, err := os.ReadFile(index); err != nil {
		return nil, nil, fmt.Errorf("hygiene: read git index %s: %w", index, err)
	}
	paths, err := listTracked(git, top)
	if err != nil {
		return nil, nil, err
	}
	// A listing that names no file is an error, not a clean tree: the guard
	// must fail closed when git's answer is unusable.
	if len(paths) == 0 {
		return nil, nil, fmt.Errorf("hygiene: git ls-files in %s listed no file", top)
	}
	return os.DirFS(top), paths, nil
}

// gitOutput runs one git subcommand through the runner and returns the answer
// with the surrounding whitespace removed. An empty answer is an error, never a
// path.
func gitOutput(git GitOutput, dir string, args ...string) (string, error) {
	out, err := git(dir, args...)
	if err != nil {
		return "", fmt.Errorf("hygiene: git %s in %s: %w", strings.Join(args, " "), dir, err)
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return "", fmt.Errorf("hygiene: git %s in %s printed nothing", strings.Join(args, " "), dir)
	}
	return value, nil
}

// listTracked lists the tracked files of the work tree at top, relative to it,
// NUL-separated from git so no file name is split.
func listTracked(git GitOutput, top string) ([]string, error) {
	out, err := git(top, "ls-files", "-z")
	if err != nil {
		return nil, fmt.Errorf("hygiene: git ls-files -z in %s: %w", top, err)
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// scanRepo scans the tracked files of the work tree that holds dir with the
// shipped rules and blocklist and no allow rows.
func scanRepo(t *testing.T, git GitOutput, dir string) []Finding {
	t.Helper()
	fsys, paths, err := loadRepoTree(git, dir)
	if err != nil {
		t.Fatalf("%v", err)
	}
	findings, _, err := Scan(fsys, paths, DefaultRules(), shippedBlocked(t), nil)
	if err != nil {
		t.Fatalf("Scan %s: %v", filepath.Base(dir), err)
	}
	return findings
}

// linkedWorktreeGit answers as git does in a repository and in a linked
// worktree of it, without starting a process. Its rev-parse answers are keyed
// by the directory git runs in, so a scan started in a subdirectory reads the
// top git names rather than the subdirectory; ls-files is answered only in a
// modelled top level. An answer it does not model is a test failure: never an
// empty result.
type linkedWorktreeGit struct {
	t        *testing.T
	toplevel map[string]string
	gitDirs  map[string]string
	tracked  []string
	fail     map[string]error // subcommand name -> the error it returns
}

func (g linkedWorktreeGit) run(dir string, args ...string) ([]byte, error) {
	g.t.Helper()
	if err, ok := g.fail[args[0]]; ok {
		return nil, err
	}
	switch args[0] {
	case "rev-parse":
		if len(args) != 2 {
			break
		}
		answers := g.toplevel
		if args[1] == "--absolute-git-dir" {
			answers = g.gitDirs
		}
		answer, ok := answers[dir]
		if !ok {
			g.t.Fatalf("the loader asked git %v in %s, which the fixture does not model", args, dir)
		}
		return []byte(answer + "\n"), nil
	case "ls-files":
		// The listing runs in the top level git named, never in the
		// directory the scan was handed.
		known := false
		for _, top := range g.toplevel {
			known = known || top == dir
		}
		if !known {
			g.t.Fatalf("the loader asked git %v in %s, which is no modelled top level", args, dir)
		}
		var out strings.Builder
		for _, path := range g.tracked {
			out.WriteString(path)
			out.WriteByte(0)
		}
		return []byte(out.String()), nil
	}
	g.t.Fatalf("the loader ran git %v in %s, which the fixture does not model", args, dir)
	return nil, nil
}

// writeFixture writes body to path, creating its directory.
func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", filepath.Base(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", filepath.Base(path), err)
	}
}
