package cmd

import (
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
)

var blockedAt = time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)

// blockedList is a memory holding gt's blocked list under head and working at
// blockedAt.
func blockedList(head, working string) *specAnnotated {
	a := loadSpecAnnotated("", "")
	a.blocked = map[string]specRigBlocked{"gt": {Head: head, Working: working, At: blockedAt,
		Issues: []specBlockedIssue{{ID: "gt-a", Priority: 1, BlockedBy: []string{"gt-x"}}, {ID: "gt-b"}}}}
	return a
}

// TestSpecBlockedMemoryAnswers: the stored list answers under the same hashes
// with each issue's id, priority and blockers, under five minutes after its
// time.
func TestSpecBlockedMemoryAnswers(t *testing.T) {
	t.Parallel()
	heads := map[string]beadsql.RigHash{"gt": {Head: "h1", Working: "w1"}}
	got, ok := blockedList("h1", "w1").rigBlocked("gt", heads, blockedAt.Add(5*time.Minute-time.Second))
	want := []*beads.Issue{{ID: "gt-a", Priority: 1, BlockedBy: []string{"gt-x"}}, {ID: "gt-b"}}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("rigBlocked = %v, %v; want %v, true", got, ok, want)
	}
}

// TestSpecBlockedMemoryRefuses: a moved, missing or empty hash, an age outside
// five minutes, a rig with no entry and a nil memory each answer nothing.
func TestSpecBlockedMemoryRefuses(t *testing.T) {
	t.Parallel()
	stored := beadsql.RigHash{Head: "h1", Working: "w1"}
	cases := []struct {
		name  string
		mem   *specAnnotated
		rig   string
		heads map[string]beadsql.RigHash
		now   time.Time
	}{
		{"head moved", blockedList("h1", "w1"), "gt", map[string]beadsql.RigHash{"gt": {Head: "h2", Working: "w1"}}, blockedAt.Add(time.Minute)},
		{"working moved", blockedList("h1", "w1"), "gt", map[string]beadsql.RigHash{"gt": {Head: "h1", Working: "w2"}}, blockedAt.Add(time.Minute)},
		{"hash missing", blockedList("h1", "w1"), "gt", map[string]beadsql.RigHash{"be": stored}, blockedAt.Add(time.Minute)},
		{"hash empty", blockedList("", ""), "gt", map[string]beadsql.RigHash{"gt": {}}, blockedAt.Add(time.Minute)},
		{"five minutes old", blockedList("h1", "w1"), "gt", map[string]beadsql.RigHash{"gt": stored}, blockedAt.Add(5 * time.Minute)},
		{"stamped after now", blockedList("h1", "w1"), "gt", map[string]beadsql.RigHash{"gt": stored}, blockedAt.Add(-time.Second)},
		{"rig not in memory", blockedList("h1", "w1"), "be", map[string]beadsql.RigHash{"be": stored}, blockedAt.Add(time.Minute)},
		{"nil memory", nil, "gt", map[string]beadsql.RigHash{"gt": stored}, blockedAt.Add(time.Minute)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, ok := tc.mem.rigBlocked(tc.rig, tc.heads, tc.now); ok || got != nil {
				t.Errorf("rigBlocked = %v, %v; want nothing", got, ok)
			}
		})
	}
}
