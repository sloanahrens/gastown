package land

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

const (
	aheadTestHead    = "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
	aheadTestNewHead = "cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34"
)

// aheadTestRecord is a record with every field set, so each one must land in the file.
func aheadTestRecord(bead, head string) AheadRecord {
	t0 := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	return AheadRecord{
		Bead: bead, Head: head, PatchID: "patch-1", OMVersion: "om 1.2.3",
		OMConfigHash: "cfg-hash", InputDigest: "digest", Base: "aaa",
		StartedAt: t0, ReviewedAt: t0.Add(90 * time.Second), ReviewSecs: 90.5,
		Outcome: "verdict", Source: "pool", Rescored: true,
		Verdict: Verdict{Verdict: VerdictApprove, Score: 0.9, Summary: "ok"},
	}
}

// aheadTestPath is where the store must put bead's record in town.
func aheadTestPath(town, rig, bead string) string {
	return filepath.Join(constants.TownRuntimePath(town), "landings", "review-ahead", rig, bead+".json")
}

// TestVerdictReuseStorePutWritesTheRecord: Put writes every field to the bead's
// file, with v at 1 and the store's rig, and a later Put replaces the file.
func TestVerdictReuseStorePutWritesTheRecord(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	store := TownAheadStore(town, "gastown")
	if err := store.Put(aheadTestRecord("gt-abc", aheadTestHead)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	path := aheadTestPath(town, "gastown", "gt-abc")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		t.Fatalf("the record is not a JSON object: %v\n%s", err, data)
	}
	wantKeys := []string{"v", "bead", "rig", "head", "patch_id", "om_version", "om_config_hash",
		"input_digest", "base", "started_at", "reviewed_at", "review_secs", "outcome", "source",
		"verdict", "rescored"}
	for _, k := range wantKeys {
		if _, ok := keys[k]; !ok {
			t.Errorf("record has no %q field: %s", k, data)
		}
	}
	if len(keys) != len(wantKeys) {
		t.Errorf("record has %d fields, want %d: %s", len(keys), len(wantKeys), data)
	}
	var got AheadRecord
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.V != 1 || got.Rig != "gastown" || got.Bead != "gt-abc" || got.Head != aheadTestHead {
		t.Errorf("stored v/rig/bead/head = %d/%q/%q/%q, want 1/gastown/gt-abc/%s",
			got.V, got.Rig, got.Bead, got.Head, aheadTestHead)
	}
	if got.Verdict.Verdict != VerdictApprove || got.Verdict.Score != 0.9 || !got.Rescored {
		t.Errorf("stored verdict = %+v, rescored = %v; want approve at 0.9, rescored", got.Verdict, got.Rescored)
	}

	if err := store.Put(aheadTestRecord("gt-abc", aheadTestNewHead)); err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if data, err = os.ReadFile(path); err != nil {
		t.Fatalf("reading the record after the second Put: %v", err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Head != aheadTestNewHead {
		t.Errorf("head after the second Put = %s, want the later record's %s", got.Head, aheadTestNewHead)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "gt-abc.json" {
		t.Errorf("record directory holds %v, want only gt-abc.json", entries)
	}
}

// TestVerdictReuseStoreFileIs0600: the record is readable by its owner only.
func TestVerdictReuseStoreFileIs0600(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := TownAheadStore(town, "gastown").Put(aheadTestRecord("gt-abc", aheadTestHead)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	info, err := os.Stat(aheadTestPath(town, "gastown", "gt-abc"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("record mode = %v, want 0600", perm)
	}
}

// TestVerdictReuseStoreRefusesWhatItCannotWrite: each refusal returns an error
// that names its cause, and leaves no record behind.
func TestVerdictReuseStoreRefusesWhatItCannotWrite(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		edit    func(s *AheadStore, rec *AheadRecord) // nil keeps the store and record valid
		setup   func(t *testing.T, town string)       // nil builds no landings tree
		wantErr string
	}{
		{"no town root", func(s *AheadStore, _ *AheadRecord) { s.TownRoot = "" }, nil, "no town root"},
		{"rig is empty", func(s *AheadStore, _ *AheadRecord) { s.Rig = "" }, nil, "invalid rig"},
		{"rig climbs out of the store", func(s *AheadStore, _ *AheadRecord) { s.Rig = ".." }, nil, "invalid rig"},
		{"rig holds a separator", func(s *AheadStore, _ *AheadRecord) { s.Rig = "a/b" }, nil, "invalid rig"},
		{"bead is empty", func(_ *AheadStore, r *AheadRecord) { r.Bead = "" }, nil, "refusing bead"},
		{"bead climbs out of the store", func(_ *AheadStore, r *AheadRecord) { r.Bead = "../gt-abc" }, nil, "refusing bead"},
		{"record cannot be encoded", func(_ *AheadStore, r *AheadRecord) { r.ReviewSecs = math.NaN() }, nil, "encoding review-ahead record"},
		{"landings dir is not private", nil, func(t *testing.T, town string) {
			dir := filepath.Join(constants.TownRuntimePath(town), "landings", "review-ahead", "gastown")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			// Group-writable, so not private; no execute bit, which the policy reads as an executable file.
			if err := os.Chmod(dir, 0o660); err != nil {
				t.Fatal(err)
			}
		}, "refusing landings dir"},
		{"landings dir cannot be made", nil, func(t *testing.T, town string) {
			parent := filepath.Join(constants.TownRuntimePath(town), "landings", "review-ahead")
			if err := os.MkdirAll(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			// A file where the rig's directory must go: MkdirAll cannot make it.
			if err := os.WriteFile(filepath.Join(parent, "gastown"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "creating landings dir"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.setup != nil {
				tc.setup(t, town)
			}
			store := AheadStore{TownRoot: town, Rig: "gastown"}
			rec := aheadTestRecord("gt-abc", aheadTestHead)
			if tc.edit != nil {
				tc.edit(&store, &rec)
			}
			err := store.Put(rec)
			if err == nil {
				t.Fatalf("Put returned nil, want an error naming %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Put error = %q, want it to name %q", err, tc.wantErr)
			}
			if _, statErr := os.Stat(aheadTestPath(town, "gastown", "gt-abc")); statErr == nil {
				t.Errorf("a refused Put left a record behind at %s", aheadTestPath(town, "gastown", "gt-abc"))
			}
		})
	}
}

// TestVerdictReuseStorePutRefusesNonOMVerdict: Put keeps a record only for the
// verdicts om itself reaches. An outcome that is not exactly "verdict" or
// "conflict", or a verdict word an om skip, error or waiver carries, is refused
// with an error that names the outcome, and no record is written.
func TestVerdictReuseStorePutRefusesNonOMVerdict(t *testing.T) {
	t.Parallel()
	badOutcomes := []string{"", "Verdict", "verdict ", "timeout", "verdict"}
	badVerdicts := []string{VerdictSkipped, VerdictErrorPrefix + "timeout", VerdictOverseerPrefix + "gt-abc"}
	for _, outcome := range badOutcomes {
		for _, verdict := range badVerdicts {
			t.Run(fmt.Sprintf("outcome %q verdict %q", outcome, verdict), func(t *testing.T) {
				t.Parallel()
				town := t.TempDir()
				rec := aheadTestRecord("gt-abc", aheadTestHead)
				rec.Outcome, rec.Verdict.Verdict = outcome, verdict
				err := TownAheadStore(town, "gastown").Put(rec)
				if err == nil {
					t.Fatalf("Put stored outcome %q with verdict %q, want a refusal", outcome, verdict)
				}
				if want := fmt.Sprintf("%q", outcome); !strings.Contains(err.Error(), want) {
					t.Errorf("Put error = %q, want it to name the outcome %s", err, want)
				}
				assertNoRecord(t, town, "gt-abc")
			})
		}
	}

	t.Run("conflict carrying a verdict", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		rec := aheadTestRecord("gt-abc", aheadTestHead)
		rec.Outcome, rec.Verdict.Verdict = "conflict", VerdictSkipped
		err := TownAheadStore(town, "gastown").Put(rec)
		if err == nil {
			t.Fatal("Put stored a conflict that carried a verdict, want a refusal")
		}
		if !strings.Contains(err.Error(), `"conflict"`) {
			t.Errorf("Put error = %q, want it to name the outcome", err)
		}
		assertNoRecord(t, town, "gt-abc")
	})

	stored := []struct {
		name    string
		outcome string
		verdict string
	}{
		{"conflict with no verdict", "conflict", ""},
		{"approve", "verdict", VerdictApprove},
		{"request_changes", "verdict", VerdictRequestChanges},
	}
	for _, tc := range stored {
		t.Run("stores "+tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			rec := aheadTestRecord("gt-abc", aheadTestHead)
			rec.Outcome, rec.Verdict.Verdict = tc.outcome, tc.verdict
			if err := TownAheadStore(town, "gastown").Put(rec); err != nil {
				t.Fatalf("Put(%q, %q): %v", tc.outcome, tc.verdict, err)
			}
			if _, err := os.Stat(aheadTestPath(town, "gastown", "gt-abc")); err != nil {
				t.Errorf("Put did not store outcome %q with verdict %q: %v", tc.outcome, tc.verdict, err)
			}
		})
	}
}

// TestVerdictReuseStoreBadComponent: a bead that is not one path component, or
// a head that is not a sha, is refused and leaves no record.
func TestVerdictReuseStoreBadComponent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		edit    func(rec *AheadRecord)
		wantErr string
	}{
		{"empty bead", func(r *AheadRecord) { r.Bead = "" }, "refusing bead"},
		{"bead climbs out of the store", func(r *AheadRecord) { r.Bead = "../gt-abc" }, "refusing bead"},
		{"bead holds a separator", func(r *AheadRecord) { r.Bead = "gt/abc" }, "refusing bead"},
		{"empty head", func(r *AheadRecord) { r.Head = "" }, "refusing head"},
		{"head is not hex", func(r *AheadRecord) { r.Head = aheadTestHead[:7] + "g" + aheadTestHead[8:] }, "refusing head"},
		{"head is uppercase", func(r *AheadRecord) { r.Head = strings.ToUpper(aheadTestHead) }, "refusing head"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			rec := aheadTestRecord("gt-abc", aheadTestHead)
			tc.edit(&rec)
			err := TownAheadStore(town, "gastown").Put(rec)
			if err == nil {
				t.Fatalf("Put returned nil, want an error naming %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Put error = %q, want it to name %q", err, tc.wantErr)
			}
			assertNoRecord(t, town, "gt-abc")
		})
	}
}

// TestVerdictReuseStoreWriteErrorIsReturned: Put returns the write's error. The
// seam here fails without writing a byte, so this test covers only the error
// return; the real atomic write, which is what leaves no partial file, is not
// exercised here.
func TestVerdictReuseStoreWriteErrorIsReturned(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeErr := errors.New("disk full")
	store := AheadStore{TownRoot: town, Rig: "gastown"}
	store.writeFile = func(string, []byte, os.FileMode) error { return writeErr }
	err := store.Put(aheadTestRecord("gt-abc", aheadTestHead))
	if err == nil {
		t.Fatal("Put returned nil, want the write's error")
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("Put error = %q, want it to wrap the write's error %q", err, writeErr)
	}
	if !strings.Contains(err.Error(), "writing review-ahead record") {
		t.Errorf("Put error = %q, want it to name the write", err)
	}
}

// TestVerdictReuseStoreFailedWriteLeavesNoRecord: a write that fails after the
// store has made its temp file leaves neither a record nor a partial file. The
// record path is a directory the atomic write's rename cannot replace, so the
// real writer fails.
func TestVerdictReuseStoreFailedWriteLeavesNoRecord(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := aheadTestPath(town, "gastown", "gt-abc")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	err := TownAheadStore(town, "gastown").Put(aheadTestRecord("gt-abc", aheadTestHead))
	if err == nil {
		t.Fatal("Put over a directory returned nil, want the write error")
	}
	if !strings.Contains(err.Error(), "writing review-ahead record") {
		t.Errorf("Put error = %q, want it to name the write", err)
	}
	info, statErr := os.Stat(path)
	if statErr != nil || !info.IsDir() {
		t.Errorf("record path after the failed write = %v (stat error %v), want the original directory", info, statErr)
	}
	entries, readErr := os.ReadDir(filepath.Dir(path))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 1 || entries[0].Name() != "gt-abc.json" {
		t.Errorf("failed write left %v in the record directory, want only gt-abc.json", entries)
	}
}

// assertNoRecord fails when the store kept a record for bead in town.
func assertNoRecord(t *testing.T, town, bead string) {
	t.Helper()
	path := aheadTestPath(town, "gastown", bead)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a refused Put left a record at %s (stat error %v)", path, err)
	}
}
