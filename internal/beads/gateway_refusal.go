package beads

import (
	"strings"
	"sync"
)

// gatewaySQLVerb is the bd verb a gateway refusal names, kept out of a call's
// arguments so internal/beadsql's guard does not read it as a hand-built bd
// argv (gt-kybol9).
const gatewaySQLVerb = "sql"

// gatewaySQLRefused reports whether err is the bd gateway's HTTP 403 refusal
// of a bd sql call, by the wording gtwrap produces: the status and the verb
// both have to be the gateway's, as with allowStaleAnswer's --allow-stale
// answer (gt-d3v0iu). Any other error is not a refusal (gt-kybol9).
func gatewaySQLRefused(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "refused the call with HTTP 403") && strings.Contains(msg, gatewaySQLVerb)
}

// SQLRefusalMemory remembers the bd gateway's refusal of a bd sql read, so a
// read the gateway already denied is not sent again (gt-gahru3). The zero value
// is ready to use and safe for concurrent use.
//
// A refusal is kept for the lifetime of the memory, so a caller that must retry
// a refused read after a token or policy change uses a fresh memory.
type SQLRefusalMemory struct {
	mu  sync.Mutex
	err error
}

// Run calls read, unless an earlier run's error satisfied gatewaySQLRefused:
// then it returns that stored error without calling read. A nil memory calls
// read every time.
func (m *SQLRefusalMemory) Run(read func() ([]byte, error)) ([]byte, error) {
	if m == nil {
		return read()
	}
	if err := m.refusal(); err != nil {
		return nil, err
	}
	out, err := read()
	if gatewaySQLRefused(err) {
		m.mu.Lock()
		if m.err == nil {
			m.err = err
		}
		m.mu.Unlock()
	}
	return out, err
}

// refusal returns the stored refusal, or nil when no run has been refused yet.
func (m *SQLRefusalMemory) refusal() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}
