package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/hygiene"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// shape joins the pieces of a fixture line so no source line of this file has
// a shape the scanner reports. The fixture names no person.
func shape(parts ...string) string {
	return strings.Join(parts, "")
}

// fixtureBlocked is a blocklist with one word no fixture line holds, so a scan
// runs with something to apply and reports only the shape rules.
func fixtureBlocked() (map[string]struct{}, error) {
	return map[string]struct{}{hygiene.HashWord("no-fixture-line-holds-this"): {}}, nil
}

// fakeGit answers git for a fixed tracked set without starting a process. A
// command it does not model is an error, never an empty answer.
type fakeGit struct {
	// top is the work tree top rev-parse prints and the directory git runs
	// in. It is the fixture tree's own directory, so a command that opened
	// any other root reads no fixture file.
	top     string
	paths   []string
	remotes []string // the names git remote lists
	origin  string   // the URL git remote get-url origin prints
	// originErr is a failed read of the origin's URL, for the case where the
	// remote is listed but its URL cannot be read.
	originErr error
	fail      map[string]error
}

func (g fakeGit) run(dir string, args ...string) ([]byte, error) {
	if err, ok := g.fail[args[0]]; ok {
		return nil, err
	}
	switch strings.Join(args, " ") {
	case "rev-parse --show-toplevel":
		if dir != "." {
			return nil, fmt.Errorf("fake git: rev-parse ran in %q, want the command's directory", dir)
		}
		return []byte(g.top + "\n"), nil
	case "ls-files -z":
		if dir != g.top {
			return nil, fmt.Errorf("fake git: ls-files ran in %q, want the work tree top %q", dir, g.top)
		}
		var out strings.Builder
		for _, path := range g.paths {
			out.WriteString(path)
			out.WriteByte(0)
		}
		return []byte(out.String()), nil
	case "remote":
		if dir != g.top {
			return nil, fmt.Errorf("fake git: remote ran in %q, want the work tree top %q", dir, g.top)
		}
		var out strings.Builder
		for _, name := range g.remotes {
			out.WriteString(name)
			out.WriteByte('\n')
		}
		return []byte(out.String()), nil
	case "remote get-url origin":
		if dir != g.top {
			return nil, fmt.Errorf("fake git: remote get-url ran in %q, want the work tree top %q", dir, g.top)
		}
		// git answers only for a remote it lists, so a fake that answered
		// anyway would pass a run that read a URL from a remote git does not
		// have.
		if !g.lists("origin") {
			return nil, errors.New("fake git: no such remote origin")
		}
		if g.originErr != nil {
			return nil, g.originErr
		}
		return []byte(g.origin + "\n"), nil
	}
	return nil, fmt.Errorf("fake git does not model %v", args)
}

// lists reports whether the fake lists the remote name.
func (g fakeGit) lists(name string) bool {
	for _, remote := range g.remotes {
		if remote == name {
			return true
		}
	}
	return false
}

