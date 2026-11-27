package rehearsal

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// gateNotes is a READY TO LAND block whose gate passed in one minute.
const gateNotes = "READY TO LAND\nBranch: b\nHead: h\nTarget: main\nWorker: toast\nGate: pass presubmit 1m, slot wait 0s"

// withGateFigure answers bead reads with gateNotes, so the run records a passing gate.
func withGateFigure(run func(context.Context, []string) ([]byte, error)) func(context.Context, []string) ([]byte, error) {
	return func(ctx context.Context, argv []string) ([]byte, error) {
		out, err := run(ctx, argv)
		if err != nil || !slices.Contains(argv, "show") {
			return out, err
		}
		return json.Marshal(map[string][]map[string]string{"data": {{"status": "closed", "notes": gateNotes}}})
	}
}

func TestRehearsalExecuteRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		noGate        bool
		interventions []string
		wantOut       string
		wantCode      int
	}{
		{name: "a passing run with no intervention prints pass and returns 0", wantOut: "pass\n", wantCode: 0},
		{name: "a run with one intervention prints fail and its reason and returns 1", interventions: []string{"held dispatch"}, wantOut: "fail\nintervention: held dispatch\n", wantCode: 1},
		{name: "a run with no gate figure prints fail and its reason and returns 1", noGate: true, wantOut: "fail\nno gate figure: the run is unmeasured\n", wantCode: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := driveOptions(t, &driveFake{}, 1, filepath.Join(t.TempDir(), "record.json"))
			if !tt.noGate {
				o.Town.Run = withGateFigure(o.Town.Run)
			}
			o.Interventions = tt.interventions
			var stdout, stderr bytes.Buffer
			if code := Execute(context.Background(), o, &stdout, &stderr); code != tt.wantCode {
				t.Errorf("Execute = %d, want %d", code, tt.wantCode)
			}
			if stdout.String() != tt.wantOut || stderr.Len() != 0 {
				t.Errorf("Execute printed %q and %q; want %q and nothing", stdout.String(), stderr.String(), tt.wantOut)
			}
		})
	}
}

func TestRehearsalExecuteDriveError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		badDir  bool
		wantErr string
	}{
		{name: "a failed filing prints one stderr line and no verdict", wantErr: "gt-rehearsal: run: drive: file set: file task 0: bd create: town call failed\n"},
		{name: "a failed filing and a failed save join on one stderr line", badDir: true, wantErr: "gt-rehearsal: run: drive: file set: file task 0: bd create: town call failed; drive: save record: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "record.json")
			if tt.badDir {
				path = filepath.Join(t.TempDir(), "missing", "record.json")
			}
			o := driveOptions(t, &driveFake{failAt: "create"}, 1, path)
			var stdout, stderr bytes.Buffer
			if code := Execute(context.Background(), o, &stdout, &stderr); code != 1 {
				t.Errorf("Execute = %d, want 1", code)
			}
			if stdout.Len() != 0 {
				t.Errorf("Execute printed a verdict %q after a Drive error, want none", stdout.String())
			}
			if strings.Count(stderr.String(), "\n") != 1 || !strings.HasPrefix(stderr.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want one line starting %q", stderr.String(), tt.wantErr)
			}
		})
	}
}
