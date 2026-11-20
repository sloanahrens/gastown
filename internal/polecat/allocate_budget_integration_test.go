//go:build integration

package polecat

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
)

// gitProcessRecorder is a git.ExecFunc: it appends one entry per git process the
// manager starts (dir and argv), then runs that process. A command it did not
// record is a command that did not run. runIn is where a call with no directory
// runs: production's bare-repo handle carries --git-dir and no cwd.
type gitProcessRecorder struct {
	runIn string
	mu    sync.Mutex
	calls []recordedGitCall
}

type recordedGitCall struct {
	dir  string
	argv []string
}

func (r *gitProcessRecorder) run(c git.ExecCall) (string, string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, recordedGitCall{c.Dir(), c.Args()})
	r.mu.Unlock()

	ctx := context.Background()
	if d := c.Timeout(); d > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, d)
		defer stop()
	}
	cmd := exec.CommandContext(ctx, "git", c.Args()...)
	if cmd.Dir = c.Dir(); cmd.Dir == "" {
		cmd.Dir = r.runIn
	}
	cmd.Env = os.Environ()
	for _, k := range c.Unset() {
		cmd.Env = slices.DeleteFunc(cmd.Env, func(kv string) bool { return strings.HasPrefix(kv, k+"=") })
	}
	cmd.Env = append(cmd.Env, c.Env()...)
	if s := c.Stdin(); s != "" {
		cmd.Stdin = strings.NewReader(s)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func (r *gitProcessRecorder) processCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *gitProcessRecorder) countDuring(fn func()) int {
	before := r.processCount()
	fn()
	return r.processCount() - before
}

// commandCount is one row of the pin: a git subcommand and how often it ran.
type commandCount struct {
	command string
	count   int
}

// counts groups the processes by subcommand, so an operand that varies per run
// (a drawn branch name) does not split a row.
func (r *gitProcessRecorder) counts() []commandCount {
	r.mu.Lock()
	defer r.mu.Unlock()
	by := map[string]int{}
	for _, c := range r.calls {
		by[gitCommand(c.argv)]++
	}
	out := make([]commandCount, 0, len(by))
	for command, count := range by {
		out = append(out, commandCount{command, count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].command < out[j].command })
	return out
}

func gitCommand(argv []string) string {
	for i := 0; i < len(argv); i++ {
		switch {
		case argv[i] == "-C" || argv[i] == "-c":
			i++
		case strings.HasPrefix(argv[i], "-"):
		default:
			return argv[i]
		}
	}
	return ""
}

// countMismatches reports every command whose count differs, a command the
// table does not list (count zero) included: the pin fails closed.
func countMismatches(table, got []commandCount) []string {
	byCommand := func(cs []commandCount) map[string]int {
		m := map[string]int{}
		for _, c := range cs {
			m[c.command] = c.count
		}
		return m
	}
	want, seen := byCommand(table), byCommand(got)
	var msgs []string
	for c, n := range want {
		if seen[c] != n {
			msgs = append(msgs, fmt.Sprintf("%s: ran %d, table says %d", c, seen[c], n))
		}
	}
	for c, n := range seen {
		if _, listed := want[c]; !listed && n > 0 {
			msgs = append(msgs, fmt.Sprintf("%s: ran %d, table says 0", c, n))
		}
	}
	sort.Strings(msgs)
	return msgs
}

// processesMismatch is the recorder test's check; the mutation rows feed it a
// wrong count, so the check itself is shown to fail.
func processesMismatch(call string, got, want int) string {
	if got != want {
		return fmt.Sprintf("%s ran %d git processes, want %d", call, got, want)
	}
	return ""
}

