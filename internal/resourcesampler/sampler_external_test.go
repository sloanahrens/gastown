// Package resourcesampler_test checks the package's exported row writer from
// where its caller stands: a source outside resourcesampler (gt-6bmp5y).
package resourcesampler_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/resourcesampler"
)

// seatRow is a row shaped as the design's table gives a seat row, built with
// the exported fields alone.
func seatRow() resourcesampler.Row {
	rig, polecat, bead, cluster := "gastown", "toast", "gt-abc", "gt-sbx"
	cpu, memLimit := 12.5, int64(2<<30)
	someCPU, someMem, fullMem := 3.5, 4.5, 0.5
	return resourcesampler.Row{
		V:                  resourcesampler.SchemaVersion,
		TS:                 "2026-11-14T09:30:05Z",
		IntervalS:          60,
		Source:             "sandbox",
		Class:              "sandbox",
		Component:          "seat",
		Name:               "mango-toast",
		Rig:                &rig,
		Polecat:            &polecat,
		Bead:               &bead,
		CPUPct:             &cpu,
		MemBytes:           1 << 30,
		MemLimitBytes:      &memLimit,
		Workspace:          "workspace-toast",
		Cluster:            &cluster,
		PressureCPUSomePct: &someCPU,
		PressureMemSomePct: &someMem,
		PressureMemFullPct: &fullMem,
	}
}

// dayFile reads the day file the writer keeps under metricsDir.
func dayFile(t *testing.T, metricsDir, day string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(metricsDir, "resources", day+".jsonl"))
	if err != nil {
		t.Fatalf("read day file: %v", err)
	}
	return data
}

func TestAppendRowWritesOneLineForAnOutsideCaller(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Date(2026, 11, 14, 9, 30, 5, 0, time.UTC)
	row := seatRow()

	if err := resourcesampler.AppendRow(dir, row, now); err != nil {
		t.Fatalf("append row: %v", err)
	}

	data := dayFile(t, dir, "2026-11-14")
	if got := strings.Count(string(data), "\n"); got != 1 {
		t.Fatalf("the call wrote %d lines, want one whole line: %q", got, data)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("the line has no terminating newline: %q", data)
	}
	var got resourcesampler.Row
	if err := json.Unmarshal(bytes.TrimSuffix(data, []byte("\n")), &got); err != nil {
		t.Fatalf("decode the line: %v", err)
	}
	if !reflect.DeepEqual(got, row) {
		t.Errorf("the line decodes to\n%+v\nwant the row\n%+v", got, row)
	}
}

func TestAppendRowKeepsEarlierLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Date(2026, 11, 14, 9, 30, 5, 0, time.UTC)

	if err := resourcesampler.AppendRow(dir, seatRow(), now); err != nil {
		t.Fatalf("append first row: %v", err)
	}
	second := seatRow()
	second.Name = "mango-furiosa"
	if err := resourcesampler.AppendRow(dir, second, now.Add(time.Minute)); err != nil {
		t.Fatalf("append second row: %v", err)
	}

	data := dayFile(t, dir, "2026-11-14")
	if got := strings.Count(string(data), "\n"); got != 2 {
		t.Fatalf("two calls wrote %d lines, want two", got)
	}
	var last resourcesampler.Row
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("decode the last line: %v", err)
	}
	if last.Name != second.Name {
		t.Errorf("the last line names %q, want the second call's %q", last.Name, second.Name)
	}
}

func TestAppendRowFilesUnderTheUTCDay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// Half past midnight on the 15th, two hours ahead of UTC, is 22:30 UTC on
	// the 14th: the reader's day, not the caller's.
	now := time.Date(2026, 11, 15, 0, 30, 0, 0, time.FixedZone("UTC+2", 2*60*60))

	if err := resourcesampler.AppendRow(dir, seatRow(), now); err != nil {
		t.Fatalf("append row: %v", err)
	}
	dayFile(t, dir, "2026-11-14")
	if _, err := os.Stat(filepath.Join(dir, "resources", "2026-11-15.jsonl")); err == nil {
		t.Fatal("the row was filed under the caller's local day, not the UTC day")
	}
}

func TestAppendRowRefusesEmptyMetricsDir(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 9, 30, 5, 0, time.UTC)

	if err := resourcesampler.AppendRow("", seatRow(), now); err == nil {
		t.Fatal("an empty metrics directory was accepted, which files the row under the working directory")
	}
}
