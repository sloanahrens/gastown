package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
)

// failedPreserve is a preserve step whose git cannot run at all: no remote, no
// repository, nothing pushed.
type failedPreserve struct{ err error }

func (f failedPreserve) PreserveBranch(string, string, string) (git.PreserveResult, error) {
	return git.PreserveResult{}, f.err
}

// A preserve that could not put the tip on the remote is the case the nuke
// blocks on, and git's sentinel is how it is recognised: the wrapper keeps the
// sentinel and the cause, so the operator sees both the verdict and what git
// said.
func TestPreserveBranchBeforeNukeReportsAPushThatDidNotPreserve(t *testing.T) {
	t.Parallel()
	cause := fmt.Errorf("%w: push rejected", git.ErrBranchNotPreserved)

	_, err := preserveBranchBeforeNuke(failedPreserve{err: cause}, "polecat/flint/gt-1", "origin", t.TempDir())
	if !errors.Is(err, git.ErrBranchNotPreserved) {
		t.Fatalf("err = %v, want it to wrap git.ErrBranchNotPreserved", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want it to keep the cause in the chain", err)
	}
}

// A preserve that failed before it could push says nothing about the remote.
// The nuke must still stop — anything but a confirmed push does — but the error
// must not claim the branch is absent from the remote when no push happened.
func TestPreserveBranchBeforeNukeKeepsAnErrorFromBeforeThePush(t *testing.T) {
	t.Parallel()
	cause := errors.New("git rev-parse: fatal: not a git repository")

	_, err := preserveBranchBeforeNuke(failedPreserve{err: cause}, "polecat/flint/gt-1", "origin", t.TempDir())
	if err == nil {
		t.Fatal("err = nil, want the preserve's failure: the nuke must not proceed")
	}
	if errors.Is(err, git.ErrBranchNotPreserved) {
		t.Fatalf("err = %v, want it NOT to claim the branch is not preserved on the remote", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want it to keep the cause in the chain", err)
	}
}

func TestPreserveFailureBlockerRequiresForceAndAcknowledgement(t *testing.T) {
	t.Parallel()
	cause := fmt.Errorf("%w: push rejected", git.ErrBranchNotPreserved)

	cases := []struct {
		name        string
		cause       error
		force, ack  bool
		wantBlocked bool
	}{
		{"preserved branch never blocks", nil, false, false, false},
		{"unpreserved, no override", cause, false, false, true},
		{"unpreserved, --force alone", cause, true, false, true},
		{"unpreserved, acknowledgement alone", cause, false, true, true},
		{"unpreserved, --force plus acknowledgement", cause, true, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := preserveFailureBlocker("gastown", "flint", tc.force, tc.ack, tc.cause)
			if (err != nil) != tc.wantBlocked {
				t.Fatalf("blocked = %v (err %v), want %v", err != nil, err, tc.wantBlocked)
			}
			if err == nil {
				return
			}
			// The operator has to be able to see both what happened and how to
			// get out of it, without re-reading the source.
			for _, want := range []string{NukeAcknowledgeUnpreservedFlag, "NOT deleted"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q must mention %q", err, want)
				}
			}
		})
	}
}

// preserveBranch is the branch the nuke fixture's polecat reports: its seat is
// a plain directory, so the Manager falls back to the derived name.
const preserveBranch = "polecat/basalt"

// preserveDeps is the nuke's deps for a preserve-step test: the process
// starters are faked, the seat question is the case's, and the progress report
// is captured instead of written to the process's stdout.
func preserveDeps(f *nukeE2E, seat nukeSeatOrigin, preserve func(string) (preserveOutcome, error)) (*nukeDeps, *bytes.Buffer) {
	out := &bytes.Buffer{}
	return &nukeDeps{
		Sessions: &noSession{},
		Releaser: &recordingReleaser{
			fakeWorkReleaser: &fakeWorkReleaser{
				beads: map[string][2]string{nukeE2EBead: {"hooked", nukeE2EAgent}},
			},
			sandboxGone: func() bool { return false },
		},
		SurvivingWork: func(string) (string, error) { return "", nil },
		MoleculeBeads: f.store,
		SeatOrigin:    seat,
		Preserve:      preserve,
		Out:           out,
	}, out
}

