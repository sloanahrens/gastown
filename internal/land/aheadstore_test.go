package land

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
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

// TestVerdictReuseStorePutKeepsNewer: Put keeps the record already stored when
// that review began later than the one Put is given, so a slow review of an old
// head that finishes last cannot replace the newer head's record.
func TestVerdictReuseStorePutKeepsNewer(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		stored   func(rec *AheadRecord) // edits the record already on disk
		incoming func(rec *AheadRecord) // edits the record Put is given
		garbled  bool                   // the path holds bytes that are not a record
		want     string                 // head the record must carry after Put
	}{
		{
			// The old head's review started first and finished last: the
			// comparison is on the start, so the stored newer head stays.
			name: "an earlier start that finished later is kept out",
			stored: func(rec *AheadRecord) {
				rec.Head = aheadTestNewHead
				rec.StartedAt = base.Add(5 * time.Minute)
				rec.ReviewedAt = base.Add(6 * time.Minute)
			},
			incoming: func(rec *AheadRecord) {
				rec.StartedAt = base
				rec.ReviewedAt = base.Add(30 * time.Minute)
			},
			want: aheadTestNewHead,
		},
		{
			name: "a later start replaces",
			stored: func(rec *AheadRecord) {
				rec.StartedAt = base
				rec.ReviewedAt = base.Add(30 * time.Minute)
			},
			incoming: func(rec *AheadRecord) {
				rec.Head = aheadTestNewHead
				rec.StartedAt = base.Add(5 * time.Minute)
				rec.ReviewedAt = base.Add(6 * time.Minute)
			},
			want: aheadTestNewHead,
		},
		{
			name:    "a file that does not decode is replaced",
			garbled: true,
			want:    aheadTestHead, // the incoming record
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			path := aheadTestPath(town, "gastown", "gt-abc")
			store := TownAheadStore(town, "gastown")
			if tc.garbled {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("not a record"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				stored := aheadTestRecord("gt-abc", aheadTestHead)
				tc.stored(&stored)
				if err := store.Put(stored); err != nil {
					t.Fatalf("seeding the stored record: %v", err)
				}
			}
			incoming := aheadTestRecord("gt-abc", aheadTestHead)
			if tc.incoming != nil {
				tc.incoming(&incoming)
			}
			if err := store.Put(incoming); err != nil {
				t.Fatalf("Put: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading the record: %v", err)
			}
			var got AheadRecord
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("the record after Put does not decode: %v\n%s", err, data)
			}
			if got.Head != tc.want {
				t.Errorf("stored head = %s, want %s", got.Head, tc.want)
			}
		})
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

// TestVerdictReuseStoreUnreadableRecordIsAnError: a stored record Put cannot
// read is an error, not an empty store. The unreadable record may be the newer
// one Put exists to keep, so Put writes nothing over it.
func TestVerdictReuseStoreUnreadableRecordIsAnError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	readErr := errors.New("permission denied")
	store := AheadStore{TownRoot: town, Rig: "gastown"}
	store.readFile = func(string) ([]byte, error) { return nil, readErr }
	err := store.Put(aheadTestRecord("gt-abc", aheadTestHead))
	if err == nil {
		t.Fatal("Put returned nil, want the read's error")
	}
	if !errors.Is(err, readErr) {
		t.Errorf("Put error = %q, want it to wrap the read's error %q", err, readErr)
	}
	if !strings.Contains(err.Error(), "reading review-ahead record") {
		t.Errorf("Put error = %q, want it to name the read", err)
	}
	assertNoRecord(t, town, "gt-abc")
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

