package deploysource

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// fakeGit is a scripted repository: each field answers one question the check
// asks, and the zero value is a merged, clean tree at HEAD abc1234 behind
// nothing. The real answers come from ExecGit, which the integration test
// runs against throwaway repositories.
type fakeGit struct {
	fetchErr error
	fetched  [][2]string

	head       string
	headErr    error
	resolved   bool
	resolveErr error

	ancestor     bool
	ancestorErr  error
	ancestorArgs [2]string

	modified    []string
	modifiedErr error

	behind    int
	behindErr error

	calls []string
}

func (f *fakeGit) Fetch(remote, branch string) error {
	f.calls = append(f.calls, "fetch")
	f.fetched = append(f.fetched, [2]string{remote, branch})
	return f.fetchErr
}

func (f *fakeGit) ShortHead() (string, error) {
	f.calls = append(f.calls, "head")
	head := f.head
	if head == "" {
		head = "abc1234"
	}
	return head, f.headErr
}

func (f *fakeGit) Resolve(ref string) (bool, error) {
	f.calls = append(f.calls, "resolve")
	return f.resolved, f.resolveErr
}

func (f *fakeGit) IsAncestor(commit, ref string) (bool, error) {
	f.calls = append(f.calls, "ancestor")
	f.ancestorArgs = [2]string{commit, ref}
	return f.ancestor, f.ancestorErr
}

func (f *fakeGit) ModifiedTracked() ([]string, error) {
	f.calls = append(f.calls, "modified")
	return f.modified, f.modifiedErr
}

func (f *fakeGit) CountBehind(ref string) (int, error) {
	f.calls = append(f.calls, "behind")
	return f.behind, f.behindErr
}

// merged is the fake of a tree that passes every rule: HEAD is an ancestor of
// origin/main and nothing tracked is modified.
func merged() *fakeGit {
	return &fakeGit{resolved: true, ancestor: true}
}

// runCheck runs the check against g and returns what it wrote and its result.
func runCheck(t *testing.T, g Git, opts Options) (string, error) {
	t.Helper()
	var w bytes.Buffer
	err := Run(g, opts, &w)
	return w.String(), err
}

// wantRefusal fails the test unless err is a refusal with this reason.
func wantRefusal(t *testing.T, err error, reason string) {
	t.Helper()
	var refusal *Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("Run = %v, want a *Refusal", err)
	}
	if refusal.Reason != reason {
		t.Errorf("refusal reason = %q, want %q", refusal.Reason, reason)
	}
}

// git's --is-ancestor answers true for an equal commit as well as for an
// ancestor, so both pass the merged check; the ancestor behind main gets its
// warning (TestBehindMainWarnsWithCount) and the equal commit none.
func TestCheckAllowsAncestorAndEqual(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		behind int
	}{
		{name: "HEAD equal to main", behind: 0},
		{name: "HEAD an ancestor of main", behind: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := merged()
			g.ancestor = true
			g.behind = tc.behind
			out, err := runCheck(t, g, Options{})
			if err != nil {
				t.Fatalf("Run = %v, want nil", err)
			}
			if tc.behind == 0 && out != "" {
				t.Errorf("output = %q, want empty", out)
			}
			if tc.behind > 0 && !strings.Contains(out, "behind origin/main") {
				t.Errorf("output = %q, want the behind warning", out)
			}
			if g.ancestorArgs != [2]string{"HEAD", DefaultMainRef} {
				t.Errorf("IsAncestor%v, want [HEAD %s]", g.ancestorArgs, DefaultMainRef)
			}
		})
	}
}

