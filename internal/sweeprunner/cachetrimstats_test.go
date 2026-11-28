package sweeprunner

import (
	"errors"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

// goodCacheTrimStats is one line exactly as cmd/sweep-run's writeCacheTrimStats writes it.
const goodCacheTrimStats = "stats total_bytes=140 planned_files=2 planned_bytes=80 removed_files=1 removed_bytes=40 absent_files=3 failed_removals=4 young_bytes=5 protected_bytes=6 over_bytes=7 cap_bytes=1048576 elapsed_ms=2000\n"

// TestReadCacheTrimStatsFigures: a good line returns every figure under its name, and a missing file is no record.
func TestReadCacheTrimStatsFigures(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if got, ok, err := ReadCacheTrimStats(dir); err != nil || ok || got != nil {
		t.Fatalf("empty directory: got=%v ok=%v err=%v; want no figures and no error", got, ok, err)
	}
	if err := os.WriteFile(filepath.Join(dir, CacheTrimStatsFileName), []byte(goodCacheTrimStats), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadCacheTrimStats(dir)
	want := map[string]int64{"claim_bytes_before": 140, "planned_files": 2, "planned_bytes": 80, "removed_files": 1, "removed_bytes": 40, "absent_files": 3, "failed_removals": 4, "young_bytes": 5, "protected_bytes": 6, "over_bytes": 7, "cap_bytes": 1048576, "elapsed_ms": 2000}
	if err != nil || !ok || !maps.Equal(got, want) {
		t.Fatalf("ReadCacheTrimStats = %v, %v, %v; want %v, true, nil", got, ok, err, want)
	}
}

// TestReadCacheTrimStatsRefusals: each unusable shape is an error naming the file, never a clean absence.
func TestReadCacheTrimStatsRefusals(t *testing.T) {
	t.Parallel()
	set := func(old, new string) string { return strings.Replace(goodCacheTrimStats, old, new, 1) }
	rows := []struct {
		name, body, want string
		symlink          bool
	}{
		{"a symlink", "", "not a regular file", true},
		{"larger than the bound", goodCacheTrimStats[:len(goodCacheTrimStats)-1] + strings.Repeat(" ", cacheTrimStatsMaxBytes+1-len(goodCacheTrimStats)) + "\n", "", false},
		{"no final newline", strings.TrimSuffix(goodCacheTrimStats, "\n"), "", false},
		{"more than one line", goodCacheTrimStats + "\n", "", false},
		{"an empty line", "\n", "not a stats line", false},
		{"no stats prefix", strings.TrimPrefix(goodCacheTrimStats, "stats "), "not a stats line", false},
		{"missing field", set("planned_files=2 ", ""), "", false},
		{"repeated field", set("cap_bytes=1048576", "cap_bytes=1048576 cap_bytes=8"), "", false},
		{"unknown field", set("over_bytes=7", "over_bytes=7 mystery=1"), "unknown or malformed field", false},
		{"field without a value", set(" over_bytes=7", " over_bytes"), "unknown or malformed field", false},
		{"negative field", set("over_bytes=7", "over_bytes=-7"), "", false},
		{"non-numeric field", set("young_bytes=5", "young_bytes=five"), "", false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, CacheTrimStatsFileName)
			var err error
			if row.symlink {
				err = os.Symlink("/nonexistent/elsewhere.log", path)
			} else {
				err = os.WriteFile(path, []byte(row.body), 0o644)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, ok, err := ReadCacheTrimStats(dir)
			switch {
			case err == nil || ok:
				t.Fatalf("ReadCacheTrimStats accepted %s, or returned figures with its error", row.name)
			case got != nil:
				t.Fatalf("ReadCacheTrimStats returned figures %v with error %v", got, err)
			case !strings.Contains(err.Error(), path):
				t.Fatalf("error %q does not name %s", err, path)
			case !strings.Contains(err.Error(), row.want):
				t.Fatalf("error %q does not say %q", err, row.want)
			case errors.Is(err, fs.ErrNotExist):
				t.Fatalf("error %q reads as an absent file", err)
			}
		})
	}
}

// TestReadCacheTrimStatsFilesystemFailures: an open, read, close or lstat failure is an error naming the file.
func TestReadCacheTrimStatsFilesystemFailures(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), CacheTrimStatsFileName)
	boom := errors.New("boom")
	opens := []func(string) (io.ReadCloser, error){
		func(string) (io.ReadCloser, error) { return nil, boom },
		func(string) (io.ReadCloser, error) { return io.NopCloser(iotest.ErrReader(boom)), nil },
		func(string) (io.ReadCloser, error) {
			return closeFails{strings.NewReader(goodCacheTrimStats), boom}, nil
		},
	}
	for i, open := range opens {
		if _, err := readCacheTrimStatsFile(path, open); !errors.Is(err, boom) || !strings.Contains(err.Error(), path) {
			t.Fatalf("failure %d: readCacheTrimStatsFile = %v; want the cause wrapped with %s", i, err, path)
		}
	}
	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadCacheTrimStats(parent); err == nil || !strings.Contains(err.Error(), filepath.Join(parent, CacheTrimStatsFileName)) {
		t.Fatalf("ReadCacheTrimStats under a regular file: %v; want an error naming the path", err)
	}
}
