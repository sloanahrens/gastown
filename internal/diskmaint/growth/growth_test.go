package growth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// base is the one instant every fixture counts hours from; the package takes
// no clock, so a test chooses the time.
var base = time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)

// at returns the nth hour's reading: used rises one GiB an hour.
func at(n int) Sample {
	return Sample{At: base.Add(time.Duration(n) * time.Hour), UsedBytes: int64(n) << 30, TotalBytes: 200 << 30}
}

// line encodes a sample the way the ring stores it.
func line(t *testing.T, s Sample) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("encoding fixture sample: %v", err)
	}
	return string(b)
}

// sameSamples reports whether got and want hold the same readings in order.
func sameSamples(got, want []Sample) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if !got[i].At.Equal(want[i].At) || got[i].UsedBytes != want[i].UsedBytes || got[i].TotalBytes != want[i].TotalBytes {
			return false
		}
	}
	return true
}

func TestAppendTrimsToNewestKeepLines(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "growth.jsonl")
	for i := 0; i < 5; i++ {
		if err := Append(path, at(i), 3); err != nil {
			t.Fatalf("Append sample %d: %v", i, err)
		}
	}
	got, corrupt, err := ReadCount(path)
	if err != nil {
		t.Fatalf("ReadCount: %v", err)
	}
	if corrupt != 0 {
		t.Errorf("ReadCount counted %d corrupt lines, want 0", corrupt)
	}
	want := []Sample{at(2), at(3), at(4)}
	if !sameSamples(got, want) {
		t.Fatalf("ReadCount = %v, want the newest 3 readings %v", got, want)
	}
}

func TestAppendKeepNotPositiveUsesDefaultKeep(t *testing.T) {
	t.Parallel()
	if DefaultKeep != 2200 {
		t.Fatalf("DefaultKeep = %d, want 2200 (about seven days at one sample per five minutes)", DefaultKeep)
	}
	path := filepath.Join(t.TempDir(), "growth.jsonl")
	lines := make([]string, 0, DefaultKeep+5)
	for i := 0; i < DefaultKeep+5; i++ {
		lines = append(lines, line(t, at(i)))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("seeding ring: %v", err)
	}
	if err := Append(path, at(9000), 0); err != nil {
		t.Fatalf("Append with keep 0: %v", err)
	}
	got, _, err := ReadCount(path)
	if err != nil {
		t.Fatalf("ReadCount: %v", err)
	}
	if len(got) != DefaultKeep {
		t.Fatalf("ring holds %d lines after Append, want DefaultKeep %d", len(got), DefaultKeep)
	}
	if want := at(6); !sameSamples(got[:1], []Sample{want}) {
		t.Errorf("oldest kept = %v, want %v: the six oldest lines must be dropped", got[0], want)
	}
	if want := at(9000); !sameSamples(got[len(got)-1:], []Sample{want}) {
		t.Errorf("newest line = %v, want the appended %v", got[len(got)-1], want)
	}
}

func TestReadSkipsAndCountsCorruptLines(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "growth.jsonl")
	lines := []string{
		line(t, at(1)),
		"not json at all",
		`{"at":"0001-01-01T00:00:00Z","used_bytes":7,"total_bytes":9}`,
		`{"at":"2026-11-19T03:00:00Z","used_bytes":-1,"total_bytes":9}`,
		"   ",
		line(t, at(2)),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("writing ring: %v", err)
	}
	got, corrupt, err := ReadCount(path)
	if err != nil {
		t.Fatalf("ReadCount: %v", err)
	}
	if corrupt != 4 {
		t.Errorf("ReadCount counted %d corrupt lines, want 4 (bad JSON, zero time, negative used, blank)", corrupt)
	}
	want := []Sample{at(1), at(2)}
	if !sameSamples(got, want) {
		t.Errorf("ReadCount = %v, want the two good readings %v", got, want)
	}
	read, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v, want no error from a ring with a corrupt line", err)
	}
	if !sameSamples(read, want) {
		t.Errorf("Read = %v, want %v", read, want)
	}
}

