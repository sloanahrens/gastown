package daemon

import (
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/patrolscan"
	"github.com/steveyegge/gastown/internal/session"
)

var _ patrolscan.StreamCutDater = (*patrolScanHost)(nil)

// StreamCut implements patrolscan.StreamCutter (gt-jpmj3s): whether the seat's
// newest transcript ends in a model stream the harness cut short. A sandbox
// seat's record names its session, and the sandbox backend reads the tail there.
// A host seat has no sandbox record and answers false with no error, since its
// transcript is on the host. Every failed read is an error naming the seat, so
// the caller never reads a failure as an intact stream.
func (h *patrolScanHost) StreamCut(rig, name string) (bool, error) {
	rec, sandboxed, err := h.sandboxRecordOf(rig, name)
	if err != nil || !sandboxed {
		return false, err
	}
	cut, err := h.d.sandboxStreamCutFor(rec)
	if err != nil {
		return false, fmt.Errorf("%s/%s: stream of sandbox %s: %w", rig, name, rec.Session, err)
	}
	return cut, nil
}

// StreamCutAt implements patrolscan.StreamCutDater (gt-btfc3p): StreamCut that
// also answers when the cut record was written. It reads the seat through the
// same lookup as StreamCut, so the two never disagree about whether a seat is
// cut. A host seat answers (false, zero time, nil), and a failed read is an
// error, never a cut time.
func (h *patrolScanHost) StreamCutAt(rig, name string) (bool, time.Time, error) {
	rec, sandboxed, err := h.sandboxRecordOf(rig, name)
	if err != nil || !sandboxed {
		return false, time.Time{}, err
	}
	cut, at, err := h.d.sandboxStreamCutAtFor(rec)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("%s/%s: stream of sandbox %s: %w", rig, name, rec.Session, err)
	}
	return cut, at, nil
}

// sandboxRecordOf is the sandbox record of the seat, sandboxed true when the seat
// has one. A host seat answers sandboxed false with no error. The record must name
// a session, since a stream cannot be read without one.
func (h *patrolScanHost) sandboxRecordOf(rig, name string) (session.SandboxRecord, bool, error) {
	sb, err := h.d.overrunSandboxSession(overrunSeat{Rig: rig, Polecat: name})
	if err != nil {
		return session.SandboxRecord{}, false, fmt.Errorf("find the sandbox of %s/%s: %w", rig, name, err)
	}
	if sb.Session == "" {
		return session.SandboxRecord{}, false, nil
	}
	rec := sb.Record
	if !sb.Read {
		read, sandboxed, err := h.d.readSandboxRecordFor(h.d.config.TownRoot, sb.Session)
		if err != nil {
			return session.SandboxRecord{}, false, fmt.Errorf("read the sandbox record of %s/%s: %w", rig, name, err)
		}
		if !sandboxed {
			return session.SandboxRecord{}, false, nil
		}
		rec = read
	}
	if rec.Session == "" {
		return session.SandboxRecord{}, false, fmt.Errorf("%s/%s: its sandbox record names no session, so its stream cannot be read", rig, name)
	}
	return rec, true, nil
}

// sandboxStreamCutFor is the stream-cut read of rec's sandbox: sandboxStreamCutFn's
// when a test set one, else the sandbox backend's.
func (d *Daemon) sandboxStreamCutFor(rec session.SandboxRecord) (bool, error) {
	if d.sandboxStreamCutFn != nil {
		return d.sandboxStreamCutFn(d.config.TownRoot, rec)
	}
	return sandboxStreamCut(d.config.TownRoot, rec)
}

// sandboxStreamCut asks the sandbox backend the record describes whether its
// session's transcript ends in a cut stream. The backend is built from the record's
// profile alone, so no seat lifecycle is needed. A backend that does not answer
// StreamCut is an error, never false.
func sandboxStreamCut(townRoot string, rec session.SandboxRecord) (bool, error) {
	return sandboxStreamCutOver(session.NewOpenShellBackend, townRoot, rec)
}

// sandboxStreamCutOver is sandboxStreamCut over newBackend, so a test can fail the
// backend's construction or hand it a backend that cannot answer.
func sandboxStreamCutOver(newBackend func(townRoot string, p session.Profile) (session.Backend, error), townRoot string, rec session.SandboxRecord) (bool, error) {
	b, err := newBackend(townRoot, session.Profile{Name: rec.Profile, Gateway: rec.Gateway, Image: rec.Image})
	if err != nil {
		return false, err
	}
	sc, ok := b.(interface {
		StreamCut(session string) (bool, error)
	})
	if !ok {
		return false, errors.New("the sandbox backend cannot read a stream cut")
	}
	return sc.StreamCut(rec.Session)
}

// sandboxStreamCutAtFor is the stream-cut read with its time of rec's sandbox:
// sandboxStreamCutAtFn's when a test set one, else the sandbox backend's.
func (d *Daemon) sandboxStreamCutAtFor(rec session.SandboxRecord) (bool, time.Time, error) {
	if d.sandboxStreamCutAtFn != nil {
		return d.sandboxStreamCutAtFn(d.config.TownRoot, rec)
	}
	return sandboxStreamCutAt(d.config.TownRoot, rec)
}

// sandboxStreamCutAt asks the sandbox backend the record describes whether its
// session's transcript ends in a cut stream, and when that cut was written. A
// backend that does not answer StreamCutAt is an error, never false.
func sandboxStreamCutAt(townRoot string, rec session.SandboxRecord) (bool, time.Time, error) {
	return sandboxStreamCutAtOver(session.NewOpenShellBackend, townRoot, rec)
}

// sandboxStreamCutAtOver is sandboxStreamCutAt over newBackend, so a test can fail
// the backend's construction or hand it a backend that cannot answer.
func sandboxStreamCutAtOver(newBackend func(townRoot string, p session.Profile) (session.Backend, error), townRoot string, rec session.SandboxRecord) (bool, time.Time, error) {
	b, err := newBackend(townRoot, session.Profile{Name: rec.Profile, Gateway: rec.Gateway, Image: rec.Image})
	if err != nil {
		return false, time.Time{}, err
	}
	sc, ok := b.(interface {
		StreamCutAt(session string) (bool, time.Time, error)
	})
	if !ok {
		return false, time.Time{}, errors.New("the sandbox backend cannot read the time of a stream cut")
	}
	return sc.StreamCutAt(rec.Session)
}
