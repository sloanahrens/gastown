package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/reaper"
)

func TestReaperDatabaseNamesTrimsConfiguredList(t *testing.T) {
	t.Parallel()
	got := parseReaperDatabaseList(" hq, gastown ,, beads ")
	want := []string{"hq", "gastown", "beads"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reaperDatabaseNames() = %#v, want %#v", got, want)
	}
}

// TestReaperAutoClosePreviewFlag pins the flag that carries the dry run's
// authorization (gt-39bu). `gt reaper run` composes its own preview in-process,
// so the flag belongs to auto-close alone; a bare `gt reaper auto-close` has to
// be able to refuse for want of it.
func TestReaperAutoClosePreviewFlag(t *testing.T) {
	t.Parallel()
	flag := reaperAutoCloseCmd.Flags().Lookup("preview")
	if flag == nil {
		t.Fatal("gt reaper auto-close has no --preview flag: a live run could not be bound to a dry run")
	}
	if flag.DefValue != "" {
		t.Errorf("--preview default = %q, want empty: a default would authorize every live run", flag.DefValue)
	}
	if runFlag := reaperRunCmd.Flags().Lookup("preview"); runFlag != nil {
		t.Error("gt reaper run takes --preview, but it previews in-process before writing and should not accept a caller's hash")
	}
}

func TestWaitBeforeReaperDatabase(t *testing.T) {
	t.Parallel()
	clk := clockwork.NewFakeClockAt(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC))
	if err := waitBeforeReaperDatabase(clk, 0, "not-a-duration"); err != nil {
		t.Fatalf("first database wait returned error: %v", err)
	}
	if err := waitBeforeReaperDatabase(clk, 1, "0s"); err != nil {
		t.Fatalf("zero-delay wait returned error: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- waitBeforeReaperDatabase(clk, 1, "250ms") }()
	if err := clk.BlockUntilContext(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(250 * time.Millisecond)
	if err := <-done; err != nil {
		t.Fatalf("250ms wait returned error: %v", err)
	}

	if err := waitBeforeReaperDatabase(clk, 1, "not-a-duration"); err == nil {
		t.Fatal("invalid delay should return an error")
	}
}

// Without a town endpoint the reaper gets no port, never a guessed 3307
// (gt-y3pgh.3).
func TestDefaultReaperEndpointWithoutTownHasNoPort(t *testing.T) {
	t.Parallel()
	host, port := reaperEndpoint("")
	if host != "127.0.0.1" || port != 0 {
		t.Fatalf("reaperEndpoint(\"\") = %s:%d, want 127.0.0.1:0", host, port)
	}
}

func TestDefaultReaperEndpointUsesTownConfig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mayorDir, "town.json"), []byte(`{"name":"test-town"}`), 0644); err != nil {
		t.Fatal(err)
	}
	doltDataDir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltDataDir, "config.yaml"), []byte("listener:\n  host: 127.0.0.2\n  port: 5507\n"), 0644); err != nil {
		t.Fatal(err)
	}

	host, port := reaperEndpoint(townRoot)
	if host != "127.0.0.2" || port != 5507 {
		t.Fatalf("defaultReaperEndpoint() = %s:%d, want 127.0.0.2:5507", host, port)
	}
}

// TestWriteAutoCloseReportPrintsTheFloorRefusal covers the reporting gap the
// below-floor refusal opened. The refusal arrives with the candidate set in the
// result, and that set is the whole report — nothing was closed, so the set the
// mis-set threshold would take is the only thing there is to act on. A report
// gated on the error instead would drop it and print "auto-closed 0" alone,
// which reads as a clean run rather than as the threshold to go fix (gt-ecpj).
func TestWriteAutoCloseReportPrintsTheFloorRefusal(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	closed := writeAutoCloseReport(&stdout, &stderr, &reaper.AutoCloseResult{
		Database:  "hq",
		Floored:   true,
		FlooredAt: time.Hour,
		ClosedEntries: []reaper.ClosedEntry{
			{ID: "hq-a", Title: "abandoned hq-a", AgeDays: 60, Database: "hq"},
			{ID: "hq-b", Title: "abandoned hq-b", AgeDays: 60, Database: "hq"},
		},
	})

	if closed != 0 {
		t.Errorf("writeAutoCloseReport returned %d closed, want 0: a below-floor sweep closes nothing", closed)
	}
	if want := reaper.FloorNotice(time.Hour, 2); !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want the below-floor notice %q", stderr.String(), want)
	}
	for _, id := range []string{"hq-a", "hq-b"} {
		if !strings.Contains(stdout.String(), id) {
			t.Errorf("stdout = %q, want the refused candidate %s listed: the set is the refusal's report",
				stdout.String(), id)
		}
	}
	if strings.Contains(stdout.String(), "auto-closed") {
		t.Errorf("stdout = %q, want no close count for a refused sweep: nothing was closed", stdout.String())
	}
}

// TestWriteAutoCloseReportHintsNoPreviewForARefusal keeps the refusal from
// reading as a pass in dry-run mode. A below-floor dry run still has a candidate
// count and a preview hash, so the ordinary "[DRY RUN] would auto-closed N —
// live run: --preview=H" line would both claim a close and hand over a hash the
// live run refuses (gt-ecpj).
func TestWriteAutoCloseReportHintsNoPreviewForARefusal(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	closed := writeAutoCloseReport(&stdout, &stderr, &reaper.AutoCloseResult{
		Database:    "hq",
		DryRun:      true,
		Floored:     true,
		FlooredAt:   time.Hour,
		PreviewHash: "abc123",
		ClosedEntries: []reaper.ClosedEntry{
			{ID: "hq-a", Title: "abandoned hq-a", AgeDays: 60, Database: "hq"},
		},
	})

	if closed != 0 {
		t.Errorf("writeAutoCloseReport returned %d closed, want 0: a refused dry run closes nothing", closed)
	}
	for _, unwanted := range []string{"would auto-closed", "--preview="} {
		if strings.Contains(stdout.String(), unwanted) {
			t.Errorf("stdout = %q, want no %q: the live run refuses this threshold", stdout.String(), unwanted)
		}
	}
	if !strings.Contains(stdout.String(), "hq-a") {
		t.Errorf("stdout = %q, want the candidate listed", stdout.String())
	}
}

// TestWriteAutoCloseReportCountsWhatAClosedSweepClosed is the other side: an
// ordinary sweep totals its closes, so the below-floor refusal's zero cannot be
// mistaken for a reporting path that never counts anything.
func TestWriteAutoCloseReportCountsWhatAClosedSweepClosed(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	closed := writeAutoCloseReport(&stdout, &stderr, &reaper.AutoCloseResult{
		Database: "hq",
		Closed:   3,
		ClosedEntries: []reaper.ClosedEntry{
			{ID: "hq-a", Title: "abandoned hq-a", AgeDays: 90, Database: "hq"},
		},
	})

	if closed != 3 {
		t.Errorf("writeAutoCloseReport returned %d closed, want 3", closed)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing: a sweep at the floor has no refusal to report", stderr.String())
	}
	if !strings.Contains(stdout.String(), "auto-closed 3 stale issues") {
		t.Errorf("stdout = %q, want the close count", stdout.String())
	}
}
