package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The wire shapes the adapter and the shim depend on: stable names, no noise
// for unset optionals.
func TestZeroValuesStayOffTheWire(t *testing.T) {
	t.Parallel() // unit tier: pure marshalling
	b, err := json.Marshal(ListFilter{Statuses: []string{"open"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"statuses":["open"]}`; got != want {
		t.Errorf("ListFilter wire = %s, want %s: zero times and unset filters must be omitted", got, want)
	}
	b, err = json.Marshal(UpdatePatch{AppendNotes: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"append_notes":"n"}`; got != want {
		t.Errorf("UpdatePatch wire = %s, want %s: a nil pointer means leave the field alone", got, want)
	}
}

func TestIssueUsesBeadsJSONNames(t *testing.T) {
	t.Parallel()
	closed := time.Date(2026, 10, 25, 11, 0, 0, 0, time.UTC)
	b, err := json.Marshal(Issue{ID: "gt-1", Title: "t", Status: "closed", Type: "task", Priority: 1, ClosedAt: &closed, CreatedAt: closed, UpdatedAt: closed})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"id":"gt-1"`, `"issue_type":"task"`, `"closed_at":"2026-10-25T11:00:00Z"`, `"priority":1`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("Issue wire %s lacks %s", b, want)
		}
	}
	if strings.Contains(string(b), "hook_bead") || strings.Contains(string(b), "labels") {
		t.Errorf("Issue wire %s carries unset optionals", b)
	}
	var back Issue
	if err := json.Unmarshal(b, &back); err != nil || back.ClosedAt == nil || !back.ClosedAt.Equal(closed) {
		t.Errorf("round trip lost closed_at: %+v, %v", back.ClosedAt, err)
	}
}

func TestEventPayloadIsRawJSON(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(Event{Seq: 3, Op: "created", IssueID: "gt-1", Payload: json.RawMessage(`{"a":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"payload":{"a":1}`) {
		t.Errorf("Event wire %s, want the payload embedded as JSON, not base64", b)
	}
}
