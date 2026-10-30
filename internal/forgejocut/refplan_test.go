package forgejocut

import (
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejoresync"
)

// The shas the ref plan tests classify: shaMiddle is an ancestor of shaTip, and
// shaSide sits on neither line.
const (
	shaMiddle = "1111111111111111111111111111111111111111"
	shaTip    = "2222222222222222222222222222222222222222"
	shaSide   = "3333333333333333333333333333333333333333"
)

// scratchDir is the scratch repository the RefGit seam is bound to.
const scratchDir = "/srv/town/import.git"

// ancestryOf answers true for the "a>b" pairs it is given and false for every
// other, so a test states the shared history as pairs.
func ancestryOf(pairs ...string) func(a, b string) bool {
	ancestors := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		ancestors[p] = true
	}
	return func(a, b string) bool { return ancestors[a+">"+b] }
}

// TestClassifyTable covers each of the five outcomes for a branch and a tag.
// Swapping the fast-forward and keep branches of Classify turns four of these
// red.
func TestClassifyTable(t *testing.T) {
	t.Parallel()
	// shaMiddle is behind shaTip: the one pair of the fake repository's history
	// the cases need.
	behind := ancestryOf(shaMiddle + ">" + shaTip)
	tests := []struct {
		name     string
		ref      string
		github   string
		forgejo  string
		ancestry func(a, b string) bool
		want     Action
	}{
		{
			name: "a branch at the same sha", ref: "refs/heads/main",
			github: shaMiddle, forgejo: shaMiddle, ancestry: behind, want: ActionSame,
		},
		{
			name: "a tag at the same sha", ref: "refs/tags/v1",
			github: shaMiddle, forgejo: shaMiddle, ancestry: behind, want: ActionSame,
		},
		{
			name: "a branch Forgejo has no copy of", ref: "refs/heads/feature",
			github: shaTip, ancestry: behind, want: ActionPush,
		},
		{
			name: "a tag Forgejo has no copy of", ref: "refs/tags/v2",
			github: shaTip, ancestry: behind, want: ActionPush,
		},
		{
			name: "a branch Forgejo is behind", ref: "refs/heads/main",
			github: shaTip, forgejo: shaMiddle, ancestry: behind, want: ActionFastForward,
		},
		{
			name: "a tag Forgejo is behind", ref: "refs/tags/v1",
			github: shaTip, forgejo: shaMiddle, ancestry: behind, want: ActionFastForward,
		},
		{
			name: "a branch Forgejo is ahead", ref: "refs/heads/main",
			github: shaMiddle, forgejo: shaTip, ancestry: behind, want: ActionKeep,
		},
		{
			name: "a tag Forgejo is ahead", ref: "refs/tags/v1",
			github: shaMiddle, forgejo: shaTip, ancestry: behind, want: ActionKeep,
		},
		{
			name: "a branch that has diverged", ref: "refs/heads/main",
			github: shaTip, forgejo: shaSide, ancestry: behind, want: ActionDiverged,
		},
		{
			name: "a tag that has diverged", ref: "refs/tags/v1",
			github: shaTip, forgejo: shaSide, ancestry: behind, want: ActionDiverged,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Classify(tt.ref, tt.github, tt.forgejo, tt.ancestry); got != tt.want {
				t.Fatalf("Classify(%q, %s, %q) = %d, want %d", tt.ref, tt.github, tt.forgejo, got, tt.want)
			}
		})
	}
}

// refGitFake is the RefGit seam: ancestry answers the pairs it holds, and an ahead
// count the pairs it stages. A count it does not hold fails the test, so the plan
// reading the wrong pair is caught rather than answered with a default.
type refGitFake struct {
	t         *testing.T
	ancestors map[string]bool
	ahead     map[string]string
}

func (f *refGitFake) IsAncestor(a, b string) bool {
	f.t.Helper()
	return f.ancestors[a+">"+b]
}

func (f *refGitFake) CommitsAhead(a, b string) string {
	f.t.Helper()
	n, ok := f.ahead[a+">"+b]
	if !ok {
		f.t.Fatalf("CommitsAhead(%s, %s): no count staged for that pair", a, b)
	}
	return n
}

