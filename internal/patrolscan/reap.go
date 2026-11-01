package patrolscan

import (
	"fmt"
	"sort"
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

// TrimEnv is the optional half of ReapEnv the directory-cap trim needs. The
// reap pass never touches a reusable seat: that is a rig's capacity, and a
// quiet skip is how a healthy pool reads. A rig at its polecat directory cap is
// the exception the trim exists for — the seats it holds are a directory apiece,
// so a sandbox seat whose work is finished and safe leaves the rig one short of
// admitting new work for ever. A host that does not implement it never trims.
type TrimEnv interface {
	// TrimmableSeats names the rig's sandbox seats, among the polecats the tick
	// listed, when the rig holds at least as many polecat directories as its
	// cap, and returns nil when it is under the cap. polecats is the tick's own
	// listing, so the host lists nothing a second time. Only sandbox seats are
	// ever named: a tmux seat's reuse is what the pool is for. The order is the
	// caller's business; an error means the cap or a seat's kind could not be
	// read, and the tick trims nothing on it.
	TrimmableSeats(rig string, polecats []string) ([]string, error)
}

// trimSeat is a seat the directory cap may trim this tick: its name, and the
// idle time its place in the trim order was decided by, so the pass reads that
// clock once.
type trimSeat struct {
	name    string
	idle    time.Time
	idleErr error
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
//
// trim is nil for every seat the directory-cap trim did not name, and holds the
// idle time the trim order was sorted by for one it did. A trim seat may be
// reusable: that veto is the only one the trim lifts, so every fact the pass
// measures about the seat's work still decides (gt-xlazq).
func (s *Scanner) reap(rig, name string, left *int, trim *trimSeat) (Finding, bool) {
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
	// A parked seat is never reusable capacity, whatever the verdict says:
	// only the allocator and `gt polecat list` read the pause marker, so
	// check-recovery reports Reusable true for a seat the allocator will
	// refuse. Trusting it here would leave the parked seats whose bead landed —
	// the case this pass exists for — un-reapable forever (gt-2knum). Safety
	// still rests on the SAFE_TO_NUKE verdict, live git state, the vetoes
	// above, and the longer ParkedGrace below.
	if rv.Reusable && !parked && trim == nil {
		return Finding{}, false // reusable capacity is never reaped
	}

	var since time.Time
	var idleErr error
	if trim != nil {
		since, idleErr = trim.idle, trim.idleErr
	} else {
		since, idleErr = s.reapEnv.IdleSince(rig, name)
	}
	if idleErr != nil {
		return unknown("idle-since unreadable", idleErr)
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
	removedDetail := func() string {
		detail := fmt.Sprintf("idle %s, %s, git state live", now.Sub(since).Round(time.Minute), rv.Verdict)
		if trim != nil {
			detail += ", trimmed at the rig's directory cap"
		}
		return detail
	}
	if s.o.Reap.DryRun {
		f.Outcome = OutcomeWouldReap
		f.Detail = removedDetail()
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
	f.Outcome, f.Detail = OutcomeReaped, removedDetail()
	return f, true
}

// trimAtDirCap is the tick's trim order: the seats the rig's directory cap may
// remove this tick, oldest idle first. It is empty when the host has no trim
// half or the rig is under its cap, and an error when the cap or a seat's kind
// could not be read — the tick then trims nothing, because a seat it cannot
// classify may be one the pass must never touch.
//
// A seat whose idle time cannot be read sorts last: it cannot be the oldest,
// and reap reports it Unknown rather than removing it.
func (s *Scanner) trimAtDirCap(rig string, polecats []string) ([]trimSeat, error) {
	if s.reapEnv == nil {
		return nil, nil
	}
	te, ok := s.reapEnv.(TrimEnv)
	if !ok {
		return nil, nil
	}
	seats, err := te.TrimmableSeats(rig, polecats)
	if err != nil {
		return nil, err
	}
	if len(seats) == 0 {
		return nil, nil
	}
	inRig := make(map[string]bool, len(polecats))
	for _, name := range polecats {
		inRig[name] = true
	}
	trim := make([]trimSeat, 0, len(seats))
	for _, name := range seats {
		if !inRig[name] {
			continue // a seat the tick did not list has no directory this tick
		}
		since, idleErr := s.reapEnv.IdleSince(rig, name)
		trim = append(trim, trimSeat{name: name, idle: since, idleErr: idleErr})
	}
	sort.SliceStable(trim, func(i, j int) bool {
		iUnknown := trim[i].idleErr != nil || trim[i].idle.IsZero()
		jUnknown := trim[j].idleErr != nil || trim[j].idle.IsZero()
		if iUnknown != jUnknown {
			return !iUnknown
		}
		if !trim[i].idle.Equal(trim[j].idle) {
			return trim[i].idle.Before(trim[j].idle)
		}
		return trim[i].name < trim[j].name
	})
	return trim, nil
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
