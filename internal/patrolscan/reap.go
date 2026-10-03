package patrolscan

import (
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/intent"
)

// Reap defaults (design: docs/plans/2026-10-03-polecat-worktree-cleanup-design.md).
const (
	DefaultReapGrace       = 30 * time.Minute
	DefaultReapParkedGrace = 24 * time.Hour
	DefaultReapMaxPerTick  = 2
)

// ReapEnv is the optional half of Env the reap pass needs. A host that does not
// implement it never reaps.
type ReapEnv interface {
	// Recovery returns the verdict of `gt polecat check-recovery --json`.
	Recovery(rig, polecat string) (Recovery, error)
	// IdleSince is when the seat last did anything; the grace clock. A zero
	// time with no error means unknown.
	IdleSince(rig, polecat string) (time.Time, error)
	// BranchClaimed returns the ID of a non-terminal bead whose notes name
	// branch (including resume_branch), "" for none.
	BranchClaimed(rig, branch string) (string, error)
	// Reap removes the seat through `gt polecat nuke`, never with --force.
	Reap(rig, polecat string) error
}

// Recovery is the slice of the check-recovery verdict the reap pass reads.
type Recovery struct {
	Verdict        string
	Reusable       bool
	SafeToNuke     bool
	Reason         string
	Branch         string
	Issue          string
	ActiveMR       string
	Blockers       []string
	GitStateSource string
}

// ReapOptions tunes the reap pass.
type ReapOptions struct {
	DryRun      bool
	Grace       time.Duration
	ParkedGrace time.Duration
	MaxPerTick  int
}

// reap decides whether one seat is removed. Vetoes run cheapest first; the
// first to fire wins. A failed read is Unknown and never acts. A quiet veto
// (someone else owns the seat, or it is reusable capacity) reports nothing.
// left is the removals still allowed this tick.
func (s *Scanner) reap(rig, name string, left *int) (Finding, bool) {
	if s.reapEnv == nil || s.o.Reap == nil {
		return Finding{}, false
	}
	f := Finding{Kind: "reap", Subject: name}
	unknown := func(why string, err error) (Finding, bool) {
		f.Outcome, f.Detail = OutcomeUnknown, why+": "+err.Error()
		return f, true
	}

	rec, err := s.env.Intent(rig, name)
	if err != nil {
		return unknown("intent record unreadable", err)
	}
	if rec.Frozen || rec.Submitted() {
		return Finding{}, false // frozen is the supervisor's, submitted is the landing worker's
	}
	parked := rec.Desired == intent.DesiredPark

	up, err := s.env.SessionExists(rig, name)
	if err != nil {
		return unknown("session unreadable", err)
	}
	now := s.o.Now()
	if up {
		return Finding{}, false
	}
	if hb := s.env.Heartbeat(rig, name); hb != nil && now.Sub(hb.At) < s.o.HeartbeatFresh {
		return Finding{}, false
	}
	work, err := s.env.AssignedWork(rig, name)
	if err != nil {
		return unknown("assigned work unreadable", err)
	}
	if work != nil && !strings.EqualFold(work.Status, "closed") {
		return Finding{}, false // the seat pass owns a seat with live work
	}

	rv, err := s.reapEnv.Recovery(rig, name)
	if err != nil {
		return unknown("recovery verdict unreadable", err)
	}
	if rv.Branch != "" {
		bead, err := s.reapEnv.BranchClaimed(rig, rv.Branch)
		if err != nil {
			return unknown("branch claim unreadable", err)
		}
		if bead != "" {
			f.Outcome, f.Detail = OutcomeSkipped, fmt.Sprintf("branch %s is claimed by %s", rv.Branch, bead)
			return f, true
		}
	}
	switch rv.Verdict {
	case "WORKING", "SUBMITTED", "PENDING_MR":
		return Finding{}, false // someone owns it
	}
	if rv.Reusable {
		return Finding{}, false // reusable capacity is never reaped
	}

	since, err := s.reapEnv.IdleSince(rig, name)
	if err != nil {
		return unknown("idle-since unreadable", err)
	}
	if since.IsZero() {
		f.Outcome, f.Detail = OutcomeSkipped, "idle-since unknown; not eligible"
		return f, true
	}
	grace := s.o.Reap.Grace
	if parked {
		grace = s.o.Reap.ParkedGrace
	}
	if now.Sub(since) < grace {
		return Finding{}, false // quiet: a line per seat per tick would flood the log
	}

	safe := rv.SafeToNuke && rv.Verdict == "SAFE_TO_NUKE" && rv.GitStateSource == "live"
	if !safe {
		f.Outcome, f.Detail = OutcomeBlocked, blockedDetail(rv)
		return f, true
	}
	if s.o.Reap.DryRun {
		f.Outcome = OutcomeWouldReap
		f.Detail = fmt.Sprintf("idle %s, %s, git state live", now.Sub(since).Round(time.Minute), rv.Verdict)
		return f, true
	}
	if *left <= 0 {
		f.Outcome, f.Detail = OutcomeSkipped, "per-tick removal cap reached"
		return f, true
	}
	if err := s.reapEnv.Reap(rig, name); err != nil {
		if s.o.IsRefusal(err) {
			f.Outcome, f.Detail = OutcomeBlocked, "nuke refused: "+err.Error()
			return f, true
		}
		f.Outcome, f.Detail = OutcomeFailed, err.Error()
		return f, true
	}
	*left--
	f.Outcome, f.Detail = OutcomeReaped, fmt.Sprintf("idle %s, %s, git state live", now.Sub(since).Round(time.Minute), rv.Verdict)
	return f, true
}

// blockedDetail names why a seat is not safe: the verdict, its blockers, and a
// git state that was not measured live.
func blockedDetail(rv Recovery) string {
	parts := []string{"verdict " + rv.Verdict}
	if rv.GitStateSource != "live" {
		parts = append(parts, "git state "+orUnknown(rv.GitStateSource)+" (not measured live)")
	}
	if len(rv.Blockers) > 0 {
		parts = append(parts, strings.Join(rv.Blockers, "; "))
	} else if rv.Reason != "" {
		parts = append(parts, rv.Reason)
	}
	return strings.Join(parts, ": ")
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