// workTree writes a fixture work tree into a fresh temporary directory and
// returns its path: the top git names and the root the command opens. The
// command reads it with os.DirFS, as production does, so a run that opened any
// other root would find none of these files.
func workTree(t *testing.T, files map[string]string) string {
	t.Helper()
	top := t.TempDir()
	for name, body := range files {
		path := filepath.Join(top, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create the directory for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return top
}

// treeWith writes a fixture work tree holding the allow list and the files and
// returns its top.
func treeWith(t *testing.T, allowJSON string, files map[string]string) string {
	t.Helper()
	tree := map[string]string{allowPath: allowJSON}
	for name, body := range files {
		tree[name] = body
	}
	return workTree(t, tree)
}

// result is one run's outcome.
type result struct {
	code   int
	stdout string
	stderr string
}

// noTown is the town finder of a command run outside any town.
func noTown() (string, error) { return "", nil }

// runFixture runs the command over the fixture work tree at top and the fake
// git.
func runFixture(t *testing.T, top string, paths, args []string, blocked blockedLoader) result {
	t.Helper()
	return runFixtureWith(t, top, paths, args, blocked, noTown)
}

// runFixtureWith runs the command with the town finder the case needs.
func runFixtureWith(t *testing.T, top string, paths, args []string, blocked blockedLoader, findTown townRootFinder) result {
	t.Helper()
	return runFixtureGit(t, top, fakeGit{top: top, paths: paths}, args, blocked, findTown)
}

// runFixtureGit runs the command with the git answers and the town finder the
// case needs. The command opens the tree with os.DirFS, the way main does, so
// the run reads the work tree at the top git names and nowhere else.
func runFixtureGit(t *testing.T, top string, git fakeGit, args []string, blocked blockedLoader, findTown townRootFinder) result {
	t.Helper()
	var stdout, stderr strings.Builder
	code := run(args, &stdout, &stderr, git.run, os.DirFS, blocked, findTown)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// fixturePaths is the tracked set a run over one fixture file sees.
func fixturePaths(file string) []string {
	paths := []string{file, allowPath}
	sort.Strings(paths)
	return paths
}

// blockedWith answers the blocklist holding words and one no fixture line holds.
func blockedWith(words ...string) blockedLoader {
	return func() (map[string]struct{}, error) {
		blocked := map[string]struct{}{hygiene.HashWord("no-fixture-line-holds-this"): {}}
		for _, word := range words {
			blocked[hygiene.HashWord(word)] = struct{}{}
		}
		return blocked, nil
	}
}

// forgeURLLine is the line a town writes its forge URL on, for the account.
func forgeURLLine(account string) string {
	return shape("clone http://forgejo:3000/", account, "/gastown.git") + "\n"
}

// townRootLine is the line a town writes its town root on.
func townRootLine(root string) string {
	return shape("mountPath: ", root) + "\n"
}

// billingLine is a finding from the shape rules that names no person: a
// billing account id with a word after it, which the output must not repeat.
var billingLine = shape("charge ", "A1B2C3-", "D4E5F6-", "A1B2C3", " recorded")

// TestRunPrintsEachFindingAsALocationAndNeverItsText covers the lister's
// output contract: one path, line and rule per finding, none of the matched
// text, and exit 1.
func TestRunPrintsEachFindingAsALocationAndNeverItsText(t *testing.T) {
	t.Parallel()
	top := treeWith(t, "[]", map[string]string{"docs/notes.md": "first line\n" + billingLine + "\n"})
	got := runFixture(t, top, []string{"docs/notes.md", allowPath}, nil, fixtureBlocked)
	if got.code != exitFindings {
		t.Fatalf("exit code = %d, want %d; stderr: %s", got.code, exitFindings, got.stderr)
	}
	if want := "docs/notes.md:2 " + hygiene.RuleBillingID + "\n"; got.stdout != want {
		t.Errorf("stdout = %q, want %q", got.stdout, want)
	}
	for _, text := range []string{billingLine, "A1B2C3", "recorded"} {
		if strings.Contains(got.stdout, text) {
			t.Errorf("stdout holds %q, which is part of the matched line", text)
		}
	}
	if got.stderr != "" {
		t.Errorf("stderr = %q, want nothing", got.stderr)
	}
}

// TestRunPrintsNothingAndExitsZeroForACleanTree covers the clean answer: no
// output and exit 0.
func TestRunPrintsNothingAndExitsZeroForACleanTree(t *testing.T) {
	t.Parallel()
	top := treeWith(t, "[]", map[string]string{"docs/notes.md": "nothing to report here\n"})
	got := runFixture(t, top, []string{"docs/notes.md", allowPath}, nil, fixtureBlocked)
	if got.code != exitClean || got.stdout != "" || got.stderr != "" {
		t.Fatalf("got exit %d, stdout %q, stderr %q; want exit 0 and no output", got.code, got.stdout, got.stderr)
	}
}

// TestRunPrintsAPathFindingWithoutALine covers a rule that reports the path
// itself, such as a file under a forbidden directory: the line has no number,
// so the location is the path and the rule.
func TestRunPrintsAPathFindingWithoutALine(t *testing.T) {
	t.Parallel()
	forbidden := ".beads/" + "keep.json"
	top := treeWith(t, "[]", map[string]string{forbidden: "{}\n"})
	got := runFixture(t, top, []string{forbidden, allowPath}, nil, fixtureBlocked)
	if want := forbidden + " " + hygiene.RuleForbiddenDir + "\n"; got.code != exitFindings || got.stdout != want {
		t.Fatalf("got exit %d, stdout %q; want exit 1 and %q", got.code, got.stdout, want)
	}
}

// TestRunKeepsOnlyFindingsUnderEachDirPrefix covers -dir: every repeated
// prefix keeps the findings under it, and a prefix keeps a path inside it,
// never a sibling whose name only starts with the same letters.
func TestRunKeepsOnlyFindingsUnderEachDirPrefix(t *testing.T) {
	t.Parallel()
	top := treeWith(t, "[]", map[string]string{
		"docs/c.md":             billingLine + "\n",
		"internal/daemon/a.go":  billingLine + "\n",
		"internal/daemonx/b.go": billingLine + "\n",
	})
	paths := []string{"docs/c.md", "internal/daemon/a.go", "internal/daemonx/b.go", allowPath}
	got := runFixture(t, top, paths, []string{"-dir", "internal/daemon", "-dir", "docs/"}, fixtureBlocked)
	want := "docs/c.md:1 " + hygiene.RuleBillingID + "\n" +
		"internal/daemon/a.go:1 " + hygiene.RuleBillingID + "\n"
	if got.code != exitFindings || got.stdout != want {
		t.Fatalf("got exit %d, stdout %q; want exit 1 and %q", got.code, got.stdout, want)
	}
}

// TestRunRefusesADirPrefixNoTrackedPathSitsUnder covers the typo case: a
// filter that keeps nothing fails the run instead of reporting a clean tree.
func TestRunRefusesADirPrefixNoTrackedPathSitsUnder(t *testing.T) {
	t.Parallel()
	top := treeWith(t, "[]", map[string]string{"docs/notes.md": "clean\n"})
	got := runFixture(t, top, []string{"docs/notes.md", allowPath}, []string{"-dir", "internal/nope"}, fixtureBlocked)
	if got.code != exitError {
		t.Fatalf("exit code = %d, want %d; stderr: %s", got.code, exitError, got.stderr)
	}
	if !strings.Contains(got.stderr, "internal/nope") {
		t.Errorf("stderr = %q, want it to name the prefix", got.stderr)
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want nothing", got.stdout)
	}
}

// TestRunExitsTwoWhenAReadFails covers the acceptance's read error: a tracked
// file the tree cannot read, a missing allow list, and an allow list that is
// not JSON each exit 2 and print no finding.
func TestRunExitsTwoWhenAReadFails(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		top   string
		paths []string
		want  string // a word the stderr must hold
	}{
		{
			name:  "a tracked file the tree cannot read",
			top:   treeWith(t, "[]", nil),
			paths: []string{"docs/missing.md", allowPath},
			want:  "docs/missing.md",
		},
		{
			name:  "a missing allow list",
			top:   workTree(t, map[string]string{"docs/notes.md": "clean\n"}),
			paths: []string{"docs/notes.md"},
			want:  allowPath,
		},
		{
			name:  "an allow list that is not a list of rows",
			top:   treeWith(t, `{"path":"docs/notes.md"}`, map[string]string{"docs/notes.md": "clean\n"}),
			paths: []string{"docs/notes.md", allowPath},
			want:  "allow rows",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := runFixture(t, tc.top, tc.paths, nil, fixtureBlocked)
			if got.code != exitError {
				t.Fatalf("exit code = %d, want %d; stderr: %s", got.code, exitError, got.stderr)
			}
			if !strings.Contains(got.stderr, tc.want) {
				t.Errorf("stderr = %q, want it to hold %q", got.stderr, tc.want)
			}
			if got.stdout != "" {
				t.Errorf("stdout = %q, want nothing", got.stdout)
			}
		})
	}
}

// TestRunRefusesGitAnswersItCannotUse covers the git answers a run must fail
// on: a failed subcommand, a top that is empty, and a listing that names no
// file. Each exits 2 rather than reporting a clean tree.
func TestRunRefusesGitAnswersItCannotUse(t *testing.T) {
	t.Parallel()
	top := treeWith(t, "[]", map[string]string{"docs/notes.md": "clean\n"})
	cases := []struct {
		name string
		git  gitRunner
	}{
		{"rev-parse fails", fakeGit{top: top, fail: map[string]error{"rev-parse": errors.New("git failed")}}.run},
		{"ls-files fails", fakeGit{top: top, paths: []string{"docs/notes.md"}, fail: map[string]error{"ls-files": errors.New("git failed")}}.run},
		{"rev-parse names no top", func(string, ...string) ([]byte, error) { return []byte("\n"), nil }},
		{"ls-files lists no file", fakeGit{top: top}.run},
		{"remote fails", fakeGit{top: top, paths: []string{"docs/notes.md"}, fail: map[string]error{"remote": errors.New("git failed")}}.run},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			code := run(nil, &stdout, &stderr, tc.git, os.DirFS, fixtureBlocked, noTown)
			if code != exitError {
				t.Fatalf("exit code = %d, want %d; stderr: %s", code, exitError, stderr.String())
			}
			if stdout.String() != "" {
				t.Errorf("stdout = %q, want nothing", stdout.String())
			}
			if stderr.Len() == 0 {
				t.Error("stderr is empty, want the reason the run could not look")
			}
		})
	}
}