func TestCheckRefusesUnmergedHead(t *testing.T) {
	t.Parallel()
	g := merged()
	g.ancestor = false
	out, err := runCheck(t, g, Options{})
	wantRefusal(t, err, "HEAD abc1234 is not merged into origin/main")
	want := "ERROR: HEAD abc1234 is not merged into origin/main\n" +
		"  Deploy merged code only: build from an up-to-date main checkout.\n" +
		"  To install unmerged code anyway (operator hot-fix): ALLOW_UNMERGED=1\n"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

// The check asks git for every tracked modification and decides what blocks
// the build itself, so .beads state (which bd rewrites) and untracked files
// pass while one modified file elsewhere refuses.
func TestCheckDirtyTrackedRefusedButBeadsAndUntrackedAllowed(t *testing.T) {
	t.Parallel()
	reason := "tracked files outside .beads/ are modified; the build would embed unmerged changes"
	for _, tc := range []struct {
		name     string
		modified []string
		wantErr  bool
	}{
		{name: "no modification passes", modified: nil},
		{name: ".beads state passes", modified: []string{".beads/issues.jsonl"}},
		{name: "a tracked file refuses", modified: []string{"internal/cmd/gt.go"}, wantErr: true},
		{name: "one tracked file among .beads state refuses", modified: []string{".beads/issues.jsonl", "file"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := merged()
			g.modified = tc.modified
			out, err := runCheck(t, g, Options{})
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Run = %v, want nil", err)
				}
				if out != "" {
					t.Errorf("output = %q, want empty", out)
				}
				return
			}
			wantRefusal(t, err, reason)
			if !strings.Contains(out, "ALLOW_UNMERGED=1") {
				t.Errorf("refusal output = %q, want the ALLOW_UNMERGED hint", out)
			}
		})
	}
}

// ALLOW_UNMERGED is the operator hot-fix: it names HEAD and the ref on stderr
// and returns before asking git anything but HEAD's id.
func TestAllowUnmergedWarnsAndSkipsEveryCheck(t *testing.T) {
	t.Parallel()
	g := &fakeGit{
		ancestorErr: errors.New("merge-base must not run under ALLOW_UNMERGED"),
		resolveErr:  errors.New("rev-parse must not run under ALLOW_UNMERGED"),
		modifiedErr: errors.New("status must not run under ALLOW_UNMERGED"),
		behindErr:   errors.New("rev-list must not run under ALLOW_UNMERGED"),
	}
	out, err := runCheck(t, g, Options{AllowUnmerged: true})
	if err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	want := "WARNING: ALLOW_UNMERGED=1 — installing abc1234 without checking that it is merged to origin/main.\n" +
		"WARNING: every town session will run this UNMERGED build until the next install from main.\n"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	if got, wantCalls := strings.Join(g.calls, ","), "fetch,head"; got != wantCalls {
		t.Errorf("git calls = %q, want %q", got, wantCalls)
	}
}

// A failed fetch warns and checks against the local copy; it does not fail
// the install, because the offline copy may be exactly right.
func TestFetchFailureOnlyWarns(t *testing.T) {
	t.Parallel()
	g := merged()
	g.fetchErr = errors.New("could not resolve host")
	out, err := runCheck(t, g, Options{})
	if err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if want := "Warning: could not fetch origin/main; checking against the local copy\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	if got := strings.Join(g.calls, ","); got != "fetch,head,resolve,ancestor,modified,behind" {
		t.Errorf("git calls = %q; the check after a failed fetch must still run", got)
	}
}

// SKIP_UPDATE_CHECK skips the fetch and nothing else (gt-9jax).
func TestSkipUpdateCheckSkipsOnlyTheFetch(t *testing.T) {
	t.Parallel()
	g := merged()
	out, err := runCheck(t, g, Options{SkipUpdateCheck: true})
	if err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if out != "" {
		t.Errorf("output = %q, want empty", out)
	}
	if got := strings.Join(g.calls, ","); got != "head,resolve,ancestor,modified,behind" {
		t.Errorf("git calls = %q, want the fetch left out", got)
	}

	g = merged()
	g.ancestor = false
	_, err = runCheck(t, g, Options{SkipUpdateCheck: true})
	wantRefusal(t, err, "HEAD abc1234 is not merged into origin/main")
}

// The ref splits at its first slash: everything before is the remote and
// everything after is the branch, as the script's ${ref%%/*} and ${ref#*/} do.
func TestCheckMainRefSplitsAtTheFirstSlash(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mainRef         string
		wantRemote      string
		wantBranch      string
		wantAncestorRef string
	}{
		{mainRef: "", wantRemote: "origin", wantBranch: "main", wantAncestorRef: "origin/main"},
		{mainRef: "upstream/feature/main", wantRemote: "upstream", wantBranch: "feature/main", wantAncestorRef: "upstream/feature/main"},
		{mainRef: "main", wantRemote: "main", wantBranch: "main", wantAncestorRef: "main"},
	} {
		t.Run(tc.mainRef, func(t *testing.T) {
			t.Parallel()
			g := merged()
			_, err := runCheck(t, g, Options{MainRef: tc.mainRef})
			if err != nil {
				t.Fatalf("Run = %v, want nil", err)
			}
			if want := [][2]string{{tc.wantRemote, tc.wantBranch}}; len(g.fetched) != 1 || g.fetched[0] != want[0] {
				t.Errorf("fetched %v, want %v", g.fetched, want)
			}
			if g.ancestorArgs[1] != tc.wantAncestorRef {
				t.Errorf("merged check against %q, want %q", g.ancestorArgs[1], tc.wantAncestorRef)
			}
		})
	}
}

