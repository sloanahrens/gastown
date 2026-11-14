package outbox

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	const beadID = "gt-swfo9e"
	fullFinding := strings.Repeat("x", maxFindingBytes)
	fullFindings := []string{fullFinding, fullFinding, fullFinding, fullFinding, fullFinding}
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
			name:  "a deferred result with an escalation",
			raw:   `{"bead_id": "gt-swfo9e", "status": "deferred", "escalation": {"text": "HELP: blocked on a missing credential"}}`,
			notes: "",
			want: Result{
				Status:     StatusDeferred,
				Escalation: &Escalation{Text: "HELP: blocked on a missing credential"},
			},
		},
		{
			name:  "an explicit null escalation and an empty ticked-box list",
			raw:   `{"bead_id": "gt-swfo9e", "status": "done", "ticked_boxes": [], "escalation": null}`,
			notes: "",
			want:  Result{Status: StatusDone, Ticked: []int{}},
		},
		{
			name:  "the most findings at the largest allowed size",
			raw:   `{"bead_id": "gt-swfo9e", "status": "done", "findings": ["` + strings.Join(fullFindings, `", "`) + `"]}`,
			notes: "",
			want:  Result{Status: StatusDone, Findings: fullFindings},
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

func TestValidateRefused(t *testing.T) {
	t.Parallel()
	const beadID = "gt-swfo9e"
	overlong := strings.Repeat("x", maxFindingBytes+1)
	tests := []struct {
		name    string
		raw     string
		beadID  string
		wantErr string
	}{
		{
			name:    "unknown label field",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "labels": ["rework"]}`,
			beadID:  beadID,
			wantErr: `unknown field "labels"`,
		},
		{
			name:    "unknown assignee field",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "assignee": "gastown/polecats/other"}`,
			beadID:  beadID,
			wantErr: `unknown field "assignee"`,
		},
		{
			name:    "unknown close field",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "close": true}`,
			beadID:  beadID,
			wantErr: `unknown field "close"`,
		},
		{
			name:    "unknown other-bead field",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "bead_ids": ["gt-other"]}`,
			beadID:  beadID,
			wantErr: `unknown field "bead_ids"`,
		},
		{
			name:    "unknown field inside the escalation",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "escalation": {"text": "HELP", "bead_id": "gt-other"}}`,
			beadID:  beadID,
			wantErr: `unknown field "bead_id"`,
		},
		{
			name:    "a bead id other than the one given",
			raw:     `{"bead_id": "gt-other", "status": "done"}`,
			beadID:  beadID,
			wantErr: "bead_id is not the bead",
		},
		{
			name:    "a result that names no bead",
			raw:     `{"status": "done"}`,
			beadID:  beadID,
			wantErr: "bead_id is not the bead",
		},
		{
			name:    "no bead id given to validate against",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done"}`,
			beadID:  "",
			wantErr: "no bead id was given",
		},
		{
			name:    "a status other than done or deferred",
			raw:     `{"bead_id": "gt-swfo9e", "status": "blocked"}`,
			beadID:  beadID,
			wantErr: "status is neither",
		},
		{
			name:    "a status of the wrong case",
			raw:     `{"bead_id": "gt-swfo9e", "status": "Done"}`,
			beadID:  beadID,
			wantErr: "status is neither",
		},
		{
			name:    "a sixth finding",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "findings": ["a", "b", "c", "d", "e", "f"]}`,
			beadID:  beadID,
			wantErr: "more than the 5 allowed",
		},
		{
			name:    "a finding over 2 KB",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "findings": ["ok", "ok", "` + overlong + `"]}`,
			beadID:  beadID,
			wantErr: "finding 2 is 2049 bytes",
		},
		{
			name:    "a negative ticked box",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "ticked_boxes": [1, -1]}`,
			beadID:  beadID,
			wantErr: "ticked box 1 is negative",
		},
		{
			name:    "a repeated ticked box",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "ticked_boxes": [2, 0, 2]}`,
			beadID:  beadID,
			wantErr: "ticked box 2 repeats",
		},
		{
			name:    "an escalation with empty text",
			raw:     `{"bead_id": "gt-swfo9e", "status": "deferred", "escalation": {"text": ""}}`,
			beadID:  beadID,
			wantErr: "escalation text is empty",
		},
		{
			name:    "trailing data after the result object",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done"} {}`,
			beadID:  beadID,
			wantErr: "trailing data",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Validate([]byte(tt.raw), nil, tt.beadID)
			if err == nil {
				t.Fatalf("Validate(%q) = %#v, nil; want an error", tt.raw, got)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate(%q) error = %q, want it to name %q", tt.raw, err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, Result{}) {
				t.Errorf("Validate(%q) returned a partial Result %#v with its error", tt.raw, got)
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
		{name: "a fractional ticked box", raw: `{"ticked_boxes": [1.5]}`},
		{name: "escalation of the wrong type", raw: `{"escalation": "HELP"}`},
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
