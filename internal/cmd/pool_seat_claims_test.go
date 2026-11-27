package cmd

import (
	"reflect"
	"testing"
)

// TestWithSeatClaims pins which claims an occupied seat already counts
// (gt-sofi3m): a sling's claim and the seat its spawn hooks are one seat
// (gt-il6mm7), matched on (agent, bead), one claim per covering seat.
func TestWithSeatClaims(t *testing.T) {
	t.Parallel()
	at := func(name, agent, bead string) poolSession {
		return poolSession{name: name, agent: agent, hookedBead: bead}
	}
	claim := func(name, agent, bead string) poolSession {
		return poolSession{name: name, agent: agent, claim: true, hookedBead: bead}
	}
	names := func(seats []poolSession) []string {
		out := make([]string, len(seats))
		for i, s := range seats {
			out[i] = s.name
		}
		return out
	}
	for _, tc := range []struct {
		name     string
		sessions []poolSession
		claims   []poolSession
		want     []string
	}{
		{"a claim whose seat hooks its bead is left out", []poolSession{at("gt-ember", "a", "gt-a")}, []poolSession{claim("c1", "a", "gt-a")}, []string{"gt-ember"}},
		{"a second claim for the bead one seat hooks is kept", []poolSession{at("gt-ember", "a", "gt-a")}, []poolSession{claim("c1", "a", "gt-a"), claim("c2", "a", "gt-a")}, []string{"gt-ember", "c2"}},
		{"two seats hooking the bead drop two claims", []poolSession{at("gt-ember", "a", "gt-a"), at("gt-ruby", "a", "gt-a")}, []poolSession{claim("c1", "a", "gt-a"), claim("c2", "a", "gt-a")}, []string{"gt-ember", "gt-ruby"}},
		{"a claim that names no bead is kept", []poolSession{at("gt-ember", "a", "gt-a")}, []poolSession{claim("c1", "a", "")}, []string{"gt-ember", "c1"}},
		{"a claim whose seat runs on another agent is kept", []poolSession{at("gt-ember", "b", "gt-a")}, []poolSession{claim("c1", "a", "gt-a")}, []string{"gt-ember", "c1"}},
		{"a claim whose matching seat is parked stale is kept", []poolSession{{name: "dead/gastown/mariner", agent: "a", parkedStale: true, hookedBead: "gt-a"}}, []poolSession{claim("c1", "a", "gt-a")}, []string{"dead/gastown/mariner", "c1"}},
		{"a claim whose matching seat is itself a claim is kept", []poolSession{claim("c0", "a", "gt-a")}, []poolSession{claim("c1", "a", "gt-a")}, []string{"c0", "c1"}},
		{"a claim whose matching seat hooks no bead is kept", []poolSession{at("gt-ember", "a", "")}, []poolSession{claim("c1", "a", "gt-a")}, []string{"gt-ember", "c1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := names(withSeatClaims(tc.sessions, tc.claims)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("withSeatClaims = %v, want %v", got, tc.want)
			}
		})
	}
}