// TestVerdictReuseStoreRoundTrip: Get returns what Put stored, and a bead with
// no record is (zero, false, nil) so its landing runs its own review.
func TestVerdictReuseStoreRoundTrip(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	store := TownAheadStore(town, "gastown")
	got, ok, err := store.Get("gt-abc")
	if err != nil {
		t.Fatalf("Get on an empty store: %v", err)
	}
	if ok || !reflect.DeepEqual(got, AheadRecord{}) {
		t.Errorf("Get on an empty store = (%+v, %v), want (zero, false)", got, ok)
	}

	if err := store.Put(aheadTestRecord("gt-abc", aheadTestHead)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok, err = store.Get("gt-abc")
	if err != nil {
		t.Fatalf("Get after Put: %v", err)
	}
	if !ok {
		t.Fatal("Get found no record after Put stored one")
	}
	want := aheadTestRecord("gt-abc", aheadTestHead)
	want.V, want.Rig = aheadRecordVersion, "gastown"
	assertSameAheadRecord(t, got, want)
}

// TestVerdictReuseStoreCorruptIsAnError: a stored file Get cannot read as the
// record it asked for is an error and no record, never a miss: truncated JSON,
// a version this reader does not know, a record naming another bead, a head
// that is not a sha, and a read that fails. A path the store cannot name is
// refused the same way.
func TestVerdictReuseStoreCorruptIsAnError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		write  func(t *testing.T, path string) // seeds the record file; nil leaves none
		store  func(*AheadStore)               // edits the store before Get
		bead   string                          // the bead Get is asked for; "" means gt-abc
		wantIn string                          // substring the error must carry
	}{
		{
			name: "truncated json",
			write: func(t *testing.T, path string) {
				writeAheadFile(t, path, []byte(`{"v":1,"bead":"gt-abc"`))
			},
			wantIn: "decoding review-ahead record",
		},
		{
			name: "a version this reader does not know",
			write: func(t *testing.T, path string) {
				rec := aheadTestRecord("gt-abc", aheadTestHead)
				rec.V = aheadRecordVersion + 1
				writeAheadFile(t, path, marshalAhead(t, rec))
			},
			wantIn: "version 2",
		},
		{
			name: "a record that names another bead",
			write: func(t *testing.T, path string) {
				rec := aheadTestRecord("gt-other", aheadTestHead)
				rec.V = aheadRecordVersion
				writeAheadFile(t, path, marshalAhead(t, rec))
			},
			wantIn: `names bead "gt-other"`,
		},
		{
			name: "a head that is not a sha",
			write: func(t *testing.T, path string) {
				rec := aheadTestRecord("gt-abc", aheadTestHead)
				rec.V, rec.Head = aheadRecordVersion, "not-a-sha"
				writeAheadFile(t, path, marshalAhead(t, rec))
			},
			wantIn: "refusing head",
		},
		{
			name: "the read fails",
			store: func(s *AheadStore) {
				s.readFile = func(string) ([]byte, error) { return nil, errors.New("permission denied") }
			},
			wantIn: "reading review-ahead record",
		},
		{
			name:   "no town root",
			store:  func(s *AheadStore) { s.TownRoot = "" },
			wantIn: "no town root",
		},
		{
			name:   "a bead that is not one path component",
			bead:   "../gt-abc",
			wantIn: "invalid bead",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			bead := tc.bead
			if bead == "" {
				bead = "gt-abc"
			}
			if tc.write != nil {
				tc.write(t, aheadTestPath(town, "gastown", bead))
			}
			store := TownAheadStore(town, "gastown")
			if tc.store != nil {
				tc.store(store)
			}
			got, ok, err := store.Get(bead)
			if err == nil {
				t.Fatalf("Get returned (%+v, %v, nil), want an error naming %q", got, ok, tc.wantIn)
			}
			if ok {
				t.Errorf("Get reported a usable record beside its error: %+v", got)
			}
			if !reflect.DeepEqual(got, AheadRecord{}) {
				t.Errorf("Get returned %+v beside its error, want the zero record", got)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("Get error = %q, want it to name %q", err, tc.wantIn)
			}
		})
	}
}

