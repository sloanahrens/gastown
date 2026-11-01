package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/metrics"
)

// interventionTestNow is the instant the record command runs at in these tests.
var interventionTestNow = time.Date(2026, 11, 1, 10, 47, 0, 0, time.UTC)

// interventionTestTown is a town whose route table names the gastown rig.
func interventionTestTown(t *testing.T) string {
	t.Helper()
	return interventionTestTownWithRoutes(t, `{"path":"gastown/mayor/rig","prefix":"gt-"}`+"\n")
}

// interventionTestTownWithRoutes is a town holding routes as its route table.
func interventionTestTownWithRoutes(t *testing.T, routes string) string {
	t.Helper()
	town := t.TempDir()
	beadsDir := filepath.Join(town, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}
	return town
}

// interventionTestCommand runs gt intervention over town, the fixed instant and
// actor, as the process builds it over the working directory, the wall clock and
// detectActor.
func interventionTestCommand(t *testing.T, town string, args ...string) error {
	t.Helper()
	cmd := newInterventionCommand(
		func() (string, error) { return town, nil },
		func() time.Time { return interventionTestNow },
		func() string { return "gastown/polecats/capable" },
	)
	cmd.SetArgs(args)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	return cmd.Execute()
}

// TestInterventionRecordRefusesBadInput runs the command as built and requires
// each bad input to exit 1, name what is wrong, and write no record.
func TestInterventionRecordRefusesBadInput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		town    func(t *testing.T) string
		args    []string
		wantErr string
	}{
		{
			name:    "an unknown kind",
			args:    []string{"record", "--kind", "seat", "--reason", "seats moved"},
			wantErr: "unknown kind",
		},
		{
			name:    "an empty reason",
			args:    []string{"record", "--kind", "hold", "--reason", "  "},
			wantErr: "empty reason",
		},
		{
			name:    "no kind",
			args:    []string{"record", "--reason", "held the queue"},
			wantErr: "kind",
		},
		{
			name:    "no reason",
			args:    []string{"record", "--kind", "hold"},
			wantErr: "reason",
		},
		{
			name:    "a takeover with no bead",
			args:    []string{"record", "--kind", "takeover", "--reason", "took the bead over"},
			wantErr: "needs a bead",
		},
		{
			name:    "a bead no route names",
			args:    []string{"record", "--kind", "takeover", "--bead", "zz-1", "--reason", "took the bead over"},
			wantErr: "no route names prefix",
		},
		{
			name:    "a bead with no prefix",
			args:    []string{"record", "--kind", "takeover", "--bead", "nosep", "--reason", "took the bead over"},
			wantErr: "names no prefix",
		},
		{
			name: "a route whose path has no rig segment",
			town: func(t *testing.T) string {
				return interventionTestTownWithRoutes(t, `{"path":"/mayor/rig","prefix":"gt-"}`)
			},
			args:    []string{"record", "--kind", "takeover", "--bead", "gt-abc", "--reason", "took the bead over"},
			wantErr: "names no rig",
		},
		{
			name: "a route table that cannot be read",
			town: func(t *testing.T) string {
				town := interventionTestTown(t)
				// A directory where the file belongs: the read fails, and it must
				// not be read as "this bead has no rig".
				path := filepath.Join(town, ".beads", "routes.jsonl")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
				return town
			},
			args:    []string{"record", "--kind", "takeover", "--bead", "gt-abc", "--reason", "took the bead over"},
			wantErr: "route table",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := interventionTestTown(t)
			if tc.town != nil {
				town = tc.town(t)
			}
			err := interventionTestCommand(t, town, tc.args...)
			if err == nil {
				t.Fatalf("record %v succeeded, want it refused", tc.args)
			}
			if got := exitCodeForError(err); got != 1 {
				t.Fatalf("record %v exits %d, want 1 (err %v)", tc.args, got, err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.wantErr)
			}
			if _, statErr := os.Stat(metrics.InterventionsPath(town)); !errors.Is(statErr, fs.ErrNotExist) {
				t.Fatalf("a refused record left %s behind (%v)", metrics.InterventionsPath(town), statErr)
			}
		})
	}
}

