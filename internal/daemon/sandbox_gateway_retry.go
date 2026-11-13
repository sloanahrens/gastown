package daemon

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/session"
)

// gatewayNoAnswerReason opens the keep reason of a seat whose gateway did not
// answer. The full reason also names the time of the last try, so a reader
// matches it by this prefix (gt-7h38pd).
const gatewayNoAnswerReason = "gateway did not answer"

// stoppedUnlandedReason is the keep reason of a Stopped sandbox whose seat's
// landing record does not prove the work landed. The sandbox stays (gt-7h38pd).
const stoppedUnlandedReason = "stopped, work not proven landed"

// sandboxKeptRecordStaleReason is the keep reason of a Stopped sandbox whose kept
// record names a bead or branch the seat no longer holds, so the seat was slung
// again since the landing. Its arguments are the recorded bead and branch, then
// the bead and branch the seat holds now (gt-7h38pd).
const sandboxKeptRecordStaleReason = "kept record names %s/%s, the seat now holds %s/%s"

// defaultSandboxGatewayRetryTicks is how many ticks a kept seat's gateway may
// stay silent before the queue raises the seat's item (gt-7h38pd).
const defaultSandboxGatewayRetryTicks = 6

// gatewayRetryLimit is the tick count at which a silent gateway's kept seat is
// raised: Daemon.sandboxGatewayRetryLimit when set, else the default.
func (d *Daemon) gatewayRetryLimit() int {
	if d.sandboxGatewayRetryLimit > 0 {
		return d.sandboxGatewayRetryLimit
	}
	return defaultSandboxGatewayRetryTicks
}

// gatewayNoAnswerKeepReason is the reason of a keep whose last gateway question
// went unanswered at last.
func gatewayNoAnswerKeepReason(last time.Time) string {
	return gatewayNoAnswerReason + " (last try " + last.UTC().Format(time.RFC3339) + ")"
}

// isGatewayNoAnswerKeep reports whether a keep reason is a gateway keep.
func isGatewayNoAnswerKeep(reason string) bool {
	return strings.HasPrefix(reason, gatewayNoAnswerReason)
}

// isStoppedPhase reports whether a gateway phase is Stopped.
func isStoppedPhase(phase string) bool {
	return strings.EqualFold(strings.TrimSpace(phase), "stopped")
}

// readKeptSeatFor reads the kept record of rig/name. A seat that is not kept
// reads as ok false with no error; a record that cannot be read is an error.
func (h *patrolScanHost) readKeptSeatFor(rig, name string) (keptSandboxSeatRecord, bool, error) {
	path, err := keptSandboxSeatPath(h.town(), rig, name)
	if err != nil {
		return keptSandboxSeatRecord{}, false, err
	}
	kept, err := readKeptSandboxSeatRecord(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return keptSandboxSeatRecord{}, false, nil
	case err != nil:
		return keptSandboxSeatRecord{}, false, err
	}
	return kept, true, nil
}

// gatewayTally is one kept seat's silent-gateway count. tries is the patrol ticks
// the gateway has stayed silent since it last answered. alarm is set when the
// count cannot be kept in the seat's record: the kept record cannot be read or
// written. The attention queue raises the seat's item at once, naming alarm,
// without waiting for the count (gt-7h38pd).
type gatewayTally struct {
	tries int
	alarm string
}

// gatewayTallyKey names a seat in Daemon.gatewayTallies.
func gatewayTallyKey(rig, name string) string {
	return rig + "/" + name
}

// updateGatewayTally applies fn to the tally of rig/name under the daemon's lock.
func (d *Daemon) updateGatewayTally(rig, name string, fn func(*gatewayTally)) {
	d.gatewayTalliesMu.Lock()
	defer d.gatewayTalliesMu.Unlock()
	if d.gatewayTallies == nil {
		d.gatewayTallies = make(map[string]gatewayTally)
	}
	key := gatewayTallyKey(rig, name)
	t := d.gatewayTallies[key]
	fn(&t)
	d.gatewayTallies[key] = t
}

// clearGatewayTally forgets rig/name's tally: its gateway answered, or the seat
// was released.
func (d *Daemon) clearGatewayTally(rig, name string) {
	d.gatewayTalliesMu.Lock()
	defer d.gatewayTalliesMu.Unlock()
	delete(d.gatewayTallies, gatewayTallyKey(rig, name))
}

// gatewayTallySnapshot copies every tally, keyed by gatewayTallyKey.
func (d *Daemon) gatewayTallySnapshot() map[string]gatewayTally {
	d.gatewayTalliesMu.Lock()
	defer d.gatewayTalliesMu.Unlock()
	out := make(map[string]gatewayTally, len(d.gatewayTallies))
	for k, v := range d.gatewayTallies {
		out[k] = v
	}
	return out
}

