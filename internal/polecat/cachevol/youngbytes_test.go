package cachevol

import (
	"testing"
	"time"
)

// TestYoungBuildBytes: the build-cache bytes the age floor keeps as of now, by the later of atime
// and mtime; module entries count nothing, the boundary matches PlanTrim's verdict, and a zero now
// is refused (gt-hnmotf).
func TestYoungBuildBytes(t *testing.T) {
	t.Parallel()
	now := at(24)
	tests := []struct {
		name    string
		listing []Entry
		want    int64
	}{
		{"young", []Entry{file("gocache/y", BuildCache, 60, 23, 23)}, 60},
		{"old", []Entry{file("gocache/o", BuildCache, 60, 20, 20)}, 0},
		{"exactly at the floor", []Entry{{Path: "gocache/f", Cache: BuildCache, Size: 60, Atime: now.Add(-buildCacheMinAge)}}, 0},
		{"unset times", []Entry{{Path: "gocache/u", Cache: BuildCache, Size: 60}}, 60},
		{"module", []Entry{file("gomodcache/d", ModuleCache, 60, 23, 23)}, 0},
		{"mtime later than atime", []Entry{{Path: "gocache/t", Cache: BuildCache, Size: 60, Atime: at(20), Mtime: at(23)}}, 60},
		{"mixed", []Entry{file("gocache/o", BuildCache, 5, 20, 20), file("gocache/y", BuildCache, 7, 23, 23), file("gomodcache/d", ModuleCache, 11, 23, 23)}, 7},
	}
	for _, tc := range tests {
		got, err := YoungBuildBytes(tc.listing, now)
		if err != nil {
			t.Fatalf("%s: YoungBuildBytes: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: YoungBuildBytes = %d, want %d", tc.name, got, tc.want)
		}
	}
	if _, err := YoungBuildBytes(nil, time.Time{}); err == nil {
		t.Error("a zero now: err = nil, want an error")
	}
}
