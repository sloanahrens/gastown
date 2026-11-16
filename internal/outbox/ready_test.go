package outbox

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestNoBeadAccessApplyWritesTheReadyBlockFromASubmittedOutbox: a result.json
// that says what it submitted has the READY TO LAND block written for the bead,
// so the landing worker finds it: branch, head and target from the outbox, the
// worker the host names the seat by, and the seat's own submitted stamp
// (gt-qk3ppr).
func TestNoBeadAccessApplyWritesTheReadyBlockFromASubmittedOutbox(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		raw      string
		wantNote string
	}{
		{
			name: "a submission with a stamp carries every field of the block",
			raw: `{"bead_id": "gt-swfo9e", "status": "done",
				"submission": {"branch": "polecat/capable/gt-swfo9e",
					"head": "0123456789abcdef0123456789abcdef01234567",
					"target": "main", "submitted_at": "2026-11-16T18:40:48Z"}}`,
			wantNote: "READY TO LAND\nBranch: polecat/capable/gt-swfo9e\n" +
				"Head: 0123456789abcdef0123456789abcdef01234567\nTarget: main\n" +
				"Worker: capable\nSubmitted: 2026-11-16T18:40:48Z",
		},
		{
			// The worker line is the seat's bare name, which the host supplies
			// and gt done writes: testSeat's actor is the full address and its
			// name is the worker, so a block built from the actor would show.
			name: "a submission with no stamp writes no Submitted line",
			raw: `{"bead_id": "gt-swfo9e", "status": "done",
				"submission": {"branch": "polecat/capable/gt-swfo9e",
					"head": "0123456789abcdef", "target": "main"}}`,
			wantNote: "READY TO LAND\nBranch: polecat/capable/gt-swfo9e\n" +
				"Head: 0123456789abcdef\nTarget: main\nWorker: capable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r, err := Validate([]byte(tt.raw), nil, testBead)
			if err != nil {
				t.Fatalf("Validate() error = %v, want nil", err)
			}
			store := &fakeStore{}
			if err := Apply(store, testBead, testSeat, acceptance, r); err != nil {
				t.Fatalf("Apply() error = %v, want nil", err)
			}
			want := []storeCall{{op: "MarkReadyToLand", beadID: testBead, text: tt.wantNote}}
			if !reflect.DeepEqual(store.calls, want) {
				t.Errorf("Apply() calls = %#v, want %#v", store.calls, want)
			}
		})
	}
}

// TestNoBeadAccessApplyRefusesABadSubmission: a submission the decoder refused,
// or a hand-built one, marks no bead ready to land, and the writes around it are
// still made (gt-qk3ppr).
func TestNoBeadAccessApplyRefusesABadSubmission(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		result    Result
		wantErr   string
		wantCalls []storeCall
	}{
		{
			name: "a deferred result that still carries a submission",
			result: Result{Status: StatusDeferred, Submission: &Submission{
				Branch: "polecat/capable/gt-swfo9e", Head: "0123456789abcdef", Target: "main",
			}},
			wantErr:   `a submission under status "deferred"; only a "done" result submits a branch`,
			wantCalls: []storeCall{{op: "SetStatus", beadID: testBead, status: "deferred"}},
		},
		{
			name: "a submission with no branch",
			result: Result{Status: StatusDone, Submission: &Submission{
				Head: "0123456789abcdef", Target: "main",
			}},
			wantErr: "the submission names no branch",
		},
		{
			name: "a submission with no head",
			result: Result{Status: StatusDone, Submission: &Submission{
				Branch: "polecat/capable/gt-swfo9e", Target: "main",
			}},
			wantErr: "the submission names no head",
		},
		{
			name: "a submission with a target of spaces",
			result: Result{Status: StatusDone, Submission: &Submission{
				Branch: "polecat/capable/gt-swfo9e", Head: "0123456789abcdef", Target: "  ",
			}},
			wantErr: "the submission names no target",
		},
		{
			name: "a hand-built submission whose branch is not a ref",
			result: Result{Status: StatusDone, Submission: &Submission{
				Branch: "-c core.pager=cat", Head: "0123456789abcdef", Target: "main",
			}},
			wantErr: "is not a name git accepts",
		},
		{
			name: "a hand-built submission whose target is not a ref",
			result: Result{Status: StatusDone, Submission: &Submission{
				Branch: "polecat/capable/gt-swfo9e", Head: "0123456789abcdef", Target: "main..next",
			}},
			wantErr: "is not a name git accepts",
		},
		{
			name: "a hand-built submission whose head is not a sha",
			result: Result{Status: StatusDone, Submission: &Submission{
				Branch: "polecat/capable/gt-swfo9e", Head: "the tip", Target: "main",
			}},
			wantErr: "is not a git commit sha",
		},
		{
			name: "a refused submission after the findings still writes them",
			result: Result{Status: StatusDone, Findings: []string{"a finding"}, Submission: &Submission{
				Branch: "polecat/capable/gt-swfo9e", Target: "main",
			}},
			wantErr: "the submission names no head",
			wantCalls: []storeCall{
				{op: "AddComment", beadID: testBead, author: testSeat.Actor, text: "a finding"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeStore{}
			err := Apply(store, testBead, testSeat, acceptance, tt.result)
			if err == nil {
				t.Fatalf("Apply() = nil, want an error naming %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Apply() error = %q, want it to name %q", err, tt.wantErr)
			}
			if !reflect.DeepEqual(store.calls, tt.wantCalls) {
				t.Errorf("Apply() calls = %#v, want %#v", store.calls, tt.wantCalls)
			}
		})
	}
}

