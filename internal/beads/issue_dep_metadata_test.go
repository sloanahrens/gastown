package beads

import (
	"fmt"
	"testing"
)

// bd prints a bead's metadata as an object when it was written as one (the
// dispatcher's spec_dispatch_timeout_attempts is), and show prints every
// dependency with that metadata. Decoded straight into a string the whole
// answer failed to parse, taking every dependency of the bead with it and, in
// the dispatcher, the tick's design/notes fetch (gt-l40eg).
func TestShowParsesDependencyMetadataOfAnyJSONShape(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		metadata string
		want     string
	}{
		{"an object", `{"spec_dispatch_timeout_attempts": "1"}`, `{"spec_dispatch_timeout_attempts": "1"}`},
		{"a JSON string", `"plain"`, "plain"},
		{"null", `null`, ""},
		{"no metadata key", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			metadata := ""
			if tc.metadata != "" {
				metadata = `,"metadata":` + tc.metadata
			}
			payload := fmt.Sprintf(`{"contract_version": 1, "schema_version": 1, "data": [{
				"id": "gt-a", "title": "a", "status": "open", "priority": 1,
				"issue_type": "task", "updated_at": "2026-11-01T00:00:00Z",
				"dependencies": [{"id": "gt-dep", "title": "dep", "status": "closed",
					"priority": 2, "issue_type": "task", "dependency_type": "blocks"%s}]
			}]}`, metadata)
			r := newRecorder(func(args []string) reply {
				if args[0] == "show" {
					return reply{stdout: payload}
				}
				return reply{err: fmt.Errorf("unexpected bd call %v", args)}
			})

			shown, err := newRecordedBeads(t.TempDir(), r).ShowMultiple([]string{"gt-a"})
			if err != nil {
				t.Fatalf("ShowMultiple: %v", err)
			}
			issue := shown["gt-a"]
			if issue == nil || len(issue.Dependencies) != 1 {
				t.Fatalf("show answered %+v, want gt-a with its one dependency", issue)
			}
			dep := issue.Dependencies[0]
			if dep.ID != "gt-dep" || dep.DependencyType != "blocks" {
				t.Errorf("dependency = %+v, want gt-dep as a blocks edge", dep)
			}
			if dep.Metadata != tc.want {
				t.Errorf("dependency metadata = %q, want %q", dep.Metadata, tc.want)
			}
		})
	}
}
