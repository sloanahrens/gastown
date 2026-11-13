package polecat

// SessionReader answers the has-session question a Manager asks while it lists
// its seats and reuses one. A sling installs a reader that answers through its
// sandbox census (ShareSessionProbes); with none installed the Manager asks its
// own tmux client, as it always has.
type SessionReader interface {
	HasSession(name string) (bool, error)
}

// ShareSessionProbes makes this Manager answer its has-session reads through
// reader until the returned end func is called. The sling's reuse step opens it
// around its seat listing and reuse. A nil reader installs nothing, and every
// read goes to the Manager's own tmux client.
func (m *Manager) ShareSessionProbes(reader SessionReader) (end func()) {
	m.sessionReader = reader
	return func() { m.sessionReader = nil }
}

// hasSession is the has-session read through the installed reader, if any, and
// otherwise through the Manager's tmux client. Callers keep their own nil check
// on m.tmux, which the reader does not replace.
func (m *Manager) hasSession(name string) (bool, error) {
	if m.sessionReader != nil {
		return m.sessionReader.HasSession(name)
	}
	return m.tmux.HasSession(name)
}
