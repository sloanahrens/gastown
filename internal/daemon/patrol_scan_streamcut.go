package daemon

import (
	"errors"
	"fmt"

	"github.com/steveyegge/gastown/internal/session"
)

// StreamCut implements patrolscan.StreamCutter (gt-jpmj3s): whether the seat's
// newest transcript ends in a model stream the harness cut short. A sandbox
// seat's record names its session, and the sandbox backend reads the tail there.
// A host seat has no sandbox record and answers false with no error, since its
// transcript is on the host. Every failed read is an error naming the seat, so
// the caller never reads a failure as an intact stream.
func (h *patrolScanHost) StreamCut(rig, name string) (bool, error) {
	sb, err := h.d.overrunSandboxSession(overrunSeat{Rig: rig, Polecat: name})
	if err != nil {
		return false, fmt.Errorf("find the sandbox of %s/%s: %w", rig, name, err)
	}
	if sb.Session == "" {
		return false, nil
	}
	rec := sb.Record
	if !sb.Read {
		read, sandboxed, err := h.d.readSandboxRecordFor(h.d.config.TownRoot, sb.Session)
		if err != nil {
			return false, fmt.Errorf("read the sandbox record of %s/%s: %w", rig, name, err)
		}
		if !sandboxed {
			return false, nil
		}
		rec = read
	}
	if rec.Session == "" {
		return false, fmt.Errorf("%s/%s: its sandbox record names no session, so its stream cannot be read", rig, name)
	}
	cut, err := h.d.sandboxStreamCutFor(rec)
	if err != nil {
		return false, fmt.Errorf("%s/%s: stream of sandbox %s: %w", rig, name, rec.Session, err)
	}
	return cut, nil
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
	b, err := session.NewOpenShellBackend(townRoot, session.Profile{Name: rec.Profile, Gateway: rec.Gateway, Image: rec.Image})
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
