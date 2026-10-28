package beads

import (
	"errors"
	"testing"
)

// notFoundEnvelope is bd's machine-mode answer to a show that resolved no id:
// data null, exit 20, the missing ids under error.ids (captured from bd on
// this host: `bd show gt-zzznosuch --json` with BD_MACHINE=1).
const notFoundEnvelope = `{
  "schema_version": 1,
  "contract_version": 1,
  "data": null,
  "pagination": null,
  "error": {
    "kind": "not_found",
    "message": "2 of 2 ids failed",
    "ids": [
      {"id": "gt-a", "kind": "not_found", "message": "no issue found matching \"gt-a\""},
      {"id": "gt-b", "kind": "not_found", "message": "no issue found matching \"gt-b\""}
    ]
  }
}`

// showAnswersThatAreNotAnIssueArray are answers a show has no business reading
// as its result: the payload bd unwrapped a broken answer to, not the array
// bd prints for a show, and bd's error envelope when it arrives on a call that
// did not fail (gt-d7b60).
var showAnswersThatAreNotAnIssueArray = []struct {
	name   string
	stdout string
}{
	{"null", "null\n"},
	{"object", `{"id":"gt-a","title":"a"}`},
	{"prose", "Issue gt-a not found\n"},
	{"not-found envelope", notFoundEnvelope},
	{"truncated array", `[{"id":"gt-a"`},
	{"null element", `[null]`},
}

// TestShowRefusesAnAnswerThatIsNotAnIssueArray: a show whose payload is not the
// issue array is a failed read, never a bead the store does not hold. The old
// parse read a null answer as an empty result, so Show answered ErrNotFound —
// callers took a broken store for a purged bead (gt-avfip).
func TestShowRefusesAnAnswerThatIsNotAnIssueArray(t *testing.T) {
	t.Parallel()
	for _, tc := range showAnswersThatAreNotAnIssueArray {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRecorder(func([]string) reply { return reply{stdout: tc.stdout} })
			got, err := newRecordedBeads(t.TempDir(), r).Show("gt-a")
			if err == nil {
				t.Fatalf("Show(%s answer) = %+v, want an error", tc.name, got)
			}
			if errors.Is(err, ErrNotFound) {
				t.Errorf("Show(%s answer) = %v, want a failed read, not ErrNotFound", tc.name, err)
			}
		})
	}
}

// TestShowMultipleRefusesAnAnswerThatIsNotAnIssueArray: it fails the same way,
// and returns no issues, so a caller cannot read a broken store as a set of
// beads that are gone.
func TestShowMultipleRefusesAnAnswerThatIsNotAnIssueArray(t *testing.T) {
	t.Parallel()
	for _, tc := range showAnswersThatAreNotAnIssueArray {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRecorder(func([]string) reply { return reply{stdout: tc.stdout} })
			got, err := newRecordedBeads(t.TempDir(), r).ShowMultiple([]string{"gt-a", "gt-b"})
			if err == nil {
				t.Fatalf("ShowMultiple(%s answer) = %v, want an error", tc.name, got)
			}
			if len(got) != 0 {
				t.Errorf("ShowMultiple(%s answer) = %v, want no issues", tc.name, got)
			}
		})
	}
}

// TestShowNotFoundEnvelopeIsAnAbsentBead: bd's not-found answer (data null,
// exit 20) is the one null payload that is an answer, and it stays absence:
// ErrNotFound from Show, an empty map from ShowMultiple.
func TestShowNotFoundEnvelopeIsAnAbsentBead(t *testing.T) {
	t.Parallel()
	r := newRecorder(func([]string) reply {
		return reply{stdout: notFoundEnvelope, stderr: "Issue gt-a not found\n", err: exitError{code: 20}}
	})
	b := newRecordedBeads(t.TempDir(), r)

	if got, err := b.Show("gt-a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Show(not-found answer) = %+v, %v, want ErrNotFound", got, err)
	}

	got, err := b.ShowMultiple([]string{"gt-a", "gt-b"})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ShowMultiple(not-found answer) = %v, want ErrNotFound", err)
	}
	if len(got) != 0 {
		t.Errorf("ShowMultiple(not-found answer) = %v, want an empty map", got)
	}
}

// TestShowEmptyArrayIsAnAbsentBead: a bd from before machine mode prints an
// empty array for a show that matched nothing, and that answer is still the
// absence it always was.
func TestShowEmptyArrayIsAnAbsentBead(t *testing.T) {
	t.Parallel()
	r := newRecorder(func([]string) reply { return reply{stdout: "[]\n"} })
	b := newRecordedBeads(t.TempDir(), r)

	if got, err := b.Show("gt-a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Show(empty array) = %+v, %v, want ErrNotFound", got, err)
	}
	got, err := b.ShowMultiple([]string{"gt-a"})
	if err != nil {
		t.Fatalf("ShowMultiple(empty array) = %v, want no error", err)
	}
	if len(got) != 0 {
		t.Errorf("ShowMultiple(empty array) = %v, want an empty map", got)
	}
}
