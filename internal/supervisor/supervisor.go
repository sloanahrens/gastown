// Package supervisor holds the town's only two lifecycle verbs, Kill and
// Restart (ADR 0003, gt-4k3fj.3). Every automated path that ends or replaces
// a seat's session calls one of them, and they enforce, in one place:
//
//   - the seat's hold: an operator park or a supervisor freeze in the seat's
//     intent record (internal/intent) refuses both verbs;
//   - submitted work: a seat whose intent record says desired=submitted (gt
//     done handed its branch to the landing worker) refuses Restart, since a
//     new session would only find finished work; Kill still works;
//   - e-stop: the town sentinel or the seat's rig sentinel refuses both
//     (running sessions finish; only an explicit kill-all may bypass it);
//   - shutdown: a `gt down` in progress refuses Restart;
//   - the restart budget: Budget restarts per seat per Window (3 per hour),
//     persisted in the intent record; the next one freezes the seat, writes it
//     as paused by the supervisor, and sends one line to the operator;
//   - actor logging: every call, done, refused or failed, appends one JSON
//     line naming the verb, seat, reason and actor to
//     <town>/.runtime/supervisor/actions.jsonl.
//
// The guards are files, so they hold in any process: the daemon owns the
// long-lived Supervisor, and the hosts that still act before their own
// tickets move them into the daemon (the witness patrol scan, gt doctor
// --fix) build one with the same rules. Nothing here reads Dolt; the agent
// bead mirror is a best-effort callback after the record is written.
package supervisor

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/session"
)

// Default budget: 3 restarts per seat per hour (epic gt-4k3fj item 8).
const (
	DefaultBudget = 3
	DefaultWindow = time.Hour
)

// ErrRefused wraps every refusal, so callers can tell "the supervisor said
// no" (log and move on) from an execution failure.
var ErrRefused = errors.New("supervisor refused")

// Refusal kinds; each refusal wraps ErrRefused and exactly one of these.
var (
	ErrPaused           = errors.New("seat is parked")
	ErrFrozen           = errors.New("seat is frozen")
	ErrSubmitted        = errors.New("work is submitted for landing")
	ErrEstop            = errors.New("e-stop is active")
	ErrShutdown         = errors.New("shutdown in progress")
	ErrBudgetExhausted  = errors.New("restart budget exhausted")
	ErrIntentUnreadable = errors.New("intent record unreadable")
)

// ErrDeclined is wrapped by a restart executor that started nothing on
// purpose (a fork-backed rig, a safety stop, a session someone else already
// raised). The attempt is logged as declined and does not spend budget.
var ErrDeclined = errors.New("restart declined by the executor")

// ErrNoStarter is returned by Restart on a Supervisor built without one.
var ErrNoStarter = errors.New("supervisor has no restart executor")

// Seat identifies a seat by its session identity.
type Seat = session.AgentIdentity

// SeatFor builds a seat. rig is empty for town-level seats; name is empty
// for singletons. Its session name resolves the rig's prefix from
// session.DefaultRegistry when asked; SeatIn resolves it up front.
func SeatFor(rig, role, name string) Seat {
	return Seat{Rig: rig, Role: session.Role(role), Name: name}
}

// SeatIn builds a seat whose rig prefix comes from reg, so naming its session
// never reads the process-wide registry.
func SeatIn(reg *session.PrefixRegistry, rig, role, name string) Seat {
	seat := SeatFor(rig, role, name)
	if rig != "" {
		seat.Prefix = reg.PrefixForRig(rig)
	}
	return seat
}

// SeatForSession parses a tmux session name into its seat.
func SeatForSession(name string) (Seat, error) {
	id, err := session.ParseSessionName(name)
	if err != nil {
		return Seat{}, err
	}
	return *id, nil
}

// IntentSeat returns the intent record key for a seat.
func IntentSeat(seat Seat) intent.Seat {
	return intent.Seat{Rig: seat.Rig, Role: string(seat.Role), Name: seat.Name}
}

// Killer is the tmux surface Kill and KillStray use. Killing a session that
// does not exist must succeed.
type Killer interface {
	KillSessionWithProcesses(name string) error
}

