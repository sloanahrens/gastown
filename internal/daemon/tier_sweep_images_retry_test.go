package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// imagesRetryCase is one state and images outcome the due rule is asked about.
type imagesRetryCase struct {
	name         string
	lastGreenSHA string
	imagesFailed bool
	retries      int    // recorded against retrySHA
	retrySHA     string // the tip the recorded retries belong to
	sinceLastRun time.Duration
	wantDue      bool
}

func TestTierSweepImagesRetryDue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	cases := []imagesRetryCase{
		{
			name: "failed with a green tip and no retries yet is due", lastGreenSHA: "green0",
			imagesFailed: true, sinceLastRun: tierSweepImagesRetryGap, wantDue: true,
		},
		{
			name: "a green images run is not due however old the sweep", lastGreenSHA: "green0",
			imagesFailed: false, sinceLastRun: time.Hour, wantDue: false,
		},
		{
			name: "no green tip recorded is not due", lastGreenSHA: "",
			imagesFailed: true, sinceLastRun: time.Hour, wantDue: false,
		},
		{
			name: "two retries for this tip leave one more", lastGreenSHA: "green0",
			imagesFailed: true, retries: 2, retrySHA: "green0", sinceLastRun: tierSweepImagesRetryGap, wantDue: true,
		},
		{
			name: "the max retries for this tip stops the loop", lastGreenSHA: "green0",
			imagesFailed: true, retries: tierSweepImagesRetryMax, retrySHA: "green0", sinceLastRun: time.Hour, wantDue: false,
		},
		{
			name: "retries recorded for another green sha do not count", lastGreenSHA: "green0",
			imagesFailed: true, retries: tierSweepImagesRetryMax, retrySHA: "older1", sinceLastRun: tierSweepImagesRetryGap, wantDue: true,
		},
		{
			name: "just short of the gap waits", lastGreenSHA: "green0",
			imagesFailed: true, sinceLastRun: tierSweepImagesRetryGap - time.Second, wantDue: false,
		},
		{
			name: "a retry inside the gap waits however few are recorded", lastGreenSHA: "green0",
			imagesFailed: true, retries: 1, retrySHA: "green0", sinceLastRun: 5 * time.Minute, wantDue: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			st := tierSweepState{
				LastRun:        now.Add(-c.sinceLastRun),
				LastGreenSHA:   c.lastGreenSHA,
				ImagesRetries:  c.retries,
				ImagesRetrySHA: c.retrySHA,
			}
			if got := tierSweepImagesRetryDue(st, now, c.imagesFailed); got != c.wantDue {
				t.Fatalf("tierSweepImagesRetryDue(%+v, imagesFailed=%v) = %v, want %v", st, c.imagesFailed, got, c.wantDue)
			}
		})
	}
}

// A record written before the retry fields existed has neither key, so it must
// read as zero retries and let the rule retry its failed images run.
func TestTierSweepImagesRetryLegacyStateReadsZero(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := tierSweepStatePath(townRoot, "gastown")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"last_run":"2026-11-16T11:30:00Z","last_sha":"green0","last_green_sha":"green0"}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := readTierSweepState(townRoot, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if st.ImagesRetries != 0 || st.ImagesRetrySHA != "" {
		t.Fatalf("legacy record read retries=%d for %q, want zero for empty", st.ImagesRetries, st.ImagesRetrySHA)
	}
	if !tierSweepImagesRetryDue(st, st.LastRun.Add(tierSweepImagesRetryGap), true) {
		t.Fatal("a legacy record with a failed images run must read as zero retries and be due")
	}
}

// The two fields survive the record's own write and read.
func TestTierSweepImagesRetryStateRoundTrip(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	want := tierSweepState{
		LastRun:        time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC),
		LastGreenSHA:   "green0",
		ImagesRetries:  2,
		ImagesRetrySHA: "green0",
	}
	if err := writeTierSweepState(townRoot, "gastown", want); err != nil {
		t.Fatal(err)
	}
	got, err := readTierSweepState(townRoot, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if got.ImagesRetries != want.ImagesRetries || got.ImagesRetrySHA != want.ImagesRetrySHA {
		t.Fatalf("round trip = %d retries for %q, want %d for %q",
			got.ImagesRetries, got.ImagesRetrySHA, want.ImagesRetries, want.ImagesRetrySHA)
	}
}
