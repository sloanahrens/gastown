package beads

import (
	"strings"
	"testing"
)

// TestNonJSONOutputIsAnError: a --json call that answers prose or nothing
// did not answer the query. Reading it as zero results is how an outage or
// a flag regression becomes "no work", "nothing stuck", "no MR" (B5-05).
func TestNonJSONOutputIsAnError(t *testing.T) {
	t.Parallel()
	for _, out := range []string{"No issues found.\n", "", "   \n"} {
		r := newRecorder(func([]string) reply { return reply{stdout: out} })
		b := newRecordedBeads(t.TempDir(), r)

		calls := map[string]func() error{
			"List": func() error { _, err := b.List(ListOptions{Priority: -1}); return err },
			"List ephemeral": func() error {
				_, err := b.List(ListOptions{Priority: -1, Ephemeral: true, Label: "gt:merge-request"})
				return err
			},
			"ListIssueStatuses":         func() error { _, err := b.ListIssueStatuses(StatusOpen); return err },
			"ListAssignedIssueStatuses": func() error { _, err := b.ListAssignedIssueStatuses("rig/polecats/x", StatusOpen); return err },
			"PreloadLabeledWisps":       func() error { return b.PreloadLabeledWisps("gt:agent") },
			"ListRigBeads":              func() error { _, err := b.ListRigBeads(); return err },
			"GetAgentBeadInStoreOnly":   func() error { _, _, err := b.GetAgentBeadInStoreOnly("gt-x"); return err },
		}
		for name, call := range calls {
			err := call()
			if err == nil {
				t.Errorf("%s with stdout %q: nil error, want non-JSON error", name, out)
				continue
			}
			if first := strings.TrimSpace(out); first != "" && !strings.Contains(err.Error(), first) {
				t.Errorf("%s: error %q does not name the output %q", name, err, first)
			}
		}
	}
}

func TestEmptyJSONArrayIsZeroResults(t *testing.T) {
	t.Parallel()
	r := newRecorder(func([]string) reply { return reply{stdout: "[]\n"} })
	b := newRecordedBeads(t.TempDir(), r)
	if issues, err := b.List(ListOptions{Priority: -1}); err != nil || len(issues) != 0 {
		t.Fatalf("List on [] = %v, %v; want no issues and no error", issues, err)
	}
	if rigs, err := b.ListRigBeads(); err != nil || len(rigs) != 0 {
		t.Fatalf("ListRigBeads on [] = %v, %v", rigs, err)
	}
}

// TestListRigBeadsIsNotTruncated: bd list caps at 50 rows by default and
// says nothing to a program (B1-04).
func TestListRigBeadsIsNotTruncated(t *testing.T) {
	t.Parallel()
	r := newRecorder(func([]string) reply { return reply{stdout: "[]"} })
	b := newRecordedBeads(t.TempDir(), r)
	if _, err := b.ListRigBeads(); err != nil {
		t.Fatal(err)
	}
	if argv := strings.Join(r.calls()[0].args, " "); !strings.Contains(argv, "--limit=0") {
		t.Fatalf("ListRigBeads argv %q lacks --limit=0", argv)
	}
}