// Options configures a Supervisor.
type Options struct {
	TownRoot string
	Tmux     Killer
	// Restart replaces any session the seat has with a fresh one. Nil makes
	// Restart return ErrNoStarter.
	Restart func(seat Seat) error
	// Mirror writes the agent-bead display mirror after the record; its
	// error is logged and never undoes an action.
	Mirror func(seat Seat, rec intent.Record) error
	// Escalate sends one line to the operator when a seat's budget runs out.
	Escalate func(seat Seat, line string)
	// Logf defaults to log.Printf.
	Logf func(format string, args ...any)
	// Now defaults to time.Now.
	Now func() time.Time
	// Budget and Window default to DefaultBudget per DefaultWindow.
	Budget int
	Window time.Duration
}

// Supervisor enforces the lifecycle guards. Its zero value is not usable;
// build one with New.
type Supervisor struct {
	o Options
}

// New returns a Supervisor with defaults filled in.
func New(o Options) *Supervisor {
	if o.Logf == nil {
		o.Logf = log.Printf
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Budget <= 0 {
		o.Budget = DefaultBudget
	}
	if o.Window <= 0 {
		o.Window = DefaultWindow
	}
	return &Supervisor{o: o}
}

// ActionLine is one line of the action log.
type ActionLine struct {
	At      time.Time `json:"at"`
	Verb    string    `json:"verb"`
	Seat    string    `json:"seat,omitempty"`
	Session string    `json:"session"`
	Reason  string    `json:"reason,omitempty"`
	Actor   string    `json:"actor"`
	Outcome string    `json:"outcome"` // done, refused, failed, declined
	Detail  string    `json:"detail,omitempty"`
}

// ActionLogPath is the supervisor's action log.
func ActionLogPath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "supervisor", "actions.jsonl")
}

const (
	outcomeDone     = "done"
	outcomeRefused  = "refused"
	outcomeFailed   = "failed"
	outcomeDeclined = "declined"
)