// TestRunRefusesAnUnreadableBlocklist covers the shipped blocklist failing to
// load: the run exits 2 rather than scanning with nothing to apply.
func TestRunRefusesAnUnreadableBlocklist(t *testing.T) {
	t.Parallel()
	top := treeWith(t, "[]", map[string]string{"docs/notes.md": "clean\n"})
	load := func() (map[string]struct{}, error) { return nil, errors.New("blocklist unreadable") }
	got := runFixture(t, top, []string{"docs/notes.md", allowPath}, nil, load)
	if got.code != exitError || !strings.Contains(got.stderr, "blocklist") {
		t.Fatalf("got exit %d, stderr %q; want exit 2 naming the blocklist", got.code, got.stderr)
	}
}

// TestRunRefusesBadFlags covers the flag answers a run must refuse: an unknown
// flag and a -dir value that keeps nothing each exit 2 before a file is read.
func TestRunRefusesBadFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
	}{
		{"an unknown flag", []string{"-nope"}},
		{"an empty -dir prefix", []string{"-dir", ""}},
		{"a -dir that names only a separator", []string{"-dir", "/"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			top := treeWith(t, "[]", nil)
			var stdout, stderr strings.Builder
			code := run(tc.args, &stdout, &stderr, fakeGit{top: top}.run, os.DirFS, fixtureBlocked, noTown)
			if code != exitError {
				t.Fatalf("exit code = %d, want %d", code, exitError)
			}
			if stdout.String() != "" {
				t.Errorf("stdout = %q, want nothing", stdout.String())
			}
			if stderr.Len() == 0 {
				t.Error("stderr is empty, want the flag error")
			}
		})
	}
}