func TestReadMissingFileIsNoSamples(t *testing.T) {
	t.Parallel()
	got, corrupt, err := ReadCount(filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil {
		t.Fatalf("ReadCount of a missing ring: %v, want no error", err)
	}
	if len(got) != 0 || corrupt != 0 {
		t.Fatalf("ReadCount = %v, %d corrupt; want no samples and no corrupt lines", got, corrupt)
	}
}

func TestReadSkipsOverlongLine(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "growth.jsonl")
	lines := []string{line(t, at(1)), strings.Repeat("x", maxLineBytes+1), line(t, at(2))}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("writing ring: %v", err)
	}
	got, corrupt, err := ReadCount(path)
	if err != nil {
		t.Fatalf("ReadCount: %v", err)
	}
	if corrupt != 1 {
		t.Errorf("ReadCount counted %d corrupt lines, want the one over %d bytes", corrupt, maxLineBytes)
	}
	want := []Sample{at(1), at(2)}
	if !sameSamples(got, want) {
		t.Errorf("ReadCount = %v, want the readings either side of the long line %v", got, want)
	}
}

func TestReadEmptyPathRefuses(t *testing.T) {
	t.Parallel()
	if _, _, err := ReadCount(""); !errors.Is(err, errEmptyPath) {
		t.Fatalf("ReadCount(\"\") = %v, want errEmptyPath", err)
	}
	if _, err := Read(""); !errors.Is(err, errEmptyPath) {
		t.Fatalf("Read(\"\") = %v, want errEmptyPath", err)
	}
}

func TestAppendRefusesInvalidSample(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		s    Sample
	}{
		{"zero time", Sample{UsedBytes: 1, TotalBytes: 2}},
		{"negative used", Sample{At: base, UsedBytes: -1, TotalBytes: 2}},
		{"negative total", Sample{At: base, UsedBytes: 1, TotalBytes: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "growth.jsonl")
			err := Append(path, tc.s, 0)
			if !errors.Is(err, errInvalidSample) {
				t.Fatalf("Append(%+v) = %v, want errInvalidSample", tc.s, err)
			}
			if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("Append wrote %s for a sample it refused (stat err %v)", path, statErr)
			}
		})
	}
}

func TestAppendRefusesUnencodableTime(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "growth.jsonl")
	s := Sample{At: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), UsedBytes: 1, TotalBytes: 2}
	if err := Append(path, s, 0); err == nil {
		t.Fatal("Append wrote a year the JSON encoder refuses")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Append left %s after a failed encode (stat err %v)", path, err)
	}
}

func TestAppendEmptyPathRefuses(t *testing.T) {
	t.Parallel()
	if err := Append("", at(1), 0); !errors.Is(err, errEmptyPath) {
		t.Fatalf("Append(\"\") = %v, want errEmptyPath", err)
	}
}

func TestAppendCreatesMissingDirectories(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "runtime", "disk-growth", "control.jsonl")
	if err := Append(path, at(1), 0); err != nil {
		t.Fatalf("Append into a missing directory: %v", err)
	}
	got, _, err := ReadCount(path)
	if err != nil {
		t.Fatalf("ReadCount: %v", err)
	}
	if !sameSamples(got, []Sample{at(1)}) {
		t.Fatalf("ReadCount = %v, want the appended reading", got)
	}
}

func TestAppendRefusesPathUnderAFile(t *testing.T) {
	t.Parallel()
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatalf("writing blocker file: %v", err)
	}
	if err := Append(filepath.Join(blocker, "growth.jsonl"), at(1), 0); err == nil {
		t.Fatal("Append under a regular file succeeded, want an error")
	}
}

func TestAppendRefusesDirectoryPath(t *testing.T) {
	t.Parallel()
	if err := Append(t.TempDir(), at(1), 0); err == nil {
		t.Fatal("Append to a directory succeeded, want an error")
	}
}

func TestAppendKeepsLinesItCannotParse(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "growth.jsonl")
	if err := os.WriteFile(path, []byte("garbage\n"+line(t, at(1))+"\n"), 0o644); err != nil {
		t.Fatalf("seeding ring: %v", err)
	}
	if err := Append(path, at(2), 0); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, corrupt, err := ReadCount(path)
	if err != nil {
		t.Fatalf("ReadCount: %v", err)
	}
	if corrupt != 1 {
		t.Errorf("ReadCount counted %d corrupt lines, want the seeded 1", corrupt)
	}
	want := []Sample{at(1), at(2)}
	if !sameSamples(got, want) {
		t.Fatalf("ReadCount = %v, want %v", got, want)
	}
}

// TestReadErrorNamesThePath keeps a read failure diagnosable: the operator
// reads this line in the daemon log, not a stack.
func TestReadErrorNamesThePath(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ring-is-a-directory")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("making directory: %v", err)
	}
	_, _, err := ReadCount(path)
	if err == nil {
		t.Fatalf("ReadCount(%s) on a directory = nil, want an error", path)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("ReadCount error %q does not name the path %q", err, path)
	}
}
