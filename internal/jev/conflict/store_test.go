package conflict

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
)

// A refusal of the account rather than the request (402, 403) is no bead
// fault: it costs the pair no attempt, the cursor holds before its placement,
// and the pair is asked again once the account can answer. However many passes
// are refused, the pair is never given up.
func TestPassAccountRefusalHoldsTheCursor(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusPaymentRequired, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			refused := true
			asker := &fakeAsker{fail: func(string) error {
				if refused {
					return &jev.StatusError{Status: status}
				}
				return nil
			}}
			p := newPass(t, []*beads.Issue{placed("gt-a", 10, 0), placed("gt-b", 20, 0)}, asker, t0)

			// One pass more than maxAttempts: a count that ends a bead fault
			// must not touch an account refusal.
			for pass := 1; pass <= maxAttempts+1; pass++ {
				if _, err := p.Run(context.Background()); err == nil {
					t.Fatalf("pass %d: a %d must stop the pass, not count against the pair", pass, status)
				}
				st, err := readPairs(p.PairsPath)
				if err != nil {
					t.Fatal(err)
				}
				if len(st.Done) != 0 || len(st.Attempts) != 0 {
					t.Fatalf("pass %d: state = %+v; a %d must leave the pair neither done nor attempted", pass, st, status)
				}
				cur, _, err := battery.ReadCursor(p.CursorPath)
				if err != nil {
					t.Fatal(err)
				}
				if cur.ID != "gt-a" {
					t.Fatalf("pass %d: cursor = %+v; it must hold before gt-b, whose pair is retried", pass, cur)
				}
			}

			refused = false
			sum, err := p.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if sum.Asked != 1 || sum.GivenUp != 0 {
				t.Errorf("after the account recovered = %+v, want the pair asked once and nothing given up", sum)
			}
		})
	}
}

// A 400 on one pair's own text is still a bead fault, so the pair spends its
// attempts and is given up after maxAttempts: the account refusals did not
// turn every 4xx into a hold. TestPassRetriesABeadFaultThenGivesUp follows the
// per-pass progression; this pins the classification from the pass's side.
func TestPassBadPairIsStillABeadFault(t *testing.T) {
	t.Parallel()
	asker := &fakeAsker{fail: func(state string) error {
		if strings.Contains(state, "Title of gt-b") {
			return &jev.StatusError{Status: http.StatusBadRequest}
		}
		return nil
	}}
	p := newPass(t, []*beads.Issue{placed("gt-a", 10, 0), placed("gt-b", 20, 0)}, asker, t0)

	for pass := 1; pass <= maxAttempts; pass++ {
		if _, err := p.Run(context.Background()); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	st, err := readPairs(p.PairsPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Done) != 1 || len(st.Attempts) != 0 {
		t.Errorf("state after %d passes = %+v, want the pair done and no attempts left", maxAttempts, st)
	}
	cur, _, err := battery.ReadCursor(p.CursorPath)
	if err != nil {
		t.Fatal(err)
	}
	if cur.ID != "gt-b" {
		t.Errorf("cursor = %+v, want it moved past gt-b once the pair is given up", cur)
	}
}

// beadFault tells a refusal of the request from everything else, which holds.
func TestBeadFaultClassifiesStatuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"400 bad request", &jev.StatusError{Status: http.StatusBadRequest}, true},
		{"422 unprocessable", &jev.StatusError{Status: http.StatusUnprocessableEntity}, true},
		{"408 request timeout", &jev.StatusError{Status: http.StatusRequestTimeout}, false},
		{"402 payment required", &jev.StatusError{Status: http.StatusPaymentRequired}, false},
		{"403 forbidden", &jev.StatusError{Status: http.StatusForbidden}, false},
		{"401 unauthorized", jev.ErrUnauthorized, false},
		{"500 server error", &jev.StatusError{Status: http.StatusInternalServerError}, false},
		{"unusable answer", jev.ErrBadResponse, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := beadFault(c.err); got != c.want {
				t.Errorf("beadFault(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
