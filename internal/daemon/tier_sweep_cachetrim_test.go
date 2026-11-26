package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/sweeprunner"
)

// TestTierSweepCacheNote pins the note a runner stage's finished line carries
// when the run left a cache trim record beside its result (gt-fa3pkl): the
// over-cap figure with the cap it was trimmed to, nothing for no record or a
// zero figure, and a note that says the record is unreadable rather than
// reading a bad record as the clean case.
func TestTierSweepCacheNote(t *testing.T) {
	t.Parallel()
	const capBytes = int64(5) << 30
	rows := []struct {
		name  string
		setup func(t *testing.T, dir string)
		want  string
	}{
		{
			name: "an over-cap record",
			setup: func(t *testing.T, dir string) {
				writeCacheTrim(t, dir, sweeprunner.CacheTrimRecord{CapBytes: capBytes, OverBytes: 100 << 20})
			},
			want: " cache over cap by 100.0MB (cap 5.0GB)",
		},
		{
			name:  "no record",
			setup: func(t *testing.T, dir string) {},
			want:  "",
		},
		{
			name: "a zero record",
			setup: func(t *testing.T, dir string) {
				writeCacheTrim(t, dir, sweeprunner.CacheTrimRecord{CapBytes: capBytes})
			},
			want: "",
		},
		{
			name: "an unreadable record",
			setup: func(t *testing.T, dir string) {
				path := filepath.Join(dir, sweeprunner.CacheTrimFileName)
				if err := os.WriteFile(path, []byte("{not a record"), 0o600); err != nil {
					t.Fatalf("writing an unreadable record: %v", err)
				}
			},
			want: " cache-trim record unreadable",
		},
		{
			name: "a record that is not a regular file",
			setup: func(t *testing.T, dir string) {
				if err := os.Mkdir(filepath.Join(dir, sweeprunner.CacheTrimFileName), 0o700); err != nil {
					t.Fatalf("making a directory in the record's place: %v", err)
				}
			},
			want: " cache-trim record unreadable",
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			r.setup(t, dir)
			if got := tierSweepCacheNote(dir); got != r.want {
				t.Fatalf("tierSweepCacheNote(%s) = %q, want %q", dir, got, r.want)
			}
		})
	}
}

// TestTierSweepCacheNoteRecordWindow pins the window sweep-run's ordering
// opens (gt-fa3pkl): it writes cache-trim.json after result.json, so a note
// composed before the record exists is empty, and a note composed after it
// names the figures. The sweep reads the record at the fold and never waits,
// so this is the behaviour that order gives it.
func TestTierSweepCacheNoteRecordWindow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if got := tierSweepCacheNote(dir); got != "" {
		t.Fatalf("tierSweepCacheNote before the record was written = %q, want the empty note", got)
	}
	writeCacheTrim(t, dir, sweeprunner.CacheTrimRecord{CapBytes: 5 << 30, OverBytes: 115661635})
	const want = " cache over cap by 110.3MB (cap 5.0GB)"
	if got := tierSweepCacheNote(dir); got != want {
		t.Fatalf("tierSweepCacheNote after the record was written = %q, want %q", got, want)
	}
}

// TestTierSweepCacheAttention pins the attention item a finished sweep run raises
// when its cache claim ended over its cap (gt-ym0o68). It folds the run through
// tierSweepRunnerStageResult, the production call site, so removing the call
// turns the over-cap row red. A repeat fold over the cap keeps one item, and a
// run with no record, a zero record, or an unreadable record raises none: the
// finished line carries the unreadable note for that last case.
func TestTierSweepCacheAttention(t *testing.T) {
	t.Parallel()
	const wantSummary = "the tier sweep's cache claim for gastown ended 110.3MB over its 5.0GB cap after every removal the rules allow"
	const capBytes = int64(5) << 30
	rows := []struct {
		name  string
		setup func(t *testing.T, dir string)
		folds int
		want  int
	}{
		{
			name: "an over-cap record raises one item",
			setup: func(t *testing.T, dir string) {
				writeCacheTrim(t, dir, sweeprunner.CacheTrimRecord{CapBytes: capBytes, OverBytes: 115661635})
			},
			folds: 1,
			want:  1,
		},
		{
			name: "a repeat fold over the cap keeps one item",
			setup: func(t *testing.T, dir string) {
				writeCacheTrim(t, dir, sweeprunner.CacheTrimRecord{CapBytes: capBytes, OverBytes: 115661635})
			},
			folds: 2,
			want:  1,
		},
		{
			name:  "no record raises none",
			setup: func(t *testing.T, dir string) {},
			folds: 1,
			want:  0,
		},
		{
			name: "a zero record raises none",
			setup: func(t *testing.T, dir string) {
				writeCacheTrim(t, dir, sweeprunner.CacheTrimRecord{CapBytes: capBytes})
			},
			folds: 1,
			want:  0,
		},
		{
			name: "an unreadable record raises none",
			setup: func(t *testing.T, dir string) {
				path := filepath.Join(dir, sweeprunner.CacheTrimFileName)
				if err := os.WriteFile(path, []byte("{not a record"), 0o600); err != nil {
					t.Fatalf("writing an unreadable record: %v", err)
				}
			},
			folds: 1,
			want:  0,
		},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			now := atHour(time.Now(), 15)
			d, _ := runnerCycleDaemon(t, now, runnerTestConfig(t), "")
			st := tierSweepStage{tiers: []string{"integration", "race"}, slot: true, rig: "gastown", tested: runnerTestSHA}
			spec, ok, err := d.tierSweepRunnerSpec(st)
			if err != nil || !ok {
				t.Fatalf("spec = %+v, ok = %v, err = %v; want the runner to take the stage", spec, ok, err)
			}
			if err := os.MkdirAll(spec.OutDir, 0o700); err != nil {
				t.Fatalf("making the run's output directory: %v", err)
			}
			r.setup(t, spec.OutDir)
			for i := 0; i < r.folds; i++ {
				d.tierSweepRunnerStageResult(st, spec, runnerTestResult(spec, now), tierSweepLaunch{exitCode: -1}, now)
			}
			items := d.cacheTrimNoOp.observed()
			if len(items) != r.want {
				t.Fatalf("attention items after %d fold(s) = %d, want %d: %+v", r.folds, len(items), r.want, items)
			}
			if r.want == 0 {
				return
			}
			got := items[0]
			if got.Key != "cache-trim-sweep-gastown" || got.Kind != attention.KindCacheTrimNoOp ||
				got.Severity != attention.SeverityLow || got.Summary != wantSummary {
				t.Fatalf("attention item = %+v, want key cache-trim-sweep-gastown, kind %s, severity %s, summary %q",
					got, attention.KindCacheTrimNoOp, attention.SeverityLow, wantSummary)
			}
		})
	}
}

// writeCacheTrim writes r into dir as the record sweep-run leaves, failing the
// test rather than returning, since every row that uses it is about the note.
func writeCacheTrim(t *testing.T, dir string, r sweeprunner.CacheTrimRecord) {
	t.Helper()
	if err := sweeprunner.WriteCacheTrim(dir, r); err != nil {
		t.Fatalf("writing a cache trim record: %v", err)
	}
}
