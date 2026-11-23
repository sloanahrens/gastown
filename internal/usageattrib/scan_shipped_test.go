package usageattrib

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/events"
)

// writeShipped stores the record a seat's end wrote for b-1 over [start,end] and returns the
// file's bytes. errText is written straight into the file, since no writer here stores a
// complete record carrying an error.
func writeShipped(t *testing.T, town string, start, end time.Time, complete bool, workspace, errText string) []byte {
	t.Helper()
	seat := "nux"
	rec := Build(Facts{Bead: "b-1", Kind: dashboard.UsageKindWork, Class: dashboard.UsageClassPolecat,
		Rig: "gastown", Seat: &seat, Workspace: workspace, Start: start, End: end, Complete: complete},
		[]SessionUsage{{ID: "s1", Complete: true, Main: []Message{
			{Model: "claude-sonnet-5-5", TS: at(30 * time.Minute), Input: 10, Output: 20}}}})
	dir := filepath.Join(town, ".runtime", "usage")
	if err := Write(dir, rec, []string{"s1"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	b := recordBytes(t, town, "b-1")
	if errText == "" {
		return b
	}
	var sr storedRecord
	if err := json.Unmarshal(b, &sr); err != nil {
		t.Fatal(err)
	}
	sr.Error = errText
	out, err := json.MarshalIndent(sr, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(WorkRecordPath(dir, "b-1"), out, 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestScanShipped: a seat leaves no transcript copy on the host, so the scan keeps the complete
// record the seat's end wrote while it covers the bead's run, and rewrites one that is
// incomplete, carries an error, names another workspace or misses the run; a bead with neither
// record nor copy is still reported incomplete as before.
func TestScanShipped(t *testing.T) {
	t.Parallel()
	end := t0.Add(time.Hour)
	stored := func(start, end time.Time, complete bool, workspace, errText string) func(*testing.T, string) []byte {
		return func(t *testing.T, town string) []byte {
			return writeShipped(t, town, start, end, complete, workspace, errText)
		}
	}
	tests := []struct {
		name                       string
		write                      func(t *testing.T, town string) []byte
		open                       bool // the bead's run never ended
		kept, incomplete, complete bool
		wantErr                    string
	}{
		{"a complete record covering the run is left alone", stored(t0, end, true, "customer-zero", ""), false, true, false, true, ""},
		{"no record and no copy is incomplete as before", nil, false, false, true, false, "no transcript"},
		{"an incomplete record is rewritten", stored(t0, end, false, "customer-zero", ""), false, false, true, false, "no transcript"},
		{"a record carrying an error is rewritten", stored(t0, end, true, "customer-zero", "its transcript could not be read"), false, false, true, false, "no transcript"},
		{"a record of another workspace is rewritten", stored(t0, end, true, "other-zero", ""), false, false, true, false, "no transcript"},
		{"a record that starts after its run is rewritten", stored(t0.Add(time.Minute), end, true, "customer-zero", ""), false, false, true, false, "no transcript"},
		{"a record that ends before its run is rewritten", stored(t0, end.Add(-time.Minute), true, "customer-zero", ""), false, false, true, false, "no transcript"},
		{"an open run is not covered by the record", stored(t0, end, true, "customer-zero", ""), true, false, true, false, "no transcript"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			var before []byte
			if tt.write != nil {
				before = tt.write(t, town)
			}
			evs := []events.Event{sling(0, "b-1", "gastown/polecats/nux"), done(time.Hour, "b-1", "gastown/polecats/nux")}
			if tt.open {
				evs = evs[:1]
			}
			res, err := Scan(ScanOptions{Town: town}, fixture{evs: evs}.deps())
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if got := slices.Contains(res.Kept, "b-1"); got != tt.kept {
				t.Fatalf("kept = %t (result %+v), want %t", got, res, tt.kept)
			}
			if got := slices.Contains(res.Incomplete, "b-1"); got != tt.incomplete {
				t.Fatalf("incomplete = %t (result %+v), want %t", got, res, tt.incomplete)
			}
			st := readRecord(t, town, "b-1")
			if st.Record.Complete != tt.complete || !strings.Contains(st.Error, tt.wantErr) {
				t.Errorf("record complete=%t error=%q, want complete=%t and error containing %q", st.Record.Complete, st.Error, tt.complete, tt.wantErr)
			}
			if tt.write == nil {
				return
			}
			if changed := !bytes.Equal(before, recordBytes(t, town, "b-1")); changed == tt.kept {
				t.Errorf("record changed on disk = %t, want %t", changed, !tt.kept)
			}
			if tt.kept && strings.Join(st.Sessions, ",") != "s1" {
				t.Errorf("sessions = %v, want [s1]", st.Sessions)
			}
		})
	}
}