// TestRunHelpExitsZero covers -h: usage is not a failed run.
func TestRunHelpExitsZero(t *testing.T) {
	t.Parallel()
	top := treeWith(t, "[]", nil)
	var stdout, stderr strings.Builder
	code := run([]string{"-h"}, &stdout, &stderr, fakeGit{top: top}.run, os.DirFS, fixtureBlocked, noTown)
	if code != exitClean {
		t.Fatalf("exit code = %d, want %d", code, exitClean)
	}
}

// errWriter fails every write.
type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

// TestRunExitsTwoWhenTheFindingsCannotBeWritten covers a failed write: the run
// exits 2 rather than 1, which would claim the list was printed.
func TestRunExitsTwoWhenTheFindingsCannotBeWritten(t *testing.T) {
	t.Parallel()
	top := treeWith(t, "[]", map[string]string{"docs/notes.md": billingLine + "\n"})
	var stderr strings.Builder
	code := run(nil, errWriter{}, &stderr, fakeGit{top: top, paths: []string{"docs/notes.md", allowPath}}.run, os.DirFS, fixtureBlocked, noTown)
	if code != exitError || !strings.Contains(stderr.String(), "write") {
		t.Fatalf("got exit %d, stderr %q; want exit 2 naming the write failure", code, stderr.String())
	}
}

// TestRunAppliesTheAllowListItReadsFromTheTree covers the allow list wiring: a
// finding an allow row excuses is not printed, since the lister reports what
// the repository has not already excused.
func TestRunAppliesTheAllowListItReadsFromTheTree(t *testing.T) {
	t.Parallel()
	line := billingLine + "\n"
	rows := []hygiene.Allow{{
		Path:   "docs/notes.md",
		Rule:   hygiene.RuleBillingID,
		Line:   hygiene.HashLine(line),
		Reason: "fixture row",
	}}
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal allow rows: %v", err)
	}
	top := treeWith(t, string(data), map[string]string{"docs/notes.md": line})
	got := runFixture(t, top, []string{"docs/notes.md", allowPath}, nil, fixtureBlocked)
	if got.code != exitClean || got.stdout != "" {
		t.Fatalf("got exit %d, stdout %q; want exit 0 and nothing printed", got.code, got.stdout)
	}
}

