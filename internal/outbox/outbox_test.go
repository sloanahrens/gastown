package outbox

import (
	"reflect"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	const beadID = "gt-swfo9e"
	tests := []struct {
		name  string
		raw   string
		notes string
		want  Result
	}{
		{
			name: "ticked boxes, findings and an escalation",
			raw: `{
				"bead_id": "gt-swfo9e",
				"status": "done",
				"ticked_boxes": [0, 2],
				"findings": [
					"the retry never resets its counter",
					"the gate names a package the branch does not change"
				],
				"escalation": {"text": "HELP: the acceptance list names a file outside the worktree"}
			}`,
			notes: "Ticked every box and ran the gate.\n",
			want: Result{
				Status:     StatusDone,
				Ticked:     []int{0, 2},
				Findings:   []string{"the retry never resets its counter", "the gate names a package the branch does not change"},
				Escalation: &Escalation{Text: "HELP: the acceptance list names a file outside the worktree"},
				Notes:      "Ticked every box and ran the gate.\n",
			},
		},
		{
			name:  "an empty notes file and a result with no findings and no escalation",
			raw:   `{"bead_id": "gt-swfo9e", "status": "done"}`,
			notes: "",
			want:  Result{Status: StatusDone},
		},
		{
			name:  "a deferred result with no findings and no escalation",
			raw:   `{"bead_id": "gt-swfo9e", "status": "deferred", "escalation": {"text": "HELP: blocked on a missing credential"}}`,
			notes: "",
			want: Result{
				Status:     StatusDeferred,
				Escalation: &Escalation{Text: "HELP: blocked on a missing credential"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Validate([]byte(tt.raw), []byte(tt.notes), beadID)
			if err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Validate() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestValidateMalformed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
	}{
		{name: "empty file", raw: ""},
		{name: "truncated object", raw: `{"status": "done"`},
		{name: "null", raw: `null`},
		{name: "array", raw: `[]`},
		{name: "string", raw: `"done"`},
		{name: "status of the wrong type", raw: `{"status": 5}`},
		{name: "ticked_boxes of the wrong type", raw: `{"ticked_boxes": "0"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Validate([]byte(tt.raw), nil, "gt-swfo9e")
			if err == nil {
				t.Fatalf("Validate(%q) = %#v, nil; want an error", tt.raw, got)
			}
			if !reflect.DeepEqual(got, Result{}) {
				t.Errorf("Validate(%q) returned a partial Result %#v with its error", tt.raw, got)
			}
		})
	}
}