// runPreserveNuke drives the production nuke over the fixture with the case's
// deps.
func runPreserveNuke(f *nukeE2E, deps *nukeDeps) error {
	r := &rig.Rig{Name: nukeE2ERig, Path: f.rigPath}
	mgr := polecat.NewManagerWithStore(r, nil, nil, townRegistry(), f.store)
	return nukePolecatFullWithOptions(nukeE2EPolecat, nukeE2ERig, mgr, r, nukePolecatOptions{Deps: deps})
}

// A sandbox seat's work is pushed from inside its sandbox, so the host's local
// ref of its branch is the spawn base. The preserve step must report origin's
// tip and must not run the host push (gt-wz5ct4).
func TestNukePreserveForASandboxSeatReadsTheOriginTipNotTheHostRef(t *testing.T) {
	t.Parallel()
	f := newNukeE2E(t)
	hostPreserveRuns := 0
	deps, out := preserveDeps(f,
		func(branch string) (string, bool, error) {
			if branch != preserveBranch {
				t.Errorf("seat question for branch %q, want %q", branch, preserveBranch)
			}
			return "812bd897f00d3b4a", true, nil
		},
		func(string) (preserveOutcome, error) {
			hostPreserveRuns++
			return preserveOutcome{RemoteRef: "origin/main", AlreadyThere: true}, nil
		},
	)
	if err := runPreserveNuke(f, deps); err != nil {
		t.Fatalf("nuke returned %v, want nil", err)
	}
	if hostPreserveRuns != 0 {
		t.Errorf("host preserve ran %d time(s), want none: a sandbox seat's host ref is not its work", hostPreserveRuns)
	}
	report := out.String()
	if !strings.Contains(report, "the seat's branch is on origin at 812bd89") {
		t.Errorf("report does not name origin's branch and tip:\n%s", report)
	}
	if !strings.Contains(report, "the host's local ref is a placeholder") {
		t.Errorf("report does not say the host ref is a placeholder:\n%s", report)
	}
	if strings.Contains(report, "reachable from origin/main") || strings.Contains(report, "nothing to preserve") {
		t.Errorf("report claims the host ref was preserved:\n%s", report)
	}
}

// When origin does not hold a sandbox seat's branch, the nuke has no remote
// copy of the work. It must say so, and never print the host path's "nothing
// to preserve", which is the line this bead exists to remove (gt-wz5ct4).
func TestNukePreserveForASandboxSeatWithNoOriginBranchDoesNotSayNothingToPreserve(t *testing.T) {
	t.Parallel()
	f := newNukeE2E(t)
	hostPreserveRuns := 0
	deps, out := preserveDeps(f,
		func(string) (string, bool, error) { return "", true, nil },
		func(string) (preserveOutcome, error) {
			hostPreserveRuns++
			return preserveOutcome{}, nil
		},
	)
	if err := runPreserveNuke(f, deps); err != nil {
		t.Fatalf("nuke returned %v, want nil", err)
	}
	if hostPreserveRuns != 0 {
		t.Errorf("host preserve ran %d time(s), want none", hostPreserveRuns)
	}
	report := out.String()
	if strings.Contains(report, "nothing to preserve") {
		t.Errorf("report says nothing to preserve for a sandbox seat whose branch origin lacks:\n%s", report)
	}
	if !strings.Contains(report, "the seat's work is not on origin") {
		t.Errorf("report does not say the work is not on origin:\n%s", report)
	}
}

