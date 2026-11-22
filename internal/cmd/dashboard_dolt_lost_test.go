package cmd

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/hostprobe"
)

// TestDoltLostSectionTotalsPeakAndRows: buckets of 5, 40 and 394 in the last three
// slots over two databases give the 10 minute, hour and 24 hour totals, the peak
// bucket with its time and a row per database; an absent field is unknown, never
// zero, and a field that read zero says so (gt-5zv50m).
func TestDoltLostSectionTotalsPeakAndRows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 22, 12, 45, 0, 0, time.UTC)
	buckets := make([]hostprobe.DoltBucket, 144)
	for i := range buckets {
		buckets[i] = hostprobe.DoltBucket{At: now.Truncate(10 * time.Minute).Add(time.Duration(i-143) * 10 * time.Minute), ByDB: map[string]int{}}
	}
	set := func(slot, total int, byDB map[string]int) {
		buckets[143-slot].Total, buckets[143-slot].ByDB = total, byDB
	}
	set(6, 100, map[string]int{"hq": 100}) // the seventh newest: in the 24 hours, not in the hour
	set(5, 7, map[string]int{"gt": 7})     // the sixth newest: the last bucket of the hour
	set(2, 5, map[string]int{"gt": 5})
	set(1, 40, map[string]int{"hq": 40})
	set(0, 394, map[string]int{"gt": 300, "hq": 94})
	field := func(f *hostprobe.Field[hostprobe.DoltConflicts]) doltLostProbe {
		return func(string, time.Time) (hostprobe.Probes, error) { return hostprobe.Probes{DoltConflicts: f}, nil }
	}
	zero := make([]hostprobe.DoltBucket, 144)
	for _, tc := range []struct {
		name     string
		probe    doltLostProbe
		wantNote string
		wantRows [][]string
		wantErr  string
	}{
		{"totals, the peak and a row per database", field(&hostprobe.Field[hostprobe.DoltConflicts]{Value: hostprobe.DoltConflicts{Buckets: buckets}}),
			"last 10 minutes: 394; last hour: 446; last 24 hours: 546; peak bucket: 394 at 2026-11-22 12:40 UTC", [][]string{{"gt", "312"}, {"hq", "234"}}, ""},
		{"an absent field is unknown, never zero", field(nil), "", nil, "dv-w4ehm"},
		{"a zero reading says 0 lost commits", field(&hostprobe.Field[hostprobe.DoltConflicts]{Value: hostprobe.DoltConflicts{Buckets: zero}}), "0 lost commits in the last 24 hours", nil, ""},
		{"a field whose own read failed", field(&hostprobe.Field[hostprobe.DoltConflicts]{Err: "docker logs: no such container"}), "", nil, "no such container"},
		{"a probe that cannot be read", func(string, time.Time) (hostprobe.Probes, error) {
			return hostprobe.Probes{}, errors.New("probe file is stale")
		}, "", nil, "stale"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := newDoltLostSection(tc.probe, func() time.Time { return now }).read(withDoltTownRoot(context.Background(), "/town"), time.Time{})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to say %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got.Note != tc.wantNote || !reflect.DeepEqual(got.Rows, tc.wantRows) {
				t.Errorf("note %q rows %q err %v, want %q and %q", got.Note, got.Rows, err, tc.wantNote, tc.wantRows)
			}
		})
	}
	t.Run("a read with no town root is an error", func(t *testing.T) {
		t.Parallel()
		_, err := newDoltLostSection(nil, time.Now).read(context.Background(), time.Time{})
		if !errors.Is(err, errNoDoltLostTownRoot) {
			t.Errorf("err = %v, want errNoDoltLostTownRoot", err)
		}
	})
}
