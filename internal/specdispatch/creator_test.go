package specdispatch

import (
	"slices"
	"strings"
	"testing"
)

// creatorDescription is a shaped spec whose Goal and Constraints are given.
func creatorDescription(goal, constraints string) string {
	return "## Goal\n" + goal + "\n\n## Constraints\n" + constraints + "\n\n## Out of scope\nNothing else.\n\n## Gate\nmake gate\n\n## Size\none worker, one MR"
}

func TestCreatorCites(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		description string
		want        []string
	}{
		{"goal cite", creatorDescription("The file is created by bead gt-aaa.", "Go only."), []string{"gt-aaa"}},
		{"constraints cite", creatorDescription("A thing exists.", "Created by bead gt-bbb."), []string{"gt-bbb"}},
		{"each id once, in order", creatorDescription("created by bead gt-aaa.", "Also created by bead gt-ccc and created by bead gt-aaa."), []string{"gt-aaa", "gt-ccc"}},
		{"bare id mention is not a cite", creatorDescription("gt-aaa blocks this; see gt-bbb.", "Go only."), nil},
		{"cite outside Goal and Constraints is ignored", "## Goal\nA thing exists.\n\n## Constraints\nGo only.\n\n## Out of scope\nThe file is created by bead gt-aaa.\n\n## Gate\nmake gate\n\n## Size\none worker, one MR", nil},
		{"dotted id, sentence period not part of it", creatorDescription("created by bead gt-13dre6.55.", "Go only."), []string{"gt-13dre6.55"}},
		{"multi-hyphen id read whole", creatorDescription("created by bead gt-gastown-crew-goose.", "Go only."), []string{"gt-gastown-crew-goose"}},
		{"hq id with a wisp segment read whole", creatorDescription("created by bead hq-wisp-i1jsk, then tests.", "Go only."), []string{"hq-wisp-i1jsk"}},
		{"underscore id read whole", creatorDescription("created by bead gt-a_b.", "Go only."), []string{"gt-a_b"}},
		{"trailing hyphen or underscore not part of id", creatorDescription("created by bead gt-abc- and created by bead gt-def_.", "Go only."), []string{"gt-abc", "gt-def"}},
		{"no cite", creatorDescription("A thing exists.", "Go only."), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CreatorCites(tc.description); !slices.Equal(got, tc.want) {
				t.Errorf("CreatorCites() = %v, want %v", got, tc.want)
			}
		})
	}
}

// creatorSpec is goodSpec whose Goal cites gt-cite as the creator, with the
// given recorded states.
func creatorSpec(cites ...CreatorCite) Spec {
	s := goodSpec()
	s.ID = "gt-self"
	s.Description = strings.Replace(goodDescription, "A thing exists.", "A thing exists, created by bead gt-cite.", 1)
	s.CreatorCites = cites
	return s
}

func TestLintCreatorCite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		cites []CreatorCite
		route Route
		want  []string
	}{
		{"blocker passes", []CreatorCite{{ID: "gt-cite", State: CreatorBlocker}}, RouteDispatch, nil},
		{"landed passes", []CreatorCite{{ID: "gt-cite", State: CreatorLanded}}, RouteDispatch, nil},
		{"no recorded cites passes", nil, RouteDispatch, nil},
		{"unlinked refused, names cite and dep add", []CreatorCite{{ID: "gt-cite", State: CreatorUnlinked}}, RouteRefuse,
			[]string{"creator cite gt-cite is open and not a blocker", "gt bead dep add gt-self gt-cite"}},
		{"retired split into refused, names reason", []CreatorCite{{ID: "gt-cite", State: CreatorRetired, Reason: "split into gt-new"}}, RouteRefuse,
			[]string{"creator cite gt-cite is retired", `"split into gt-new"`}},
		{"retired superseded refused, names reason", []CreatorCite{{ID: "gt-cite", State: CreatorRetired, Reason: "superseded by gt-new"}}, RouteRefuse,
			[]string{"creator cite gt-cite is retired", `"superseded by gt-new"`}},
		{"retired folded into refused, names reason", []CreatorCite{{ID: "gt-cite", State: CreatorRetired, Reason: "folded into gt-new"}}, RouteRefuse,
			[]string{"creator cite gt-cite is retired", `"folded into gt-new"`}},
		{"retired other reason refused, names reason", []CreatorCite{{ID: "gt-cite", State: CreatorRetired, Reason: "no longer needed"}}, RouteRefuse,
			[]string{"creator cite gt-cite is retired", `"no longer needed"`}},
		{"retired empty reason refused", []CreatorCite{{ID: "gt-cite", State: CreatorRetired}}, RouteRefuse,
			[]string{"creator cite gt-cite is retired", `closed ""`}},
		{"unreadable refused", []CreatorCite{{ID: "gt-cite", State: CreatorUnreadable}}, RouteRefuse,
			[]string{"creator cite gt-cite could not be read"}},
		{"unrecognized state refused", []CreatorCite{{ID: "gt-cite", State: "orphaned"}}, RouteRefuse,
			[]string{"creator cite gt-cite has unrecognized state"}},
		{"cite with no recorded state refused", []CreatorCite{{ID: "gt-other", State: CreatorLanded}}, RouteRefuse,
			[]string{"creator cite gt-cite has no recorded state"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := Lint(creatorSpec(tc.cites...), defaultTemplate())
			if v.Route != tc.route {
				t.Fatalf("Route = %s, want %s: %s", v.Route, tc.route, v.Line("gt-self"))
			}
			var reasons []string
			for _, r := range v.Refusals {
				reasons = append(reasons, r.Reason)
			}
			joined := strings.Join(reasons, "\n")
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Errorf("refusals = %q, want %q", joined, want)
				}
			}
			if tc.route == RouteRefuse && v.Field != "creator cite" {
				t.Errorf("Field = %q, want %q", v.Field, "creator cite")
			}
		})
	}
}

// TestLintCreatorCiteSelf: a bead waits on nothing for the file it creates
// itself, so a cite of its own id passes in any recorded state (gt-qbav5c).
func TestLintCreatorCiteSelf(t *testing.T) {
	t.Parallel()
	const self = "A thing exists, created by bead gt-self."
	own := CreatorCite{ID: "gt-self", State: CreatorUnlinked}
	tests := []struct {
		name, goal string
		cites      []CreatorCite
		want       string
	}{
		{"own cite recorded unlinked passes", self, []CreatorCite{own}, ""},
		{"own cite recorded unreadable passes", self, []CreatorCite{{ID: "gt-self", State: CreatorUnreadable}}, ""},
		{"own cite with no state recorded passes", self, []CreatorCite{{ID: "gt-other", State: CreatorLanded}}, ""},
		{"own cite and another unlinked refused, names only the other", "A thing exists, created by bead gt-self and created by bead gt-other.",
			[]CreatorCite{own, {ID: "gt-other", State: CreatorUnlinked}}, "creator cite gt-other is open and not a blocker"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := creatorSpec(tc.cites...)
			s.Description = strings.Replace(goodDescription, "A thing exists.", tc.goal, 1)
			v := Lint(s, defaultTemplate())
			if tc.want == "" {
				if v.Route != RouteDispatch {
					t.Fatalf("Route = %s, want %s: %s", v.Route, RouteDispatch, v.Line("gt-self"))
				}
				return
			}
			if len(v.Refusals) != 1 || !strings.Contains(v.Refusals[0].Reason, tc.want) {
				t.Fatalf("refusals = %+v, want only %q", v.Refusals, tc.want)
			}
		})
	}
}
