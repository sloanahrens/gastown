package git

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// reply is a canned answer to one git call: what git printed and how it
// exited (0 is success).
type reply struct {
	stdout string
	stderr string
	code   int
}

// ok is a successful reply printing stdout.
func ok(stdout string) reply { return reply{stdout: stdout} }

// fail is a failed reply: git's exit status and its stderr.
func fail(code int, stderr string) reply { return reply{stderr: stderr, code: code} }

// gitExit is a non-zero git exit, matched like *exec.ExitError.
type gitExit int

func (e gitExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e gitExit) ExitCode() int { return int(e) }

// scripted is a runFunc that answers git calls from a table and records every
// call it was sent. A key is the call's arguments joined by spaces; a key
// ending in " *" matches any call starting with the rest. Each key holds a
// queue of replies: a call takes the next one, and the last one repeats. A
// call no key matches exits 128 with "unscripted git call" on stderr and is
// recorded in unscripted.
type scripted struct {
	mu         sync.Mutex
	answers    map[string][]reply
	calls      []gitCall
	unscripted []string
}

func newScripted(answers map[string]reply) *scripted {
	s := &scripted{answers: map[string][]reply{}}
	for k, r := range answers {
		s.answers[k] = []reply{r}
	}
	return s
}

// on sets key's replies, replacing any it had: successive calls receive them
// in order, and the last one repeats.
func (s *scripted) on(key string, rs ...reply) *scripted {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers[key] = rs
	return s
}

func (s *scripted) run(c gitCall) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
	joined := strings.Join(c.args, " ")
	key, found := joined, false
	if _, found = s.answers[joined]; !found {
		best := -1
		for k := range s.answers {
			prefix, isPrefix := strings.CutSuffix(k, " *")
			if isPrefix && (joined == prefix || strings.HasPrefix(joined, prefix+" ")) && len(prefix) > best {
				key, best, found = k, len(prefix), true
			}
		}
	}
	if !found {
		s.unscripted = append(s.unscripted, joined)
		return "", "unscripted git call: " + joined + "\n", gitExit(128)
	}
	queue := s.answers[key]
	r := queue[0]
	if len(queue) > 1 {
		s.answers[key] = queue[1:]
	}
	if r.code != 0 {
		return r.stdout, r.stderr, gitExit(r.code)
	}
	return r.stdout, r.stderr, nil
}

// sent returns the joined argv of every call, in order.
func (s *scripted) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	for i, c := range s.calls {
		out[i] = strings.Join(c.args, " ")
	}
	return out
}

// sentCall returns the first call whose joined argv is args, or false.
func (s *scripted) sentCall(args string) (gitCall, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if strings.Join(c.args, " ") == args {
			return c, true
		}
	}
	return gitCall{}, false
}

// hasSent reports whether a call with exactly args was sent.
func (s *scripted) hasSent(args string) bool {
	_, found := s.sentCall(args)
	return found
}

// noUnscripted fails t when any call went unanswered.
func (s *scripted) noUnscripted(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.unscripted) > 0 {
		t.Fatalf("unscripted git calls: %q", s.unscripted)
	}
}

// newTestGit returns a Git on a fresh temp working directory that sends
// every git call to s.
func newTestGit(t *testing.T, s *scripted) *Git {
	t.Helper()
	return &Git{workDir: t.TempDir(), exec: s.run}
}
