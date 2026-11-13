package polecat

import (
	"errors"

	"github.com/steveyegge/gastown/internal/session"
)

// SessionReader answers the has-session question a Manager asks while it lists
// its seats and reuses one. A sling installs a reader that answers through its
// sandbox census (SetSessionProbes); with none installed the Manager asks its
// own tmux client, as it always has.
type SessionReader interface {
	HasSession(name string) (bool, error)
}

// sessionReaderSlot holds one installed reader. The Manager stores it whole
// through an atomic pointer, so a concurrent read never sees a torn value.
type sessionReaderSlot struct {
	reader SessionReader
}

// SetSessionProbes makes this Manager answer its has-session reads through
// reader until a later call replaces it, and through its own tmux client again
// when reader is nil. The sling calls it once, on the manager openSlingRig
// builds for the command, and nowhere else: that manager, the command and the
// census its reader consults all end together, so the seam needs no end of its
// own.
func (m *Manager) SetSessionProbes(reader SessionReader) {
	if reader == nil {
		m.sessionReader.Store(nil)
		return
	}
	m.sessionReader.Store(&sessionReaderSlot{reader: reader})
}

// hasSession is the has-session read through the installed reader, if any, and
// otherwise through the Manager's tmux client. A reader that answers
// session.ErrSeatNotLive is a seat with no server to ask, so the read is false
// with no tmux read. Any other reader error means the reader could not answer
// (a listing or probe failed), and the read takes the Manager's own tmux client
// as it does with no reader. A Manager with no client either answers not
// running.
func (m *Manager) hasSession(name string) (bool, error) {
	if slot := m.sessionReader.Load(); slot != nil {
		running, err := slot.reader.HasSession(name)
		switch {
		case err == nil:
			return running, nil
		case errors.Is(err, session.ErrSeatNotLive):
			return false, nil
		}
	}
	if m.tmux == nil {
		return false, nil
	}
	return m.tmux.HasSession(name)
}
