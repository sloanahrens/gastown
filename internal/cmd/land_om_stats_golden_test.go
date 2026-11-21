package cmd

import (
	"bytes"
	"os"
	"testing"
)

// TestLandOmStatsOutputUnchanged pins what gt land om-stats prints without
// --classes, text and --json, byte for byte. The goldens were captured by
// running landOmStatsReportFrom over this fixture on origin/main 40906496e,
// before the single-read change, so that change is held to them (gt-gck6py).
// The output does not follow the host's zone: it is identical under TZ=UTC,
// Asia/Tokyo and America/Los_Angeles, so the fixture's local-zone days are kept.
func TestLandOmStatsOutputUnchanged(t *testing.T) {
	t.Parallel()
	records, log := omStatsFixtureLines(t)
	for _, tt := range []struct {
		name, golden string
		asJSON       bool
	}{
		{"text", "testdata/land_om_stats_unchanged.golden", false},
		{"json", "testdata/land_om_stats_unchanged_json.golden", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			want, err := os.ReadFile(tt.golden)
			if err != nil {
				t.Fatalf("reading the golden: %v", err)
			}
			fsys := &landOmStatsFakeFS{files: map[string]string{"/town/reviews.jsonl": records, "/town/daemon.log": log}}
			var got bytes.Buffer
			if err := landOmStatsReportFrom(fsys, omStatsNoTown(t), &got, "", "/town/reviews.jsonl", "/town/daemon.log", tt.asJSON); err != nil {
				t.Fatalf("landOmStatsReportFrom: %v", err)
			}
			if !bytes.Equal(got.Bytes(), want) {
				t.Errorf("%s output differs from %s:\n got: %q\nwant: %q", tt.name, tt.golden, got.String(), want)
			}
		})
	}
}
