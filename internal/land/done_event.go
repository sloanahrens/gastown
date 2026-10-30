package land

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/events"
)

// doneEventDedupeWindow is how far before a submission's stamp the host looks
// for a done event a seat wrote for itself. A host seat writes its own event
// just before the submission note, so the two stamps are within minutes; the
// window covers a slow gate between the note and the event.
const doneEventDedupeWindow = 15 * time.Minute

// Event payload fields are bounded so a seat's own submission cannot put an
// unbounded or unprintable string into the host's log (gt-1m9gr).
const (
	eventSeatNameLimit = 64
	eventBeadIDLimit   = 128
	eventBranchLimit   = 255
)

// DoneEventRecorder writes the done event a sandboxed seat could not write
// itself (gt-1m9gr). A seat's gt done appends to the events log of the town it
// runs in, which for a sandbox seat is the rendered skeleton extracted for the
// sandbox and deleted with it. The host therefore sees the submission in the
// bead's notes but no done event in its own log, and the crash detector, the
// feed and gt log lose the seat's turn.
//
// The recorder is the host's record, not the seat's: it writes to the town
// root it was built with (never one resolved from the working directory), and
// it writes the seat the host slung rather than the one the submission block
// names.
//
// It is not safe for concurrent use: the landing worker's single goroutine
// calls it.
type DoneEventRecorder struct {
	// TownRoot is the town whose events log receives the event.
	TownRoot string
	// Assignee answers the bead's assignee, the host's own record of the seat
	// it slung (<rig>/polecats/<name>). The submission's Worker is a string
	// the seat wrote into the bead's notes, so it is only trusted once it
	// matches this record; nil refuses every seat rather than guessing.
	Assignee func(beadID string) (string, error)
	// Now is the host clock: when the host first saw the submission. Nil means
	// time.Now.
	Now func() time.Time
	// Append writes one feed event. Nil means events.LogFeedTo.
	Append func(townRoot, eventType, actor string, payload map[string]interface{}) error

	// written holds the submissions this recorder has already answered, keyed
	// by bead and stamp, so a seat's stamp later than the host's clock cannot
	// make the per-pass call write twice.
	written map[string]bool
	log     doneEventLog
}

// Record writes w's done event unless the town's log already holds one for the
// bead, and reports whether it wrote one. A crew submission (no Worker) writes
// nothing: it has no seat to end, and its own gt done wrote the log.
func (r *DoneEventRecorder) Record(w Work) (bool, error) {
	if w.Worker == "" {
		return false, nil
	}
	if r.TownRoot == "" {
		return false, fmt.Errorf("done event for %s: no town root, so there is no log to write and the seat's turn would stay unrecorded", w.BeadID)
	}
	if !validSeatPart(w.Rig) || !validSeatPart(w.Worker) {
		return false, fmt.Errorf("done event for %s: %q/%q is not a seat", w.BeadID, w.Rig, w.Worker)
	}
	if !validEventPayload(w.BeadID, eventBeadIDLimit) {
		return false, fmt.Errorf("done event for %q: not a bead id", w.BeadID)
	}
	if !validEventPayload(w.Branch, eventBranchLimit) {
		return false, fmt.Errorf("done event for %s: branch %q is not a branch name", w.BeadID, w.Branch)
	}
	if r.written == nil {
		r.written = make(map[string]bool)
	}
	key := w.BeadID + "\x00" + w.Submitted.UTC().Format(time.RFC3339Nano)
	if r.written[key] {
		return false, nil
	}
	// The log is read before the assignee is because a bead whose done event
	// is already there needs no write and no seat check, and its assignee may
	// already be cleared.
	at, cutoff := r.times(w)
	if err := r.log.refresh(filepath.Join(r.TownRoot, events.EventsFile)); err != nil {
		return false, err
	}
	skip, err := r.log.alreadyDone(w.BeadID, cutoff)
	if err != nil {
		return false, err
	}
	if skip {
		r.written[key] = true
		return false, nil
	}
	if err := r.checkAssignee(w); err != nil {
		return false, err
	}
	payload := events.DonePayload(w.BeadID, w.Branch)
	payload["at"] = at.UTC().Format(time.RFC3339)
	if err := r.append()(r.TownRoot, events.TypeDone, w.Rig+"/polecats/"+w.Worker, payload); err != nil {
		return false, fmt.Errorf("writing the done event for %s: %w", w.BeadID, err)
	}
	r.written[key] = true
	return true, nil
}

// checkAssignee binds the submission's seat to the host's own record of what
// it slung: a seat may name any Worker in its READY TO LAND block, and the
// actor of a done event is what the crash detector and the feed read, so the
// note's word alone would let one seat end another's turn.
func (r *DoneEventRecorder) checkAssignee(w Work) error {
	if r.Assignee == nil {
		return fmt.Errorf("done event for %s: no assignee reader, so %s/%s cannot be bound to the host's record", w.BeadID, w.Rig, w.Worker)
	}
	assignee, err := r.Assignee(w.BeadID)
	if err != nil {
		return fmt.Errorf("done event for %s: reading the assignee: %w", w.BeadID, err)
	}
	if rig, seat, ok := splitPolecatSeat(assignee); !ok || rig != w.Rig || seat != w.Worker {
		return fmt.Errorf("done event for %s: the submission names %s/%s but the bead was slung to %q", w.BeadID, w.Rig, w.Worker, assignee)
	}
	return nil
}

