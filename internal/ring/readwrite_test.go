package ring

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// now is the instant every read in these tests judges a record at.
var now = time.Date(2026, 10, 30, 20, 0, 0, 0, time.UTC)

// tempTown makes a town root holding mayor/town.json with the given top-level
// tree, written through the config kernel so the file is in its own form.
func tempTown(t *testing.T, tree map[string]any) string {
	t.Helper()
	root := t.TempDir()
	path := townPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// townWith is tempTown plus a valid record stored through Write.
func townWith(t *testing.T, rec Record) string {
	t.Helper()
	root := tempTown(t, map[string]any{"type": "town", "version": 2, "name": "test"})
	if err := Write(root, rec); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return root
}

// writeSection stores raw as the town's ring section, bypassing Write so a
// test can put something in it that Write refuses.
func writeSection(t *testing.T, root string, raw any) {
	t.Helper()
	err := config.UpdateRawJSON(townPath(root), func(tree map[string]any) (bool, error) {
		tree[SectionKey] = raw
		return true, nil
	})
	if err != nil {
		t.Fatalf("storing the ring section: %v", err)
	}
}

func mustState(t *testing.T, root string) State {
	t.Helper()
	st, err := Read(root, testKey, now)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return st
}

// TestReadValid: a signed record survives the JSON round trip, including a
// time in another zone and with sub-second precision, because the signature
// covers the canonical UTC form.
func TestReadValid(t *testing.T) {
	t.Parallel()
	east := time.FixedZone("UTC+2", 2*60*60)
	cases := []struct {
		name string
		rec  Record
	}{
		{"whole seconds", Record{ForgedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), ForgedBy: "mayor"}},
		{"nanoseconds in another zone", Record{
			ForgedAt:  now.Add(-time.Hour).In(east).Add(123456789 * time.Nanosecond),
			ExpiresAt: now.Add(time.Hour).In(east).Add(987654321 * time.Nanosecond),
			ForgedBy:  "mayor",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec, err := Sign(tc.rec, testKey)
			if err != nil {
				t.Fatal(err)
			}
			st := mustState(t, townWith(t, rec))
			if st.Kind != Valid {
				t.Fatalf("state = %v, want valid", st)
			}
			if st.Reason != "" {
				t.Errorf("valid state carries reason %q, want none", st.Reason)
			}
			if st.Record == nil || !st.Record.ExpiresAt.Equal(rec.ExpiresAt) || st.Record.ForgedBy != "mayor" {
				t.Errorf("state record = %+v, want the stored record", st.Record)
			}
		})
	}
}

func TestReadWanedAtTheExpiry(t *testing.T) {
	t.Parallel()
	for _, at := range []struct {
		name string
		now  time.Time
	}{
		{"the instant of expiry", now},
		{"after the expiry", now.Add(time.Minute)},
	} {
		t.Run(at.name, func(t *testing.T) {
			t.Parallel()
			rec := Record{ForgedAt: now.Add(-Lease), ExpiresAt: now, ForgedBy: "mayor"}
			rec, err := Sign(rec, testKey)
			if err != nil {
				t.Fatal(err)
			}
			st, err := Read(townWith(t, rec), testKey, at.now)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if st.Kind != Waned || st.Reason != ReasonExpired {
				t.Fatalf("state = %v, want waned (%s)", st, ReasonExpired)
			}
			if st.Record == nil {
				t.Error("a waned state keeps the record it read")
			}
		})
	}
}

