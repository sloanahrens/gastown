package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/patrolscan"
	"github.com/steveyegge/gastown/internal/session"
)

// The escalation close nudge: a live seat that raised an escalation and stayed
// at its prompt, waiting on it, is told once when that escalation closes, with
// the close reason (gt-jx0s09). Closing an escalation resumes a seat whose
// session ended (gt-4hduq); this covers the seat that never went away. The
// patrol_scan tick runs it once per cycle, after the rigs are scanned.
//
// A close is a move from open to closed between two ticks, so the pass only
// announces an escalation whose id it saw open on an earlier tick. That state
// lives in the daemon's state directory, so a restarted daemon neither forgets
// an escalation that closed while it was down nor nudges again one it already
// announced.

// escalationCloseNudgeFile is the state file, under <town>/daemon.
const escalationCloseNudgeFile = "escalation_close_nudge.json"

// escalationCloseReasonMax bounds the close reason the nudge carries, in
// characters.
const escalationCloseReasonMax = 400

// escalationCloseState is what the pass remembers between ticks.
type escalationCloseState struct {
	// Watch holds the escalations seen open at the last tick, and the closed
	// ones still owed a nudge (the seat was busy, or a read or the send failed).
	Watch map[string]bool `json:"watch,omitempty"`
	// Announced holds the escalations already nudged, or settled without a
	// nudge because the seat no longer held their bead or had no live session.
	Announced map[string]bool `json:"announced,omitempty"`
}

// escalationSeat is what the pass reads of a polecat's session.
type escalationSeat struct {
	// Live is true while the seat's session exists.
	Live bool
	// Idle is true while the live session sits at its input prompt. The pass
	// nudges only an idle seat, since a nudge to a seat that never goes idle is
	// not delivered; a busy seat is tried again on a later tick.
	Idle bool
}

// escalationCloseSeams are the reads and the side effect of one pass. The
// production pass fills them in (nudgeClosedEscalations); the tests set them.
type escalationCloseSeams struct {
	// escalations lists the escalations a rig's polecat seats raised, open and
	// closed.
	escalations func(rig string) ([]patrolscan.Escalation, error)
	// holding is the id of the work bead the seat holds, "" when it holds none.
	holding func(rig, name string) (string, error)
	// seat reads the seat's session.
	seat func(rig, name string) (escalationSeat, error)
	// nudge delivers msg to target, a "rig/name" seat address.
	nudge func(target, msg string) error
	// logf reports what the pass did and what it could not read.
	logf func(format string, args ...any)
}

// escalationOutcome is what the pass does with one closed, watched escalation.
type escalationOutcome int

const (
	// escalationNudged: the seat was nudged with the close.
	escalationNudged escalationOutcome = iota
	// escalationSettled: no nudge is due, because the seat no longer holds the
	// bead, has no live session, or the escalation names no bead of a polecat
	// seat. Nothing is retried.
	escalationSettled
	// escalationPending: the nudge is owed but cannot go yet, because the seat
	// is busy or a read or the send failed. The next tick tries again.
	escalationPending
)

// nudgeClosedEscalations runs one pass over the rigs with the production
// seams. The state is read from and written to <town>/daemon.
func (d *Daemon) nudgeClosedEscalations(h *patrolScanHost, rigs []string) {
	path := filepath.Join(d.config.TownRoot, "daemon", escalationCloseNudgeFile)
	st, err := loadEscalationCloseState(path)
	if err != nil {
		// An unreadable state is replaced, not trusted: starting from nothing
		// watches no escalation, so no nudge can fire on the old reading.
		d.logger.Printf("escalation_close: %v; starting from an empty state", err)
		st = escalationCloseState{}
	}
	seams := escalationCloseSeams{
		escalations: h.PolecatEscalations,
		holding: func(rig, name string) (string, error) {
			work, err := h.AssignedWork(rig, name)
			if err != nil || work == nil {
				return "", err
			}
			return work.ID, nil
		},
		seat: d.escalationSeatState,
		nudge: func(target, msg string) error {
			ctx, cancel := context.WithTimeout(context.Background(), overrunNudgeTimeout)
			defer cancel()
			return d.notify().Nudge(ctx, target, msg, notify.Queue())
		},
		logf: d.logger.Printf,
	}
	next, ok := runEscalationCloseNudges(seams, st, rigs)
	if !ok {
		return
	}
	if err := saveEscalationCloseState(path, next); err != nil {
		// The nudges of this tick are already out. If they are not saved, the
		// next tick may send them again.
		d.logger.Printf("escalation_close: cannot save %s: %v", escalationCloseNudgeFile, err)
	}
}

