package daemon

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
)

const sweepCacheProgReadPath = "/run/logs/gocacheprog-stats.jsonl"

// sweepCacheProgInfo is the stat result a test names: only mode and size matter.
type sweepCacheProgInfo struct {
	fs.FileInfo
	mode fs.FileMode
	size int64
}

func (i sweepCacheProgInfo) Mode() fs.FileMode { return i.mode }
func (i sweepCacheProgInfo) Size() int64       { return i.size }

func sweepCacheProgStat(info fs.FileInfo, err error) func(string) (fs.FileInfo, error) {
	return func(string) (fs.FileInfo, error) { return info, err }
}

func TestTierSweepCacheProgRead(t *testing.T) {
	t.Parallel()
	body := []byte("{\"gets\":3}\n{\"puts\":1}\n")
	data, present, err := readSweepCacheProgStats(sweepCacheProgReadPath, sweepCacheProgStat(sweepCacheProgInfo{mode: 0o644, size: int64(len(body))}, nil), func(string) ([]byte, error) { return body, nil })
	if err != nil || !present || string(data) != string(body) {
		t.Fatalf("whole file = %q, %v, %v; want it back, true, nil", data, present, err)
	}
	data, present, err = readSweepCacheProgStats(sweepCacheProgReadPath, sweepCacheProgStat(nil, fs.ErrNotExist), func(string) ([]byte, error) { return nil, errors.New("read an absent file") })
	if err != nil || present || data != nil {
		t.Fatalf("absent file = %q, %v, %v; want nil, false, nil", data, present, err)
	}
	boom := errors.New("boom")
	readBoom := func(string) ([]byte, error) { return nil, boom }
	rows := []struct {
		name string
		stat func(string) (fs.FileInfo, error)
		read func(string) ([]byte, error)
		want string
	}{
		{"a path that is not a regular file", sweepCacheProgStat(sweepCacheProgInfo{mode: fs.ModeDir | 0o755, size: 10}, nil), readBoom, "not a regular file"},
		{"a file over the limit", sweepCacheProgStat(sweepCacheProgInfo{mode: 0o644, size: sweepCacheProgStatsMaxBytes + 1}, nil), readBoom, "over the"},
		{"a failing stat", sweepCacheProgStat(nil, boom), readBoom, "stat: boom"},
		{"a failing read", sweepCacheProgStat(sweepCacheProgInfo{mode: 0o644, size: 4}, nil), readBoom, "read: boom"},
		{"a read that stopped short", sweepCacheProgStat(sweepCacheProgInfo{mode: 0o644, size: 10}, nil), func(string) ([]byte, error) { return []byte("12345"), nil }, "not read whole"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			data, present, err := readSweepCacheProgStats(sweepCacheProgReadPath, row.stat, row.read)
			switch {
			case err == nil:
				t.Fatalf("accepted %s", row.name)
			case !present:
				t.Fatalf("%s = present false with error %v; only an absent file is a clean absence", row.name, err)
			case data != nil:
				t.Fatalf("returned %q with error %v", data, err)
			case !strings.Contains(err.Error(), sweepCacheProgReadPath):
				t.Fatalf("error %q does not name %s", err, sweepCacheProgReadPath)
			case !strings.Contains(err.Error(), row.want):
				t.Fatalf("error %q does not say %q", err, row.want)
			case errors.Is(err, fs.ErrNotExist):
				t.Fatalf("error %q reads as an absent file", err)
			}
		})
	}
}