// TestRefPlanSortedAndLogged pins the order and the exact line of every outcome.
// The GitHub listing comes in unsorted, so the order asserted here is the plan's
// own sort.
func TestRefPlanSortedAndLogged(t *testing.T) {
	t.Parallel()
	github := []forgejoresync.Ref{
		{Name: "refs/tags/v2", SHA: shaTip},
		{Name: "refs/heads/main", SHA: shaMiddle},
		{Name: "refs/heads/feature", SHA: shaTip},
		{Name: "refs/heads/stable", SHA: shaMiddle},
		{Name: "refs/tags/v1", SHA: shaSide},
	}
	forgejo := []forgejoresync.Ref{
		{Name: "refs/heads/feature", SHA: shaMiddle},
		{Name: "refs/heads/main", SHA: shaTip},
		{Name: "refs/heads/stable", SHA: shaMiddle},
		{Name: "refs/heads/retired", SHA: shaSide},
		{Name: "refs/tags/v1", SHA: shaMiddle},
	}
	refs := &refGitFake{
		t:         t,
		ancestors: map[string]bool{shaMiddle + ">" + shaTip: true},
		ahead:     map[string]string{shaMiddle + ">" + shaTip: "2"},
	}
	want := []Decision{
		{
			Ref:     forgejoresync.Ref{Name: "refs/heads/feature", SHA: shaTip},
			Forgejo: shaMiddle,
			Action:  ActionFastForward,
			Line:    "will import refs/heads/feature (fast-forward)",
		},
		{
			Ref:     forgejoresync.Ref{Name: "refs/heads/main", SHA: shaMiddle},
			Forgejo: shaTip,
			Action:  ActionKeep,
			Line: "kept: refs/heads/main — Forgejo is ahead of GitHub by 2 commits (forgejo " +
				shaTip + ", github " + shaMiddle + ")",
		},
		{
			Ref:     forgejoresync.Ref{Name: "refs/heads/stable", SHA: shaMiddle},
			Forgejo: shaMiddle,
			Action:  ActionSame,
			Line:    "up to date: refs/heads/stable is " + shaMiddle + " on both",
		},
		{
			Ref:     forgejoresync.Ref{Name: "refs/tags/v1", SHA: shaSide},
			Forgejo: shaMiddle,
			Action:  ActionDiverged,
			Line: "diverged: refs/tags/v1 — Forgejo and GitHub have diverged (forgejo " +
				shaMiddle + ", github " + shaSide + ")",
		},
		{
			Ref:     forgejoresync.Ref{Name: "refs/tags/v2", SHA: shaTip},
			Forgejo: "",
			Action:  ActionPush,
			Line:    "will import refs/tags/v2 (Forgejo has no copy)",
		},
	}
	if got := Plan(github, forgejo, refs); !slices.Equal(got, want) {
		t.Fatalf("Plan =\n%+v\nwant\n%+v", got, want)
	}
}

// gitRunnerFake is the Runner seam: it records each argv and answers with the
// output and error it is given.
type gitRunnerFake struct {
	out   []byte
	err   error
	calls [][]string
}

func (f *gitRunnerFake) run(argv []string) ([]byte, error) {
	f.calls = append(f.calls, argv)
	return f.out, f.err
}

// TestCommitsAheadUsesTheGitSeam drives CommitsAhead through a fake runner: no
// process starts, and a sha that is not an object id never reaches an argv.
func TestCommitsAheadUsesTheGitSeam(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		a         string
		b         string
		out       string
		err       error
		want      string
		wantCalls int
	}{
		{name: "git answers", a: shaMiddle, b: shaTip, out: "2\n", want: "2", wantCalls: 1},
		{name: "no trailing newline", a: shaMiddle, b: shaTip, out: "0", want: "0", wantCalls: 1},
		{name: "git fails", a: shaMiddle, b: shaTip, err: errExit, want: "?", wantCalls: 1},
		{name: "an option cannot ride in", a: "--upload-pack=touch", b: shaTip, want: "?"},
		{name: "a short sha is not an object id", a: "abc123", b: shaTip, want: "?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run := &gitRunnerFake{out: []byte(tt.out), err: tt.err}
			got := ExecRefGit{Dir: scratchDir, Run: run.run}.CommitsAhead(tt.a, tt.b)
			if got != tt.want {
				t.Fatalf("CommitsAhead(%q, %q) = %q, want %q", tt.a, tt.b, got, tt.want)
			}
			if len(run.calls) != tt.wantCalls {
				t.Fatalf("CommitsAhead ran %q, want %d call(s)", run.calls, tt.wantCalls)
			}
			if tt.wantCalls == 0 {
				return
			}
			want := []string{"git", "-C", scratchDir, "rev-list", "--count", tt.a + ".." + tt.b}
			if !slices.Equal(run.calls[0], want) {
				t.Fatalf("CommitsAhead ran %q, want %q", run.calls[0], want)
			}
		})
	}
}

// TestClassifyAncestryUsesTheGitSeam drives the ancestry CommitsAhead's sibling
// answers Classify through: a fake runner, so no process starts.
func TestClassifyAncestryUsesTheGitSeam(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		a         string
		b         string
		err       error
		want      bool
		wantCalls int
	}{
		{name: "an ancestor git confirms", a: shaMiddle, b: shaTip, want: true, wantCalls: 1},
		{name: "git says no", a: shaTip, b: shaMiddle, err: errExit, want: false, wantCalls: 1},
		{name: "an option cannot ride in", a: "--upload-pack=touch", b: shaTip, want: false},
		{name: "a short sha is not an object id", a: "abc123", b: shaTip, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run := &gitRunnerFake{err: tt.err}
			got := ExecRefGit{Dir: scratchDir, Run: run.run}.IsAncestor(tt.a, tt.b)
			if got != tt.want {
				t.Fatalf("IsAncestor(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
			if len(run.calls) != tt.wantCalls {
				t.Fatalf("IsAncestor ran %q, want %d call(s)", run.calls, tt.wantCalls)
			}
			if tt.wantCalls == 0 {
				return
			}
			want := []string{"git", "-C", scratchDir, "merge-base", "--is-ancestor", tt.a, tt.b}
			if !slices.Equal(run.calls[0], want) {
				t.Fatalf("IsAncestor ran %q, want %q", run.calls[0], want)
			}
		})
	}
}