// A host seat's local ref is its work, and today's push path stays.
func TestNukePreserveForAHostSeatIsUnchanged(t *testing.T) {
	t.Parallel()
	f := newNukeE2E(t)
	hostPreserveRuns := 0
	deps, out := preserveDeps(f, nukeE2EHostSeat,
		func(branch string) (preserveOutcome, error) {
			hostPreserveRuns++
			if branch != preserveBranch {
				t.Errorf("host preserve for branch %q, want %q", branch, preserveBranch)
			}
			return preserveOutcome{RemoteRef: "origin/main", AlreadyThere: true}, nil
		},
	)
	if err := runPreserveNuke(f, deps); err != nil {
		t.Fatalf("nuke returned %v, want nil", err)
	}
	if hostPreserveRuns != 1 {
		t.Errorf("host preserve ran %d time(s), want once", hostPreserveRuns)
	}
	if report := out.String(); !strings.Contains(report, "already reachable from origin/main — nothing to preserve") {
		t.Errorf("host seat report lost its preserve line:\n%s", report)
	}
}

// A sandbox seat's branch on origin is not merged work: Step 4 must report the
// ref the landing worker takes, never the wording alreadyOnRemote carries for
// work already on the target branch (overseer decision, gt-0wyoc0).
func TestNukePreserveForASandboxSeatOnOriginIsNotReportedAsMerged(t *testing.T) {
	t.Parallel()
	f := newNukeE2E(t)
	deps, out := preserveDeps(f,
		func(string) (string, bool, error) { return "812bd897f00d3b4a", true, nil },
		func(string) (preserveOutcome, error) {
			t.Error("host preserve ran for a sandbox seat")
			return preserveOutcome{}, nil
		},
	)
	if err := runPreserveNuke(f, deps); err != nil {
		t.Fatalf("nuke returned %v, want nil", err)
	}
	report := out.String()
	if !strings.Contains(report, preserveBranch+" is on origin at 812bd89; the landing worker takes it from there") {
		t.Errorf("Step 4 does not report the origin ref as waiting on the landing worker:\n%s", report)
	}
	if strings.Contains(report, "already on origin/"+preserveBranch) || strings.Contains(report, "work already on") {
		t.Errorf("Step 4 reports an unmerged sandbox branch as merged:\n%s", report)
	}
}

// An agent bead that cannot be read leaves the seat's kind unknown, which
// cannot be shown to be a host seat: reading its host ref is the false
// "nothing to preserve" this bead removes, so the nuke refuses and names the
// read and the override (overseer decision, gt-0wyoc0).
func TestNukePreserveForASeatWithAnUnreadableAgentBeadRefusesAndNamesTheReason(t *testing.T) {
	t.Parallel()
	f := newNukeE2E(t)
	seat := nukeSeatOriginFromReads(
		polecatGitStateReads{
			agent: func() (string, error) {
				return "", errors.New("bd show gt-gastown-polecat-basalt: connection refused")
			},
			probe: func(string, string) (polecat.LiveGitState, bool) { return polecat.LiveGitState{}, false },
		},
		func(string) (string, error) {
			t.Error("origin was read for a seat whose kind is unknown")
			return "", nil
		},
		"gastown-basalt",
	)
	hostPreserveRuns := 0
	deps, _ := preserveDeps(f, seat, func(string) (preserveOutcome, error) {
		hostPreserveRuns++
		return preserveOutcome{}, nil
	})
	err := runPreserveNuke(f, deps)
	if err == nil || !strings.Contains(err.Error(), "refusing to nuke") {
		t.Fatalf("nuke with an unreadable agent bead = %v, want the preserve refusal", err)
	}
	for _, want := range []string{"agent bead", "gt-gastown-polecat-basalt", NukeAcknowledgeUnpreservedFlag} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q:\n%v", want, err)
		}
	}
	if hostPreserveRuns != 0 {
		t.Errorf("host preserve ran %d time(s), want none", hostPreserveRuns)
	}
	if _, statErr := os.Stat(f.polecatAt); statErr != nil {
		t.Errorf("polecat sandbox gone after a refused nuke: %v", statErr)
	}
}

