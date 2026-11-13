package polecat

import (
	"errors"

	"github.com/steveyegge/gastown/internal/session"
)

// SessionReader answers the has-session question a Manager asks while it lists
// its seats and reuses one. A sling installs a reader that answers through its
// sandbox census (ShareSessionProbes); with none installed the Manager asks its
// own tmux client, as it always has.
type SessionReader interface {
	HasSession(name string) (bool, error)
}

// sessionReaderSlot is one installed reader. The Manager holds a pointer to it,
// so an install and its end func swap whole slots, and a read never sees a
// reader torn in half.
type sessionReaderSlot struct {
	reader SessionReader
}

// ShareSessionProbes makes this Manager answer its has-session reads through
// reader until the returned end func is called. The sling's reuse step opens it
// around its seat listing and reuse. A nil reader installs nothing. The end func
// restores the reader that was installed before this call, so nested installs
// unwind in order; with none before, every read goes to the Manager's own tmux
// client again.
func (m *Manager) ShareSessionProbes(reader SessionReader) (end func()) {
	var next *sessionReaderSlot
	if reader != nil {
		next = &sessionReaderSlot{reader: reader}
	}
	prev := m.sessionReader.Swap(next)
	return func() { m.sessionReader.Store(prev) }
}

// hasSession is the has-session read through the installed reader, if any, and
// otherwise through the Manager's tmux client. A reader that answers
// session.ErrSeatNotLive is a seat with no server to ask, so the read is false
// with no tmux read. Any other reader error means the reader could not answer
// (a listing or probe failed), and the read takes the Manager's own tmux client
// as it does with no reader. Callers keep their own nil check on m.tmux, which
// the reader does not replace.
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
	return m.tmux.HasSession(name)
}