// TestInterventionRecordAppendsTheLine runs the command as built and reads the
// line it wrote: the bead's rig from the town's route table, the record's fields
// as the flags supplied them, stamped at the command's instant.
func TestInterventionRecordAppendsTheLine(t *testing.T) {
	t.Parallel()
	town := interventionTestTown(t)
	err := interventionTestCommand(t, town,
		"record", "--kind", "takeover", "--bead", "gt-abc", "--reason", "the polecat stalled on the gate")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	path := metrics.InterventionsPath(town)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("%d lines, want 1: %q", len(lines), data)
	}
	var got metrics.InterventionRecord
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("decoding the line: %v", err)
	}
	want := metrics.InterventionRecord{
		TS:     interventionTestNow,
		Kind:   "takeover",
		Bead:   "gt-abc",
		Rig:    "gastown",
		Reason: "the polecat stalled on the gate",
		Actor:  "gastown/polecats/capable",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("line = %#v; want %#v", got, want)
	}
}

// TestInterventionRecordWithoutABead records a hold with no bead and an --actor
// override: the line names the town rather than a rig, and the actor the flag
// gave it.
func TestInterventionRecordWithoutABead(t *testing.T) {
	t.Parallel()
	town := interventionTestTown(t)
	err := interventionTestCommand(t, town,
		"record", "--kind", "hold", "--reason", "paused dispatch until the gate question is answered",
		"--actor", "town/crew/merlin")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	path := metrics.InterventionsPath(town)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var got metrics.InterventionRecord
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decoding the line: %v", err)
	}
	if got.Kind != "hold" || got.Bead != "" || got.Rig != "" {
		t.Errorf("line = %#v; want a hold with no bead and no rig", got)
	}
	if got.Actor != "town/crew/merlin" {
		t.Errorf("actor = %q; want the --actor override", got.Actor)
	}
}

// TestInterventionRecordOnATownBead records a town-level bead: its route is ".",
// so the line carries no rig rather than a guessed one.
func TestInterventionRecordOnATownBead(t *testing.T) {
	t.Parallel()
	town := interventionTestTownWithRoutes(t, `{"path":".","prefix":"hq-"}`+"\n")
	err := interventionTestCommand(t, town,
		"record", "--kind", "waiver", "--bead", "hq-abc", "--reason", "waived the om review on a known head")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	data, err := os.ReadFile(metrics.InterventionsPath(town))
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	var got metrics.InterventionRecord
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decoding the line: %v", err)
	}
	if got.Bead != "hq-abc" || got.Rig != "" {
		t.Errorf("line = %#v; want bead hq-abc and no rig", got)
	}
}

// TestInterventionRecordReportsAFailedWrite requires a record the host refuses
// to write to exit 1 naming the failure, rather than report the intervention as
// recorded.
func TestInterventionRecordReportsAFailedWrite(t *testing.T) {
	t.Parallel()
	town := interventionTestTown(t)
	if err := os.MkdirAll(filepath.Join(town, ".runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A file where the record's directory belongs: the directory cannot be made.
	if err := os.WriteFile(filepath.Join(town, ".runtime", "metrics"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := interventionTestCommand(t, town, "record", "--kind", "hold", "--reason", "paused dispatch")
	if err == nil {
		t.Fatal("record on an unwritable path succeeded, want it refused")
	}
	if got := exitCodeForError(err); got != 1 {
		t.Fatalf("record exits %d, want 1 (err %v)", got, err)
	}
	if !strings.Contains(err.Error(), "creating") {
		t.Fatalf("error = %q, want it to name the directory it could not create", err)
	}
}

// TestInterventionRecordOutsideATown requires the command to refuse when the
// working directory names no town, rather than record into a guessed path.
func TestInterventionRecordOutsideATown(t *testing.T) {
	t.Parallel()
	cmd := newInterventionCommand(
		func() (string, error) { return "", errors.New("no town root") },
		func() time.Time { return interventionTestNow },
		func() string { return "gastown/polecats/capable" },
	)
	cmd.SetArgs([]string{"record", "--kind", "hold", "--reason", "paused dispatch"})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("record outside a town succeeded, want it refused")
	}
	if got := exitCodeForError(err); got != 1 {
		t.Fatalf("record outside a town exits %d, want 1 (err %v)", got, err)
	}
	if !strings.Contains(err.Error(), "not in a Gas Town workspace") {
		t.Fatalf("error = %q, want it to say the workspace is missing", err)
	}
}