// TestVerdictReuseStoreGetRefusesNonOMVerdict: a hand-written record whose
// outcome or verdict the store would not have written is an error and no
// record, so a caller never reads a verdict om did not reach as reusable.
func TestVerdictReuseStoreGetRefusesNonOMVerdict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		outcome string
		verdict string
		wantIn  string
	}{
		{"skipped", aheadOutcomeVerdict, VerdictSkipped, `"skipped"`},
		{"an om error", aheadOutcomeVerdict, VerdictErrorPrefix + "timeout", `"error:timeout"`},
		{"an overseer waiver", aheadOutcomeVerdict, VerdictOverseerPrefix + "gt-abc", `"overseer:gt-abc"`},
		{"no verdict at all", aheadOutcomeVerdict, "", `with verdict ""`},
		{"an outcome with a trailing space", aheadOutcomeVerdict + " ", VerdictApprove, `"verdict "`},
		{"an outcome the store does not know", "timeout", VerdictApprove, `"timeout"`},
		{"a conflict carrying a verdict", aheadOutcomeConflict, VerdictSkipped, `"conflict"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			rec := aheadTestRecord("gt-abc", aheadTestHead)
			rec.V, rec.Rig = aheadRecordVersion, "gastown"
			rec.Outcome, rec.Verdict.Verdict = tc.outcome, tc.verdict
			writeAheadFile(t, aheadTestPath(town, "gastown", "gt-abc"), marshalAhead(t, rec))
			got, ok, err := TownAheadStore(town, "gastown").Get("gt-abc")
			if err == nil {
				t.Fatalf("Get returned (%+v, %v, nil) for outcome %q with verdict %q, want a refusal",
					got, ok, tc.outcome, tc.verdict)
			}
			if ok || !reflect.DeepEqual(got, AheadRecord{}) {
				t.Errorf("Get returned (%+v, %v) beside its error, want (zero, false)", got, ok)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("Get error = %q, want it to name %s", err, tc.wantIn)
			}
		})
	}
}

// TestVerdictReuseStoreHintRejecting: Hint reports the head the record's review
// judged and whether that review refuses the work. A conflict, a request_changes
// and an approval whose findings block the landing are rejecting; a clean
// approval is not; no record is ("", false, nil); and a record the store cannot
// trust is an error, never a miss and never a clean answer.
func TestVerdictReuseStoreHintRejecting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		bead     string                          // "" means gt-abc
		seed     func(t *testing.T, town string) // writes the record file; nil leaves the store empty
		store    func(*AheadStore)               // edits the store before Hint; nil keeps it
		wantHead string
		wantRej  bool
		wantErr  string
	}{
		{name: "no record"},
		{
			name:     "approve",
			seed:     func(t *testing.T, town string) { seedAheadRecord(t, town, aheadTestRecord("gt-abc", aheadTestHead)) },
			wantHead: aheadTestHead,
		},
		{
			name: "approve with a major finding",
			seed: func(t *testing.T, town string) {
				rec := aheadTestRecord("gt-abc", aheadTestHead)
				rec.Verdict.Findings = []Finding{{ID: "F1", Severity: "major", Path: "a.go", Line: 1, Title: "big"}}
				seedAheadRecord(t, town, rec)
			},
			wantHead: aheadTestHead,
			wantRej:  true,
		},
		{
			name: "request_changes",
			seed: func(t *testing.T, town string) {
				rec := aheadTestRecord("gt-abc", aheadTestHead)
				rec.Verdict = Verdict{Verdict: VerdictRequestChanges, Score: 0.2, Summary: "rework"}
				seedAheadRecord(t, town, rec)
			},
			wantHead: aheadTestHead,
			wantRej:  true,
		},
		{
			name: "conflict",
			seed: func(t *testing.T, town string) {
				rec := aheadTestRecord("gt-abc", aheadTestHead)
				rec.Outcome, rec.Verdict = aheadOutcomeConflict, Verdict{}
				seedAheadRecord(t, town, rec)
			},
			wantHead: aheadTestHead,
			wantRej:  true,
		},
		{
			name: "a file that does not decode",
			seed: func(t *testing.T, town string) {
				writeAheadFile(t, aheadTestPath(town, "gastown", "gt-abc"), []byte(`{"v":1,"bead":"gt-abc"`))
			},
			wantErr: "decoding review-ahead record",
		},
		{
			name:    "a bead that is not one path component",
			bead:    "../gt-abc",
			wantErr: "invalid bead",
		},
		{
			name: "the read fails",
			store: func(s *AheadStore) {
				s.readFile = func(string) ([]byte, error) { return nil, errors.New("permission denied") }
			},
			wantErr: "reading review-ahead record",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			bead := tc.bead
			if bead == "" {
				bead = "gt-abc"
			}
			if tc.seed != nil {
				tc.seed(t, town)
			}
			store := TownAheadStore(town, "gastown")
			if tc.store != nil {
				tc.store(store)
			}
			head, rejecting, err := store.Hint(bead)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("Hint = (%q, %v, nil), want an error naming %q", head, rejecting, tc.wantErr)
				}
				if head != "" || rejecting {
					t.Errorf("Hint returned (%q, %v) beside its error, want (\"\", false)", head, rejecting)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("Hint error = %q, want it to name %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Hint: %v", err)
			}
			if head != tc.wantHead {
				t.Errorf("Hint head = %q, want %q", head, tc.wantHead)
			}
			if rejecting != tc.wantRej {
				t.Errorf("Hint rejecting = %v, want %v", rejecting, tc.wantRej)
			}
		})
	}
}

// TestVerdictReuseStoreDelete: Delete removes the record, so the next Get is a
// miss; deleting a bead with no record is nil; a remove the filesystem refuses
// is an error and leaves the record; and a path the store would not have
// written is refused rather than removed.
func TestVerdictReuseStoreDelete(t *testing.T) {
	t.Parallel()

	t.Run("delete then Get is a miss", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		store := TownAheadStore(town, "gastown")
		if err := store.Put(aheadTestRecord("gt-abc", aheadTestHead)); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := store.Delete("gt-abc"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, ok, err := store.Get("gt-abc"); err != nil || ok {
			t.Errorf("Get after Delete = (ok %v, err %v), want a miss", ok, err)
		}
		if _, err := os.Stat(aheadTestPath(town, "gastown", "gt-abc")); !os.IsNotExist(err) {
			t.Errorf("record file after Delete: stat error %v, want it gone", err)
		}
	})

	t.Run("deleting a missing record is nil", func(t *testing.T) {
		t.Parallel()
		if err := TownAheadStore(t.TempDir(), "gastown").Delete("gt-abc"); err != nil {
			t.Errorf("Delete of a bead with no record = %v, want nil", err)
		}
	})

	t.Run("a failing remove is an error", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		seedAheadRecord(t, town, aheadTestRecord("gt-abc", aheadTestHead))
		removeErr := errors.New("read-only filesystem")
		store := TownAheadStore(town, "gastown")
		store.removeFile = func(string) error { return removeErr }
		err := store.Delete("gt-abc")
		if err == nil {
			t.Fatal("Delete returned nil, want the remove's error")
		}
		if !errors.Is(err, removeErr) {
			t.Errorf("Delete error = %q, want it to wrap the remove's error %q", err, removeErr)
		}
		if !strings.Contains(err.Error(), "deleting review-ahead record") {
			t.Errorf("Delete error = %q, want it to name the delete", err)
		}
		if _, statErr := os.Stat(aheadTestPath(town, "gastown", "gt-abc")); statErr != nil {
			t.Errorf("a failed Delete lost the record: %v", statErr)
		}
	})

	t.Run("a remove that finds the record already gone is nil", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		seedAheadRecord(t, town, aheadTestRecord("gt-abc", aheadTestHead))
		store := TownAheadStore(town, "gastown")
		store.removeFile = func(string) error { return os.ErrNotExist }
		if err := store.Delete("gt-abc"); err != nil {
			t.Errorf("Delete whose remove found no file = %v, want nil", err)
		}
	})

	t.Run("a directory at the record path is refused and left", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		path := aheadTestPath(town, "gastown", "gt-abc")
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		err := TownAheadStore(town, "gastown").Delete("gt-abc")
		if err == nil {
			t.Fatal("Delete over a directory returned nil, want a refusal")
		}
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("Delete error = %q, want it to name the path's kind", err)
		}
		info, statErr := os.Stat(path)
		if statErr != nil || !info.IsDir() {
			t.Errorf("record path after the refused Delete = %v (stat error %v), want the directory left in place", info, statErr)
		}
	})

	t.Run("an unreadable path is an error", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		// A regular file where the rig's directory must be: the lookup fails
		// with "not a directory", which is not the "no record" answer.
		rigDir := filepath.Dir(aheadTestPath(town, "gastown", "gt-abc"))
		if err := os.MkdirAll(filepath.Dir(rigDir), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rigDir, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := TownAheadStore(town, "gastown").Delete("gt-abc")
		if err == nil {
			t.Fatal("Delete with an unreachable record path returned nil, want an error")
		}
		if !strings.Contains(err.Error(), "checking review-ahead record") {
			t.Errorf("Delete error = %q, want it to name the check", err)
		}
	})

	t.Run("a bead that is not one path component is refused", func(t *testing.T) {
		t.Parallel()
		err := TownAheadStore(t.TempDir(), "gastown").Delete("../gt-abc")
		if err == nil {
			t.Fatal("Delete of ../gt-abc returned nil, want a refusal")
		}
		if !strings.Contains(err.Error(), "invalid bead") {
			t.Errorf("Delete error = %q, want it to name the bead", err)
		}
	})
}

// seedAheadRecord writes rec to its bead's record path with the store's v and rig.
func seedAheadRecord(t *testing.T, town string, rec AheadRecord) {
	t.Helper()
	rec.V, rec.Rig = aheadRecordVersion, "gastown"
	writeAheadFile(t, aheadTestPath(town, "gastown", rec.Bead), marshalAhead(t, rec))
}

// writeAheadFile writes data to path under a private directory, standing in for
// a file the store would not have written.
func writeAheadFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// marshalAhead encodes rec as the store writes it.
func marshalAhead(t *testing.T, rec AheadRecord) []byte {
	t.Helper()
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("encoding the record: %v", err)
	}
	return data
}

// assertSameAheadRecord fails when got and want differ as the store writes
// them: the stored form is the contract, not time.Time's internals.
func assertSameAheadRecord(t *testing.T, got, want AheadRecord) {
	t.Helper()
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshaling the record Get returned: %v", err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshaling the wanted record: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("Get returned\n%s\nwant\n%s", gotJSON, wantJSON)
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

// aheadRunInWindow runs first, and from the window between its read of the
// stored record and its write starts second. While first holds the store lock,
// second cannot reach the store, so the check made in that window is that the
// lock is held: TryLock must fail. The check takes no sleep and no yield, and
// it fails at once when the lock is removed (gt-guahwz).
func aheadRunInWindow(t *testing.T, store *AheadStore, first, second func() error, what string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	secondDone := make(chan struct{})
	store.betweenReadAndWrite = func() {
		go func() {
			defer close(secondDone)
			if err := second(); err != nil {
				t.Errorf("%s: %v", what, err)
			}
		}()
		if aheadStoreMu.TryLock() {
			aheadStoreMu.Unlock()
			t.Errorf("%s started while the first operation held no store lock between its read and its write", what)
		}
	}
	if err := first(); err != nil {
		t.Fatalf("first operation: %v", err)
	}
	select {
	case <-secondDone:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestAheadStorePutKeepsNewerUnderRace: a Put that read the store before a later
// review's Put wrote writes only after it, so the later record stays (gt-guahwz).
func TestAheadStorePutKeepsNewerUnderRace(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	base := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	older, newer := TownAheadStore(town, "gastown"), TownAheadStore(town, "gastown")
	oldRec, newRec := aheadTestRecord("gt-abc", aheadTestHead), aheadTestRecord("gt-abc", aheadTestNewHead)
	oldRec.StartedAt, oldRec.ReviewedAt = base, base.Add(30*time.Minute) // the slow review, finishing last
	newRec.StartedAt, newRec.ReviewedAt = base.Add(5*time.Minute), base.Add(6*time.Minute)
	aheadRunInWindow(t, older,
		func() error { return older.Put(oldRec) },
		func() error { return newer.Put(newRec) },
		"the later Put")
	got, ok, err := TownAheadStore(town, "gastown").Get("gt-abc")
	if err != nil || !ok || got.Head != aheadTestNewHead {
		t.Errorf("Get after both Puts = (head %q, ok %v, err %v), want the later review's %s",
			got.Head, ok, err, aheadTestNewHead)
	}
}

// TestAheadStoreDeleteHoldsThePutLock: a Delete started between a Put's read and
// its write runs after that Put writes, so no record is left stored (gt-guahwz).
func TestAheadStoreDeleteHoldsThePutLock(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	base := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	seed := aheadTestRecord("gt-abc", aheadTestHead)
	seed.StartedAt = base
	seedAheadRecord(t, town, seed)
	putter, deleter := TownAheadStore(town, "gastown"), TownAheadStore(town, "gastown")
	incoming := aheadTestRecord("gt-abc", aheadTestNewHead)
	incoming.StartedAt = base.Add(5 * time.Minute)
	aheadRunInWindow(t, putter,
		func() error { return putter.Put(incoming) },
		func() error { return deleter.Delete("gt-abc") },
		"the Delete")
	assertNoRecord(t, town, "gt-abc")
}

// TestAheadStoreDeleteRemovesUnderTheStoreLock: Delete removes the record only
// while it holds the store lock, so a remove cannot race a Put's read-to-write
// window (gt-guahwz). The check runs inside the remove, where the lock is held.
func TestAheadStoreDeleteRemovesUnderTheStoreLock(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	seedAheadRecord(t, town, aheadTestRecord("gt-abc", aheadTestHead))
	deleter := TownAheadStore(town, "gastown")
	deleter.removeFile = func(path string) error {
		if aheadStoreMu.TryLock() {
			aheadStoreMu.Unlock()
			t.Error("Delete removed the record without holding the store lock")
		}
		return os.Remove(path)
	}
	if err := deleter.Delete("gt-abc"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	assertNoRecord(t, town, "gt-abc")
}