// TestIntegrationAllocateAndAddGitProcesses pins the git commands one
// AllocateAndAdd runs. The table states the counts once and its sum is the
// pinned total.
func TestIntegrationAllocateAndAddGitProcesses(t *testing.T) {
	m, rec := newAllocateFixture(t)
	if _, _, err := m.AllocateAndAdd(AddOptions{}); err != nil {
		t.Fatalf("AllocateAndAdd: %v", err)
	}
	table := []commandCount{
		{"cat-file", 1}, {"config", 4}, {"fetch", 1}, {"rev-parse", 4},
		{"show-ref", 1}, {"worktree", 2},
	}
	if msgs := countMismatches(table, rec.counts()); len(msgs) > 0 {
		t.Fatalf("allocate ran git commands the table does not pin:\n%s", strings.Join(msgs, "\n"))
	}
	total := 0
	for _, c := range table {
		total += c.count
	}
	if n := rec.processCount(); n != total {
		t.Fatalf("allocate ran %d git processes, table sums to %d", n, total)
	}
	for _, mut := range []struct {
		name string
		edit func([]commandCount) []commandCount
	}{
		{"a count raised by one", func(c []commandCount) []commandCount { c[0].count++; return c }},
		{"a count lowered by one", func(c []commandCount) []commandCount { c[0].count--; return c }},
		{"a command the table omits", func(c []commandCount) []commandCount { return c[1:] }},
	} {
		if countMismatches(mut.edit(append([]commandCount(nil), table...)), rec.counts()) == nil {
			t.Fatalf("%s was not reported", mut.name)
		}
	}
}

// TestIntegrationGitProcessRecorderSeesEveryProcess checks the recorder at the
// process boundary: a call that starts two git processes records two entries,
// a call that starts none records none.
func TestIntegrationGitProcessRecorderSeesEveryProcess(t *testing.T) {
	_, rec := newAllocateFixture(t)
	seat := filepath.Join(t.TempDir(), "seat")
	runGit(t, rec.runIn, "worktree", "add", "-q", "-b", "recbranch", seat, "origin/main")

	two := git.NewGitWithExec(seat, rec.run)
	if msg := processesMismatch("CommonDir", rec.countDuring(func() { _, _ = two.CommonDir() }), 2); msg != "" {
		t.Fatal(msg)
	}
	missing := git.NewGitWithExec(filepath.Join(t.TempDir(), "gone"), rec.run)
	if msg := processesMismatch("a missing directory's ConfigGet", rec.countDuring(func() { _, _ = missing.ConfigGet("x") }), 0); msg != "" {
		t.Fatal(msg)
	}
	// The same check rejects a count one off in either direction, so neither
	// assertion above is vacuous.
	if processesMismatch("none-call", 1, 0) == "" || processesMismatch("two-call", 1, 2) == "" {
		t.Fatal("processesMismatch accepted a count one off")
	}
}

// newAllocateFixture builds a rig whose .repo.git has origin/main and a Manager
// over an injected store, every git handle backed by a gitProcessRecorder.
func newAllocateFixture(t *testing.T) (*Manager, *gitProcessRecorder) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("chmod fixture root: %v", err)
	}
	work := filepath.Join(root, "work")
	runGit(t, root, "init", "-q", work)
	runGit(t, work, "symbolic-ref", "HEAD", "refs/heads/main")
	seedFile(t, filepath.Join(work, "README.md"), "fixture\n")
	runGit(t, work, "add", "README.md")
	runGit(t, work, "commit", "-qm", "fixture")
	origin := filepath.Join(root, "origin.git")
	runGit(t, root, "clone", "-q", "--bare", work, origin)

	rigPath := filepath.Join(root, "rig")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o700); err != nil {
		t.Fatalf("make rig beads dir: %v", err)
	}
	seedFile(t, filepath.Join(rigPath, ".beads", "config.yaml"), "")
	bare := constants.RigBareRepoPath(rigPath)
	runGit(t, root, "init", "-q", "--bare", bare)
	runGit(t, bare, "remote", "add", "origin", origin)
	runGit(t, bare, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	runGit(t, bare, "fetch", "-q", "origin")

	rec := &gitProcessRecorder{runIn: bare}
	db := newPolecatDB()
	m := NewManagerWithStore(&rig.Rig{Name: "rig", Path: rigPath, IdentityBeads: db}, nil, nil, nil, db)
	// The production opener builds *git.Git itself, so each handle source needs
	// a recorder-backed handle: Open through exec, OpenDir and RepoFile (which
	// carry no runner seam) with the recorder as their runner. The bare handle
	// keeps OpenDir's empty working directory and names its repository through
	// GIT_DIR, so its process list is the one production runs.
	m.gits = gitOpener{
		exec: rec.run,
		openDir: func(string, string) gitRepo {
			return git.NewGitWithExec("", rec.run).WithEnv([]string{"GIT_DIR=" + bare})
		},
		repoFile: func(path string) rig.RepoFileReader { return git.NewGitWithExec(path, rec.run) },
	}
	return m, rec
}
