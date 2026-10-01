package mail

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/steveyegge/gastown/internal/beads"
)

// bdScript is a bdRunner for the unit tier: it records every bd call
// and answers it with answer (stdout, stderr and an exit code, 0 for
// success). A nil answer prints nothing and succeeds.
type bdScript struct {
	mu     sync.Mutex
	calls  []bdCall
	answer func(c bdCall) (stdout, stderr string, code int)
}

func (s *bdScript) run(_ context.Context, c bdCall) ([]byte, []byte, error) {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	answer := s.answer
	s.mu.Unlock()
	if answer == nil {
		return nil, nil, nil
	}
	stdout, stderr, code := answer(c)
	if code != 0 {
		return []byte(stdout), []byte(stderr), bdExit(code)
	}
	return []byte(stdout), []byte(stderr), nil
}

// recorded returns the calls made so far.
func (s *bdScript) recorded() []bdCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bdCall(nil), s.calls...)
}

// argvs returns each call's arguments joined by spaces.
func (s *bdScript) argvs() []string {
	var out []string
	for _, c := range s.recorded() {
		out = append(out, strings.Join(c.Args, " "))
	}
	return out
}

// bdExit is a bd exit status, matched like *exec.ExitError.
type bdExit int

func (e bdExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e bdExit) ExitCode() int { return int(e) }

// noBeadsDatabase answers every bd call as bd does in a directory with no
// beads database.
func noBeadsDatabase(context.Context, bdCall) ([]byte, []byte, error) {
	return nil, []byte("Error: no beads database found\nHint: run 'bd where' to inspect the resolved workspace, run 'bd doctor' to diagnose, or 'bd init' to create a new database\n      or set BEADS_DIR to point to your .beads directory\n"), bdExit(1)
}

// noTownBeads is a town with no channel or agent beads: no channel exists
// and no agent is muted.
type noTownBeads struct{}

func (noTownBeads) GetChannelBead(string) (*beads.Issue, *beads.ChannelFields, error) {
	return nil, nil, nil
}

func (noTownBeads) GetAgentNotificationLevel(string) (string, error) {
	return "", beads.ErrNotFound
}

func (noTownBeads) EnforceChannelRetention(string) error { return nil }