func TestBehindMainWarnsWithCount(t *testing.T) {
	t.Parallel()
	g := merged()
	g.behind = 3
	out, err := runCheck(t, g, Options{})
	if err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	want := "Warning: HEAD abc1234 is 3 commit(s) behind origin/main; installing an older main build.\n"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
}

func TestUnresolvableMainRefRefuses(t *testing.T) {
	t.Parallel()
	g := merged()
	g.resolved = false
	out, err := runCheck(t, g, Options{})
	wantRefusal(t, err, "cannot resolve origin/main, so cannot verify that abc1234 is merged")
	if !strings.Contains(out, "ERROR: cannot resolve origin/main") || !strings.Contains(out, "ALLOW_UNMERGED=1") {
		t.Errorf("output = %q, want the refusal and its hint", out)
	}
	if got := strings.Join(g.calls, ","); got != "fetch,head,resolve" {
		t.Errorf("git calls = %q, want the run ended at the unresolved ref", got)
	}
}

// A git question that could not be answered is an error, never a pass: the
// check must not report a tree as merged when it failed to look.
func TestCheckGitFailureIsNotAPass(t *testing.T) {
	t.Parallel()
	failure := errors.New("git failed")
	for _, tc := range []struct {
		name string
		g    *fakeGit
	}{
		{name: "HEAD unreadable", g: &fakeGit{headErr: failure}},
		{name: "ref unresolvable with an error", g: &fakeGit{resolved: true, resolveErr: failure}},
		{name: "ancestry unanswerable", g: &fakeGit{resolved: true, ancestor: true, ancestorErr: failure}},
		{name: "status unreadable", g: &fakeGit{resolved: true, ancestor: true, modifiedErr: failure}},
		{name: "behind unreadable", g: &fakeGit{resolved: true, ancestor: true, behindErr: failure}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := runCheck(t, tc.g, Options{})
			if err == nil {
				t.Fatal("Run = nil, want an error")
			}
			var refusal *Refusal
			if errors.As(err, &refusal) {
				t.Fatalf("Run = %v, want a check error rather than a refusal", err)
			}
			if !errors.Is(err, failure) {
				t.Errorf("Run = %v, want it to wrap the git failure", err)
			}
		})
	}
}