// TestNoBeadAccessApplyFailedReadyWriteIsReported: a block the store refused is
// the failure Apply returns, and the note written before it stays written
// (gt-qk3ppr).
func TestNoBeadAccessApplyFailedReadyWriteIsReported(t *testing.T) {
	t.Parallel()
	readyErr := errors.New("the label store is read-only")
	store := &fakeStore{fail: func(call storeCall) error {
		if call.op == "MarkReadyToLand" {
			return readyErr
		}
		return nil
	}}
	r := Result{Status: StatusDone, Notes: "a note", Submission: &Submission{
		Branch: "polecat/capable/gt-swfo9e", Head: "0123456789abcdef", Target: "main",
	}}
	err := Apply(store, testBead, testSeat, acceptance, r)
	if err == nil || !strings.Contains(err.Error(), "mark gt-swfo9e ready to land") || !strings.Contains(err.Error(), readyErr.Error()) {
		t.Fatalf("Apply() error = %v, want it to name the ready write and its failure", err)
	}
	want := []storeCall{
		{op: "AppendNotes", beadID: testBead, text: "a note"},
		{op: "MarkReadyToLand", beadID: testBead, text: "READY TO LAND\nBranch: polecat/capable/gt-swfo9e\nHead: 0123456789abcdef\nTarget: main\nWorker: capable"},
	}
	if !reflect.DeepEqual(store.calls, want) {
		t.Errorf("Apply() calls = %#v, want %#v", store.calls, want)
	}
}

// TestNoBeadAccessValidateRefusesABadSubmission: a submission result.json cannot
// answer for is refused whole, so the host applies nothing from it (gt-qk3ppr).
func TestNoBeadAccessValidateRefusesABadSubmission(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{
			name:    "a submission with a deferred status",
			raw:     `{"bead_id": "gt-swfo9e", "status": "deferred", "submission": {"branch": "b", "head": "h", "target": "main"}}`,
			wantErr: `a submission under status "deferred"; only a "done" result submits a branch`,
		},
		{
			name:    "a submission with no branch",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "submission": {"head": "h", "target": "main"}}`,
			wantErr: "submission names no branch",
		},
		{
			name:    "a submission whose branch is only spaces",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "submission": {"branch": "  ", "head": "h", "target": "main"}}`,
			wantErr: "submission names no branch",
		},
		{
			name:    "a submission with no head",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "submission": {"branch": "b", "target": "main"}}`,
			wantErr: "submission names no head",
		},
		{
			name:    "a submission with no target",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "submission": {"branch": "b", "head": "h"}}`,
			wantErr: "submission names no target",
		},
		{
			name:    "a submitted_at that is not an RFC 3339 time",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "submission": {"branch": "b", "head": "0123456789abcdef", "target": "main", "submitted_at": "yesterday"}}`,
			wantErr: "submitted_at is not an RFC 3339 time",
		},
		{
			name:    "a branch git reads as an option",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "submission": {"branch": "--upload-pack=/tmp/x", "head": "0123456789abcdef", "target": "main"}}`,
			wantErr: "is not a name git accepts",
		},
		{
			name:    "a branch that is not a ref",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "submission": {"branch": "polecat/../main", "head": "0123456789abcdef", "target": "main"}}`,
			wantErr: "is not a name git accepts",
		},
		{
			name:    "a target that is not a ref",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "submission": {"branch": "polecat/capable/gt-swfo9e", "head": "0123456789abcdef", "target": "HEAD"}}`,
			wantErr: "is not a name git accepts",
		},
		{
			name:    "a head that is not a commit sha",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "submission": {"branch": "polecat/capable/gt-swfo9e", "head": "main", "target": "main"}}`,
			wantErr: "is not a git commit sha",
		},
		{
			name:    "a head shorter than the shortest abbreviation the block carries",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "submission": {"branch": "polecat/capable/gt-swfo9e", "head": "0123ab", "target": "main"}}`,
			wantErr: "is not a git commit sha",
		},
		{
			// The worker is the host's to name: an outbox that names one is
			// refused as an unknown field, so it cannot claim another worker's
			// submission (gt-1m9gr).
			name:    "a submission that names its own worker",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "submission": {"branch": "b", "head": "h", "target": "main", "worker": "other"}}`,
			wantErr: `unknown field "worker"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Validate([]byte(tt.raw), nil, testBead)
			if err == nil {
				t.Fatalf("Validate(%s) = %#v, nil; want an error", tt.raw, got)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate(%s) error = %q, want it to name %q", tt.raw, err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, Result{}) {
				t.Errorf("Validate(%s) returned a partial Result %#v with its error", tt.raw, got)
			}
		})
	}
}

// TestNoBeadAccessApplyRefusesASubmissionTheStoreCannotWrite: a store that
// applies an outbox but cannot mark a bead ready to land refuses the submission
// with an error, so the host never records a landing nothing picks up
// (gt-qk3ppr).
func TestNoBeadAccessApplyRefusesASubmissionTheStoreCannotWrite(t *testing.T) {
	t.Parallel()
	store := storeWithoutReady{f: &fakeStore{}}
	r := Result{Status: StatusDone, Submission: &Submission{
		Branch: "polecat/capable/gt-swfo9e", Head: "0123456789abcdef", Target: "main",
	}}
	err := Apply(store, testBead, testSeat, acceptance, r)
	if err == nil || !strings.Contains(err.Error(), "cannot write a READY TO LAND block") {
		t.Fatalf("Apply() error = %v, want the store's refusal to write the block", err)
	}
}
