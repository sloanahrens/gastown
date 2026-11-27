package hostprobe

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// goCacheTrimWire is the go_cache_trim field as devops gt-mac-watch publishes
// it: a literal, so a renamed key fails here rather than drifting silently.
const goCacheTrimWire = `"go_cache_trim":{"value":{"last_good":"2026-11-26T17:00:00Z"},"err":""}`

// writeProbes writes body as a probe file under a fresh temp dir.
func writeProbes(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probes.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write the fixture: %v", err)
	}
	return path
}

// TestGoCacheTrimReadsThePublishedWireText: the publisher's literal wire text
// decodes with its last_good and no error.
func TestGoCacheTrimReadsThePublishedWireText(t *testing.T) {
	t.Parallel()
	now := testAt()
	path := writeProbes(t, fmt.Sprintf(`{"v":3,"at":%q,%s}`, now.Format(time.RFC3339), goCacheTrimWire))
	got, err := Read(path, now, 3*time.Minute)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.GoCacheTrim == nil || got.GoCacheTrim.Err != "" {
		t.Fatalf("GoCacheTrim = %+v, want the reading the file carries with an empty err", got.GoCacheTrim)
	}
	want := time.Date(2026, 11, 26, 17, 0, 0, 0, time.UTC)
	if !got.GoCacheTrim.Value.LastGood.Equal(want) {
		t.Errorf("GoCacheTrim.Value.LastGood = %v, want %v", got.GoCacheTrim.Value.LastGood, want)
	}
}

// TestGoCacheTrimIsNilWhenTheFileHasNoKey: a version 3 file written before the
// field decodes with it nil, not a zero reading a caller could take for a trim.
func TestGoCacheTrimIsNilWhenTheFileHasNoKey(t *testing.T) {
	t.Parallel()
	now := testAt()
	path := writeProbes(t, fmt.Sprintf(`{"v":3,"at":%q,"mem_free_pct":{"value":94}}`, now.Format(time.RFC3339)))
	got, err := Read(path, now, 3*time.Minute)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.GoCacheTrim != nil {
		t.Errorf("GoCacheTrim = %+v, want nil for a file without the key", *got.GoCacheTrim)
	}
}

// TestGoCacheTrimKeepsSchemaVersion3: the field is optional, so it does not
// bump the schema.
func TestGoCacheTrimKeepsSchemaVersion3(t *testing.T) {
	t.Parallel()
	if SchemaVersion != 3 {
		t.Errorf("SchemaVersion = %d, want 3", SchemaVersion)
	}
}