// TestReadAbsentWithItsReason: every way a reader loses the record is Absent,
// and each names its own cause.
func TestReadAbsentWithItsReason(t *testing.T) {
	t.Parallel()
	signedRec := func(t *testing.T) Record {
		t.Helper()
		rec, err := Sign(Record{ForgedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), ForgedBy: "mayor"}, testKey)
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	cases := []struct {
		name   string
		town   func(t *testing.T) string
		reason string
	}{
		{"no section", func(t *testing.T) string {
			return tempTown(t, map[string]any{"type": "town", "version": 2, "name": "test"})
		}, ReasonNoSection},
		{"not a record", func(t *testing.T) string {
			root := tempTown(t, map[string]any{"type": "town", "version": 2, "name": "test"})
			writeSection(t, root, "the one ring")
			return root
		}, ReasonUnparseable},
		{"a null section", func(t *testing.T) string {
			root := tempTown(t, map[string]any{"type": "town", "version": 2, "name": "test"})
			writeSection(t, root, nil)
			return root
		}, ReasonUnparseable},
		{"no expires_at", func(t *testing.T) string {
			root := tempTown(t, map[string]any{"type": "town", "version": 2, "name": "test"})
			rec := signedRec(t)
			writeSection(t, root, map[string]any{
				"forged_at": rec.ForgedAt,
				"forged_by": rec.ForgedBy,
				"hmac":      rec.HMAC,
			})
			return root
		}, ReasonNoExpiry},
		{"no signature", func(t *testing.T) string {
			root := tempTown(t, map[string]any{"type": "town", "version": 2, "name": "test"})
			rec := signedRec(t)
			rec.HMAC = ""
			writeSection(t, root, map[string]any{
				"forged_at":  rec.ForgedAt,
				"expires_at": rec.ExpiresAt,
				"forged_by":  rec.ForgedBy,
			})
			return root
		}, ReasonUnsigned},
		{"signed by another key", func(t *testing.T) string {
			rec, err := Sign(Record{ForgedAt: now, ExpiresAt: now.Add(Lease), ForgedBy: "mayor"}, []byte("another key"))
			if err != nil {
				t.Fatal(err)
			}
			return townWith(t, rec)
		}, ReasonBadSignature},
		{"a signed field changed after signing", func(t *testing.T) string {
			rec := signedRec(t)
			rec.ForgedBy = "someone else"
			return townWith(t, rec)
		}, ReasonBadSignature},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := mustState(t, tc.town(t))
			if st.Kind != Absent {
				t.Fatalf("state = %v, want absent", st)
			}
			if st.Reason != tc.reason {
				t.Errorf("reason = %q, want %q", st.Reason, tc.reason)
			}
			if st.Record != nil {
				t.Errorf("an absent state exposes a record: %+v", st.Record)
			}
		})
	}
}

// TestReadRefusesAnUnreadableTown: a town whose config cannot be read is not a
// town with no ring — the caller has to act on it.
func TestReadRefusesAnUnreadableTown(t *testing.T) {
	t.Parallel()
	t.Run("no town.json", func(t *testing.T) {
		t.Parallel()
		_, err := Read(t.TempDir(), testKey, now)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("Read of a town with no mayor/town.json = %v, want an os.IsNotExist error", err)
		}
	})
	t.Run("unparseable town.json", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		path := townPath(root)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(root, testKey, now); err == nil {
			t.Fatal("Read of an unparseable mayor/town.json = nil error, want a refusal")
		}
	})
}

func TestReadRefusesNoKey(t *testing.T) {
	t.Parallel()
	root := tempTown(t, map[string]any{"type": "town", "version": 2, "name": "test"})
	if _, err := Read(root, nil, now); !errors.Is(err, ErrNoKey) {
		t.Fatalf("Read with no key = %v, want ErrNoKey", err)
	}
}

func TestWriteRefuses(t *testing.T) {
	t.Parallel()
	root := tempTown(t, map[string]any{"type": "town", "version": 2, "name": "test"})
	rec, err := Sign(Record{ForgedAt: now, ExpiresAt: now.Add(Lease), ForgedBy: "mayor"}, testKey)
	if err != nil {
		t.Fatal(err)
	}
	noExpiry := rec
	noExpiry.ExpiresAt = time.Time{}
	if err := Write(root, noExpiry); !errors.Is(err, ErrNoExpiry) {
		t.Errorf("Write of a record with no expires_at = %v, want ErrNoExpiry", err)
	}
	unsigned := rec
	unsigned.HMAC = ""
	if err := Write(root, unsigned); !errors.Is(err, ErrUnsigned) {
		t.Errorf("Write of an unsigned record = %v, want ErrUnsigned", err)
	}
	if st := mustState(t, root); st.Kind != Absent || st.Reason != ReasonNoSection {
		t.Errorf("a refused Write left %v behind, want absent (%s)", st, ReasonNoSection)
	}
}

// TestWriteDoesNotCreateTheTown: the ring section is a section of an existing
// mayor/town.json, never a reason to bring one into being.
func TestWriteDoesNotCreateTheTown(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	rec, err := Sign(Record{ForgedAt: now, ExpiresAt: now.Add(Lease), ForgedBy: "mayor"}, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(root, rec); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Write into a town with no mayor/town.json = %v, want an os.IsNotExist error", err)
	}
	if _, err := os.Stat(townPath(root)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Write created %s", townPath(root))
	}
}