// TestRunExemptsTheIdentitiesTheFlagsName covers the two identity flags: each
// exempts its identity where the town writes it, and without the flag the same
// line is a finding.
func TestRunExemptsTheIdentitiesTheFlagsName(t *testing.T) {
	t.Parallel()
	townRoot := shape("/", "Users/", "fixtureaccount/gt")
	mountLine := shape("mountPath: ", townRoot) + "\n"
	owner := shape("fixture", "owner")
	cloneLine := shape("clone http://forgejo:3000/", owner, "/gastown.git") + "\n"
	cases := []struct {
		name     string
		file     string
		body     string
		blocked  []string
		args     []string
		wantCode int
		wantOut  string
	}{
		{
			name:     "the town root the flag names is exempted",
			file:     "config/mount.yaml",
			body:     mountLine,
			args:     []string{"-town-root", townRoot},
			wantCode: exitClean,
		},
		{
			name:     "without the flag the town root is a finding",
			file:     "config/mount.yaml",
			body:     mountLine,
			wantCode: exitFindings,
			wantOut:  "config/mount.yaml:1 " + hygiene.RuleHomePath + "\n",
		},
		{
			name:     "the forge owner the flag names is exempted in its URL",
			file:     "config/clone.yaml",
			body:     cloneLine,
			blocked:  []string{owner},
			args:     []string{"-forge-owner", owner},
			wantCode: exitClean,
		},
		{
			name:     "without the flag the forge owner is a finding",
			file:     "config/clone.yaml",
			body:     cloneLine,
			blocked:  []string{owner},
			wantCode: exitFindings,
			wantOut:  "config/clone.yaml:1 " + hygiene.RuleBlockedWord + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			load := func() (map[string]struct{}, error) {
				blocked := map[string]struct{}{hygiene.HashWord("no-fixture-line-holds-this"): {}}
				for _, word := range tc.blocked {
					blocked[hygiene.HashWord(word)] = struct{}{}
				}
				return blocked, nil
			}
			top := treeWith(t, "[]", map[string]string{tc.file: tc.body})
			paths := []string{tc.file, allowPath}
			sort.Strings(paths)
			got := runFixture(t, top, paths, tc.args, load)
			if got.code != tc.wantCode || got.stdout != tc.wantOut {
				t.Fatalf("got exit %d, stdout %q; want exit %d and %q (stderr: %s)", got.code, got.stdout, tc.wantCode, tc.wantOut, got.stderr)
			}
		})
	}
}

// TestRunDerivesTheForgeOwnerFromTheOriginURL covers the default run: the
// account it exempts is the first path element of the origin remote URL, in
// each form git writes that URL, and a work tree with no origin remote exempts
// no account while still reporting what it finds.
func TestRunDerivesTheForgeOwnerFromTheOriginURL(t *testing.T) {
	t.Parallel()
	account := shape("te", "am")
	file := "config/clone.yaml"
	cases := []struct {
		name     string
		remotes  []string
		origin   string
		wantCode int
		wantOut  string
	}{
		{name: "an https origin URL", remotes: []string{"origin"}, origin: shape("https://forge.invalid:3000/", account, "/gastown.git"), wantCode: exitClean},
		{name: "an ssh:// origin URL", remotes: []string{"origin"}, origin: shape("ssh://git@forge.invalid:22/", account, "/gastown.git"), wantCode: exitClean},
		{name: "an scp-like origin URL", remotes: []string{"origin"}, origin: shape("git@forge.invalid:", account, "/gastown.git"), wantCode: exitClean},
		{name: "a local path origin", remotes: []string{"origin"}, origin: shape("/", account, "/gastown.git"), wantCode: exitClean},
		{
			// The URL is there under another name, so a run that read it would
			// exempt the account: only the remote's name makes it the origin.
			name:     "no remote named origin",
			remotes:  []string{"upstream", "originx"},
			origin:   shape("https://forge.invalid:3000/", account, "/gastown.git"),
			wantCode: exitFindings,
			wantOut:  file + ":1 " + hygiene.RuleBlockedWord + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			top := treeWith(t, "[]", map[string]string{file: forgeURLLine(account)})
			git := fakeGit{top: top, paths: fixturePaths(file), remotes: tc.remotes, origin: tc.origin}
			got := runFixtureGit(t, top, git, nil, blockedWith(account), noTown)
			if got.code != tc.wantCode || got.stdout != tc.wantOut {
				t.Fatalf("got exit %d, stdout %q; want exit %d and %q (stderr: %s)", got.code, got.stdout, tc.wantCode, tc.wantOut, got.stderr)
			}
		})
	}
}

