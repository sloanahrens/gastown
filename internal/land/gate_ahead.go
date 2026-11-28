package land

import "context"

// GateAhead starts building and gating w ahead of its turn on the front car's
// merge commit and reports whether a run started; a nil slot starts nothing
// (train 3c). The held car names the front commit as its base, and a build
// that fails runs no gate and ends the run with its reason, logged once.
func (l *Lander) GateAhead(ctx context.Context, w Work, frontMerged string) bool {
	g := l.open(l.Repo)
	var cleanup func()
	run := func(ctx context.Context, report func(merged string)) (AheadGate, error) {
		dir, merged, c, err := l.gateAheadBuild(g, w, frontMerged)
		if err != nil {
			l.logf("%s: gate-ahead: not started: %v", w.BeadID, err)
			return AheadGate{}, err
		}
		cleanup = c
		// As Land does, so a log or a repair names the branch and commit CI
		// tested rather than recomputing them.
		w.CandidateBranch, w.CandidateHead = w.Candidate(), merged
		// Reported before the gate runs, so a Take or Close that ends the car
		// during the gate wait still knows what to discard.
		report(merged)
		res := l.Candidate.Run(ctx, l.open(dir), dir, w, merged)
		// The slot stamps the key; the merged commit comes from the report.
		return AheadGate{Result: res}, nil
	}
	// A Take or Close that lets the car go runs this once the run has ended:
	// the build's worktree is removed and the candidate branch the gate pushed
	// is deleted, so the car leaves nothing behind (gt-oh8tng). The cleanup
	// runs first, so a failed Discard cannot keep the worktree. The slot calls
	// it only after the run reported a merged commit, the one point the build
	// left a worktree.
	discard := func(gate AheadGate) {
		cleanup()
		l.Candidate.Discard(context.WithoutCancel(ctx), w, gate.Result)
	}
	return l.Ahead2.Start(ctx, w.BeadID, w.Head, frontMerged, run, discard)
}
