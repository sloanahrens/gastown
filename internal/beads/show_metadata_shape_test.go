package beads

import (
	"testing"
)

// TestShowMultipleReadsMetadataInAnyJSONShape pins ShowMultiple's read of a
// metadata column in each shape bd can print (gt-euvnd): an object, a JSON
// string, absent, and null on a dependency. It covers the issue and its
// dependencies.
func TestShowMultipleReadsMetadataInAnyJSONShape(t *testing.T) {
	t.Parallel()
	dependencyObject := `[{"id":"gt-1","dependencies":[{"id":"gt-2","metadata":{"holder":"ace"}}]}]`
	dependencyString := `[{"id":"gt-1","dependencies":[{"id":"gt-2","metadata":"{}"}]}]`
	dependencyAbsent := `[{"id":"gt-1","dependencies":[{"id":"gt-2"}]}]`
	dependencyNull := `[{"id":"gt-1","dependencies":[{"id":"gt-2","metadata":null}]}]`

	tests := []struct {
		name      string
		stdout    string
		wantIssue string
		wantDeps  int
		wantDep   string
	}{
		{
			name:      "issue metadata object",
			stdout:    `[{"id":"gt-1","metadata":{"delegated_from":"gt-9"}}]`,
			wantIssue: `{"delegated_from":"gt-9"}`,
		},
		{
			name:      "issue metadata string",
			stdout:    `[{"id":"gt-1","metadata":"{}"}]`,
			wantIssue: `"{}"`,
		},
		{
			name:   "issue metadata absent",
			stdout: `[{"id":"gt-1"}]`,
		},
		{
			name:     "dependency metadata object",
			stdout:   dependencyObject,
			wantDeps: 1,
			wantDep:  `{"holder":"ace"}`,
		},
		{
			name:     "dependency metadata string",
			stdout:   dependencyString,
			wantDeps: 1,
			wantDep:  `{}`,
		},
		{
			name:     "dependency metadata absent",
			stdout:   dependencyAbsent,
			wantDeps: 1,
		},
		{
			name:     "dependency metadata null",
			stdout:   dependencyNull,
			wantDeps: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRecorder(func([]string) reply { return reply{stdout: tc.stdout} })
			got, err := newRecordedBeads(t.TempDir(), r).ShowMultiple([]string{"gt-1"})
			if err != nil {
				t.Fatalf("ShowMultiple(%s) = %v, want the bead read", tc.name, err)
			}
			issue := got["gt-1"]
			if issue == nil {
				t.Fatalf("ShowMultiple(%s) returned no gt-1", tc.name)
			}
			if tc.wantIssue == "" {
				if len(issue.Metadata) != 0 {
					t.Errorf("issue metadata = %s, want none", issue.Metadata)
				}
			} else if string(issue.Metadata) != tc.wantIssue {
				t.Errorf("issue metadata = %s, want %s", issue.Metadata, tc.wantIssue)
			}
			if len(issue.Dependencies) != tc.wantDeps {
				t.Fatalf("dependencies = %d, want %d", len(issue.Dependencies), tc.wantDeps)
			}
			for _, dep := range issue.Dependencies {
				if dep.Metadata != tc.wantDep {
					t.Errorf("dependency metadata = %q, want %q", dep.Metadata, tc.wantDep)
				}
			}
		})
	}
}
