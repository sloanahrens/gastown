package beads

import (
	"strconv"
	"strings"
	"testing"
)

// molStepJSON builds one entry of bd's ready --mol steps array, in the shape
// bd 1.2.2 prints: the issue behind an "issue" key, parallel metadata beside it.
func molStepJSON(id string) string {
	return `{"issue":{"id":"` + id + `","title":"step","status":"open","priority":2,"issue_type":"task"},"parallel_group":"group-1","parallel_info":{}}`
}

// molEnvelope wraps steps the way bd ready --mol --json prints them.
func molEnvelope(steps ...string) string {
	return `{"molecule_id":"gt-mol-1","molecule_title":"mol-polecat-work","total_steps":9,"ready_steps":` +
		strconv.Itoa(len(steps)) + `,"steps":[` + strings.Join(steps, ",") + `],"parallel_groups":{}}`
}

// TestParseReadyMolOutput pins the gt-e2d9d fix in the CLI-path parser: bd
// wraps ready --mol's steps in an envelope whose entries hold the issue behind
// "issue", so the flat decode that predates the envelope fails with "cannot
// unmarshal object into Go value of type []*beads.Issue" on every molecule. A
// step entry that carries no issue is an error — a shape this parser does not
// understand must not read as an empty molecule.
func TestParseReadyMolOutput(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantIDs []string
		wantErr bool
	}{
		{
			name:    "envelope with two steps",
			in:      molEnvelope(molStepJSON("gt-step-1"), molStepJSON("gt-step-2")),
			wantIDs: []string{"gt-step-1", "gt-step-2"},
		},
		{
			name: "envelope with no ready steps",
			in:   molEnvelope(),
		},
		{
			name:    "legacy plain array",
			in:      `[{"id":"gt-step-1","title":"step","status":"open","priority":2,"issue_type":"task"}]`,
			wantIDs: []string{"gt-step-1"},
		},
		{name: "step without an issue", in: `{"molecule_id":"gt-mol-1","steps":[{"parallel_group":"group-1"}]}`, wantErr: true},
		{name: "object with no steps key", in: `{"molecule_id":"gt-mol-1"}`, wantErr: true},
		{name: "steps not an array", in: `{"molecule_id":"gt-mol-1","steps":{}}`, wantErr: true},
		{name: "malformed json", in: `{"molecule_id":`, wantErr: true},
		{name: "empty input", in: ``, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issues, err := parseReadyMolOutput([]byte(tt.in))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %d issues", len(issues))
				}
				return
			}
			if err != nil {
				t.Fatalf("parseReadyMolOutput: %v", err)
			}
			ids := make([]string, len(issues))
			for i, issue := range issues {
				ids[i] = issue.ID
			}
			if strings.Join(ids, ",") != strings.Join(tt.wantIDs, ",") {
				t.Fatalf("ids = %v, want %v", ids, tt.wantIDs)
			}
		})
	}
}

// TestReadyForMolReadsBDsMolEnvelope drives the CLI path end to end against the
// document a real bd prints under machine mode (BD_MACHINE=1): the mol envelope
// inside the machine envelope's data, pagination null. ReadyForMol's job is to
// come back with the step issues in bd's order, not the envelope.
func TestReadyForMolReadsBDsMolEnvelope(t *testing.T) {
	t.Parallel()
	data := molEnvelope(molStepJSON("gt-step-1"), molStepJSON("gt-step-2"))
	out := `{"schema_version":1,"contract_version":1,"data":` + data + `,"pagination":null,"error":null}`
	rec := newRecorder(func(args []string) reply {
		if len(args) > 0 && args[0] == "ready" {
			return reply{stdout: out}
		}
		return reply{}
	})

	issues, err := newRecordedBeads(t.TempDir(), rec).ReadyForMol("gt-mol-1")
	if err != nil {
		t.Fatalf("ReadyForMol: %v", err)
	}
	if len(issues) != 2 || issues[0].ID != "gt-step-1" || issues[1].ID != "gt-step-2" {
		t.Fatalf("ReadyForMol = %v, want [gt-step-1 gt-step-2]", issues)
	}
	argv := strings.Join(rec.calls()[0].args, " ")
	if !strings.Contains(argv, "ready --mol gt-mol-1 --json") {
		t.Errorf("argv %q, want a bd ready --mol query", argv)
	}
}

// TestReadyForMolDropsTheMoleculeRoot drives the CLI path against the envelope
// a real bd prints for a live molecule, where the root is one of the ready
// steps beside the step that is actually next. A walker that keeps the root
// takes it for a second ready step and continues to the molecule itself
// instead of the next step (gt-mejma).
func TestReadyForMolDropsTheMoleculeRoot(t *testing.T) {
	t.Parallel()
	data := molEnvelope(molStepJSON("gt-mol-1"), molStepJSON("gt-step-1"))
	out := `{"schema_version":1,"contract_version":1,"data":` + data + `,"pagination":null,"error":null}`
	rec := newRecorder(func(args []string) reply {
		if len(args) > 0 && args[0] == "ready" {
			return reply{stdout: out}
		}
		return reply{}
	})

	issues, err := newRecordedBeads(t.TempDir(), rec).ReadyForMol("gt-mol-1")
	if err != nil {
		t.Fatalf("ReadyForMol: %v", err)
	}
	if len(issues) != 1 || issues[0].ID != "gt-step-1" {
		t.Fatalf("ReadyForMol = %v, want [gt-step-1]", issues)
	}
}
