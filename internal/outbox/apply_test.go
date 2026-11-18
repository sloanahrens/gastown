package outbox

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const testBead = "gt-swfo9e"

// testSeat is the seat an outbox is applied for: the full address its comments
// are written under and the bare name its READY TO LAND block names as the
// worker.
var testSeat = Seat{Actor: "gastown/polecats/capable", Worker: "capable"}

// storeCall is one call the fake store received.
type storeCall struct {
	op     string
	beadID string
	author string
	text   string
	status string
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

func (f *fakeStore) SetTitle(beadID, title string) error {
	return f.record(storeCall{op: "SetTitle", beadID: beadID, text: title})
}

func (f *fakeStore) SetAcceptance(beadID, text string) error {
	return f.record(storeCall{op: "SetAcceptance", beadID: beadID, text: text})
}

func (f *fakeStore) AddComment(beadID, author, text string) error {
	return f.record(storeCall{op: "AddComment", beadID: beadID, author: author, text: text})
}

func (f *fakeStore) SetStatus(beadID, status string) error {
	return f.record(storeCall{op: "SetStatus", beadID: beadID, status: status})
}

func (f *fakeStore) Escalate(beadID, actor, text string) error {
	return f.record(storeCall{op: "Escalate", beadID: beadID, author: actor, text: text})
}

func (f *fakeStore) MarkReadyToLand(beadID, note string) error {
	return f.record(storeCall{op: "MarkReadyToLand", beadID: beadID, text: note})
}

// storeWithoutReady forwards the six Store writes to f and has no
// MarkReadyToLand: the store of a host that applies an outbox but cannot mark a
// bead ready to land.
type storeWithoutReady struct{ f *fakeStore }

func (s storeWithoutReady) AppendNotes(beadID, text string) error {
	return s.f.AppendNotes(beadID, text)
}

func (s storeWithoutReady) SetTitle(beadID, title string) error {
	return s.f.SetTitle(beadID, title)
}

func (s storeWithoutReady) SetAcceptance(beadID, text string) error {
	return s.f.SetAcceptance(beadID, text)
}

func (s storeWithoutReady) AddComment(beadID, author, text string) error {
	return s.f.AddComment(beadID, author, text)
}

func (s storeWithoutReady) SetStatus(beadID, status string) error {
	return s.f.SetStatus(beadID, status)
}

func (s storeWithoutReady) Escalate(beadID, actor, text string) error {
	return s.f.Escalate(beadID, actor, text)
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
				Status:   StatusDone,
				Notes:    "Ticked the first and third box and ran the gate.\n",
				Ticked:   []int{0, 2},
				Findings: []string{"the retry never resets its counter", "the gate names a package the branch does not change"},
			},
			want: []storeCall{
				{op: "AppendNotes", beadID: testBead, text: "Ticked the first and third box and ran the gate.\n"},
				{op: "SetAcceptance", beadID: testBead, text: tickedOneAndThree},
				{op: "AddComment", beadID: testBead, author: testSeat.Actor, text: "the retry never resets its counter"},
				{op: "AddComment", beadID: testBead, author: testSeat.Actor, text: "the gate names a package the branch does not change"},
			},
		},
		{
			name:       "a title and a replacement acceptance are written before the notes and the acceptance whole",
			acceptance: acceptance,
			result: Result{
				Status:     StatusDone,
				Title:      strPtr("DISPROVEN: the retry resets; the counter is reset at the call site"),
				Acceptance: strPtr("- [ ] the refusal names the verb"),
				Notes:      "The title was disproved.\n",
			},
			want: []storeCall{
				{op: "SetTitle", beadID: testBead, text: "DISPROVEN: the retry resets; the counter is reset at the call site"},
				{op: "AppendNotes", beadID: testBead, text: "The title was disproved.\n"},
				{op: "SetAcceptance", beadID: testBead, text: "- [ ] the refusal names the verb"},
			},
		},
		{
			name:       "a tick indexes into the replacement acceptance, not the block the host read",
			acceptance: acceptance,
			result: Result{
				Status:     StatusDone,
				Acceptance: strPtr("- [ ] the first of the new block\n- [ ] the second of the new block"),
				Ticked:     []int{1},
			},
			want: []storeCall{
				{op: "SetAcceptance", beadID: testBead, text: "- [ ] the first of the new block\n- [x] the second of the new block"},
			},
		},
		{
			name:       "a replacement identical to the block the host read writes no acceptance",
			acceptance: acceptance,
			result:     Result{Status: StatusDone, Acceptance: strPtr(acceptance)},
			want:       nil,
		},
		{
			name:       "a deferred result writes the status deferred once after its other writes",
			acceptance: acceptance,
			result: Result{
				Status:   StatusDeferred,
				Notes:    "The third criterion needs a decision.\n",
				Findings: []string{"the retry never resets its counter"},
			},
			want: []storeCall{
				{op: "AppendNotes", beadID: testBead, text: "The third criterion needs a decision.\n"},
				{op: "AddComment", beadID: testBead, author: testSeat.Actor, text: "the retry never resets its counter"},
				{op: "SetStatus", beadID: testBead, status: "deferred"},
			},
		},
		{
			name:       "a deferred result with no other write writes only the status",
			acceptance: acceptance,
			result:     Result{Status: StatusDeferred},
			want: []storeCall{
				{op: "SetStatus", beadID: testBead, status: "deferred"},
			},
		},
		{
			name:       "a done result writes no status",
			acceptance: acceptance,
			result:     Result{Status: StatusDone},
			want:       nil,
		},
		{
			name:       "a done result raises its escalation once under the seat's actor",
			acceptance: acceptance,
			result: Result{
				Status:     StatusDone,
				Escalation: &Escalation{Text: "HELP: the acceptance list names a missing file"},
			},
			want: []storeCall{
				{op: "Escalate", beadID: testBead, author: testSeat.Actor, text: "HELP: the acceptance list names a missing file"},
			},
		},
		{
			name:       "an escalation is raised last, after the deferred status",
			acceptance: acceptance,
			result: Result{
				Status:     StatusDeferred,
				Notes:      "The third criterion needs a decision.\n",
				Escalation: &Escalation{Text: "HELP: the third criterion needs a decision"},
			},
			want: []storeCall{
				{op: "AppendNotes", beadID: testBead, text: "The third criterion needs a decision.\n"},
				{op: "SetStatus", beadID: testBead, status: "deferred"},
				{op: "Escalate", beadID: testBead, author: testSeat.Actor, text: "HELP: the third criterion needs a decision"},
			},
		},
		{
			name:       "a result without an escalation raises none",
			acceptance: acceptance,
			result: Result{
				Status:   StatusDone,
				Notes:    "Ran the gate.\n",
				Findings: []string{"the retry never resets its counter"},
			},
			want: []storeCall{
				{op: "AppendNotes", beadID: testBead, text: "Ran the gate.\n"},
				{op: "AddComment", beadID: testBead, author: testSeat.Actor, text: "the retry never resets its counter"},
			},
		},
		{
			name:       "an empty notes text writes no note",
			acceptance: acceptance,
			result:     Result{Status: StatusDone, Findings: []string{"the retry never resets its counter"}},
			want: []storeCall{
				{op: "AddComment", beadID: testBead, author: testSeat.Actor, text: "the retry never resets its counter"},
			},
		},
		{
			name:       "a tick of a box already ticked writes no acceptance",
			acceptance: acceptance,
			result:     Result{Status: StatusDone, Ticked: []int{1}},
			want:       nil,
		},
		{
			name:       "a tick skips a fenced copy of a later box that AcceptanceItems does not read",
			acceptance: "- [ ] the first criterion\n```\n- [ ] the second criterion\n```\n- [ ] the second criterion",
			result:     Result{Status: StatusDone, Ticked: []int{1}},
			want: []storeCall{
				{op: "SetAcceptance", beadID: testBead, text: "- [ ] the first criterion\n```\n- [ ] the second criterion\n```\n- [x] the second criterion"},
			},
		},
		{
			name:       "a nested box is a checkbox line, as AcceptanceItems reads it",
			acceptance: "- [ ] the first criterion\n  - [ ] its nested criterion",
			result:     Result{Status: StatusDone, Ticked: []int{1}},
			want: []storeCall{
				{op: "SetAcceptance", beadID: testBead, text: "- [ ] the first criterion\n  - [x] its nested criterion"},
			},
		},
		{
			name:       "a fence that never closes hides no box, as AcceptanceItems reads it",
			acceptance: "- [ ] the first criterion\n```\n- [ ] the second criterion",
			result:     Result{Status: StatusDone, Ticked: []int{1}},
			want: []storeCall{
				{op: "SetAcceptance", beadID: testBead, text: "- [ ] the first criterion\n```\n- [x] the second criterion"},
			},
		},
		{
			name:       "an unboxed item between boxes shifts no tick index",
			acceptance: "- [ ] the first criterion\n- the middle criterion, with no box\n- [ ] the third criterion",
			result:     Result{Status: StatusDone, Ticked: []int{1}},
			want: []storeCall{
				{op: "SetAcceptance", beadID: testBead, text: "- [ ] the first criterion\n- the middle criterion, with no box\n- [x] the third criterion"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeStore{}
			if err := Apply(store, testBead, testSeat, tt.acceptance, tt.result, originHoldsTheSubmission); err != nil {
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
			r := Result{Status: StatusDone, Notes: "a note", Ticked: tt.ticked, Findings: []string{"a finding"}}
			err := Apply(store, testBead, testSeat, tt.acceptance, r, originHoldsTheSubmission)
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
				{op: "AddComment", beadID: testBead, author: testSeat.Actor, text: "a finding"},
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
	acceptanceErr := errors.New("the acceptance store is read-only")
	findingErr := errors.New("the comment store refused the finding")
	statusErr := errors.New("the bead is already closed")
	store := &fakeStore{fail: func(call storeCall) error {
		switch {
		case call.op == "AppendNotes":
			return notesErr
		case call.op == "SetAcceptance":
			return acceptanceErr
		case call.op == "AddComment" && call.text == "the second finding":
			return findingErr
		case call.op == "SetStatus":
			return statusErr
		}
		return nil
	}}
	r := Result{
		Status:   StatusDeferred,
		Notes:    "a note that fails",
		Ticked:   []int{0},
		Findings: []string{"the first finding", "the second finding"},
	}
	err := Apply(store, testBead, testSeat, acceptance, r, originHoldsTheSubmission)
	if err == nil {
		t.Fatal("Apply() = nil with four failed writes, want every failure")
	}
	for _, want := range []string{
		notesErr.Error(), "append notes",
		acceptanceErr.Error(), "set acceptance",
		findingErr.Error(), "add finding 1",
		statusErr.Error(), "set status",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Apply() error = %q, want it to name %q", err, want)
		}
	}
	// The status write is attempted after the failed finding, and the finding
	// after the failed one is still written.
	want := []storeCall{
		{op: "AppendNotes", beadID: testBead, text: "a note that fails"},
		{op: "SetAcceptance", beadID: testBead, text: "## Acceptance\n- [x] the first criterion\n- [x] the second criterion\n- [ ] the third criterion"},
		{op: "AddComment", beadID: testBead, author: testSeat.Actor, text: "the first finding"},
		{op: "AddComment", beadID: testBead, author: testSeat.Actor, text: "the second finding"},
		{op: "SetStatus", beadID: testBead, status: "deferred"},
	}
	if !reflect.DeepEqual(store.calls, want) {
		t.Errorf("Apply() calls = %#v, want %#v", store.calls, want)
	}
}

func TestApplyFailedEscalationIsJoinedWithEarlierErrors(t *testing.T) {
	t.Parallel()
	notesErr := errors.New("the notes store is locked")
	escalateErr := errors.New("the escalation store is read-only")
	store := &fakeStore{fail: func(call storeCall) error {
		switch call.op {
		case "AppendNotes":
			return notesErr
		case "Escalate":
			return escalateErr
		}
		return nil
	}}
	r := Result{
		Status:     StatusDeferred,
		Notes:      "a note that fails",
		Escalation: &Escalation{Text: "HELP: the third criterion needs a decision"},
	}
	err := Apply(store, testBead, testSeat, acceptance, r, originHoldsTheSubmission)
	if err == nil {
		t.Fatal("Apply() = nil with the note and the escalation failed, want both failures")
	}
	for _, want := range []string{notesErr.Error(), "append notes", escalateErr.Error(), "escalate on"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Apply() error = %q, want it to name %q", err, want)
		}
	}
	// The status write between the two failures is still made, and the
	// escalation is raised after it.
	want := []storeCall{
		{op: "AppendNotes", beadID: testBead, text: "a note that fails"},
		{op: "SetStatus", beadID: testBead, status: "deferred"},
		{op: "Escalate", beadID: testBead, author: testSeat.Actor, text: "HELP: the third criterion needs a decision"},
	}
	if !reflect.DeepEqual(store.calls, want) {
		t.Errorf("Apply() calls = %#v, want %#v", store.calls, want)
	}
}

// TestNoBeadAccessApplyCreatesTheEscalationFromAValidatedOutbox: the escalation
// bead is created once from a decoded outbox, as the seat that wrote it
// (gt-5rmlks).
func TestNoBeadAccessApplyCreatesTheEscalationFromAValidatedOutbox(t *testing.T) {
	t.Parallel()
	const raw = `{"bead_id": "gt-swfo9e", "status": "deferred",
		"escalation": {"text": "HELP: the acceptance list names a missing file"}}`
	r, err := Validate([]byte(raw), nil, testBead)
	if err != nil {
		t.Fatalf("Validate() error = %v, want nil", err)
	}
	store := &fakeStore{}
	if err := Apply(store, testBead, testSeat, acceptance, r, originHoldsTheSubmission); err != nil {
		t.Fatalf("Apply() error = %v, want nil", err)
	}
	want := []storeCall{
		{op: "SetStatus", beadID: testBead, status: "deferred"},
		{op: "Escalate", beadID: testBead, author: testSeat.Actor, text: "HELP: the acceptance list names a missing file"},
	}
	if !reflect.DeepEqual(store.calls, want) {
		t.Errorf("Apply() calls = %#v, want %#v", store.calls, want)
	}
}

// TestNoBeadAccessApplyEscalationOnlyWhenTheRequestHasText: an outbox that asks
// for no help creates no escalation, and one whose request is empty or only
// whitespace creates none and is reported by Apply, whether it came through the
// decoder or a host built the Result by hand. The decoder's own refusal of an
// empty request is tested in outbox_test.go (gt-5rmlks, gt-n91cyx).
func TestNoBeadAccessApplyEscalationOnlyWhenTheRequestHasText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// raw is the result.json to decode; empty means result is handed to
		// Apply directly, as a host that built it without Validate would.
		raw     string
		result  Result
		wantErr string
	}{
		{
			name:   "an outbox with no request creates none",
			raw:    `{"bead_id": "gt-swfo9e", "status": "done"}`,
			result: Result{},
		},
		{
			// The decoder checks only for "", so this request reaches Apply and
			// Apply's own guard is what refuses it.
			name:    "an outbox whose request is only whitespace is refused by Apply",
			raw:     `{"bead_id": "gt-swfo9e", "status": "done", "escalation": {"text": "  \n\t"}}`,
			wantErr: "escalation text is empty",
		},
		{
			name:    "a hand-built result with an empty request creates none and is reported",
			result:  Result{Status: StatusDone, Escalation: &Escalation{}},
			wantErr: "escalation text is empty",
		},
		{
			name:    "a hand-built result with a whitespace-only request creates none and is reported",
			result:  Result{Status: StatusDone, Escalation: &Escalation{Text: "  \n\t"}},
			wantErr: "escalation text is empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeStore{}
			r := tt.result
			var err error
			if tt.raw != "" {
				r, err = Validate([]byte(tt.raw), nil, testBead)
			}
			// A host stops at the outbox it could not validate and applies
			// nothing.
			if err == nil {
				err = Apply(store, testBead, testSeat, acceptance, r, originHoldsTheSubmission)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("error = %v, want nil", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to name %q", err, tt.wantErr)
			}
			if len(store.calls) != 0 {
				t.Errorf("store calls = %#v, want no escalation bead created", store.calls)
			}
		})
	}
}

