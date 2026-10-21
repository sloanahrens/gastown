package testsel

import (
	"context"

	"github.com/steveyegge/gastown/internal/land"
)

// Packages reads a gate's per-package results from the gate's own output.
// *land.CandidateGate satisfies it.
type Packages interface {
	Packages(ctx context.Context, dir string, res land.CandidateResult) ([]land.PackageResult, error)
}

// Shadow is a rig's candidate gate with the battery running beside it. Run
// starts the battery's pass before the gate, runs the gate exactly as it would
// run alone, then finishes the pass with the gate's per-package results. The
// verdict Shadow returns is the gate's verdict, untouched.
type Shadow struct {
	// Gate is the rig's candidate gate.
	Gate land.Candidate
	// Results reads the gate's per-package results after its verdict.
	Results Packages
	// Pass is the battery.
	Pass *Pass
	// Rig is the rig the landing belongs to.
	Rig string
	// Logf reports a failure of the battery. It never changes the landing.
	Logf func(format string, args ...any)
}

// Run is the gate's run with the battery beside it.
func (s Shadow) Run(ctx context.Context, wt land.Repo, dir string, w land.Work, head string) land.CandidateResult {
	run := s.Pass.Start(ctx, Landing{Bead: w.BeadID, Rig: s.Rig, Base: head + "^1", Head: head, Dir: dir})
	res := s.Gate.Run(ctx, wt, dir, w, head)
	var results []land.PackageResult
	if res.Err == nil && res.State != land.CandidateSilent {
		pkgs, err := s.Results.Packages(ctx, dir, res)
		if err != nil {
			s.logf("%s: the gate's per-package results could not be read: %v", w.BeadID, err)
		}
		results = pkgs
	}
	if err := run.Finish(results); err != nil {
		s.logf("%s: the test-selection line was not written: %v", w.BeadID, err)
	}
	return res
}

// Discard is the gate's own, so a landing that ends without merging still
// deletes its candidate branch.
func (s Shadow) Discard(ctx context.Context, w land.Work, res land.CandidateResult) {
	s.Gate.Discard(ctx, w, res)
}

func (s Shadow) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf("test-selection: "+format, args...)
	}
}
