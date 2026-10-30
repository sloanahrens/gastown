package forgejocut

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejoresync"
)

// The shas the ref plan tests classify: shaMiddle is an ancestor of shaTip, and
// shaSide sits on neither line. tagObject is an annotated tag object that peels to
// shaSide.
const (
	shaMiddle = "1111111111111111111111111111111111111111"
	shaTip    = "2222222222222222222222222222222222222222"
	shaSide   = "3333333333333333333333333333333333333333"
	tagObject = "4444444444444444444444444444444444444444"
)

// scratchDir is the scratch repository the RefGit seam is bound to.
const scratchDir = "/srv/town/import.git"

// errRead is what the fake's refused read answers.
var errRead = errors.New("the scratch repository could not answer")

// ancestryOf answers true for the "a>b" pairs it is given and false for every
// other, so a test states the shared history as pairs.
func ancestryOf(pairs ...string) Ancestry {
	ancestors := make(map[string]bool, len(pairs))
	for _, p := range pairs {
		ancestors[p] = true
	}
	return func(a, b string) (bool, error) { return ancestors[a+">"+b], nil }
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
		github   string
		forgejo  string
		ancestry Ancestry
		want     Action
	}{
		{
			name:   "a branch at the same sha",
			github: shaMiddle, forgejo: shaMiddle, ancestry: behind, want: ActionSame,
		},
		{
			name:   "a tag at the same sha",
			github: shaMiddle, forgejo: shaMiddle, ancestry: behind, want: ActionSame,
		},
		{
			name:   "a branch Forgejo has no copy of",
			github: shaTip, ancestry: behind, want: ActionPush,
		},
		{
			name:   "a tag Forgejo has no copy of",
			github: shaTip, ancestry: behind, want: ActionPush,
		},
		{
			name:   "a branch Forgejo is behind",
			github: shaTip, forgejo: shaMiddle, ancestry: behind, want: ActionFastForward,
		},
		{
			name:   "a tag Forgejo is behind",
			github: shaTip, forgejo: shaMiddle, ancestry: behind, want: ActionFastForward,
		},
		{
			name:   "a branch Forgejo is ahead",
			github: shaMiddle, forgejo: shaTip, ancestry: behind, want: ActionKeep,
		},
		{
			name:   "a tag Forgejo is ahead",
			github: shaMiddle, forgejo: shaTip, ancestry: behind, want: ActionKeep,
		},
		{
			name:   "a branch that has diverged",
			github: shaTip, forgejo: shaSide, ancestry: behind, want: ActionDiverged,
		},
		{
			name:   "a tag that has diverged",
			github: shaTip, forgejo: shaSide, ancestry: behind, want: ActionDiverged,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Classify(tt.github, tt.forgejo, tt.ancestry)
			if err != nil {
				t.Fatalf("Classify(%s, %q): %v", tt.github, tt.forgejo, err)
			}
			if got != tt.want {
				t.Fatalf("Classify(%s, %q) = %d, want %d", tt.github, tt.forgejo, got, tt.want)
			}
		})
	}
}

// refGitFake is the RefGit seam: peeled answers the commit each sha names,
// ancestry answers the pairs it holds, and an ahead count the pairs it stages. A
// sha or pair it does not hold fails the test, so the plan reading the wrong one
// is caught rather than answered with a default. unpeelable names the shas Peel
// refuses, ancFail names the ancestry pairs whose read fails, and ancErr makes
// every ancestry read fail.
type refGitFake struct {
	t          *testing.T
	peeled     map[string]string
	unpeelable map[string]bool
	ancestors  map[string]bool
	ahead      map[string]string
	ancFail    map[string]error
	ancErr     error
}

func (f *refGitFake) Peel(sha string) (string, error) {
	f.t.Helper()
	if f.unpeelable[sha] {
		return "", errRead
	}
	commit, ok := f.peeled[sha]
	if !ok {
		f.t.Fatalf("Peel(%s): no commit staged for that sha", sha)
	}
	return commit, nil
}

