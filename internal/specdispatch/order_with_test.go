package specdispatch

import (
	"strings"
	"testing"
)

// orderedIDs runs OrderWith over specs and returns the ids in the order they
// come out, space-separated, so a row reads as the ranking it asserts.
func orderedIDs(specs []Spec, eff map[string]Inherited) string {
	OrderWith(specs, eff)
	ids := make([]string, 0, len(specs))
	for _, s := range specs {
		ids = append(ids, s.ID)
	}
	return strings.Join(ids, " ")
}

func TestOrderWith(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		specs []Spec
		eff   map[string]Inherited
		want  string
	}{
		{
			// gt-inh is P2 on its own, but blocks a P1 bead: it ranks at P1, so
			// it goes after the P0 and before the ordinary P2, though it was
			// created after the ordinary P2.
			name: "inheriting P2 sorts before an ordinary P2 and after a P0",
			specs: []Spec{
				{ID: "gt-ord", Priority: 2, CreatedAt: "2026-09-29T10:00:00Z"},
				{ID: "gt-inh", Priority: 2, CreatedAt: "2026-09-29T12:00:00Z"},
				{ID: "gt-p0", Priority: 0, CreatedAt: "2026-09-29T13:00:00Z"},
			},
			eff:  map[string]Inherited{"gt-inh": {Priority: 1, From: "gt-blocked"}},
			want: "gt-p0 gt-inh gt-ord",
		},
		{
			// An inherited priority is a candidate's own only when it is better:
			// gt-p0 is P0 already, and an inherited P2 must not push it behind
			// the older P2 bead.
			name: "a spec already P0 with an inherited P2 keeps P0",
			specs: []Spec{
				{ID: "gt-p2", Priority: 2, CreatedAt: "2026-09-29T10:00:00Z"},
				{ID: "gt-p0", Priority: 0, CreatedAt: "2026-09-29T12:00:00Z"},
			},
			eff:  map[string]Inherited{"gt-p0": {Priority: 2, From: "gt-blocked"}},
			want: "gt-p0 gt-p2",
		},
		{
			// Effective priority is the sort key, so the ties after it fall to
			// created_at and then id, as Order does. gt-b and gt-a both rank P1,
			// with the same created_at; the id decides.
			name: "equal effective priority ties break by created_at then id",
			specs: []Spec{
				{ID: "gt-c", Priority: 1, CreatedAt: "2026-09-29T11:00:00Z"},
				{ID: "gt-b", Priority: 2, CreatedAt: "2026-09-29T10:00:00Z"},
				{ID: "gt-a", Priority: 1, CreatedAt: "2026-09-29T10:00:00Z"},
			},
			eff:  map[string]Inherited{"gt-b": {Priority: 1, From: "gt-blocked"}},
			want: "gt-a gt-b gt-c",
		},
		{
			name: "nil map orders exactly as Order does",
			specs: []Spec{
				{ID: "gt-c", Priority: 2, CreatedAt: "2026-09-29T10:00:00Z"},
				{ID: "gt-b", Priority: 1, CreatedAt: "2026-09-29T12:00:00Z"},
				{ID: "gt-a", Priority: 2, CreatedAt: "2026-09-29T10:00:00Z"},
				{ID: "gt-d", Priority: 1, CreatedAt: "2026-09-29T11:00:00Z"},
				{ID: "gt-e", Priority: 2, CreatedAt: "garbage"},
				{ID: "gt-f", Priority: 2, CreatedAt: "2026-09-28T10:00:00Z"},
			},
			eff:  nil,
			want: "gt-d gt-b gt-f gt-a gt-c gt-e",
		},
	}
	for _, tc := range cases {
		if got := orderedIDs(tc.specs, tc.eff); got != tc.want {
			t.Errorf("%s: order = %s, want %s", tc.name, got, tc.want)
		}
	}
}
