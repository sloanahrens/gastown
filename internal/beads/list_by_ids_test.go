package beads

import (
	"slices"
	"strings"
	"testing"
)

// ListByIDs reads the ids in one bd list query, whatever the status, and keys
// the issues by id; an id the store lacks is simply not in the answer.
func TestListByIDsIsOneQueryForEveryStatus(t *testing.T) {
	t.Parallel()
	r := newRecorder(func([]string) reply {
		return reply{stdout: `[
			{"id": "gt-a", "title": "a", "status": "closed", "close_reason": "Landed on main", "notes": "n1"},
			{"id": "gt-b", "title": "b", "status": "open", "priority": 2}
		]`}
	})
	got, err := ListByIDs(newRecordedBeads(t.TempDir(), r), []string{"gt-a", "gt-nosuch", "gt-b"})
	if err != nil {
		t.Fatalf("ListByIDs: %v", err)
	}
	if len(got) != 2 || got["gt-a"].CloseReason != "Landed on main" || got["gt-a"].Notes != "n1" || got["gt-b"].Status != "open" {
		t.Errorf("ListByIDs = %v, want gt-a and gt-b with their fields", got)
	}
	var queries [][]string
	for _, c := range r.recorded {
		if len(c.args) > 0 && c.args[0] == "list" {
			queries = append(queries, c.args)
		}
	}
	if len(queries) != 1 {
		t.Fatalf("ListByIDs made %d list queries, want 1: %v", len(queries), r.recorded)
	}
	for _, want := range []string{"--json", "--status=all", "--limit=0", "--id=gt-a,gt-nosuch,gt-b"} {
		if !slices.Contains(queries[0], want) {
			t.Errorf("query %q lacks %s", strings.Join(queries[0], " "), want)
		}
	}
}

func TestListByIDsWithNoIDsRunsNothing(t *testing.T) {
	t.Parallel()
	r := newRecorder(nil)
	got, err := ListByIDs(newRecordedBeads(t.TempDir(), r), nil)
	if err != nil || len(got) != 0 {
		t.Errorf("ListByIDs(nil) = %v, %v; want empty", got, err)
	}
	for _, c := range r.recorded {
		if len(c.args) > 0 && c.args[0] == "list" {
			t.Errorf("ListByIDs(nil) ran bd %v", c.args)
		}
	}
}
