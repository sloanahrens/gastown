package cmd

import (
	"errors"
	"reflect"
	"testing"
)

// liveSessions is the set of sessions a shutdown test's fake tmux still has.
type liveSessions map[string]bool

func (l liveSessions) HasSession(name string) (bool, error) { return l[name], nil }

// A shutdown deletes a sandboxed session's sandbox once its session is gone,
// and only then: a session that survived the kill keeps its sandbox, and one
// that was already gone was not this shutdown's to clean.
func TestKillSessionsInOrderCleansUpAfterTheKill(t *testing.T) {
	t.Parallel()
	live := liveSessions{"gt-a": true, "gt-stuck": true, "gt-b": true}
	var events []string
	st := shutdownStopper{
		stop: func(sess string) error {
			events = append(events, "stop "+sess)
			if sess != "gt-stuck" {
				delete(live, sess)
			}
			return nil
		},
		cleanup: func(sess string) error {
			events = append(events, "cleanup "+sess)
			if sess == "gt-b" {
				return errors.New("gateway unreachable")
			}
			return nil
		},
	}

	stopped := killSessionsInOrder(live, []string{"gt-a", "gt-gone", "gt-stuck", "gt-b"}, st)

	want := []string{"stop gt-a", "cleanup gt-a", "stop gt-stuck", "stop gt-b", "cleanup gt-b"}
	if !reflect.DeepEqual(events, want) {
		t.Errorf("events = %v, want %v", events, want)
	}
	if stopped != 2 {
		t.Errorf("stopped = %d, want 2 (gt-a, and gt-b whose sandbox cleanup failed); gt-stuck survived, gt-gone was already gone", stopped)
	}
}
