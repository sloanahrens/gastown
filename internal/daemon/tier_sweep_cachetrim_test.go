package daemon

import (
	"os"
	"path/filepath"
	"testing"

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

// writeCacheTrim writes r into dir as the record sweep-run leaves, failing the
// test rather than returning, since every row that uses it is about the note.
func writeCacheTrim(t *testing.T, dir string, r sweeprunner.CacheTrimRecord) {
	t.Helper()
	if err := sweeprunner.WriteCacheTrim(dir, r); err != nil {
		t.Fatalf("writing a cache trim record: %v", err)
	}
}
