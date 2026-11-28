package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/steveyegge/gastown/internal/tmux"
)

// roleErrLister is fakeLister with the GT_ROLE read of the named sessions
// returning a chosen error, so a test can drive either warning branch.
type roleErrLister struct {
	*fakeLister
	roleErrs map[string]error
}

func (l *roleErrLister) GetEnvironment(session, key string) (string, error) {
	if err, ok := l.roleErrs[session]; ok {
		return "", err
	}
	return l.fakeLister.GetEnvironment(session, key)
}

// TestPoolNotFoundSession pins gt-566kfn: a listed session tmux reports as
// gone is named as that, not as a failed GT_ROLE read; any other error keeps
// today's line. Both leave the session out and keep the other, and comparing
// the error with == turns the wrapped row red.
func TestPoolNotFoundSession(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, want string
		roleErr    error
	}{
		{"wrapped not-found is a listed session",
			"warning: session gt-capable is listed but tmux reports no such session; not counted as live\n",
			fmt.Errorf("show-environment: %w", tmux.ErrSessionNotFound)},
		{"another read error keeps today's line",
			"warning: session gt-capable's GT_ROLE cannot be read, leaving it out of the live read: unknown variable\n",
			errors.New("unknown variable")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			var warn bytes.Buffer
			lister := &roleErrLister{
				fakeLister: &fakeLister{sessions: map[string]map[string]string{
					"gt-marble":  {"GT_ROLE": "gastown/polecats/marble"},
					"gt-capable": {"GT_ROLE": "gastown/polecats/capable"},
				}},
				roleErrs: map[string]error{"gt-capable": c.roleErr},
			}
			got, err := listPolecatSessionsTo(lister, nil, nil, poolTestNow, &warn)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].name != "gt-marble" {
				t.Fatalf("sessions = %v, want only gt-marble", got)
			}
			if warn.String() != c.want {
				t.Fatalf("warning = %q, want %q", warn.String(), c.want)
			}
		})
	}
}