// runEscalationCloseNudges is one tick's pass over the rigs. It returns the
// state the next tick starts from. It returns false when a rig's escalations
// cannot be read: then nothing is judged, and the caller keeps the state it
// had.
func runEscalationCloseNudges(s escalationCloseSeams, st escalationCloseState, rigs []string) (escalationCloseState, bool) {
	next := escalationCloseState{Watch: map[string]bool{}, Announced: map[string]bool{}}
	for _, rig := range rigs {
		escs, err := s.escalations(rig)
		if err != nil {
			s.logf("escalation_close: %s: cannot read escalations, nudging nothing this tick: %v", rig, err)
			return st, false
		}
		sort.Slice(escs, func(i, j int) bool { return escs[i].ID < escs[j].ID })
		for _, e := range escs {
			switch {
			case e.Open:
				next.Watch[e.ID] = true
			case st.Announced[e.ID]:
				next.Announced[e.ID] = true
			case st.Watch[e.ID]:
				if judgeEscalationClose(s, rig, e) == escalationPending {
					next.Watch[e.ID] = true
				} else {
					next.Announced[e.ID] = true
				}
			}
		}
	}
	return next, true
}

// judgeEscalationClose decides what a closed, watched escalation owes its
// seat. The seat is named by escalated_by; the bead by related_bead. The seat
// is nudged only when it still holds that bead, has a live session, and sits
// at its prompt.
func judgeEscalationClose(s escalationCloseSeams, rig string, e patrolscan.Escalation) escalationOutcome {
	name, ok := strings.CutPrefix(strings.TrimSpace(e.EscalatedBy), rig+"/polecats/")
	if !ok || name == "" || strings.ContainsAny(name, "/ \t\r\n") || strings.TrimSpace(e.RelatedBead) == "" {
		// The name becomes a session name and a nudge target, so one that is
		// not a single path element names no seat.
		return escalationSettled
	}
	holds, err := s.holding(rig, name)
	if err != nil {
		s.logf("escalation_close: %s/%s: cannot read the held bead for %s, will retry: %v", rig, name, e.ID, err)
		return escalationPending
	}
	if strings.TrimSpace(holds) != strings.TrimSpace(e.RelatedBead) {
		return escalationSettled
	}
	seat, err := s.seat(rig, name)
	if err != nil {
		s.logf("escalation_close: %s/%s: cannot read the session for %s, will retry: %v", rig, name, e.ID, err)
		return escalationPending
	}
	if !seat.Live {
		// An ended seat is resumed by patrol_scan (gt-4hduq), not nudged.
		return escalationSettled
	}
	if !seat.Idle {
		return escalationPending
	}
	target := rig + "/" + name
	if err := s.nudge(target, escalationCloseMessage(e)); err != nil {
		s.logf("escalation_close: %s: nudge for %s failed, will retry: %v", target, e.ID, err)
		return escalationPending
	}
	s.logf("escalation_close: nudged %s: escalation %s closed", target, e.ID)
	return escalationNudged
}

// escalationCloseMessage is what the seat reads when its escalation closes.
// A close with no reason says so, and a reason past escalationCloseReasonMax
// characters is cut there.
func escalationCloseMessage(e patrolscan.Escalation) string {
	reason := strings.TrimSpace(e.CloseReason)
	if reason == "" {
		reason = "no close reason was given"
	}
	if r := []rune(reason); len(r) > escalationCloseReasonMax {
		reason = string(r[:escalationCloseReasonMax])
	}
	return fmt.Sprintf("Your escalation %s was closed: %s. Continue.", e.ID, reason)
}

// escalationSeatState reads a polecat seat's session: live when it exists,
// and idle when that session sits at its input prompt.
func (d *Daemon) escalationSeatState(rig, name string) (escalationSeat, error) {
	sessionName := session.PolecatSessionName(d.prefixRegistry().PrefixForRig(rig), name)
	alive, err := d.tmux.HasSession(sessionName)
	if err != nil {
		return escalationSeat{}, err
	}
	if !alive {
		return escalationSeat{}, nil
	}
	return escalationSeat{Live: true, Idle: d.tmux.IsIdle(sessionName)}, nil
}

// loadEscalationCloseState reads the state file. A missing file is the first
// tick's empty state, not an error.
func loadEscalationCloseState(path string) (escalationCloseState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return escalationCloseState{}, nil
		}
		return escalationCloseState{}, err
	}
	var st escalationCloseState
	if err := json.Unmarshal(data, &st); err != nil {
		return escalationCloseState{}, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return st, nil
}

// saveEscalationCloseState writes the state the next tick starts from.
func saveEscalationCloseState(path string, st escalationCloseState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return atomicfile.WriteJSON(path, st)
}