// TestNoBeadAccessApplyWritesTheTitleAndAcceptanceOfAValidatedOutbox: a
// file-mode seat reports a `gt bead update` through its outbox, and the host
// writes the title and the acceptance block it decoded, in Apply's order. A
// tick the decoded outbox carries indexes into the replacement block, not the
// block the host read (gt-pm55bt, gt-htwlio).
func TestNoBeadAccessApplyWritesTheTitleAndAcceptanceOfAValidatedOutbox(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		want []storeCall
	}{
		{
			name: "the title and the replacement acceptance are written",
			raw:  `{"bead_id": "gt-swfo9e", "status": "deferred", "title": "DISPROVEN: the retry resets", "acceptance": "- [ ] the refusal names the verb"}`,
			want: []storeCall{
				{op: "SetTitle", beadID: testBead, text: "DISPROVEN: the retry resets"},
				{op: "SetAcceptance", beadID: testBead, text: "- [ ] the refusal names the verb"},
				{op: "SetStatus", beadID: testBead, status: "deferred"},
			},
		},
		{
			name: "a tick indexes into the acceptance block the outbox carries",
			raw:  `{"bead_id": "gt-swfo9e", "status": "deferred", "acceptance": "- [ ] the first of the new block\n- [ ] the second of the new block", "ticked_boxes": [1]}`,
			want: []storeCall{
				{op: "SetAcceptance", beadID: testBead, text: "- [ ] the first of the new block\n- [x] the second of the new block"},
				{op: "SetStatus", beadID: testBead, status: "deferred"},
			},
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
			if err := Apply(store, testBead, testSeat, acceptance, r, originHoldsTheSubmission); err != nil {
				t.Fatalf("Apply() error = %v, want nil", err)
			}
			if !reflect.DeepEqual(store.calls, tt.want) {
				t.Errorf("Apply() calls = %#v, want %#v", store.calls, tt.want)
			}
		})
	}
}

func TestApplyRefusedStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		result    Result
		wantErr   string
		wantCalls []storeCall
	}{
		{
			// A zero-value Result is what a host that skipped Validate hands
			// Apply; it must not read as a clean run with nothing written.
			name:    "the zero-value Result",
			result:  Result{},
			wantErr: `result status "" is neither "done" nor "deferred"`,
		},
		{
			name:    "a status the decoder never returns",
			result:  Result{Status: Status("closed")},
			wantErr: `result status "closed" is neither "done" nor "deferred"`,
		},
		{
			name:    "an unset status with other writes",
			result:  Result{Notes: "a note", Findings: []string{"a finding"}},
			wantErr: `result status "" is neither "done" nor "deferred"`,
			wantCalls: []storeCall{
				{op: "AppendNotes", beadID: testBead, text: "a note"},
				{op: "AddComment", beadID: testBead, author: testSeat.Actor, text: "a finding"},
			},
		},
		{
			name:    "an unset status still raises the escalation, after it",
			result:  Result{Escalation: &Escalation{Text: "HELP: no status was set"}},
			wantErr: `result status "" is neither "done" nor "deferred"`,
			wantCalls: []storeCall{
				{op: "Escalate", beadID: testBead, author: testSeat.Actor, text: "HELP: no status was set"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeStore{}
			err := Apply(store, testBead, testSeat, acceptance, tt.result, originHoldsTheSubmission)
			if err == nil {
				t.Fatalf("Apply() = nil, want an error naming %s", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Apply() error = %q, want it to name %s", err, tt.wantErr)
			}
			if !reflect.DeepEqual(store.calls, tt.wantCalls) {
				t.Errorf("Apply() calls = %#v, want %#v", store.calls, tt.wantCalls)
			}
		})
	}
}

// TestNoBeadAccessApplyRefusesAnEmptyTitleOrAcceptance: Validate refuses both,
// and a hand-built Result must not write them either, because either write
// replaces what the bead has with nothing (gt-9smgr). The test is named inside
// the no-bead-access selection so the gate that runs those names runs it
// (gt-htwlio).
func TestNoBeadAccessApplyRefusesAnEmptyTitleOrAcceptance(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		result    Result
		wantErr   string
		wantCalls []storeCall
	}{
		{
			name:    "an empty title",
			result:  Result{Status: StatusDone, Title: strPtr("  ")},
			wantErr: "the result's title is empty",
		},
		{
			name:    "an empty acceptance",
			result:  Result{Status: StatusDone, Acceptance: strPtr("")},
			wantErr: "the criteria are not replaced",
		},
		{
			name:    "a tick into an empty acceptance writes no acceptance",
			result:  Result{Status: StatusDone, Acceptance: strPtr(""), Ticked: []int{0}},
			wantErr: "the criteria are not replaced",
		},
		{
			name:    "the writes after an empty acceptance are still made",
			result:  Result{Status: StatusDeferred, Acceptance: strPtr("\n"), Findings: []string{"a finding"}},
			wantErr: "the criteria are not replaced",
			wantCalls: []storeCall{
				{op: "AddComment", beadID: testBead, author: testSeat.Actor, text: "a finding"},
				{op: "SetStatus", beadID: testBead, status: "deferred"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeStore{}
			err := Apply(store, testBead, testSeat, acceptance, tt.result, originHoldsTheSubmission)
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
