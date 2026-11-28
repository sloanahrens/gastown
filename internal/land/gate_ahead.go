package land

import "context"

// GateAhead starts building and gating w ahead of its turn on the front car's
// merge commit and reports whether a run started; a nil slot starts nothing
// (train 3c). The held car names the front commit as its base, and a build
// that fails runs no gate and ends the run with its reason, logged once.
func (l *Lander) GateAhead(ctx context.Context, w Work, frontMerged string) bool {
	g := l.open(l.Repo)
	run := func(ctx context.Context, report func(merged string)) (AheadGate, error) {
		// The build's cleanup is unused until gt-oh8tng gives the slot a discard
		// that runs it: a car let go leaves its worktree and candidate behind.
		dir, merged, _, err := l.gateAheadBuild(g, w, frontMerged)
		if err != nil {
			l.logf("%s: gate-ahead: not started: %v", w.BeadID, err)
			return AheadGate{}, err
		}
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
	return l.Ahead2.Start(ctx, w.BeadID, w.Head, frontMerged, run, func(AheadGate) {})
}
