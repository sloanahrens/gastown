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

// writeShipped stores the record a seat's end wrote for b-1 over [start,end]; errText goes
// straight into the file, since no writer here files a complete record carrying an error.
func writeShipped(t *testing.T, town string, start, end time.Time, complete bool, workspace, errText string) []byte {
	t.Helper()
	seat := "nux"
	rec := Build(Facts{Bead: "b-1", Kind: dashboard.UsageKindWork, Class: dashboard.UsageClassPolecat,
		Rig: "gastown", Seat: &seat, Workspace: workspace, Start: start, End: end, Complete: complete},
		[]SessionUsage{{ID: "s1", Complete: true, Main: []Message{
			{Model: "claude-sonnet-5-5", TS: at(30 * time.Minute), Input: 10, Output: 20}}}})
	dir := filepath.Join(town, ".runtime", "usage")
	if err := os.MkdirAll(filepath.Join(dir, recordsSubdir), 0o755); err != nil {
		t.Fatal(err)
	}
	if errText != "" {
		out, err := json.MarshalIndent(storedRecord{UsageRecord: rec, Sessions: []string{"s1"}, Error: errText}, "", " ")
		if err == nil {
			err = os.WriteFile(WorkRecordPath(dir, "b-1"), out, 0o644)
		}
		if err != nil {
			t.Fatal(err)
		}
	} else if err := Write(dir, rec, []string{"s1"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return recordBytes(t, town, "b-1")
}

// TestScanShipped: no transcript copy reaches the host, so the scan keeps the complete record
// the seat's end wrote while it covers the bead's run, and rewrites an incomplete, failed,
// foreign-workspace or stale one.
func TestScanShipped(t *testing.T) {
	t.Parallel()
	end := t0.Add(time.Hour)
	ended := []events.Event{sling(0, "b-1", "gastown/polecats/nux"), done(time.Hour, "b-1", "gastown/polecats/nux")}
	tests := []struct {
		name                           string
		write                          bool
		evs                            []events.Event
		start, end                     time.Time
		complete                       bool
		workspace, errText             string
		kept, incomplete, wantComplete bool
		wantErr                        string
	}{
		{"a complete record covering the run is left alone", true, ended, t0, end, true, "customer-zero", "", true, false, true, ""},
		{"no record and no copy is incomplete as before", false, ended, time.Time{}, time.Time{}, false, "", "", false, true, false, "no transcript"},
		{"an incomplete record is rewritten", true, ended, t0, end, false, "customer-zero", "", false, true, false, "no transcript"},
		{"a record carrying an error is rewritten", true, ended, t0, end, true, "customer-zero", "its transcript could not be read", false, true, false, "no transcript"},
		{"a record of another workspace is rewritten", true, ended, t0, end, true, "other-zero", "", false, true, false, "no transcript"},
		{"a record that starts after its run is rewritten", true, ended, t0.Add(time.Minute), end, true, "customer-zero", "", false, true, false, "no transcript"},
		{"a record that ends before its run is rewritten", true, ended, t0, end.Add(-time.Minute), true, "customer-zero", "", false, true, false, "no transcript"},
		{"an open run is not covered by the record", true, ended[:1], t0, end, true, "customer-zero", "", false, true, false, "no transcript"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			var before []byte
			if tt.write {
				before = writeShipped(t, town, tt.start, tt.end, tt.complete, tt.workspace, tt.errText)
			}
			res, err := Scan(ScanOptions{Town: town}, fixture{evs: tt.evs}.deps())
			if err != nil {
				t.Fatalf("Scan: %v", err)
			}
			kept, incomplete := slices.Contains(res.Kept, "b-1"), slices.Contains(res.Incomplete, "b-1")
			if kept != tt.kept || incomplete != tt.incomplete {
				t.Fatalf("kept = %t incomplete = %t (result %+v), want %t and %t", kept, incomplete, res, tt.kept, tt.incomplete)
			}
			if st := readRecord(t, town, "b-1"); st.Record.Complete != tt.wantComplete || !strings.Contains(st.Error, tt.wantErr) {
				t.Errorf("record complete=%t error=%q, want complete=%t and error containing %q", st.Record.Complete, st.Error, tt.wantComplete, tt.wantErr)
			}
			if tt.kept && !bytes.Equal(before, recordBytes(t, town, "b-1")) {
				t.Error("the stored record changed on disk")
			}
		})
	}
}
