package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/steveyegge/gastown/internal/hygiene"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// fixtureTop is the work tree top the fake git names. The files come from an
// in-memory tree, so the path never has to exist.
const fixtureTop = "/tree"

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
	paths []string
	fail  map[string]error
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
		return []byte(fixtureTop + "\n"), nil
	case "ls-files -z":
		if dir != fixtureTop {
			return nil, fmt.Errorf("fake git: ls-files ran in %q, want the work tree top %q", dir, fixtureTop)
		}
		var out strings.Builder
		for _, path := range g.paths {
			out.WriteString(path)
			out.WriteByte(0)
		}
		return []byte(out.String()), nil
	}
	return nil, fmt.Errorf("fake git does not model %v", args)
}

// fixedFS returns the in-memory tree whatever top git names: the command reads
// from the walk's top, and the fixture stands in for it.
func fixedFS(fsys fs.FS) workTreeFS {
	return func(string) fs.FS { return fsys }
}

// treeWith builds an in-memory work tree holding the allow list and files.
func treeWith(allowJSON string, files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{allowPath: &fstest.MapFile{Data: []byte(allowJSON)}}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

// result is one run's outcome.
type result struct {
	code   int
	stdout string
	stderr string
}

// runFixture runs the command over an in-memory tree and the fake git.
func runFixture(t *testing.T, fsys fs.FS, paths, args []string, blocked blockedLoader) result {
	t.Helper()
	var stdout, stderr strings.Builder
	code := run(args, &stdout, &stderr, fakeGit{paths: paths}.run, fixedFS(fsys), blocked)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// billingLine is a finding from the shape rules that names no person: a
// billing account id with a word after it, which the output must not repeat.
var billingLine = shape("charge ", "A1B2C3-", "D4E5F6-", "A1B2C3", " recorded")

// TestRunPrintsEachFindingAsALocationAndNeverItsText covers the lister's
// output contract: one path, line and rule per finding, none of the matched
// text, and exit 1.
func TestRunPrintsEachFindingAsALocationAndNeverItsText(t *testing.T) {
	t.Parallel()
	fsys := treeWith("[]", map[string]string{"docs/notes.md": "first line\n" + billingLine + "\n"})
	got := runFixture(t, fsys, []string{"docs/notes.md", allowPath}, nil, fixtureBlocked)
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
	fsys := treeWith("[]", map[string]string{"docs/notes.md": "nothing to report here\n"})
	got := runFixture(t, fsys, []string{"docs/notes.md", allowPath}, nil, fixtureBlocked)
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
	fsys := treeWith("[]", map[string]string{forbidden: "{}\n"})
	got := runFixture(t, fsys, []string{forbidden, allowPath}, nil, fixtureBlocked)
	if want := forbidden + " " + hygiene.RuleForbiddenDir + "\n"; got.code != exitFindings || got.stdout != want {
		t.Fatalf("got exit %d, stdout %q; want exit 1 and %q", got.code, got.stdout, want)
	}
}

// TestRunKeepsOnlyFindingsUnderEachDirPrefix covers -dir: every repeated
// prefix keeps the findings under it, and a prefix keeps a path inside it,
// never a sibling whose name only starts with the same letters.
func TestRunKeepsOnlyFindingsUnderEachDirPrefix(t *testing.T) {
	t.Parallel()
	fsys := treeWith("[]", map[string]string{
		"docs/c.md":             billingLine + "\n",
		"internal/daemon/a.go":  billingLine + "\n",
		"internal/daemonx/b.go": billingLine + "\n",
	})
	paths := []string{"docs/c.md", "internal/daemon/a.go", "internal/daemonx/b.go", allowPath}
	got := runFixture(t, fsys, paths, []string{"-dir", "internal/daemon", "-dir", "docs/"}, fixtureBlocked)
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
	fsys := treeWith("[]", map[string]string{"docs/notes.md": "clean\n"})
	got := runFixture(t, fsys, []string{"docs/notes.md", allowPath}, []string{"-dir", "internal/nope"}, fixtureBlocked)
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
		fsys  fs.FS
		paths []string
		want  string // a word the stderr must hold
	}{
		{
			name:  "a tracked file the tree cannot read",
			fsys:  treeWith("[]", nil),
			paths: []string{"docs/missing.md", allowPath},
			want:  "docs/missing.md",
		},
		{
			name:  "a missing allow list",
			fsys:  fstest.MapFS{"docs/notes.md": &fstest.MapFile{Data: []byte("clean\n")}},
			paths: []string{"docs/notes.md"},
			want:  allowPath,
		},
		{
			name:  "an allow list that is not a list of rows",
			fsys:  treeWith(`{"path":"docs/notes.md"}`, map[string]string{"docs/notes.md": "clean\n"}),
			paths: []string{"docs/notes.md", allowPath},
			want:  "allow rows",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := runFixture(t, tc.fsys, tc.paths, nil, fixtureBlocked)
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
	fsys := treeWith("[]", map[string]string{"docs/notes.md": "clean\n"})
	cases := []struct {
		name string
		git  gitRunner
	}{
		{"rev-parse fails", fakeGit{fail: map[string]error{"rev-parse": errors.New("git failed")}}.run},
		{"ls-files fails", fakeGit{paths: []string{"docs/notes.md"}, fail: map[string]error{"ls-files": errors.New("git failed")}}.run},
		{"rev-parse names no top", func(string, ...string) ([]byte, error) { return []byte("\n"), nil }},
		{"ls-files lists no file", fakeGit{}.run},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr strings.Builder
			code := run(nil, &stdout, &stderr, tc.git, fixedFS(fsys), fixtureBlocked)
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
	fsys := treeWith("[]", map[string]string{"docs/notes.md": "clean\n"})
	load := func() (map[string]struct{}, error) { return nil, errors.New("blocklist unreadable") }
	got := runFixture(t, fsys, []string{"docs/notes.md", allowPath}, nil, load)
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
			var stdout, stderr strings.Builder
			code := run(tc.args, &stdout, &stderr, fakeGit{}.run, fixedFS(treeWith("[]", nil)), fixtureBlocked)
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
	var stdout, stderr strings.Builder
	code := run([]string{"-h"}, &stdout, &stderr, fakeGit{}.run, fixedFS(treeWith("[]", nil)), fixtureBlocked)
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
	fsys := treeWith("[]", map[string]string{"docs/notes.md": billingLine + "\n"})
	var stderr strings.Builder
	code := run(nil, errWriter{}, &stderr, fakeGit{paths: []string{"docs/notes.md", allowPath}}.run, fixedFS(fsys), fixtureBlocked)
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
	fsys := treeWith(string(data), map[string]string{"docs/notes.md": line})
	got := runFixture(t, fsys, []string{"docs/notes.md", allowPath}, nil, fixtureBlocked)
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
			fsys := treeWith("[]", map[string]string{tc.file: tc.body})
			paths := []string{tc.file, allowPath}
			sort.Strings(paths)
			got := runFixture(t, fsys, paths, tc.args, load)
			if got.code != tc.wantCode || got.stdout != tc.wantOut {
				t.Fatalf("got exit %d, stdout %q; want exit %d and %q (stderr: %s)", got.code, got.stdout, tc.wantCode, tc.wantOut, got.stderr)
			}
		})
	}
}
