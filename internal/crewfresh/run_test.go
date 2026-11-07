package crewfresh

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeGit answers Run's questions from fields a case sets, and records every
// call so a test can tell which ones were made.
type fakeGit struct {
	fetchErr    error
	branch      string
	branchErr   error
	clean       bool
	cleanErr    error
	behind      int
	behindErr   error
	ancestor    bool
	ancestorErr error
	ffErr       error

	calls       []string
	fetchLimits []time.Duration
	ffCalls     int
}

func (f *fakeGit) Fetch(timeout time.Duration) error {
	f.calls = append(f.calls, "Fetch")
	f.fetchLimits = append(f.fetchLimits, timeout)
	return f.fetchErr
}

func (f *fakeGit) Branch() (string, error) {
	f.calls = append(f.calls, "Branch")
	return f.branch, f.branchErr
}

func (f *fakeGit) Clean() (bool, error) {
	f.calls = append(f.calls, "Clean")
	return f.clean, f.cleanErr
}

func (f *fakeGit) Behind() (int, error) {
	f.calls = append(f.calls, "Behind")
	return f.behind, f.behindErr
}

func (f *fakeGit) HeadIsAncestorOfOriginMain() (bool, error) {
	f.calls = append(f.calls, "HeadIsAncestorOfOriginMain")
	return f.ancestor, f.ancestorErr
}

func (f *fakeGit) FastForwardMain() error {
	f.calls = append(f.calls, "FastForwardMain")
	f.ffCalls++
	return f.ffErr
}

// fakeGitFor is a clone in state s, with the fetch and the two reads that can
// fail failing exactly when s says so.
func fakeGitFor(s State) *fakeGit {
	g := &fakeGit{
		branch:   s.Branch,
		clean:    s.Clean,
		behind:   s.Behind,
		ancestor: s.HeadIsAncestor,
	}
	if !s.FetchOK {
		g.fetchErr = errors.New("could not reach origin")
	}
	if s.CleanUnreadable {
		g.cleanErr = errors.New("status failed")
	}
	if s.AncestryUnreadable {
		g.ancestorErr = errors.New("merge-base failed")
	}
	return g
}

// A fetch that fails is reported, not returned: the clone is still read, the
// numbers are the last fetch's, and nothing moves (gt-0gscf).
func TestRunFetchFailureStillReportsAndReturnsNoError(t *testing.T) {
	t.Parallel()
	g := fakeGitFor(State{FetchOK: false, Branch: mainBranch, Clean: true, Behind: 3, HeadIsAncestor: true})

	got, err := Run(g, 30*time.Second)
	if err != nil {
		t.Fatalf("Run with a failed fetch = error %v, want nil", err)
	}
	if want := "fetch failed so the numbers are as of the last fetch"; got.Message != want {
		t.Errorf("Report.Message = %q, want %q", got.Message, want)
	}
	if got.Action != ActionNone {
		t.Errorf("Report.Action = %d, want ActionNone", got.Action)
	}
	if g.ffCalls != 0 {
		t.Errorf("FastForwardMain called %d times, want 0", g.ffCalls)
	}
	// The rest of the clone is still read, so the report carries its numbers
	// and branch.
	wantCalls := []string{"Fetch", "Branch", "Clean", "Behind", "HeadIsAncestorOfOriginMain"}
	if !slices.Equal(g.calls, wantCalls) {
		t.Errorf("calls = %v, want %v", g.calls, wantCalls)
	}
	if !slices.Equal(g.fetchLimits, []time.Duration{30 * time.Second}) {
		t.Errorf("Fetch timeouts = %v, want [30s]", g.fetchLimits)
	}
}

// FastForwardMain is called exactly once, and only for the one row the table
// allows it on; every row's report is the one the table expects (gt-0gscf).
func TestRunFastForwardsExactlyOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range decideCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g := fakeGitFor(tc.state)

			got, err := Run(g, time.Minute)
			if err != nil {
				t.Fatalf("Run(%+v) = error %v, want nil", tc.state, err)
			}
			wantFF := 0
			if tc.wantAction == ActionFastForward {
				wantFF = 1
			}
			if g.ffCalls != wantFF {
				t.Errorf("FastForwardMain called %d times, want %d", g.ffCalls, wantFF)
			}
			want := Report{Action: tc.wantAction, Behind: tc.wantBehind, Message: tc.wantMessage}
			if got != want {
				t.Errorf("Run(%+v) = %+v, want %+v", tc.state, got, want)
			}
		})
	}
}

