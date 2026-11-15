package outbox

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const (
	testBead  = "gt-swfo9e"
	testActor = "gastown/polecats/capable"
)

// storeCall is one call the fake store received.
type storeCall struct {
	op     string
	beadID string
	author string
	text   string
}

// fakeStore records every call Apply makes, in order, and fails the calls fail
// asks it to fail.
type fakeStore struct {
	calls []storeCall
	fail  func(storeCall) error
}

func (f *fakeStore) record(call storeCall) error {
	f.calls = append(f.calls, call)
	if f.fail != nil {
		return f.fail(call)
	}
	return nil
}

func (f *fakeStore) AppendNotes(beadID, text string) error {
	return f.record(storeCall{op: "AppendNotes", beadID: beadID, text: text})
}

func (f *fakeStore) SetAcceptance(beadID, text string) error {
	return f.record(storeCall{op: "SetAcceptance", beadID: beadID, text: text})
}

func (f *fakeStore) AddComment(beadID, author, text string) error {
	return f.record(storeCall{op: "AddComment", beadID: beadID, author: author, text: text})
}

// acceptance is the bead's acceptance text the ticks index into. Its second box
// is already ticked.
const acceptance = `## Acceptance
- [ ] the first criterion
- [x] the second criterion
- [ ] the third criterion`

func TestApply(t *testing.T) {
	t.Parallel()
	const tickedOneAndThree = `## Acceptance
- [x] the first criterion
- [x] the second criterion
- [x] the third criterion`
	tests := []struct {
		name       string
		acceptance string
		result     Result
		want       []storeCall
	}{
		{
			name:       "notes, ticks and findings are every call, in order",
			acceptance: acceptance,
			result: Result{
				Notes:    "Ticked the first and third box and ran the gate.\n",
				Ticked:   []int{0, 2},
				Findings: []string{"the retry never resets its counter", "the gate names a package the branch does not change"},
			},
			want: []storeCall{
				{op: "AppendNotes", beadID: testBead, text: "Ticked the first and third box and ran the gate.\n"},
				{op: "SetAcceptance", beadID: testBead, text: tickedOneAndThree},
				{op: "AddComment", beadID: testBead, author: testActor, text: "the retry never resets its counter"},
				{op: "AddComment", beadID: testBead, author: testActor, text: "the gate names a package the branch does not change"},
			},
		},
		{
			name:       "an empty notes text writes no note",
			acceptance: acceptance,
			result:     Result{Findings: []string{"the retry never resets its counter"}},
			want: []storeCall{
				{op: "AddComment", beadID: testBead, author: testActor, text: "the retry never resets its counter"},
			},
		},
		{
			name:       "a tick of a box already ticked writes no acceptance",
			acceptance: acceptance,
			result:     Result{Ticked: []int{1}},
			want:       nil,
		},
		{
			name:       "an empty result writes nothing",
			acceptance: acceptance,
			result:     Result{},
			want:       nil,
		},
		{
			name:       "a tick skips a fenced copy of a later box that AcceptanceItems does not read",
			acceptance: "- [ ] the first criterion\n```\n- [ ] the second criterion\n```\n- [ ] the second criterion",
			result:     Result{Ticked: []int{1}},
			want: []storeCall{
				{op: "SetAcceptance", beadID: testBead, text: "- [ ] the first criterion\n```\n- [ ] the second criterion\n```\n- [x] the second criterion"},
			},
		},
		{
			name:       "a nested box is a checkbox line, as AcceptanceItems reads it",
			acceptance: "- [ ] the first criterion\n  - [ ] its nested criterion",
			result:     Result{Ticked: []int{1}},
			want: []storeCall{
				{op: "SetAcceptance", beadID: testBead, text: "- [ ] the first criterion\n  - [x] its nested criterion"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeStore{}
			if err := Apply(store, testBead, testActor, tt.acceptance, tt.result); err != nil {
				t.Fatalf("Apply() error = %v, want nil", err)
			}
			if !reflect.DeepEqual(store.calls, tt.want) {
				t.Errorf("Apply() calls = %#v, want %#v", store.calls, tt.want)
			}
		})
	}
}

func TestApplyRefusedTick(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		acceptance string
		ticked     []int
		wantErr    string
	}{
		{
			name:       "a tick index past the end",
			acceptance: acceptance,
			ticked:     []int{3},
			wantErr:    "tick index 3 is not one of the 3 checkbox lines",
		},
		{
			name:       "a negative tick index",
			acceptance: acceptance,
			ticked:     []int{-1},
			wantErr:    "tick index -1 is not one of the 3 checkbox lines",
		},
		{
			name:       "a tick on an acceptance with no checkbox line",
			acceptance: "- the only criterion, with no box",
			ticked:     []int{0},
			wantErr:    "tick index 0 is not one of the 0 checkbox lines",
		},
		{
			name:       "a good tick before a bad one is not written",
			acceptance: acceptance,
			ticked:     []int{0, 4},
			wantErr:    "tick index 4 is not one of the 3 checkbox lines",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeStore{}
			r := Result{Notes: "a note", Ticked: tt.ticked, Findings: []string{"a finding"}}
			err := Apply(store, testBead, testActor, tt.acceptance, r)
			if err == nil {
				t.Fatalf("Apply() = nil, want an error naming %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Apply() error = %q, want it to name %q", err, tt.wantErr)
			}
			// The refused acceptance is not written; the note and the finding
			// after the refusal are.
			want := []storeCall{
				{op: "AppendNotes", beadID: testBead, text: "a note"},
				{op: "AddComment", beadID: testBead, author: testActor, text: "a finding"},
			}
			if !reflect.DeepEqual(store.calls, want) {
				t.Errorf("Apply() calls = %#v, want %#v", store.calls, want)
			}
		})
	}
}

func TestApplyFailedWriteDoesNotStopTheRest(t *testing.T) {
	t.Parallel()
	notesErr := errors.New("the notes store is locked")
	findingErr := errors.New("the comment store refused the finding")
	store := &fakeStore{fail: func(call storeCall) error {
		switch {
		case call.op == "AppendNotes":
			return notesErr
		case call.op == "AddComment" && call.text == "the second finding":
			return findingErr
		}
		return nil
	}}
	r := Result{
		Notes:    "a note that fails",
		Ticked:   []int{0},
		Findings: []string{"the first finding", "the second finding"},
	}
	err := Apply(store, testBead, testActor, acceptance, r)
	if err == nil {
		t.Fatal("Apply() = nil with two failed writes, want both failures")
	}
	for _, want := range []string{notesErr.Error(), findingErr.Error(), "append notes", "add finding 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Apply() error = %q, want it to name %q", err, want)
		}
	}
	want := []storeCall{
		{op: "AppendNotes", beadID: testBead, text: "a note that fails"},
		{op: "SetAcceptance", beadID: testBead, text: "## Acceptance\n- [x] the first criterion\n- [x] the second criterion\n- [ ] the third criterion"},
		{op: "AddComment", beadID: testBead, author: testActor, text: "the first finding"},
		{op: "AddComment", beadID: testBead, author: testActor, text: "the second finding"},
	}
	if !reflect.DeepEqual(store.calls, want) {
		t.Errorf("Apply() calls = %#v, want %#v", store.calls, want)
	}
}