// TestRunDerivesTheTownRoot covers the default run's other identity: the town
// root the workspace finder reports is exempted, and a command run outside a
// town exempts no path while still reporting what it finds.
func TestRunDerivesTheTownRoot(t *testing.T) {
	t.Parallel()
	root := shape("/", "Users/", "fixtureaccount/gt")
	file := "config/mount.yaml"
	cases := []struct {
		name     string
		findTown townRootFinder
		wantCode int
		wantOut  string
	}{
		{name: "the town the finder reports is exempted", findTown: townAt(root), wantCode: exitClean},
		{
			name:     "outside a town no root is exempted",
			findTown: noTown,
			wantCode: exitFindings,
			wantOut:  file + ":1 " + hygiene.RuleHomePath + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			top := treeWith(t, "[]", map[string]string{file: townRootLine(root)})
			got := runFixtureWith(t, top, fixturePaths(file), nil, fixtureBlocked, tc.findTown)
			if got.code != tc.wantCode || got.stdout != tc.wantOut {
				t.Fatalf("got exit %d, stdout %q; want exit %d and %q (stderr: %s)", got.code, got.stdout, tc.wantCode, tc.wantOut, got.stderr)
			}
		})
	}
}

// townAt is a workspace finder reporting root.
func townAt(root string) townRootFinder {
	return func() (string, error) { return root, nil }
}

// TestRunForgeOwnerFlagOverridesTheDerivedValue covers the acceptance: the
// account -forge-owner names is the one exempted, and the account the origin
// URL names is not.
func TestRunForgeOwnerFlagOverridesTheDerivedValue(t *testing.T) {
	t.Parallel()
	derived := shape("te", "am")
	flagOwner := shape("oth", "er")
	file := "config/clone.yaml"
	cases := []struct {
		name     string
		account  string
		wantCode int
		wantOut  string
	}{
		{name: "the flag's account is exempted", account: flagOwner, wantCode: exitClean},
		{
			name:     "the derived account is not exempted",
			account:  derived,
			wantCode: exitFindings,
			wantOut:  file + ":1 " + hygiene.RuleBlockedWord + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			top := treeWith(t, "[]", map[string]string{file: forgeURLLine(tc.account)})
			git := fakeGit{
				top:     top,
				paths:   fixturePaths(file),
				remotes: []string{"origin"},
				origin:  shape("https://forge.invalid:3000/", derived, "/gastown.git"),
			}
			got := runFixtureGit(t, top, git, []string{"-forge-owner", flagOwner}, blockedWith(derived, flagOwner), noTown)
			if got.code != tc.wantCode || got.stdout != tc.wantOut {
				t.Fatalf("got exit %d, stdout %q; want exit %d and %q (stderr: %s)", got.code, got.stdout, tc.wantCode, tc.wantOut, got.stderr)
			}
		})
	}
}

// TestRunTownRootFlagOverridesTheDerivedValue covers the acceptance's other
// flag: the path -town-root names is the one exempted, and the path the
// workspace finder reports is not.
func TestRunTownRootFlagOverridesTheDerivedValue(t *testing.T) {
	t.Parallel()
	derived := shape("/", "Users/", "fixtureaccount/gt")
	flagRoot := shape("/", "srv/", "fixturetown")
	file := "config/mount.yaml"
	cases := []struct {
		name     string
		root     string
		wantCode int
		wantOut  string
	}{
		{name: "the flag's root is exempted", root: flagRoot, wantCode: exitClean},
		{
			name:     "the derived root is not exempted",
			root:     derived,
			wantCode: exitFindings,
			wantOut:  file + ":1 " + hygiene.RuleHomePath + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			top := treeWith(t, "[]", map[string]string{file: townRootLine(tc.root)})
			got := runFixtureWith(t, top, fixturePaths(file), []string{"-town-root", flagRoot}, fixtureBlocked, townAt(derived))
			if got.code != tc.wantCode || got.stdout != tc.wantOut {
				t.Fatalf("got exit %d, stdout %q; want exit %d and %q (stderr: %s)", got.code, got.stdout, tc.wantCode, tc.wantOut, got.stderr)
			}
		})
	}
}