// A clone whose branch or commit count cannot be read is reported as an error,
// and Run asks no further questions of it (gt-0gscf).
func TestRunUnreadableCloneReturnsError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		g         *fakeGit
		wantCalls []string
		wantCause string
	}{
		{
			name:      "branch unreadable",
			g:         &fakeGit{branchErr: errors.New("HEAD is unreadable")},
			wantCalls: []string{"Fetch", "Branch"},
			wantCause: "HEAD is unreadable",
		},
		{
			name:      "commits behind unreadable",
			g:         &fakeGit{branch: mainBranch, clean: true, behindErr: errors.New("origin/main is not a commit")},
			wantCalls: []string{"Fetch", "Branch", "Clean", "Behind"},
			wantCause: "origin/main is not a commit",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Run(tc.g, time.Minute)
			if err == nil {
				t.Fatal("Run = nil error, want an error")
			}
			if !strings.Contains(err.Error(), tc.wantCause) {
				t.Errorf("Run error = %q, want it to name %q", err, tc.wantCause)
			}
			if !slices.Equal(tc.g.calls, tc.wantCalls) {
				t.Errorf("calls = %v, want %v", tc.g.calls, tc.wantCalls)
			}
			if tc.g.ffCalls != 0 {
				t.Errorf("FastForwardMain called %d times, want 0", tc.g.ffCalls)
			}
		})
	}
}

// A fast-forward git refuses is reported as a failed move and an error, so the
// caller never reads the report as a clone that moved (gt-0gscf).
func TestRunFailedFastForwardIsReportedAndReturned(t *testing.T) {
	t.Parallel()
	g := fakeGitFor(State{FetchOK: true, Branch: mainBranch, Clean: true, Behind: 4, HeadIsAncestor: true})
	g.ffErr = errors.New("index is locked")

	got, err := Run(g, time.Minute)
	if err == nil {
		t.Fatal("Run with a refused fast-forward = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "index is locked") {
		t.Errorf("Run error = %q, want it to name the fast-forward failure", err)
	}
	if got.Action != ActionNone {
		t.Errorf("Report.Action = %d, want ActionNone", got.Action)
	}
	if want := "behind 4 but the fast-forward failed so main did not move"; got.Message != want {
		t.Errorf("Report.Message = %q, want %q", got.Message, want)
	}
}

// A clone whose changes or ancestry cannot be read is left where it is; the
// report names the read that failed, unless an earlier check already explains
// the block (gt-0gscf).
func TestRunUnreadableReadBlocksTheMoveAndNamesTheBlocker(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		g           *fakeGit
		wantMessage string
	}{
		{
			name: "changes unreadable",
			g: &fakeGit{
				branch: mainBranch, clean: true, cleanErr: errors.New("status failed"),
				behind: 3, ancestor: true,
			},
			wantMessage: "behind 3 but the clone's changes could not be read so nothing moved",
		},
		{
			name: "ancestry unreadable",
			g: &fakeGit{
				branch: mainBranch, clean: true, behind: 3,
				ancestor: true, ancestorErr: errors.New("merge-base failed"),
			},
			wantMessage: "behind 3 but main's ancestry could not be read so nothing moved",
		},
		{
			name: "changes unreadable on another branch keeps the branch line",
			g: &fakeGit{
				branch: "crew/operator/gt-9", cleanErr: errors.New("status failed"),
				behind: 3, ancestor: true,
			},
			wantMessage: "behind 3 but on branch crew/operator/gt-9 so main is left alone",
		},
		{
			name: "dirty clone keeps the changes line when ancestry is unreadable",
			g: &fakeGit{
				branch: mainBranch, clean: false, behind: 3,
				ancestor: true, ancestorErr: errors.New("merge-base failed"),
			},
			wantMessage: "behind 3 but the clone has local changes so nothing moved",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Run(tc.g, time.Minute)
			if err != nil {
				t.Fatalf("Run = error %v, want nil", err)
			}
			if got.Action != ActionNone {
				t.Errorf("Report.Action = %d, want ActionNone", got.Action)
			}
			if got.Message != tc.wantMessage {
				t.Errorf("Report.Message = %q, want %q", got.Message, tc.wantMessage)
			}
			if tc.g.ffCalls != 0 {
				t.Errorf("FastForwardMain called %d times, want 0", tc.g.ffCalls)
			}
		})
	}
}