// times answers the event's at stamp and the cutoff a done event must predate
// to count as this submission's. The seat's stamp may pull the end earlier
// than the host's first sight, never later (closeAttempt in landings.go); a
// stamp the host's clock is already past is what the seat worked, and one from
// the future is the seat's word for a time it did not have, so the first sight
// stands in for it. A submission with no stamp is dated by the first sight.
func (r *DoneEventRecorder) times(w Work) (at, cutoff time.Time) {
	now := r.now()
	at = now
	cutoff = now.Add(-doneEventDedupeWindow)
	if !w.Submitted.IsZero() {
		cutoff = w.Submitted.Add(-doneEventDedupeWindow)
		if !w.Submitted.After(now) {
			at = w.Submitted
		}
	}
	return at, cutoff
}

func (r *DoneEventRecorder) now() time.Time {
	if r.Now == nil {
		return time.Now()
	}
	return r.Now()
}

func (r *DoneEventRecorder) append() func(townRoot, eventType, actor string, payload map[string]interface{}) error {
	if r.Append != nil {
		return r.Append
	}
	return events.LogFeedTo
}

// validSeatPart reports whether s can be one part of a seat address: the
// character set role.Actor emits, so a rig or polecat name that would restyle
// the actor is refused.
func validSeatPart(s string) bool {
	if s == "" || len(s) > eventSeatNameLimit {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-':
		default:
			return false
		}
	}
	return true
}

// splitPolecatSeat reads <rig>/polecats/<name>, the address a sling assigns a
// bead. Anything else (a crew address, a human, an empty assignee) is not a
// polecat seat and is refused.
func splitPolecatSeat(assignee string) (rig, name string, ok bool) {
	parts := strings.Split(assignee, "/")
	if len(parts) != 3 || parts[1] != "polecats" {
		return "", "", false
	}
	if !validSeatPart(parts[0]) || !validSeatPart(parts[2]) {
		return "", "", false
	}
	return parts[0], parts[2], true
}

// validEventPayload reports whether s is a printable field of the given bound.
// A note's branch and bead id are strings an author can write, so the log gets
// neither a control byte nor an unbounded value.
func validEventPayload(s string, limit int) bool {
	if s == "" || len(s) > limit {
		return false
	}
	for _, c := range s {
		if c <= ' ' || c == 0x7f {
			return false
		}
	}
	return true
}

// doneEventLog answers, per bead, whether the town's events log already holds
// a done event at or after a cutoff.
//
// The whole log is read on each refresh. The prune patrol caps it at 1 MiB,
// and the landing worker asks once per waiting submission per pass, so the
// read is cheap next to the state a tail read needs to stay correct across a
// log the prune replaces under it.
//
// A line that is not a JSON event cannot name a bead and is skipped. A done
// event that names the bead but carries no readable time marks the bead
// unjudged, and a bead stays unjudged for the life of this read: the recorder
// then fails closed rather than risk a duplicate, which is what a bead's
// remaining passes pay for one corrupt line (gt-1m9gr).
type doneEventLog struct {
	latest   map[string]time.Time
	unjudged map[string]bool
}

// refresh reads the log at path into the per-bead state. A missing log is a
// town that logged nothing: it reads as empty rather than as an error.
func (l *doneEventLog) refresh(path string) error {
	l.latest = make(map[string]time.Time)
	l.unjudged = make(map[string]bool)
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is the town's own events log
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading %s: %w", path, err)
	}
	l.fold(data)
	return nil
}

// alreadyDone reports whether the log holds a done event for bead dated at or
// after cutoff, and refuses to judge the bead when one of its done events has
// no readable time.
func (l *doneEventLog) alreadyDone(bead string, cutoff time.Time) (bool, error) {
	if l.unjudged[bead] {
		return false, fmt.Errorf("done event for %s: the log holds a done event for it with no readable time, so a duplicate cannot be ruled out", bead)
	}
	if at, ok := l.latest[bead]; ok && !at.Before(cutoff) {
		return true, nil
	}
	return false, nil
}

// fold reads every line of b, keeping each done event's bead and the latest
// time the bead's events carry.
func (l *doneEventLog) fold(b []byte) {
	for _, line := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ev struct {
			Type    string         `json:"type"`
			TS      string         `json:"ts"`
			Payload map[string]any `json:"payload"`
		}
		if err := json.Unmarshal(line, &ev); err != nil || ev.Type != events.TypeDone {
			continue
		}
		bead, _ := ev.Payload["bead"].(string)
		if bead == "" {
			continue
		}
		at, ok := doneEventTime(ev.Payload, ev.TS)
		if !ok {
			l.unjudged[bead] = true
			continue
		}
		if at.After(l.latest[bead]) {
			l.latest[bead] = at
		}
	}
}

// doneEventTime is when a done event happened. The host's own record writes
// the seat's stamp as the payload's at, because the line's ts is when the host
// wrote it, not when the seat's turn ended; a seat's own gt done leaves at
// unset and its ts is the time it wrote.
func doneEventTime(payload map[string]any, ts string) (time.Time, bool) {
	for _, raw := range []any{payload["at"], ts} {
		s, _ := raw.(string)
		if s == "" {
			continue
		}
		if at, err := time.Parse(time.RFC3339, s); err == nil {
			return at, true
		}
	}
	return time.Time{}, false
}