// record logs an action line to the logger and appends it to the action log.
func (s *Supervisor) record(l ActionLine) {
	l.At = s.o.Now().UTC()
	s.o.Logf("supervisor: %s %s (%s) by %s: %s — %s%s", l.Verb, l.Session, l.Seat, l.Actor, l.Outcome, l.Reason, detailSuffix(l.Detail))
	data, err := json.Marshal(l)
	if err != nil {
		return
	}
	path := ActionLogPath(s.o.TownRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.o.Logf("supervisor: action log dir: %v", err)
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // G304: town runtime path
	if err != nil {
		s.o.Logf("supervisor: action log: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		s.o.Logf("supervisor: action log write: %v", err)
	}
}

func detailSuffix(d string) string {
	if d == "" {
		return ""
	}
	return " (" + d + ")"
}

// refuse logs a refusal and returns it as an error wrapping ErrRefused and
// kind.
func (s *Supervisor) refuse(l ActionLine, kind error, detail string) error {
	l.Outcome, l.Detail = outcomeRefused, detail
	s.record(l)
	return refusal(kind, detail)
}

// guard runs the checks both verbs share: a readable, unheld intent record
// and no e-stop covering the seat.
func (s *Supervisor) guard(seat Seat, l ActionLine) error {
	rec, err := intent.Read(s.o.TownRoot, IntentSeat(seat))
	if err != nil {
		return s.refuse(l, ErrIntentUnreadable, err.Error())
	}
	if rec.Held() {
		kind := ErrPaused
		if rec.Frozen {
			kind = ErrFrozen
		}
		return s.refuseSeat(seat, l, kind, rec.HoldReason())
	}
	if on, err := estop.ActiveFor(s.o.TownRoot, seat.Rig); on {
		detail := "e-stop sentinel present"
		if err != nil {
			detail = err.Error()
		}
		return s.refuseSeat(seat, l, ErrEstop, detail)
	}
	return nil
}

// refuseHeldUnderLock refuses an action whose hold was found inside the
// seat's lock, after the guard passed. The record may have become
// unreadable in between (intent.Update starts such a record as held), which
// is reported as ErrIntentUnreadable, not as a park.
func (s *Supervisor) refuseHeldUnderLock(seat Seat, l ActionLine, held string) error {
	rec, err := intent.Read(s.o.TownRoot, IntentSeat(seat))
	switch {
	case err != nil:
		return s.refuse(l, ErrIntentUnreadable, err.Error())
	case rec.Frozen:
		return s.refuseSeat(seat, l, ErrFrozen, held)
	default:
		return s.refuseSeat(seat, l, ErrPaused, held)
	}
}

// errRepeat aborts the intent update of a refusal already on record.
var errRepeat = errors.New("repeat refusal")

// refuseSeat refuses an action on a seat whose record is readable. The
// refusal is always logged; it is written to the action log and to the
// seat's last_action only when it differs from the last refusal on record
// (verb, actor, detail) or that one is older than the budget window, so a
// caller asking every heartbeat for a parked seat leaves one line per window.
func (s *Supervisor) refuseSeat(seat Seat, l ActionLine, kind error, detail string) error {
	now := s.o.Now().UTC()
	_, err := intent.Update(s.o.TownRoot, IntentSeat(seat), func(r *intent.Record) error {
		if la := r.LastAction; la != nil && la.Verb == l.Verb && la.Outcome == outcomeRefused &&
			la.Actor == l.Actor && la.Detail == detail && now.Sub(la.At) < s.o.Window {
			return errRepeat
		}
		r.LastAction = &intent.Action{Verb: l.Verb, Reason: l.Reason, Actor: l.Actor, Outcome: outcomeRefused, Detail: detail, At: now}
		return nil
	})
	if errors.Is(err, errRepeat) {
		s.o.Logf("supervisor: %s %s by %s: refused again (%s)", l.Verb, l.Session, l.Actor, detail)
		return refusal(kind, detail)
	}
	return s.refuse(l, kind, detail)
}

// refusal builds the error every refusal returns.
func refusal(kind error, detail string) error {
	if detail == "" {
		return fmt.Errorf("%w: %w", ErrRefused, kind)
	}
	return fmt.Errorf("%w: %w: %s", ErrRefused, kind, detail)
}

// Kill ends the seat's session and records desired=stop. It is refused when
// the seat is held or e-stopped. The hold is checked again under the seat's
// lock, and the session is killed while that lock is held, so a pause (which
// takes the same lock) cannot land between the check and the kill. Killing a
// seat with no session succeeds.
func (s *Supervisor) Kill(seat Seat, reason, actor string) error {
	name := seat.SessionName()
	l := ActionLine{Verb: "kill", Seat: IntentSeat(seat).String(), Session: name, Reason: reason, Actor: actor}
	if err := s.guard(seat, l); err != nil {
		return err
	}
	now := s.o.Now().UTC()
	var held string
	var killed bool
	var killErr error
	rec, err := intent.Update(s.o.TownRoot, IntentSeat(seat), func(r *intent.Record) error {
		if r.Held() {
			held = r.HoldReason()
			return errHeld
		}
		if killErr = s.o.Tmux.KillSessionWithProcesses(name); killErr != nil {
			return killErr
		}
		killed = true
		r.Desired = intent.DesiredStop
		r.Progress = nil
		r.Actor, r.UpdatedAt = actor, now
		r.LastAction = &intent.Action{Verb: "kill", Reason: reason, Actor: actor, Outcome: outcomeDone, At: now}
		return nil
	})
	switch {
	case errors.Is(err, errHeld):
		return s.refuseHeldUnderLock(seat, l, held)
	case killErr != nil:
		l.Outcome, l.Detail = outcomeFailed, killErr.Error()
		s.record(l)
		return fmt.Errorf("killing %s: %w", name, killErr)
	case !killed:
		// The seat's lock could not be taken: nothing was killed.
		l.Outcome, l.Detail = outcomeFailed, err.Error()
		s.record(l)
		return fmt.Errorf("killing %s: %w", name, err)
	}
	l.Outcome = outcomeDone
	if err != nil {
		l.Detail = "session killed; intent record not updated: " + err.Error()
	}
	s.record(l)
	s.mirror(seat, rec, err)
	return nil
}

// errHeld aborts an intent update that found the seat held after the guard.
var errHeld = errors.New("held")

// errSubmitted aborts a restart's intent update that found the seat's work
// submitted for landing.
var errSubmitted = errors.New("submitted")

// Restart replaces the seat's session through the configured executor,
// within the seat's restart budget. The attempt is counted before it runs,
// so a restart that fails still spends budget: a seat whose agent dies at
// startup is frozen after Budget attempts instead of looping.
func (s *Supervisor) Restart(seat Seat, reason, actor string) error {
	name := seat.SessionName()
	l := ActionLine{Verb: "restart", Seat: IntentSeat(seat).String(), Session: name, Reason: reason, Actor: actor}
	if s.o.Restart == nil {
		l.Outcome, l.Detail = outcomeFailed, ErrNoStarter.Error()
		s.record(l)
		return ErrNoStarter
	}
	if err := s.guard(seat, l); err != nil {
		return err
	}
	if ShutdownInProgress(s.o.TownRoot) {
		return s.refuse(l, ErrShutdown, "")
	}

	now := s.o.Now().UTC()
	var exhausted bool
	var held, submitted string
	rec, err := intent.Update(s.o.TownRoot, IntentSeat(seat), func(r *intent.Record) error {
		if r.Held() {
			held = r.HoldReason()
			return errHeld
		}
		if r.Submitted() {
			submitted = "desired=submitted"
			if r.WorkBead != "" {
				submitted += ": " + r.WorkBead
			}
			return errSubmitted
		}
		r.Restarts = pruneBefore(r.Restarts, now.Add(-s.o.Window))
		r.Actor, r.UpdatedAt = actor, now
		if len(r.Restarts) >= s.o.Budget {
			exhausted = true
			r.Frozen = true
			r.Reason = fmt.Sprintf("restart budget exhausted: %d restarts in %s (last: %s)", len(r.Restarts), s.o.Window, reason)
			r.PausedBy = "supervisor"
			r.PausedAt = now
			r.LastAction = &intent.Action{Verb: "restart", Reason: reason, Actor: actor, Outcome: outcomeRefused, Detail: r.Reason, At: now}
			return nil
		}
		r.Restarts = append(r.Restarts, now)
		r.Desired = intent.DesiredRun
		r.IncarnationID = newIncarnationID()
		r.Progress = nil
		r.LastAction = &intent.Action{Verb: "restart", Reason: reason, Actor: actor, Outcome: "started", At: now}
		return nil
	})
	switch {
	case errors.Is(err, errHeld):
		return s.refuseHeldUnderLock(seat, l, held)
	case errors.Is(err, errSubmitted):
		return s.refuseSeat(seat, l, ErrSubmitted, submitted)
	case err != nil:
		// The budget cannot be counted, so the restart cannot be allowed.
		return s.refuse(l, ErrIntentUnreadable, err.Error())
	case exhausted:
		line := fmt.Sprintf("%s (%s) frozen: %d restarts in %s; last reason %q by %s. Clear with: gt agent resume %s",
			IntentSeat(seat), name, s.o.Budget, s.o.Window, reason, actor, seat.Address())
		if s.o.Escalate != nil {
			s.o.Escalate(seat, line)
		}
		s.mirror(seat, rec, nil)
		return s.refuse(l, ErrBudgetExhausted, rec.Reason)
	}

	// Re-check the hold just before the executor, which may take minutes and
	// so runs outside the seat lock: a pause that landed after the budget
	// was counted still wins, and the stamp is given back.
	if rec, err := intent.Read(s.o.TownRoot, IntentSeat(seat)); err != nil || rec.Held() {
		s.releaseStamp(seat, now)
		if err != nil {
			return s.refuse(l, ErrIntentUnreadable, err.Error())
		}
		return s.refuseSeat(seat, l, ErrPaused, rec.HoldReason())
	}

	runErr := s.o.Restart(seat)
	outcome := outcomeDone
	switch {
	case errors.Is(runErr, ErrDeclined):
		outcome = outcomeDeclined
		l.Detail = runErr.Error()
	case runErr != nil:
		outcome = outcomeFailed
		l.Detail = runErr.Error()
	}
	rec, uerr := intent.Update(s.o.TownRoot, IntentSeat(seat), func(r *intent.Record) error {
		if r.LastAction != nil {
			r.LastAction.Outcome = outcome
		}
		if outcome == outcomeDeclined {
			// Nothing was started: give back the stamp this attempt took.
			r.Restarts = withoutStamp(r.Restarts, now)
		}
		return nil
	})
	l.Outcome = outcome
	s.record(l)
	s.mirror(seat, rec, uerr)
	if runErr != nil {
		return fmt.Errorf("restarting %s: %w", name, runErr)
	}
	return nil
}

// KillStray ends a session that belongs to no seat (a ghost with a stale
// prefix, a duplicate on the wrong tmux socket, a session for a rig the town
// no longer has). It honors the town e-stop and is logged like Kill; there is
// no seat record to hold or update. A name that parses to a rig also honors
// that rig's e-stop.
func (s *Supervisor) KillStray(sessionName, reason, actor string) error {
	l := ActionLine{Verb: "kill-stray", Session: sessionName, Reason: reason, Actor: actor}
	rig := ""
	if seat, err := SeatForSession(sessionName); err == nil {
		rig = seat.Rig
	}
	if on, err := estop.ActiveFor(s.o.TownRoot, rig); on {
		detail := "e-stop sentinel present"
		if err != nil {
			detail = err.Error()
		}
		return s.refuse(l, ErrEstop, detail)
	}
	if err := s.o.Tmux.KillSessionWithProcesses(sessionName); err != nil {
		l.Outcome, l.Detail = outcomeFailed, err.Error()
		s.record(l)
		return fmt.Errorf("killing %s: %w", sessionName, err)
	}
	l.Outcome = outcomeDone
	s.record(l)
	return nil
}

// mirror runs the display-mirror callback, logging its failure.
func (s *Supervisor) mirror(seat Seat, rec intent.Record, updateErr error) {
	if s.o.Mirror == nil || updateErr != nil {
		return
	}
	if err := s.o.Mirror(seat, rec); err != nil {
		s.o.Logf("supervisor: agent-bead mirror for %s failed (display only): %v", IntentSeat(seat), err)
	}
}

// ClearHold undoes a supervisor freeze and empties the seat's restart
// budget. An operator park is left alone: that is `gt agent resume`.
func ClearHold(townRoot string, seat Seat, actor string) error {
	_, err := intent.Update(townRoot, IntentSeat(seat), func(r *intent.Record) error {
		r.Restarts = nil
		if r.Frozen {
			r.Frozen = false
			if r.PausedBy == "supervisor" {
				r.Reason, r.PausedBy, r.PausedAt = "", "", time.Time{}
			}
		}
		r.Actor, r.UpdatedAt = actor, time.Now().UTC()
		return nil
	})
	return err
}

// ShutdownInProgress reports whether `gt down` holds the town's shutdown
// lock (<town>/daemon/shutdown.lock). A lock that cannot be probed reads as
// held.
func ShutdownInProgress(townRoot string) bool {
	lockPath := filepath.Join(townRoot, "daemon", "shutdown.lock")
	if _, err := os.Stat(lockPath); os.IsNotExist(err) {
		return false
	}
	lock := flock.New(lockPath)
	locked, err := lock.TryLock()
	if err != nil {
		return true
	}
	if locked {
		_ = lock.Unlock()
		return false
	}
	return true
}

// releaseStamp gives back the budget stamp a refused attempt took.
func (s *Supervisor) releaseStamp(seat Seat, at time.Time) {
	_, _ = intent.Update(s.o.TownRoot, IntentSeat(seat), func(r *intent.Record) error {
		r.Restarts = withoutStamp(r.Restarts, at)
		return nil
	})
}

// withoutStamp removes the last restart stamp equal to at.
func withoutStamp(ts []time.Time, at time.Time) []time.Time {
	for i := len(ts) - 1; i >= 0; i-- {
		if ts[i].Equal(at) {
			return append(ts[:i], ts[i+1:]...)
		}
	}
	return ts
}

func pruneBefore(ts []time.Time, cutoff time.Time) []time.Time {
	out := ts[:0:0]
	for _, t := range ts {
		if !t.Before(cutoff) {
			out = append(out, t)
		}
	}
	return out
}

func newIncarnationID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
