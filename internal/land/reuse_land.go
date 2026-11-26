package land

import "context"

// reuseAhead answers whether the landing may reuse the review stored for w
// instead of running om again (gt-5cfuf2). A nil Ahead turns the question off:
// no key is computed and no record is read.
//
// hit is true only when the store has a record, its outcome is a verdict,
// ReuseMiss finds nothing against the work's current key, and the rig's bar at
// base can be read. On a hit the verdict goes through applyBar against today's
// bar, so a stored approve below it comes back as request_changes (gt-dz7a2d).
// rec is that record, so a caller reads the verdict and a miss can name the
// record it refused. key is the work's current key whenever it could be
// computed, hit or miss.
//
// err is non-nil only when the answer is unknown: the key could not be computed,
// the record could not be read, or the bar could not be read. A caller must then
// neither reuse the record nor delete it. A record that simply does not match is
// a miss with a nil error.
//
// Every miss that had a record and every error logs one line through l.Out
// naming the bead and the reason. A missing record and a nil Ahead log nothing.
func (l *Lander) reuseAhead(ctx context.Context, g, wt Repo, w Work, in ReviewInput, base, merged string) (rec AheadRecord, key ReviewKey, hit bool, err error) {
	if l.Ahead == nil {
		return AheadRecord{}, ReviewKey{}, false, nil
	}
	key, touched, err := l.reviewKey(ctx, g, wt, w, in, base, merged)
	if err != nil {
		l.logf("%s: the stored review is not reused: the review key could not be computed (%v)", w.BeadID, err)
		return AheadRecord{}, ReviewKey{}, false, err
	}
	rec, ok, err := l.Ahead.Get(w.BeadID)
	if err != nil {
		l.logf("%s: the stored review is not reused: the record could not be read (%v)", w.BeadID, err)
		return AheadRecord{}, key, false, err
	}
	if !ok {
		return AheadRecord{}, key, false, nil
	}
	// A conflict record is a merge attempt that reached no verdict, so it is
	// nothing to reuse: the landing keeps its own conflict rejection.
	if rec.Outcome != aheadOutcomeVerdict {
		l.logf("%s: the stored review is not reused: the record is a merge conflict, not a verdict", w.BeadID)
		return rec, key, false, nil
	}
	if reason := ReuseMiss(g, rec, key, base, touched, l.aheadNow(), l.ReuseExpiry); reason != "" {
		l.logf("%s: the stored review is not reused: %s", w.BeadID, reason)
		return rec, key, false, nil
	}
	bar, err := OMThresholdAt(g, base)
	if err != nil {
		l.logf("%s: the stored review is not reused: the rig's bar at %s could not be read (%v)", w.BeadID, shortSHA(base), err)
		return rec, key, false, err
	}
	rec.Verdict = applyBar(rec.Verdict, bar)
	return rec, key, true, nil
}
