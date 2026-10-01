package beads

import (
	"slices"
	"testing"
)

// TestParseMolReadyOutput is the gt-gmfcc unit test for the CLI-path parser:
// bd answers `ready --mol` with a molecule object, whose steps hold the
// issues. A bare array — the shape the parser took before bd wrapped the
// steps — still parses; a step without its issue is a shape change under the
// same key and must be loud, because a quiet drop is what the auto-continue
// path cannot tell from "no more steps".
func TestParseMolReadyOutput(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		wantID []string // nil means the call is expected to fail
	}{
		{
			name:   "molecule object",
			in:     `{"molecule_id":"gt-mol-1","ready_steps":2,"total_steps":9,"steps":[{"issue":{"id":"gt-mol-1.1","title":"one","status":"open","priority":2,"issue_type":"task"},"parallel_group":"group-1"},{"issue":{"id":"gt-mol-1.2","title":"two","status":"open","priority":2,"issue_type":"task"},"parallel_group":"group-1"}]}`,
			wantID: []string{"gt-mol-1.1", "gt-mol-1.2"},
		},
		{
			name:   "molecule object with no ready steps",
			in:     `{"molecule_id":"gt-mol-1","ready_steps":0,"total_steps":9,"steps":[]}`,
			wantID: []string{},
		},
		{
			name:   "legacy plain array",
			in:     `[{"id":"gt-mol-1.3","title":"three","status":"open","priority":2,"issue_type":"task"}]`,
			wantID: []string{"gt-mol-1.3"},
		},
		{
			// No stdout is not the empty page — that is a molecule object
			// with "steps":[]. An empty read that parsed as "no ready steps"
			// would read as a finished molecule.
			name: "empty input",
			in:   "",
		},
		{
			name: "malformed json",
			in:   `{"molecule_id":`,
		},
		{
			name: "step without its issue",
			in:   `{"molecule_id":"gt-mol-1","steps":[{"parallel_group":"group-1"}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issues, err := parseMolReadyOutput([]byte(tt.in))

			if tt.wantID == nil {
				if err == nil {
					t.Fatalf("expected an error, got %v", issues)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseMolReadyOutput: %v", err)
			}
			ids := make([]string, len(issues))
			for i, issue := range issues {
				ids[i] = issue.ID
			}
			if !slices.Equal(ids, tt.wantID) {
				t.Fatalf("issues = %v, want %v", ids, tt.wantID)
			}
		})
	}
}

// TestReadyForMolUnwrapsBDMolReadyObject drives the CLI path through the bd
// seam with the two wrappers bd 1.2.2 puts around the ready steps: the
// machine envelope every gastown subprocess asks for, and the molecule object
// inside it. Unwrapping only the machine envelope left ReadyForMol holding an
// object where it wanted issues (gt-gmfcc).
func TestReadyForMolUnwrapsBDMolReadyObject(t *testing.T) {
	const bdStdout = `{
  "schema_version": 1,
  "contract_version": 1,
  "data": {
    "molecule_id": "gt-mol-1",
    "molecule_title": "mol-polecat-work",
    "total_steps": 9,
    "ready_steps": 2,
    "parallel_groups": {"group-1": ["gt-mol-1.1", "gt-mol-1.2"]},
    "steps": [
      {"issue": {"id": "gt-mol-1.1", "title": "one", "status": "open", "priority": 2, "issue_type": "task"}, "parallel_group": "group-1", "parallel_info": {"is_ready": true}},
      {"issue": {"id": "gt-mol-1.2", "title": "two", "status": "open", "priority": 2, "issue_type": "task"}, "parallel_group": "group-1", "parallel_info": {"is_ready": true}}
    ]
  },
  "pagination": null,
  "error": null
}`

	r := newRecorder(func([]string) reply { return reply{stdout: bdStdout} })
	b := newRecordedBeads(t.TempDir(), r)

	issues, err := b.ReadyForMol("gt-mol-1")
	if err != nil {
		t.Fatalf("ReadyForMol: %v", err)
	}
	if len(issues) != 2 || issues[0].ID != "gt-mol-1.1" || issues[1].ID != "gt-mol-1.2" {
		ids := make([]string, len(issues))
		for i, issue := range issues {
			ids[i] = issue.ID
		}
		t.Fatalf("issues = %v, want [gt-mol-1.1 gt-mol-1.2]", ids)
	}

	if want := "ready --mol gt-mol-1 --json -n 100"; len(r.argvs()) != 1 || r.argvs()[0] != want {
		t.Fatalf("bd calls = %v, want [%s]", r.argvs(), want)
	}
}
