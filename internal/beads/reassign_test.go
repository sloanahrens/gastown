package beads

import (
	"strings"
	"testing"
)

func TestFormatReassignmentNote(t *testing.T) {
	tests := []struct {
		name      string
		from      string
		to        string
		requester string
		branches  []string
		want      []string
		notWant   []string
	}{
		{
			name:      "both assignees and a branch",
			from:      "om/polecats/jasper",
			to:        "om/polecats/obsidian",
			requester: "mayor",
			branches:  []string{"polecat/jasper/om-bkq+mtsvd1we"},
			want: []string{
				"REASSIGNED: om/polecats/jasper -> om/polecats/obsidian",
				"Branch: polecat/jasper/om-bkq+mtsvd1we",
				"By: mayor",
			},
		},
		{
			// The decommission path releases a bead without a successor.
			name:      "release has no successor",
			from:      "gastown/polecats/toast",
			to:        "",
			requester: "witness",
			want:      []string{"REASSIGNED: gastown/polecats/toast -> (unassigned)"},
		},
		{
			// Origin answered and matched nothing: the auditor can stop looking.
			name:      "queried origin with no match",
			from:      "a/polecats/one",
			to:        "a/polecats/two",
			requester: "mayor",
			branches:  []string{},
			want:      []string{"Branch: (none on origin)"},
		},
		{
			// An unreachable remote must not read as "no branch existed": that
			// would tell an auditor to stop looking when a branch may be there.
			name:      "unreachable origin is unknown, not none",
			from:      "a/polecats/one",
			to:        "a/polecats/two",
			requester: "mayor",
			branches:  nil,
			want:      []string{"Branch: (unknown)"},
			notWant:   []string{"(none on origin)"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatReassignmentNote(tt.from, tt.to, tt.requester, tt.branches)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("note missing %q; got:\n%s", want, got)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("note must not claim %q; got:\n%s", notWant, got)
				}
			}
		})
	}
}

func TestFormatReassignmentNoteListsEveryBranch(t *testing.T) {
	got := formatReassignmentNote("a/polecats/one", "a/polecats/two", "mayor", []string{
		"polecat/one/a-x+mu2",
		"polecat/one/a-x+mu1",
	})
	for _, branch := range []string{"Branch: polecat/one/a-x+mu2", "Branch: polecat/one/a-x+mu1"} {
		if !strings.Contains(got, branch) {
			t.Errorf("note missing %q; got:\n%s", branch, got)
		}
	}
	if strings.Contains(got, "(none on origin)") {
		t.Errorf("note must not claim no branches when it listed some:\n%s", got)
	}
}
