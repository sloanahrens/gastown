package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// tierSweepProgressGolden is the record the runner writes with integration done and race running.
const tierSweepProgressGolden = `{"rig":"gastown","sha":"6193c7a7","tiers":[{"tier":"integration","verdict":"GREEN","passed":82,"failed":0,"skipped":0,"secs":1129}],"running":"race"}`

func tierSweepProgressFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "progress.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadTierSweepProgressGoldenRecord(t *testing.T) {
	t.Parallel()
	got, ok, err := readTierSweepProgress(tierSweepProgressFile(t, tierSweepProgressGolden))
	want := TierSweepProgress{Rig: "gastown", SHA: "6193c7a7", Running: "race",
		Tiers: []dashboard.TierSweepRunStage{{Tier: "integration", Verdict: "GREEN", Passed: 82, Secs: 1129}}}
	if err != nil || !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("readTierSweepProgress(golden) = %+v, %v, %v; want %+v, true, nil", got, ok, err, want)
	}
}

func TestReadTierSweepProgressMissingFileIsNoProgress(t *testing.T) {
	t.Parallel()
	got, ok, err := readTierSweepProgress(filepath.Join(t.TempDir(), "progress.json"))
	if err != nil || ok || !reflect.DeepEqual(got, TierSweepProgress{}) {
		t.Fatalf("readTierSweepProgress(missing) = %+v, %v, %v; want zero, false, nil", got, ok, err)
	}
}

func TestReadTierSweepProgressEmptyTiersList(t *testing.T) {
	t.Parallel()
	got, ok, err := readTierSweepProgress(tierSweepProgressFile(t, `{"rig":"gastown","sha":"6193c7a7","tiers":[],"running":"integration"}`))
	if err != nil || !ok || len(got.Tiers) != 0 || got.Running != "integration" {
		t.Fatalf("readTierSweepProgress(empty tiers) = %+v, %v, %v; want no tiers, running integration, true, nil", got, ok, err)
	}
}

// TestReadTierSweepProgressRefusesUnreadableFile: an existing but unreadable record is an error naming its path.
func TestReadTierSweepProgressRefusesUnreadableFile(t *testing.T) {
	t.Parallel()
	dirAtPath := filepath.Join(t.TempDir(), "progress.json")
	if err := os.Mkdir(dirAtPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"corrupt JSON":      tierSweepProgressFile(t, `{"rig":"gastown","tiers":[`),
		"empty file":        tierSweepProgressFile(t, ""),
		"directory at path": dirAtPath,
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			_, ok, err := readTierSweepProgress(path)
			if err == nil || ok || !strings.Contains(err.Error(), path) {
				t.Errorf("readTierSweepProgress() ok = %v, err = %v; want false and an error naming %s", ok, err, path)
			}
		})
	}
}