// TestRunFlagsNeedNoDerivation covers the override's other half: with both
// flags given the run asks git for no origin and the finder for no town, so a
// git and a finder that would fail cannot fail the run.
func TestRunFlagsNeedNoDerivation(t *testing.T) {
	t.Parallel()
	account := shape("fixture", "account")
	root := shape("/", "srv/", "fixturetown")
	file := "config/clone.yaml"
	top := treeWith(t, "[]", map[string]string{file: forgeURLLine(account)})
	git := fakeGit{
		top:   top,
		paths: fixturePaths(file),
		fail:  map[string]error{"remote": errors.New("git failed")},
	}
	findTown := func() (string, error) { return "", errors.New("finder failed") }
	args := []string{"-forge-owner", account, "-town-root", root}
	got := runFixtureGit(t, top, git, args, blockedWith(account), findTown)
	if got.code != exitClean || got.stdout != "" || got.stderr != "" {
		t.Fatalf("got exit %d, stdout %q, stderr %q; want exit 0 and no output", got.code, got.stdout, got.stderr)
	}
}

// TestRunRefusesAnOriginURLItCannotRead covers the origin remote git lists but
// whose URL it cannot read: a look that failed exits 2 rather than scanning
// with no account exempted.
func TestRunRefusesAnOriginURLItCannotRead(t *testing.T) {
	t.Parallel()
	file := "config/clone.yaml"
	top := treeWith(t, "[]", map[string]string{file: forgeURLLine(shape("te", "am"))})
	git := fakeGit{top: top, paths: fixturePaths(file), remotes: []string{"origin"}, originErr: errors.New("git failed")}
	got := runFixtureGit(t, top, git, nil, fixtureBlocked, noTown)
	if got.code != exitError {
		t.Fatalf("exit code = %d, want %d; stderr: %s", got.code, exitError, got.stderr)
	}
	if !strings.Contains(got.stderr, "origin") {
		t.Errorf("stderr = %q, want it to name the origin URL it could not read", got.stderr)
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want nothing", got.stdout)
	}
}

// TestRunRefusesAnAccountThatIsNotAUsableName covers the account the scan would
// exempt that is not a name, whether it came from the origin URL or the flag:
// the run exits 2 and names -forge-owner, since the exemption is spliced into
// the lines it checks.
func TestRunRefusesAnAccountThatIsNotAUsableName(t *testing.T) {
	t.Parallel()
	file := "config/clone.yaml"
	cases := []struct {
		name   string
		origin string
		args   []string
	}{
		{name: "an account the origin URL names", origin: shape("https://forge.invalid:3000/../gastown", ".git")},
		{name: "an account the flag names", args: []string{"-forge-owner", shape("../", "gastown")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			top := treeWith(t, "[]", map[string]string{file: "clean\n"})
			git := fakeGit{top: top, paths: fixturePaths(file), remotes: []string{"origin"}, origin: tc.origin}
			got := runFixtureGit(t, top, git, tc.args, fixtureBlocked, noTown)
			if got.code != exitError {
				t.Fatalf("exit code = %d, want %d; stderr: %s", got.code, exitError, got.stderr)
			}
			if !strings.Contains(got.stderr, "-forge-owner") {
				t.Errorf("stderr = %q, want it to name -forge-owner", got.stderr)
			}
			if got.stdout != "" {
				t.Errorf("stdout = %q, want nothing", got.stdout)
			}
		})
	}
}

// TestRunRefusesAFailedTownLookup covers a workspace finder that cannot report
// the town: the run exits 2 rather than scanning as if it ran outside a town.
func TestRunRefusesAFailedTownLookup(t *testing.T) {
	t.Parallel()
	file := "config/mount.yaml"
	findTown := func() (string, error) { return "", errors.New("getting current directory") }
	top := treeWith(t, "[]", map[string]string{file: "clean\n"})
	got := runFixtureWith(t, top, fixturePaths(file), nil, fixtureBlocked, findTown)
	if got.code != exitError {
		t.Fatalf("exit code = %d, want %d; stderr: %s", got.code, exitError, got.stderr)
	}
	if !strings.Contains(got.stderr, "town root") {
		t.Errorf("stderr = %q, want it to name the town root", got.stderr)
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want nothing", got.stdout)
	}
}
