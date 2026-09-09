package quota

import "time"

// ResumeGrace is how long past a session's announced reset time to wait
// before nudging it to resume — avoids racing the CLI's own reset check,
// which fires at the announced instant.
const ResumeGrace = 1 * time.Minute

// ResumeCandidate is a rate-limited session whose own announced reset time
// has already passed and is ready to be nudged back to work.
type ResumeCandidate struct {
	Session   string
	ResetsAt  string
	ResetTime time.Time
}

// PlanResume finds rate-limited sessions whose announced reset time has
// already passed by at least ResumeGrace.
//
// Account rotation (the rest of this package) fixes an account-level rate
// limit by moving a session to fresh credentials. It does nothing for a
// session-scoped Claude Code usage limit — the "You've hit your session
// limit · resets 5pm" banner — which every account shares alike and which
// clears on its own once the announced reset time passes (gt-omg1: a whole
// town's worth of agents hit this simultaneously and none of them
// self-resumed, because nothing revisited them after the reset). This is
// the fallback for exactly that case: once the CLI's own clock says the
// session is usable again, nudge it rather than leaving it to sit idle.
//
// Sessions with an unparseable ResetsAt are skipped rather than guessed at
// — a wrong resume time either nudges too early (wasted, since the CLI
// still refuses input) or leaves the session idle longer than necessary,
// and both are recoverable on the next scan once the text is parseable.
func PlanResume(scanned []ScanResult, now time.Time) []ResumeCandidate {
	var candidates []ResumeCandidate
	for _, r := range scanned {
		if !r.RateLimited || r.ResetsAt == "" {
			continue
		}
		resetTime, err := ParseResetTime(r.ResetsAt, now)
		if err != nil {
			continue
		}
		if now.Before(resetTime.Add(ResumeGrace)) {
			continue
		}
		candidates = append(candidates, ResumeCandidate{
			Session:   r.Session,
			ResetsAt:  r.ResetsAt,
			ResetTime: resetTime,
		})
	}
	return candidates
}
