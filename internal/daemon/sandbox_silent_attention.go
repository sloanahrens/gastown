package daemon

// A sandbox pod that is Running, with a reachable session, can still have an
// agent that makes no progress: a seat parked at a prompt or looping on a
// failed command holds its pool slot and its bead, and nothing in the queue
// says so until the overrun bound (gt-oq4ele). The collector here reads, per
// live kubernetes sandbox seat, the newest transcript its agent wrote inside
// the sandbox, and raises one item per seat whose newest transcript is older
// than sandboxSilentAfter.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
)

const (
	// sandboxSilentAfter is how long a live sandbox seat's newest transcript
	// may stand before it raises an item. An agent running a long command can
	// be quiet for minutes, so the window is generous: half an hour with no
	// transcript is a seat whose agent may be stuck, not one inside a turn
	// (gt-oq4ele).
	sandboxSilentAfter = 30 * time.Minute

	// sandboxSilentKeyPrefix is the item key's prefix; the seat's rig and
	// polecat follow it, so one seat has one item however many ticks it stays
	// silent.
	sandboxSilentKeyPrefix = "sandbox-silent:"

	// sandboxSilentReadTimeout bounds one seat's transcript listing. A gateway
	// that does not answer (a wedged VM, a hung exec) costs this seat its read
	// and not the seats behind it in the attention tick.
	sandboxSilentReadTimeout = 10 * time.Second
)

// collectSilentSandboxSeats raises one high item per live kubernetes sandbox
// seat whose newest transcript inside its sandbox is more than
// sandboxSilentAfter old. The item names the seat and its bead, so the
// overseer reads whether the agent is working before the overrun bound stops
// it. A readable listing with a newer transcript, a seat younger than the
// window, and a seat that has written no transcript yet each raise nothing,
// which clears a seat's item.
//
// A seat is judged only when the town's settings still hold its profile and
// that profile is kubernetes: the docker driver is out of this item's scope,
// and a profile the settings no longer hold is an unknown the pass refuses
// rather than reads as a seat that is not kubernetes.
//
// A registry or profile read that fails returns the error and no items, so the
// previous tick's items are kept rather than cleared by an unanswered read. A
// record that cannot be read, or that names another session or a name an exec
// must not address, fails the collection the same way. A listing that cannot
// be read raises no new item and says so once per seat; the item the seat
// already had is kept, since an unanswered gateway is not a seat whose
// transcripts are recent. The tick's deadline ends the pass before the next
// seat's read, and such a pass fails, so its previous items are kept and no
// seat is cleared by a gateway that stopped answering mid-pass.
func (s *attentionSources) collectSilentSandboxSeats(ctx context.Context) ([]attention.Item, error) {
	d := s.d
	names, err := session.SandboxSessions(d.config.TownRoot)
	if err != nil {
		return nil, fmt.Errorf("listing the sandbox records: %w", err)
	}
	if len(names) == 0 {
		pruneSandboxSilentUnreadable(d, nil)
		return nil, nil
	}
	profiles, err := d.diskMaintenanceSeams().sandboxes()
	if err != nil {
		return nil, fmt.Errorf("reading the town's sandbox profiles: %w", err)
	}
	// The marks the seats of this pass keep: a seat this pass lists is live, and
	// every other mark is dropped after the loop.
	live := make(map[string]bool, len(names))
	var out []attention.Item
	for _, name := range names {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("the tick ended with the sandbox seats from %s unjudged: %w", name, ctx.Err())
		}
		rec, ok, err := session.ReadSandboxRecord(d.config.TownRoot, name)
		if err != nil {
			return nil, fmt.Errorf("reading the sandbox record %s: %w", name, err)
		}
		// !ok with no error: another process removed the record between the
		// listing and the read, so the seat has ended and writes no item.
		if !ok {
			continue
		}
		if rec.Session != name {
			return nil, fmt.Errorf("the sandbox record %s names the session %s", name, rec.Session)
		}
		if err := polecat.CheckSandboxSeatNames(rec.Rig, rec.Polecat); err != nil {
			return nil, fmt.Errorf("the sandbox record %s: %w", name, err)
		}
		live[sandboxSilentUnreadableKey(rec)] = true
		profile, ok := profiles[rec.Profile]
		if !ok {
			return nil, fmt.Errorf("the sandbox record %s names the profile %q, which the town's settings do not hold", name, rec.Profile)
		}
		if profile.Driver != agentconfig.SandboxDriverKubernetes {
			continue
		}
		if it, raised := s.silentSandboxSeat(ctx, rec); raised {
			out = append(out, it)
		}
	}
	pruneSandboxSilentUnreadable(d, live)
	slices.SortFunc(out, func(a, b attention.Item) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}

