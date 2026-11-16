package outbox

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	t.Parallel()
	const beadID = "gt-swfo9e"
	fullFinding := strings.Repeat("x", maxFindingBytes)
	fullFindings := []string{fullFinding, fullFinding, fullFinding, fullFinding, fullFinding}
	baseResult := `{"bead_id": "gt-swfo9e", "status": "done"}`
	underCapResult := baseResult + strings.Repeat(" ", maxResultBytes-len(baseResult)-1)
	underCapNotes := strings.Repeat("x", maxNotesBytes-1)
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
			name:  "the title and the acceptance block an update wrote",
			raw:   `{"bead_id": "gt-swfo9e", "status": "deferred", "title": "DISPROVEN: the retry resets", "acceptance": "- [ ] the refusal names the verb"}`,
			notes: "",
			want: Result{
				Status:     StatusDeferred,
				Title:      strPtr("DISPROVEN: the retry resets"),
				Acceptance: strPtr("- [ ] the refusal names the verb"),
			},
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
		{
			name:  "a result one byte under the 64 KB cap",
			raw:   underCapResult,
			notes: "",
			want:  Result{Status: StatusDone},
		},
		{
			name:  "notes one byte under the 256 KB cap",
			raw:   baseResult,
			notes: underCapNotes,
			want:  Result{Status: StatusDone, Notes: underCapNotes},
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
	baseResult := `{"bead_id": "gt-swfo9e", "status": "done"}`
	overCapResult := baseResult + strings.Repeat(" ", maxResultBytes-len(baseResult)+1)
	overCapNotes := strings.Repeat("x", maxNotesBytes+1)
	tests := []struct {
		name    string
		raw     string
		notes   string
		beadID  string
		wantErr string
	}{
		{
			name:    "a result one byte over the 64 KB cap",
			raw:     overCapResult,
			beadID:  beadID,
			wantErr: fmt.Sprintf("result.json is %d bytes, more than the %d-byte cap", len(overCapResult), maxResultBytes),
		},
		{
			name:    "notes one byte over the 256 KB cap",
			raw:     baseResult,
			notes:   overCapNotes,
			beadID:  beadID,
			wantErr: fmt.Sprintf("notes.md is %d bytes, more than the %d-byte cap", len(overCapNotes), maxNotesBytes),
		},
		{
			name:    "invalid UTF-8 in the result",
			raw:     "{\"bead_id\": \"gt-swfo9e\", \"status\": \"done\", \"findings\": [\"\xff\"]}",
			beadID:  beadID,
			wantErr: "result.json is not valid UTF-8",
		},
		{
			name:    "invalid UTF-8 in the notes",
			raw:     baseResult,
			notes:   "note\xff",
			beadID:  beadID,
			wantErr: "notes.md is not valid UTF-8",
		},
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
			name:    "an empty title",
			raw:     `{"bead_id": "gt-swfo9e", "status": "deferred", "title": ""}`,
			beadID:  beadID,
			wantErr: "title is empty",
		},
		{
			name:    "a title of spaces",
			raw:     `{"bead_id": "gt-swfo9e", "status": "deferred", "title": "  "}`,
			beadID:  beadID,
			wantErr: "title is empty",
		},
		{
			name:    "an empty acceptance",
			raw:     `{"bead_id": "gt-swfo9e", "status": "deferred", "acceptance": ""}`,
			beadID:  beadID,
			wantErr: "acceptance is empty",
		},
		{
			name:    "an acceptance of spaces",
			raw:     `{"bead_id": "gt-swfo9e", "status": "deferred", "acceptance": "\n\t"}`,
			beadID:  beadID,
			wantErr: "acceptance is empty",
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
			got, err := Validate([]byte(tt.raw), []byte(tt.notes), tt.beadID)
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

// strPtr returns a pointer to s, for the Result fields an outbox may omit.
func strPtr(s string) *string { return &s }

// TestEncodeRoundTrips: every field a seat can write survives the encoder and
// the decoder, so a write for one change keeps the others a seat recorded
// earlier.
func TestEncodeRoundTrips(t *testing.T) {
	t.Parallel()
	const beadID = "gt-swfo9e"
	const notes = "a note\n"
	tests := []struct {
		name   string
		result Result
	}{
		{
			name: "a title, a replacement acceptance and every other field",
			result: Result{
				Status:     StatusDeferred,
				Title:      strPtr("DISPROVEN: the retry resets"),
				Acceptance: strPtr("- [ ] the refusal names the verb"),
				Ticked:     []int{0, 2},
				Findings:   []string{"the retry never resets its counter"},
				Escalation: &Escalation{Text: "HELP: the third criterion needs a decision"},
			},
		},
		{
			name:   "a result with no optional field",
			result: Result{Status: StatusDone},
		},
		{
			name: "a submission, the branch a file-mode gt done pushed",
			result: Result{Status: StatusDone, Submission: &Submission{
				Branch:      "polecat/capable/gt-swfo9e",
				Head:        "0123456789abcdef0123456789abcdef01234567",
				Target:      "main",
				SubmittedAt: time.Date(2026, 11, 16, 18, 40, 48, 0, time.UTC),
			}},
		},
		{
			name: "a submission with no stamp",
			result: Result{Status: StatusDone, Submission: &Submission{
				Branch: "polecat/capable/gt-swfo9e", Head: "0123456789abcdef", Target: "main",
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw := Encode(beadID, tt.result)
			got, err := Validate(raw, []byte(notes), beadID)
			if err != nil {
				t.Fatalf("Validate(%s) error = %v", raw, err)
			}
			want := tt.result
			want.Notes = notes
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Validate(Encode(r)) = %#v, want %#v", got, want)
			}
		})
	}
}
