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

// A named read fills Dependents, so its empty answer is [] on the wire and a
// field no encoder filled stays off it: a client tells "none" from "not read".
func TestEmptyDependentsAreAnArrayOnlyWhenFilled(t *testing.T) {
	t.Parallel() // unit tier: pure marshalling
	cases := []struct {
		name string
		deps []IssueDep
		want string
	}{
		{"unfilled", nil, ""},
		{"filled and empty", []IssueDep{}, `"dependents":[]`},
		{"filled", []IssueDep{{ID: "gt-w", Title: "w", Status: "open", DependencyType: "tracks"}}, `"dependents":[{"id":"gt-w"`},
	}
	for _, c := range cases {
		b, err := json.Marshal(Issue{ID: "gt-1", Dependents: c.deps})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := strings.Contains(string(b), `"dependents"`)
		if c.want == "" {
			if got {
				t.Errorf("%s: Issue wire %s carries dependents, want the field absent: nothing filled it", c.name, b)
			}
			continue
		}
		if !strings.Contains(string(b), c.want) {
			t.Errorf("%s: Issue wire %s lacks %s", c.name, b, c.want)
		}
	}

	for name, tc := range map[string]struct {
		wire    string
		filled  bool
		wantLen int
	}{
		"absent":      {`{"id":"gt-1"}`, false, 0},
		"null":        {`{"id":"gt-1","dependents":null}`, false, 0},
		"empty array": {`{"id":"gt-1","dependents":[]}`, true, 0},
	} {
		var is Issue
		if err := json.Unmarshal([]byte(tc.wire), &is); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if filled := is.Dependents != nil; filled != tc.filled || len(is.Dependents) != tc.wantLen {
			t.Errorf("%s: decoded Dependents = %#v, want filled=%v with %d entries", name, is.Dependents, tc.filled, tc.wantLen)
		}
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