// silentSandboxSeat judges one live seat: it returns the item the seat raises,
// and whether it raises one. A seat younger than sandboxSilentAfter, one with
// no transcript yet, and one whose newest transcript is recent each raise
// nothing. A listing that cannot be read raises nothing new and keeps the
// seat's previous item, so a gateway that did not answer does not clear a real
// alarm; its failure is logged once per seat.
func (s *attentionSources) silentSandboxSeat(ctx context.Context, rec session.SandboxRecord) (attention.Item, bool) {
	key := sandboxSilentKeyPrefix + rec.Rig + "/" + rec.Polecat
	// A seat younger than the window has not been silent, whatever its clock
	// skew says about a transcript.
	if !rec.CreatedAt.IsZero() && s.now.Sub(rec.CreatedAt) <= sandboxSilentAfter {
		return attention.Item{}, false
	}
	readCtx, cancel := context.WithTimeout(ctx, sandboxSilentReadTimeout)
	defer cancel()
	listed, err := s.sandboxSeatTranscripts(readCtx, rec)
	if err != nil {
		s.logSilentSandboxSeatUnreadable(rec, err)
		for _, it := range s.prev.Items {
			if it.Key == key {
				return it, true
			}
		}
		return attention.Item{}, false
	}
	newest, ok := newestSandboxTranscript(listed)
	if !ok {
		return attention.Item{}, false // no transcript yet: not silent
	}
	silent := s.now.Sub(newest)
	if silent <= sandboxSilentAfter {
		return attention.Item{}, false
	}
	return attention.Item{
		Key:      key,
		Kind:     attention.KindSandboxSilent,
		Severity: attention.SeverityHigh,
		Rig:      rec.Rig,
		Bead:     rec.WorkBead,
		Summary:  fmt.Sprintf("%s/%s running but its newest transcript is %s old", rec.Rig, rec.Polecat, parkedAge(silent)),
	}, true
}

// newestSandboxTranscript is the newest modification time the listing carries,
// and whether it carries any transcript at all.
func newestSandboxTranscript(listed []session.SandboxTranscript) (time.Time, bool) {
	var newest time.Time
	for _, f := range listed {
		if f.MTime.After(newest) {
			newest = f.MTime
		}
	}
	return newest, !newest.IsZero()
}

// sandboxSeatTranscripts reads a seat's transcript listing through the daemon's
// seam, or through the linked openshell backend when no test supplied one.
func (s *attentionSources) sandboxSeatTranscripts(ctx context.Context, rec session.SandboxRecord) ([]session.SandboxTranscript, error) {
	if s.d.sandboxTranscripts != nil {
		return s.d.sandboxTranscripts(ctx, s.d.config.TownRoot, rec)
	}
	return session.ListSeatTranscripts(ctx, s.d.config.TownRoot, rec)
}

// sandboxSilentUnreadableKey names one seat for the log-once marks: the session
// and the record's creation time, so a new seat reusing a polecat's name is a
// new seat.
func sandboxSilentUnreadableKey(rec session.SandboxRecord) string {
	return rec.Session + "\x00" + rec.CreatedAt.UTC().Format(time.RFC3339Nano)
}

// logSilentSandboxSeatUnreadable says once per seat that its transcript listing
// could not be read.
func (s *attentionSources) logSilentSandboxSeatUnreadable(rec session.SandboxRecord, err error) {
	key := sandboxSilentUnreadableKey(rec)
	if _, logged := s.d.sandboxSilentUnreadable.LoadOrStore(key, true); logged {
		return
	}
	s.d.logger.Printf("attention: sandbox seat %s/%s: its transcript listing cannot be read, so this tick cannot judge it: %v", rec.Rig, rec.Polecat, err)
}

// pruneSandboxSilentUnreadable drops the log-once marks of seats a completed
// pass no longer lists, so a seat that ended does not leave its mark for the
// daemon's life. A pass that fails before its loop ends never reaches this, so
// it keeps its marks.
func pruneSandboxSilentUnreadable(d *Daemon, live map[string]bool) {
	d.sandboxSilentUnreadable.Range(func(key, _ any) bool {
		if !live[key.(string)] {
			d.sandboxSilentUnreadable.Delete(key)
		}
		return true
	})
}

// silentSandboxSeatItem picks the silent-seat items out of the kind's previous
// items, so a failed collector keeps only its own (collectAttention).
func silentSandboxSeatItem(it attention.Item) bool {
	return it.Kind == attention.KindSandboxSilent && strings.HasPrefix(it.Key, sandboxSilentKeyPrefix)
}