// alarmGatewayTries sets the alarm of rig/name's tally to why the count cannot
// be kept. The item the queue raises names that reason.
func (h *patrolScanHost) alarmGatewayTries(rig, name string, err error) {
	reason := fmt.Sprintf("cannot count gateway tries for %s/%s: %v", rig, name, err)
	h.d.updateGatewayTally(rig, name, func(t *gatewayTally) { t.alarm = reason })
}

// writeKeptSeat writes rig/name's kept record through the host's seam, else
// writeKeptSandboxSeat.
func (h *patrolScanHost) writeKeptSeat(rig, name string, rec keptSandboxSeatRecord) error {
	if h.keptSeatWrite != nil {
		return h.keptSeatWrite(h.town(), rig, name, rec)
	}
	return writeKeptSandboxSeat(h.town(), rig, name, rec)
}

// recordGatewayNoAnswer keeps rig/name's seat because its gateway did not answer
// at now, with landing as the seat's landing facts so a later pass can finish
// the seat's end. A keep already for the gateway keeps its since. tick is true
// for a patrol tick that found the gateway silent, and false for the landing's
// own question, which is the first try and not a tick. A keep that cannot be
// read is dated from now. The count of ticks is kept in memory as well as the
// record: when the record cannot be written, the tally's alarm raises the seat
// (gt-7h38pd).
func (h *patrolScanHost) recordGatewayNoAnswer(rig, name string, landing sandboxSeatLanding, tick bool) {
	now := h.d.clk().Now().UTC()
	since, prevTries := now, 0
	prev, ok, err := h.readKeptSeatFor(rig, name)
	if err != nil {
		h.d.logger.Printf("sandbox seat end %s/%s: its kept record is unreadable, so the gateway keep is dated from now: %v", rig, name, err)
	}
	if ok && isGatewayNoAnswerKeep(prev.Reason) {
		since, prevTries = prev.Since, prev.Tries
	}
	tries := prevTries
	h.d.updateGatewayTally(rig, name, func(t *gatewayTally) {
		if tick {
			t.tries = max(t.tries, prevTries) + 1
		}
		tries = max(tries, t.tries)
	})
	rec := keptSandboxSeatRecord{Bead: landing.BeadID, Reason: gatewayNoAnswerKeepReason(now), Since: since, Tries: tries, Landing: &landing}
	if err := h.writeKeptSeat(rig, name, rec); err != nil {
		h.alarmGatewayTries(rig, name, err)
		h.d.logger.Printf("sandbox seat end %s/%s: cannot record the gateway keep: %v; the seat is kept all the same", rig, name, err)
	}
}

// countGatewayTry counts a patrol tick that found the gateway of the sandbox rec
// names silent. A seat whose end is recorded with landing facts is counted. A
// record that cannot be read raises the tally's alarm on this tick, since its
// tries cannot be kept. Any other seat is asked again on the next tick with
// nothing recorded (gt-7h38pd).
func (h *patrolScanHost) countGatewayTry(rec session.SandboxRecord) {
	if rec.Rig == "" || rec.Polecat == "" {
		return
	}
	kept, ok, err := h.readKeptSeatFor(rec.Rig, rec.Polecat)
	switch {
	case err != nil:
		h.alarmGatewayTries(rec.Rig, rec.Polecat, err)
		h.d.logger.Printf("patrol_scan: sandbox record %s: its kept record cannot be read, so the gateway try is not counted: %v", rec.Session, err)
	case !ok || kept.Landing == nil:
		return
	default:
		h.recordGatewayNoAnswer(rec.Rig, rec.Polecat, *kept.Landing, true)
	}
}

// settleGatewayKeep replaces a gateway keep with the reason the seat's end gives
// once the gateway has answered and the end keeps the seat, so the keep does not
// go on saying the gateway is silent (gt-7h38pd). A keep for any other reason is
// left as it is.
func (h *patrolScanHost) settleGatewayKeep(rec session.SandboxRecord, why string) {
	if rec.Rig == "" || rec.Polecat == "" {
		return
	}
	kept, ok, err := h.readKeptSeatFor(rec.Rig, rec.Polecat)
	if err != nil {
		h.d.logger.Printf("patrol_scan: sandbox record %s: its kept record cannot be read, so its gateway keep is not settled: %v", rec.Session, err)
		return
	}
	if !ok || kept.Landing == nil || !isGatewayNoAnswerKeep(kept.Reason) {
		return
	}
	h.recordKeptSeat(rec.Rig, rec.Polecat, *kept.Landing, why)
}
