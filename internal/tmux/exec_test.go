package tmux

import (
	"reflect"
	"testing"
)

func TestHasSessionSendsHasSession(t *testing.T) {
	t.Parallel()
	s := newScripted(nil)
	tm := newTmuxForTest("gt-test-x", s.exec, nil)
	ok, err := tm.HasSession("alpha")
	if err != nil || !ok {
		t.Fatalf("HasSession = %v, %v; want true, nil", ok, err)
	}
	want := tmuxCall{name: "tmux", socket: "gt-test-x", args: []string{"has-session", "-t", "=alpha"}}
	if calls := s.all(); len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}