func (f *refGitFake) IsAncestor(a, b string) (bool, error) {
	f.t.Helper()
	pair := a + ">" + b
	if err, ok := f.ancFail[pair]; ok {
		return false, err
	}
	if f.ancErr != nil {
		return false, f.ancErr
	}
	return f.ancestors[pair], nil
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
// own sort. No branch stages a peel, so peeling one fails the test.
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
		peeled:    map[string]string{shaSide: shaSide, shaMiddle: shaMiddle},
		ancestors: map[string]bool{shaMiddle + ">" + shaTip: true},
		ahead:     map[string]string{shaMiddle + ">" + shaTip: "2"},
	}
	want := []Decision{
		{
			Ref: "refs/heads/feature", Github: shaTip, Forgejo: shaMiddle,
			Action: ActionFastForward,
			Line:   "will import refs/heads/feature (fast-forward)",
		},
		{
			Ref: "refs/heads/main", Github: shaMiddle, Forgejo: shaTip,
			Action: ActionKeep,
			Line: "kept: refs/heads/main — Forgejo is ahead of GitHub by 2 commits (forgejo " +
				shaTip + ", github " + shaMiddle + ")",
		},
		{
			Ref: "refs/heads/stable", Github: shaMiddle, Forgejo: shaMiddle,
			Action: ActionSame,
			Line:   "up to date: refs/heads/stable is " + shaMiddle + " on both",
		},
		{
			Ref: "refs/tags/v1", Github: shaSide, Forgejo: shaMiddle,
			Action: ActionDiverged,
			Line: "diverged: refs/tags/v1 — Forgejo and GitHub have diverged (forgejo " +
				shaMiddle + ", github " + shaSide + ")",
		},
		{
			Ref: "refs/tags/v2", Github: shaTip, Forgejo: "",
			Action: ActionPush,
			Line:   "will import refs/tags/v2 (Forgejo has no copy)",
		},
	}
	got, err := Plan(github, forgejo, refs)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Plan =\n%+v\nwant\n%+v", got, want)
	}
}

// TestRefPlanPeelsAnAnnotatedTag pins that a tag is classified by the commit its
// tag object peels to: the two sides hold different tag objects naming shaSide, so
// the import leaves the tag alone and the line names the commit both sides hold.
// Comparing the listed shas instead classifies it diverged.
func TestRefPlanPeelsAnAnnotatedTag(t *testing.T) {
	t.Parallel()
	github := []forgejoresync.Ref{{Name: "refs/tags/v3", SHA: tagObject}}
	forgejo := []forgejoresync.Ref{{Name: "refs/tags/v3", SHA: shaTip}}
	refs := &refGitFake{
		t:      t,
		peeled: map[string]string{tagObject: shaSide, shaTip: shaSide},
	}
	got, err := Plan(github, forgejo, refs)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	want := []Decision{{
		Ref: "refs/tags/v3", Github: shaSide, Forgejo: shaSide,
		Action: ActionSame,
		Line:   "up to date: refs/tags/v3 is " + shaSide + " on both",
	}}
	if !slices.Equal(got, want) {
		t.Fatalf("Plan =\n%+v\nwant\n%+v", got, want)
	}
}

