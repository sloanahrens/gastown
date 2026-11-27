package done

import (
	"errors"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/outbox"
)

// TestNoBeadAccessDeferredAfterFailedPush: the deferred result for a failed
// push validates with the failure after any earlier escalation text, no
// submission, and the fields it started with.
func TestNoBeadAccessDeferredAfterFailedPush(t *testing.T) {
	t.Parallel()
	title, acceptance := "the task", "- [ ] one\n- [x] two"
	pushErr, earlier := errors.New("connection reset by peer"), "HELP: a missing file"
	for name, tc := range map[string]struct {
		in      outbox.Result
		pushErr error
		want    outbox.Result
	}{
		"submission dropped": {in: outbox.Result{Status: outbox.StatusDone, Title: &title, Acceptance: &acceptance, Ticked: []int{0, 1}, Findings: []string{"a defect"}, Submission: &outbox.Submission{Branch: "b", Head: "abcdef1", Target: "main"}}, pushErr: pushErr,
			want: outbox.Result{Status: outbox.StatusDeferred, Title: &title, Acceptance: &acceptance, Ticked: []int{0, 1}, Findings: []string{"a defect"}, Escalation: &outbox.Escalation{Text: "the push of the branch failed: connection reset by peer"}}},
		"earlier text in front": {in: outbox.Result{Escalation: &outbox.Escalation{Text: earlier}, Ticked: []int{0}}, pushErr: pushErr,
			want: outbox.Result{Status: outbox.StatusDeferred, Ticked: []int{0}, Escalation: &outbox.Escalation{Text: earlier + "\n\nthe push of the branch failed: connection reset by peer"}}},
		"no push error": {in: outbox.Result{Ticked: []int{0}},
			want: outbox.Result{Status: outbox.StatusDeferred, Ticked: []int{0}, Escalation: &outbox.Escalation{Text: "the push of the branch failed"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			valid, err := outbox.Validate(outbox.Encode("gt-uwxe4w", deferredAfterFailedPush(tc.in, tc.pushErr)), nil, "gt-uwxe4w")
			if err != nil {
				t.Fatalf("the deferred result does not validate: %v", err)
			}
			if !reflect.DeepEqual(valid, tc.want) {
				t.Errorf("result = %+v, want %+v", valid, tc.want)
			}
		})
	}
}
