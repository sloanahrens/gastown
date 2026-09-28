package tmux

import (
	"context"
	"reflect"
	"testing"
)

type call struct {
	name string
	args []string
}

// recorder returns an execFunc that records calls and answers from out.
func recorder(out map[string]string) (execFunc, *[]call) {
	var calls []call
	return func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
		calls = append(calls, call{name, args})
		return []byte(out[name]), nil, nil
	}, &calls
}

func TestHasSessionSendsHasSession(t *testing.T) {
	t.Parallel()
	ex, calls := recorder(nil)
	tm := newTmuxForTest("gt-test-x", ex, nil)
	ok, err := tm.HasSession("alpha")
	if err != nil || !ok {
		t.Fatalf("HasSession = %v, %v; want true, nil", ok, err)
	}
	want := call{"tmux", []string{"-u", "-L", "gt-test-x", "has-session", "-t", "=alpha"}}
	if len(*calls) == 0 || !reflect.DeepEqual((*calls)[0], want) {
		t.Fatalf("calls = %+v, want first %+v", *calls, want)
	}
}
