package beads

import (
	"errors"
	"testing"
)

// showPartialEnvelope is bd 461b0f0's machine-mode answer to
// "bd show --json gt-a gt-nosuch gt-b" (exit 22), captured from the Dolt test
// container: the issues it found in data, the missing id in error.ids.
const showPartialEnvelope = `{
  "schema_version": 1,
  "contract_version": 1,
  "data": [
    {"id": "gt-a", "title": "a", "status": "open", "priority": 2, "issue_type": "task"},
    {"id": "gt-b", "title": "b", "status": "open", "priority": 2, "issue_type": "task"}
  ],
  "pagination": null,
  "error": {
    "kind": "partial",
    "message": "1 of 3 ids failed",
    "ids": [{"id": "gt-nosuch", "kind": "not_found", "message": "no issue found matching \"gt-nosuch\""}]
  }
}`

// TestShowMultipleSkipsMissingIDsOfAPartialShow: ShowMultiple returns the
// issues that exist and leaves out the ones bd says are not found, as the
// fake does; bd with the machine surface reports that as a partial failure.
func TestShowMultipleSkipsMissingIDsOfAPartialShow(t *testing.T) {
	t.Parallel()
	r := newRecorder(func([]string) reply {
		return reply{
			stdout: showPartialEnvelope,
			stderr: "Issue gt-nosuch not found\nHint: this ID may have never existed",
			err:    exitError{code: 22},
		}
	})
	got, err := newRecordedBeads(t.TempDir(), r).ShowMultiple([]string{"gt-a", "gt-nosuch", "gt-b"})
	if err != nil {
		t.Fatalf("ShowMultiple: %v", err)
	}
	if len(got) != 2 || got["gt-a"] == nil || got["gt-b"] == nil || got["gt-a"].Title != "a" {
		t.Errorf("ShowMultiple = %v, want exactly gt-a and gt-b", got)
	}
}

// TestShowMultiplePartialWithAnUnreadableIDFails: a partial show is only
// absence when every failed id is not_found; any other failure leaves the
// answer unknown.
func TestShowMultiplePartialWithAnUnreadableIDFails(t *testing.T) {
	t.Parallel()
	r := newRecorder(func([]string) reply {
		return reply{
			stdout: `{"schema_version":1,"contract_version":1,"data":[{"id":"gt-a","title":"a"}],"pagination":null,` +
				`"error":{"kind":"partial","message":"1 of 2 ids failed","ids":[{"id":"gt-x","kind":"route_unreachable","message":"no route"}]}}`,
			err: exitError{code: 22},
		}
	})
	_, err := newRecordedBeads(t.TempDir(), r).ShowMultiple([]string{"gt-a", "gt-x"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ShowMultiple = %v, want ErrUnavailable", err)
	}
}
