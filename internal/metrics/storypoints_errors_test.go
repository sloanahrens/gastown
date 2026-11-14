package metrics

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestMetricsStoryPointsRefusesAnUnreadableTable: a landings table the reader
// cannot attribute stops the step with an error and prints no report, so a bad
// table never reads as an empty one.
func TestMetricsStoryPointsRefusesAnUnreadableTable(t *testing.T) {
	t.Parallel()
	town := newCommandTown(t)
	if _, err := town.run(t, CommandExtract); err != nil {
		t.Fatalf("extract: %v", err)
	}
	ragged := []byte("bead,rig,sp,sp_actual\r\ngt-1,gastown\r\n")
	if err := os.WriteFile(filepath.Join(town.outDir(), "landings.csv"), ragged, 0o644); err != nil {
		t.Fatalf("write ragged table: %v", err)
	}
	town.out.Reset()
	if _, err := town.run(t, CommandStoryPoints); err == nil {
		t.Fatalf("storypoints returned no error for a ragged table; output %q", town.out.String())
	}
	if got := town.out.String(); got != "" {
		t.Errorf("out = %q, want no report for an unreadable table", got)
	}
}

// TestStorypointsNoExtractErrors: with no extract holding a landings table the
// step says to run the extract first and names the root it searched. A
// directory or table it cannot read is an error, never a reason to fall back.
func TestStorypointsNoExtractErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// setup builds the town's metrics root; nil leaves it absent.
		setup func(t *testing.T, town *commandTown)
		// readDir, when set, is the directory listing the step gets instead of
		// the host's; nilReadDir leaves the seam unwired.
		readDir    func(string) ([]DirEntry, error)
		nilReadDir bool
		// want is a fragment of the error; wantPath, when set, is a path the
		// error must name.
		want     string
		wantPath func(town *commandTown) string
	}{
		{
			name: "no metrics root",
			want: "run extract first",
		},
		{
			name:       "an unwired listing seam",
			nilReadDir: true,
			want:       "Env.ReadDir is unset",
		},
		{
			name: "a root with no dated extract",
			setup: func(t *testing.T, town *commandTown) {
				if err := os.MkdirAll(filepath.Join(filepath.Dir(town.outDir()), "cache"), 0o755); err != nil {
					t.Fatalf("make cache: %v", err)
				}
			},
			want: "run extract first",
		},
		{
			name: "an unreadable root",
			// The listing seam returns the permission error, because a chmod
			// cannot make the root unreadable to a test that runs as root.
			readDir: func(string) ([]DirEntry, error) {
				return nil, fs.ErrPermission
			},
			want: "list ",
		},
		{
			name: "a newer day's table unreadable",
			setup: func(t *testing.T, town *commandTown) {
				// An older dated extract a fallback could read, and a newer day
				// whose table cannot be read: the newer day is an error, not
				// skipped for the older extract.
				town.writeExtractAt(t, "2026-10-20")
				if err := os.MkdirAll(filepath.Join(filepath.Dir(town.outDir()), "2026-10-22", "landings.csv"), 0o755); err != nil {
					t.Fatalf("make the newer day's table a directory: %v", err)
				}
			},
			want: "read ",
			wantPath: func(town *commandTown) string {
				return filepath.Join(filepath.Dir(town.outDir()), "2026-10-22", landingsName)
			},
		},
		{
			name: "today's table unreadable",
			setup: func(t *testing.T, town *commandTown) {
				// A directory named landings.csv reads as an error, not as an
				// absent file.
				if err := os.MkdirAll(filepath.Join(town.outDir(), "landings.csv"), 0o755); err != nil {
					t.Fatalf("make today's table a directory: %v", err)
				}
			},
			// A listing with no day in it, so a run that mistook the unreadable
			// table for an absent one reports the search instead.
			readDir:  func(string) ([]DirEntry, error) { return nil, nil },
			want:     "read ",
			wantPath: func(town *commandTown) string { return filepath.Join(town.outDir(), landingsName) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := newCommandTown(t)
			if tc.setup != nil {
				tc.setup(t, town)
			}
			if tc.readDir != nil {
				town.env.ReadDir = tc.readDir
			}
			if tc.nilReadDir {
				town.env.ReadDir = nil
			}
			town.out.Reset()
			code, err := town.run(t, CommandStoryPoints)
			if err == nil {
				t.Fatalf("storypoints returned no error (code %d); output %q", code, town.out.String())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), filepath.Dir(town.outDir())) && tc.want == "run extract first" {
				t.Errorf("err = %v, want it to name the root searched %s", err, filepath.Dir(town.outDir()))
			}
			if tc.wantPath != nil {
				if path := tc.wantPath(town); !strings.Contains(err.Error(), path) {
					t.Errorf("err = %v, want it to name %s", err, path)
				}
			}
			if got := town.out.String(); got != "" {
				t.Errorf("out = %q, want no report", got)
			}
		})
	}
}

// TestBootstrapSpearmanRefusesWhatHasNoInterval: a length mismatch and fewer than
// two pairs have no interval, and the bootstrap says so rather than returning one.
func TestBootstrapSpearmanRefusesWhatHasNoInterval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		x, y []float64
	}{
		{name: "length mismatch", x: []float64{1, 2, 3}, y: []float64{1, 2}},
		{name: "one pair", x: []float64{1}, y: []float64{1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := BootstrapSpearman(tc.x, tc.y, bootstrapResamples, bootstrapSeed); err == nil {
				t.Error("BootstrapSpearman returned no error")
			}
		})
	}
}

// TestShadowReportRefusesAMalformedMinutesCell: an estimated-minutes cell that is
// not a positive number stops the report, since the trigger table over it would
// count a bead wrongly.
func TestShadowReportRefusesAMalformedMinutesCell(t *testing.T) {
	t.Parallel()
	for _, cell := range []string{"0", "-3", "soon"} {
		rows := append(measuredRows(60), spLandingRow(97, "5", "5", "haiku", "all"))
		rows[len(rows)-1]["sp_minutes"] = cell
		var out bytes.Buffer
		if err := WriteStoryPointsReport(&out, rows); err == nil {
			t.Errorf("sp_minutes %q: WriteStoryPointsReport returned no error; output:\n%s", cell, out.String())
		}
	}
}

// TestShadowReportRefusesAMalformedMeasureCell: a measured-points cell that is
// not a point on the scale stops the report, the same as a bad estimate.
func TestShadowReportRefusesAMalformedMeasureCell(t *testing.T) {
	t.Parallel()
	rows := append(measuredRows(60), spLandingRow(96, "5", strconv.Itoa(4), "haiku", "all"))
	var out bytes.Buffer
	err := WriteStoryPointsReport(&out, rows)
	if err == nil || !strings.Contains(err.Error(), "sp_actual") {
		t.Errorf("err = %v, want an sp_actual refusal", err)
	}
}