// A known sandbox seat whose origin ref cannot be read fails closed the same
// way: nothing shows where its work is.
func TestNukePreserveForASandboxSeatWithAnUnreadableOriginRefusesTheNuke(t *testing.T) {
	t.Parallel()
	f := newNukeE2E(t)
	seat := nukeSeatOriginFromReads(
		polecatGitStateReads{
			agent: func() (string, error) { return nukeE2EAgent, nil },
			probe: func(string, string) (polecat.LiveGitState, bool) { return polecat.LiveGitState{}, true },
		},
		func(string) (string, error) { return "", errors.New("ls-remote: could not resolve host") },
		"gastown-basalt",
	)
	deps, _ := preserveDeps(f, seat, func(string) (preserveOutcome, error) {
		t.Error("host preserve ran for a sandbox seat")
		return preserveOutcome{}, nil
	})
	err := runPreserveNuke(f, deps)
	if err == nil || !strings.Contains(err.Error(), "refusing to nuke") {
		t.Fatalf("nuke with an unreadable origin = %v, want the preserve refusal", err)
	}
	if !strings.Contains(err.Error(), "refs/heads/"+preserveBranch) {
		t.Errorf("refusal does not name the origin ref it could not read:\n%v", err)
	}
}

// A missing agent bead is a host seat, as checkPolecatSafety answers it; an
// unreadable one is an error. Only a sandbox seat's origin ref is read.
func TestNukeSeatOriginReadsTheAgentBeadKindAndOrigin(t *testing.T) {
	t.Parallel()
	const session = "gastown-basalt"
	cases := []struct {
		name       string
		agent      string
		agentErr   error
		sandboxed  bool
		tip        string
		tipErr     error
		wantTip    string
		wantSand   bool
		wantErr    string
		wantOrigin bool
	}{
		{name: "host seat", agent: nukeE2EAgent},
		{name: "no agent bead is a host seat"},
		{
			name:     "unreadable agent bead is an error",
			agentErr: errors.New("bd show: connection refused"),
			wantErr:  "agent bead",
		},
		{
			name: "sandbox seat answers origin's tip", agent: nukeE2EAgent, sandboxed: true,
			tip: "812bd897f00d", wantTip: "812bd897f00d", wantSand: true, wantOrigin: true,
		},
		{
			name: "sandbox seat origin lacks the branch", agent: nukeE2EAgent, sandboxed: true,
			wantSand: true, wantOrigin: true,
		},
		{
			name: "sandbox seat with an unreadable origin ref is an error", agent: nukeE2EAgent, sandboxed: true,
			tipErr: errors.New("ls-remote: could not resolve host"), wantErr: "refs/heads/" + preserveBranch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			originReads := 0
			seat := nukeSeatOriginFromReads(
				polecatGitStateReads{
					agent: func() (string, error) { return tc.agent, tc.agentErr },
					probe: func(agent, sessionName string) (polecat.LiveGitState, bool) {
						if agent != tc.agent || sessionName != session {
							t.Errorf("probe asked about agent %q session %q, want %q %q", agent, sessionName, tc.agent, session)
						}
						return polecat.LiveGitState{}, tc.sandboxed
					},
				},
				func(branch string) (string, error) {
					originReads++
					if branch != preserveBranch {
						t.Errorf("origin read for branch %q, want %q", branch, preserveBranch)
					}
					return tc.tip, tc.tipErr
				},
				session,
			)
			tip, sandboxed, err := seat(preserveBranch)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it naming %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if tip != tc.wantTip || sandboxed != tc.wantSand {
				t.Errorf("answer = (%q, %v), want (%q, %v)", tip, sandboxed, tc.wantTip, tc.wantSand)
			}
			want := 0
			if tc.wantOrigin {
				want = 1
			}
			if originReads != want {
				t.Errorf("origin reads = %d, want %d", originReads, want)
			}
		})
	}
}