// TestWriteRoundTripsAndKeepsEveryOtherSection: Write touches the ring section
// alone; the registry, the overseer and every other section read back exactly
// as they were.
func TestWriteRoundTripsAndKeepsEveryOtherSection(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := townPath(root)
	town := &config.TownConfig{
		Type:    "town",
		Version: config.CurrentTownVersion,
		Name:    "gastown",
		Dolt:    &config.DoltEndpoint{Port: 3307},
		Registry: &config.RigsConfig{Version: config.CurrentRigsVersion, Rigs: map[string]config.RigEntry{
			"gastown": {GitURL: "https://example.invalid/gastown.git", AddedAt: now},
		}},
		Overseer: &config.OverseerConfig{Name: "the overseer"},
	}
	if err := config.WriteConfigJSON(path, town, 0o600); err != nil {
		t.Fatalf("writing the town: %v", err)
	}
	before := sections(t, path)

	rec := Record{ForgedAt: now, ExpiresAt: now.Add(Lease), ForgedBy: "mayor"}
	rec, err := Sign(rec, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(root, rec); err != nil {
		t.Fatalf("Write: %v", err)
	}

	after := sections(t, path)
	for key, was := range before {
		if key == SectionKey {
			continue
		}
		if after[key] != was {
			t.Errorf("section %q changed:\nbefore: %s\nafter:  %s", key, was, after[key])
		}
	}
	if _, has := after[SectionKey]; !has {
		t.Fatal("Write left no ring section")
	}
	st := mustState(t, root)
	if st.Kind != Valid || !st.Record.ExpiresAt.Equal(rec.ExpiresAt) || st.Record.ForgedBy != rec.ForgedBy {
		t.Fatalf("Read after Write = %v (%+v), want the record written", st, st.Record)
	}
}

// TestWrittenSectionStillLoadsAsATown: the ring section is registered on
// TownConfig, so the strict decoder a gt verb runs accepts the file ring
// wrote. Without that registration every town.json with a ring would fail to
// load.
func TestWrittenSectionStillLoadsAsATown(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := townPath(root)
	if err := config.WriteConfigJSON(path, &config.TownConfig{Type: "town", Version: config.CurrentTownVersion, Name: "gastown"}, 0o600); err != nil {
		t.Fatal(err)
	}
	rec, err := Sign(Record{ForgedAt: now, ExpiresAt: now.Add(Lease), ForgedBy: "mayor"}, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(root, rec); err != nil {
		t.Fatal(err)
	}
	town, err := config.LoadTownConfig(path)
	if err != nil {
		t.Fatalf("LoadTownConfig of a town with a ring = %v", err)
	}
	if len(town.Ring) == 0 {
		t.Error("the loaded town dropped the ring section")
	}
}

// TestSaveTownConfigKeepsTheRing: a TownConfig built without the ring still
// saves over a town that has one, as the registry and overseer sections do.
func TestSaveTownConfigKeepsTheRing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := townPath(root)
	if err := config.WriteConfigJSON(path, &config.TownConfig{Type: "town", Version: config.CurrentTownVersion, Name: "gastown"}, 0o600); err != nil {
		t.Fatal(err)
	}
	rec, err := Sign(Record{ForgedAt: now, ExpiresAt: now.Add(Lease), ForgedBy: "mayor"}, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := Write(root, rec); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveTownConfig(path, &config.TownConfig{Type: "town", Version: config.CurrentTownVersion, Name: "renamed"}); err != nil {
		t.Fatalf("SaveTownConfig: %v", err)
	}
	if st := mustState(t, root); st.Kind != Valid {
		t.Fatalf("state after SaveTownConfig = %v, want valid", st)
	}
}

// sections maps every top-level key of path to its JSON, so a test can compare
// one section before and after a write. The file's own formatting is the
// kernel's; a section's bytes are what this compares.
func sections(t *testing.T, path string) map[string]string {
	t.Helper()
	tree, err := config.ReadRawJSON(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	out := make(map[string]string, len(tree))
	for key, value := range tree {
		data, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatalf("encoding section %q: %v", key, err)
		}
		out[key] = string(data)
	}
	return out
}