// TestRefPlanReportsAFailedRead pins that a read git could not answer stops the
// plan with an error, deciding no ref at all. A failed ancestry read reported as
// diverged would send the operator to reconcile a divergence that is not there.
func TestRefPlanReportsAFailedRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		github  []forgejoresync.Ref
		forgejo []forgejoresync.Ref
		refs    *refGitFake
	}{
		{
			name:    "a GitHub tag peel that fails",
			github:  []forgejoresync.Ref{{Name: "refs/tags/v1", SHA: tagObject}},
			forgejo: []forgejoresync.Ref{{Name: "refs/tags/v1", SHA: shaTip}},
			refs: &refGitFake{
				peeled:     map[string]string{shaTip: shaTip},
				unpeelable: map[string]bool{tagObject: true},
			},
		},
		{
			name:    "a Forgejo tag peel that fails",
			github:  []forgejoresync.Ref{{Name: "refs/tags/v1", SHA: tagObject}},
			forgejo: []forgejoresync.Ref{{Name: "refs/tags/v1", SHA: shaTip}},
			refs: &refGitFake{
				peeled:     map[string]string{tagObject: shaSide},
				unpeelable: map[string]bool{shaTip: true},
			},
		},
		{
			name:    "an ancestry read that fails",
			github:  []forgejoresync.Ref{{Name: "refs/heads/main", SHA: shaTip}},
			forgejo: []forgejoresync.Ref{{Name: "refs/heads/main", SHA: shaSide}},
			refs:    &refGitFake{ancErr: errRead},
		},
		{
			// The first read answers no, so the second is the one that fails.
			name:    "the second ancestry read that fails",
			github:  []forgejoresync.Ref{{Name: "refs/heads/main", SHA: shaTip}},
			forgejo: []forgejoresync.Ref{{Name: "refs/heads/main", SHA: shaSide}},
			refs:    &refGitFake{ancFail: map[string]error{shaTip + ">" + shaSide: errRead}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.refs.t = t
			plan, err := Plan(tt.github, tt.forgejo, tt.refs)
			if !errors.Is(err, errRead) {
				t.Fatalf("Plan error = %v, want %v", err, errRead)
			}
			if plan != nil {
				t.Fatalf("Plan returned %+v beside the error; a failed read decides no ref", plan)
			}
		})
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

// exitStatus is a child's exit status, matched through ExitCode() as
// *exec.ExitError is, so a unit test drives the branch without starting a process.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitStatus) ExitCode() int { return int(e) }

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

// TestIsAncestorUsesTheGitSeam drives the ancestry reads Classify runs through a
// fake runner: exit 1 is git's no, and every other failure is an error, so a read
// that never happened cannot pass for one that did.
func TestIsAncestorUsesTheGitSeam(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		a         string
		b         string
		err       error
		want      bool
		wantErr   bool
		wantCalls int
	}{
		{name: "an ancestor git confirms", a: shaMiddle, b: shaTip, want: true, wantCalls: 1},
		{name: "git answers no", a: shaTip, b: shaMiddle, err: exitStatus(1), want: false, wantCalls: 1},
		{name: "git fails to read", a: shaTip, b: shaMiddle, err: exitStatus(128), wantErr: true, wantCalls: 1},
		{name: "git never started", a: shaTip, b: shaMiddle, err: errors.New("exec: git: not found"), wantErr: true, wantCalls: 1},
		{name: "an option cannot ride in", a: "--upload-pack=touch", b: shaTip, wantErr: true},
		{name: "a short sha is not an object id", a: "abc123", b: shaTip, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run := &gitRunnerFake{err: tt.err}
			got, err := ExecRefGit{Dir: scratchDir, Run: run.run}.IsAncestor(tt.a, tt.b)
			if (err != nil) != tt.wantErr {
				t.Fatalf("IsAncestor(%q, %q) error = %v, wantErr %v", tt.a, tt.b, err, tt.wantErr)
			}
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

// TestPeelUsesTheGitSeam drives Peel through a fake runner: an annotated tag
// object peels to its commit, and a sha that is not an object id never reaches an
// argv.
func TestPeelUsesTheGitSeam(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		sha       string
		out       string
		err       error
		want      string
		wantErr   bool
		wantCalls int
	}{
		{name: "a commit names itself", sha: shaTip, out: shaTip + "\n", want: shaTip, wantCalls: 1},
		{name: "an annotated tag peels to its commit", sha: tagObject, out: shaSide + "\n", want: shaSide, wantCalls: 1},
		{name: "git fails", sha: tagObject, err: errExit, wantErr: true, wantCalls: 1},
		{name: "git prints no object id", sha: tagObject, out: "fatal: ambiguous argument\n", wantErr: true, wantCalls: 1},
		{name: "an option cannot ride in", sha: "--upload-pack=touch", wantErr: true},
		{name: "a short sha is not an object id", sha: "abc123", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			run := &gitRunnerFake{out: []byte(tt.out), err: tt.err}
			got, err := ExecRefGit{Dir: scratchDir, Run: run.run}.Peel(tt.sha)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Peel(%q) error = %v, wantErr %v", tt.sha, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("Peel(%q) = %q, want %q", tt.sha, got, tt.want)
			}
			if len(run.calls) != tt.wantCalls {
				t.Fatalf("Peel ran %q, want %d call(s)", run.calls, tt.wantCalls)
			}
			if tt.wantCalls == 0 {
				return
			}
			wantArgv := []string{"git", "-C", scratchDir, "rev-parse", "--verify", tt.sha + "^{commit}"}
			if !slices.Equal(run.calls[0], wantArgv) {
				t.Fatalf("Peel ran %q, want %q", run.calls[0], wantArgv)
			}
		})
	}
}
