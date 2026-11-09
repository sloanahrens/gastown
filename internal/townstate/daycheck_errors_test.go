package townstate

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCheckGitSpeedIsNotRunWhenItCannotMeasure drives the git speed check's
// refusals: a run past its bound, and a baseline that cannot be read. Each is
// a row that did not run, so it counts as a failure, and names what stopped it.
func TestCheckGitSpeedIsNotRunWhenItCannotMeasure(t *testing.T) {
	t.Parallel()
	validBaseline := `{"rigs":{"gastown":{"status --short":0.1,"fetch --dry-run":0.1}}}`
	tests := []struct {
		name     string
		files    map[string]string
		dirs     []string
		runs     map[string][]time.Duration
		failRow  string
		wantText string
	}{
		{
			name:     "a git run past its bound is not run",
			files:    map[string]string{gitBaselineRel: validBaseline},
			runs:     map[string][]time.Duration{"status --short": {61 * time.Second, 61 * time.Second, 61 * time.Second}},
			failRow:  "git speed gastown: status --short",
			wantText: "did not finish within 1m0s",
		},
		{
			name:     "a baseline that does not parse is not run",
			files:    map[string]string{gitBaselineRel: "{not json"},
			failRow:  "git speed baseline",
			wantText: "reading daemon/town-state-git-baseline.json",
		},
		{
			name:     "a baseline that cannot be read is not run",
			dirs:     []string{gitBaselineRel},
			failRow:  "git speed baseline",
			wantText: "reading daemon/town-state-git-baseline.json",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := writeTown(t, townFiles{files: tt.files, dirs: tt.dirs})
			host := newFakeHost(gitAnswer(tt.runs))
			r := findRow(t, CheckGitSpeed(context.Background(), host.env(town)), tt.failRow)
			if r.OK || !strings.HasPrefix(r.Result, "not run: ") || !strings.Contains(r.Result, tt.wantText) {
				t.Errorf("row %q = %+v, want not run mentioning %q", r.Check, r, tt.wantText)
			}
		})
	}
}

// TestCheckDaemonAndLandingAreNotRunWhenTheirFilesCannotBeRead drives the
// refusals that read the cut-over record and the daemon log. A cut-over record
// that cannot be read stops the landing check too, since it reads the same
// record.
func TestCheckDaemonAndLandingAreNotRunWhenTheirFilesCannotBeRead(t *testing.T) {
	t.Parallel()
	goodCutover := map[string]string{CutoverRecordRel: cutoverJSON(cutoverTime)}
	tests := []struct {
		name       string
		files      map[string]string
		dirs       []string
		wantText   string
		landingToo bool
	}{
		{
			name:     "a missing daemon log is not run",
			files:    goodCutover,
			wantText: "reading daemon/daemon.log",
		},
		{
			name:     "a daemon log that cannot be read is not run",
			files:    goodCutover,
			dirs:     []string{daemonLogRel},
			wantText: "reading daemon/daemon.log",
		},
		{
			name:       "a cut-over record that does not parse is not run",
			files:      map[string]string{CutoverRecordRel: "{not json"},
			wantText:   "reading daemon/town-state-cutover.json",
			landingToo: true,
		},
		{
			name:       "a cut-over time that is not RFC 3339 is not run",
			files:      map[string]string{CutoverRecordRel: `{"cutover_at":"yesterday"}`},
			wantText:   "cutover_at is not RFC 3339",
			landingToo: true,
		},
		{
			name:       "a cut-over record that cannot be read is not run",
			dirs:       []string{CutoverRecordRel},
			wantText:   "reading daemon/town-state-cutover.json",
			landingToo: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := writeTown(t, townFiles{files: tt.files, dirs: tt.dirs})
			host := newFakeHost(func(_, _ string, _ []string) fakeAnswer {
				return fakeAnswer{out: "GREEN\n", took: time.Millisecond}
			})
			env := host.env(town)
			rows := CheckDaemonStart(context.Background(), env)
			for _, prefix := range []string{"daemon log: starts", "daemon log: panics"} {
				r := findRow(t, rows, prefix)
				if r.OK || !strings.HasPrefix(r.Result, "not run: ") || !strings.Contains(r.Result, tt.wantText) {
					t.Errorf("row %q = %+v, want not run mentioning %q", r.Check, r, tt.wantText)
				}
			}
			if !tt.landingToo {
				return
			}
			r := CheckLandingSinceCutover(env)
			if r.OK || !strings.HasPrefix(r.Result, "not run: ") || !strings.Contains(r.Result, tt.wantText) {
				t.Errorf("landing row = %+v, want not run mentioning %q", r, tt.wantText)
			}
		})
	}
}

// TestCheckRecordBaselineRefusesUnwritableState drives the baseline record's
// refusals: a state path that cannot be checked, a state directory or temporary
// file or baseline path that cannot be written, and a registry that names no
// rig or does not parse. Each refusal names what it could not do.
func TestCheckRecordBaselineRefusesUnwritableState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		force    bool
		files    map[string]string
		dirs     []string
		rigsJSON string
		wantText string
	}{
		{
			name:     "a state path under a file is refused by the existence check",
			files:    map[string]string{"daemon": "not a directory"},
			wantText: "checking daemon/town-state-git-baseline.json",
		},
		{
			name:     "a state directory that is a file fails the write when forced",
			force:    true,
			files:    map[string]string{"daemon": "not a directory"},
			wantText: "creating the daemon state directory",
		},
		{
			name:     "a temporary baseline path that is a directory fails the write",
			dirs:     []string{gitBaselineRel + ".tmp"},
			wantText: "writing daemon/town-state-git-baseline.json",
		},
		{
			name:     "a baseline path that is a directory fails the replace when forced",
			force:    true,
			dirs:     []string{gitBaselineRel},
			wantText: "writing daemon/town-state-git-baseline.json",
		},
		{
			name:     "a registry that names no rig is refused",
			rigsJSON: `{"version":1,"rigs":{}}`,
			wantText: "the town has no rigs to time",
		},
		{
			name:     "a registry that does not parse is refused",
			rigsJSON: "{not json",
			wantText: "reading the rig registry mayor/rigs.json",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := writeTown(t, townFiles{rigsJSON: tt.rigsJSON, files: tt.files, dirs: tt.dirs})
			host := newFakeHost(gitAnswer(nil))
			_, err := RecordGitBaseline(context.Background(), host.env(town), tt.force)
			if err == nil || !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.wantText)
			}
		})
	}
}

// TestCheckRecordBaselineRefusesAnUnencodableBaseline drives the encoder's
// refusal directly: a median JSON cannot carry is refused, and nothing is
// written under the state directory.
func TestCheckRecordBaselineRefusesAnUnencodableBaseline(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, "daemon", "town-state-git-baseline.json")
	err := writeGitBaseline(path, GitBaseline{Rigs: map[string]map[string]float64{
		"gastown": {"status --short": math.NaN()},
	}})
	if err == nil || !strings.Contains(err.Error(), "encoding daemon/town-state-git-baseline.json") {
		t.Fatalf("err = %v, want the encoding refusal", err)
	}
}
