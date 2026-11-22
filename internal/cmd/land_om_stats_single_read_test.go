package cmd

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// landOmStatsBrokenRead is a filesystem whose files open and then fail to read.
type landOmStatsBrokenRead struct{}

func (landOmStatsBrokenRead) Open(string) (io.ReadCloser, error) {
	return io.NopCloser(readFails{}), nil
}

type readFails struct{}

func (readFails) Read([]byte) (int, error) { return 0, errors.New("the disk went away") }

// TestLandOmStatsReadsRecordsOnce: a report opens the records file once, for the
// town's default path and for a named one, so a second report of the run cannot
// read a second snapshot (om finding 1a35ff311959). A records file that opens
// and cannot be read is an error naming the path (gt-4be1z5).
func TestLandOmStatsReadsRecordsOnce(t *testing.T) {
	t.Parallel()
	records, log := omStatsFixtureLines(t)
	for _, tt := range []struct{ name, recordsFlag string }{
		{"the town's default path", ""},
		{"a named path", "/named/reviews.jsonl"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := func() (string, error) { return "/town", nil }
			in, err := landOmStatsResolvePaths(tt.recordsFlag, "/town/daemon.log", town)
			if err != nil {
				t.Fatalf("resolving the paths: %v", err)
			}
			fake := &landOmStatsFakeFS{files: map[string]string{in.records: records, in.log: log}}
			var out strings.Builder
			if err := landOmStatsReportFrom(fake, town, &out, "", tt.recordsFlag, "/town/daemon.log", false, false); err != nil {
				t.Fatalf("landOmStatsReportFrom: %v", err)
			}
			opens := 0
			for _, name := range fake.opened {
				if name == in.records {
					opens++
				}
			}
			if opens != 1 {
				t.Errorf("the records path %q was opened %d times, want once (opened: %v)", in.records, opens, fake.opened)
			}
		})
	}
	t.Run("a records file that cannot be read", func(t *testing.T) {
		t.Parallel()
		err := landOmStatsReportFrom(landOmStatsBrokenRead{}, omStatsNoTown(t), io.Discard, "", "/named/reviews.jsonl", "/named/daemon.log", false, false)
		if err == nil || !strings.Contains(err.Error(), "/named/reviews.jsonl") || !strings.Contains(err.Error(), "the disk went away") {
			t.Errorf("error = %v, want one naming the path and the read failure", err)
		}
	})
}