func TestOptionsFromEnv(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		env  map[string]string
		want Options
	}{
		{name: "nothing set", env: nil, want: Options{}},
		{name: "empty counts as unset", env: map[string]string{MainRefEnv: "", SkipUpdateCheckEnv: "", AllowUnmergedEnv: ""}, want: Options{}},
		{name: "every switch set", env: map[string]string{MainRefEnv: "upstream/main", SkipUpdateCheckEnv: "1", AllowUnmergedEnv: "1"}, want: Options{MainRef: "upstream/main", SkipUpdateCheck: true, AllowUnmerged: true}},
		{name: "the ref alone", env: map[string]string{MainRefEnv: "upstream/main"}, want: Options{MainRef: "upstream/main"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := OptionsFromEnv(func(key string) string { return tc.env[key] })
			if got != tc.want {
				t.Errorf("OptionsFromEnv = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// porcelainPaths reads the lines git status prints: the path follows the two
// status characters, and a rename names both sides.
func TestPorcelainPaths(t *testing.T) {
	t.Parallel()
	out := " M file\n" +
		" M .beads/issues.jsonl\n" +
		"R  old -> new\n" +
		"A  added\n" +
		"\n"
	want := []string{"file", ".beads/issues.jsonl", "old", "new", "added"}
	got := porcelainPaths(out)
	if len(got) != len(want) {
		t.Fatalf("porcelainPaths = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("porcelainPaths = %q, want %q", got, want)
		}
	}
}

// scripting is a Runner that answers every command the same way: ExecGit's
// branches differ only in the stdout and exit code git reports.
func scripting(out string, code int) Runner {
	return func(string, ...string) (string, int) { return out, code }
}

// ExecGit turns what git reports into the check's answers, and every failure
// to answer is an error rather than a quiet "no". The runner is a seam, so
// this drives the branches without starting a process.
func TestExecGitAnswersAndFailures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		run  Runner
		call func(ExecGit) (any, error)
		want any
		// wantErr is a substring the error must carry, "" for no error.
		wantErr string
	}{
		{name: "fetch exits 0", run: scripting("", 0), call: func(g ExecGit) (any, error) { return nil, g.Fetch("origin", "main") }},
		{name: "fetch names a failed exit", run: scripting("", 128), call: func(g ExecGit) (any, error) { return nil, g.Fetch("origin", "main") }, wantErr: "git fetch origin main --quiet: exit status 128"},
		{name: "fetch names a command that could not run", run: scripting("", -1), call: func(g ExecGit) (any, error) { return nil, g.Fetch("origin", "main") }, wantErr: "git fetch origin main --quiet"},
		{name: "HEAD is trimmed", run: scripting("abc1234\n", 0), call: func(g ExecGit) (any, error) { return g.ShortHead() }, want: "abc1234"},
		{name: "HEAD names a failed exit", run: scripting("", 128), call: func(g ExecGit) (any, error) { return g.ShortHead() }, wantErr: "git rev-parse --short HEAD: exit status 128"},
		{name: "HEAD names a command that could not run", run: scripting("", -1), call: func(g ExecGit) (any, error) { return g.ShortHead() }, wantErr: "git rev-parse --short HEAD"},
		{name: "a ref that resolves", run: scripting("", 0), call: func(g ExecGit) (any, error) { return g.Resolve("origin/main") }, want: true},
		{name: "exit 1 is an unknown ref", run: scripting("", 1), call: func(g ExecGit) (any, error) { return g.Resolve("origin/main") }, want: false},
		{name: "another exit is an error", run: scripting("", 128), call: func(g ExecGit) (any, error) { return g.Resolve("origin/main") }, want: false, wantErr: "exit status 128"},
		{name: "a command that could not run is an error", run: scripting("", -1), call: func(g ExecGit) (any, error) { return g.Resolve("origin/main") }, want: false, wantErr: "git rev-parse --verify --quiet origin/main"},
		{name: "an ancestor", run: scripting("", 0), call: func(g ExecGit) (any, error) { return g.IsAncestor("HEAD", "origin/main") }, want: true},
		{name: "exit 1 is not an ancestor", run: scripting("", 1), call: func(g ExecGit) (any, error) { return g.IsAncestor("HEAD", "origin/main") }, want: false},
		{name: "another exit is an error", run: scripting("", 128), call: func(g ExecGit) (any, error) { return g.IsAncestor("HEAD", "origin/main") }, want: false, wantErr: "git merge-base --is-ancestor HEAD origin/main: exit status 128"},
		{name: "a command that could not run is an error", run: scripting("", -1), call: func(g ExecGit) (any, error) { return g.IsAncestor("HEAD", "origin/main") }, want: false, wantErr: "merge-base"},
		{name: "status is parsed", run: scripting(" M file\n", 0), call: func(g ExecGit) (any, error) { return g.ModifiedTracked() }, want: []string{"file"}},
		{name: "status names a failed exit", run: scripting("", 128), call: func(g ExecGit) (any, error) { return g.ModifiedTracked() }, wantErr: "git status --porcelain --untracked-files=no -- .: exit status 128"},
		{name: "status names a command that could not run", run: scripting("", -1), call: func(g ExecGit) (any, error) { return g.ModifiedTracked() }, wantErr: "git status"},
		{name: "a count", run: scripting("3\n", 0), call: func(g ExecGit) (any, error) { return g.CountBehind("origin/main") }, want: 3},
		{name: "a count names a failed exit", run: scripting("", 128), call: func(g ExecGit) (any, error) { return g.CountBehind("origin/main") }, wantErr: "exit status 128"},
		{name: "a count that is not a number is an error", run: scripting("many\n", 0), call: func(g ExecGit) (any, error) { return g.CountBehind("origin/main") }, wantErr: "the count is not a number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.call(ExecGit{Dir: "repo", Run: tc.run})
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("call = (%v, nil), want an error containing %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("call = %v, want nil", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("call = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// The count error names the count and not what git printed, which the check
// must never copy into a message.
func TestCountBehindDoesNotEchoOutput(t *testing.T) {
	t.Parallel()
	_, err := ExecGit{Run: scripting("secret-token\n", 0)}.CountBehind("origin/main")
	if err == nil {
		t.Fatal("CountBehind = nil, want an error")
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Errorf("error = %q, want it to leave the captured output out", err)
	}
}
