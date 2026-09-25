package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/reaper"
)

func TestReaperDatabaseNamesTrimsConfiguredList(t *testing.T) {
	oldDB := reaperDB
	t.Cleanup(func() { reaperDB = oldDB })

	reaperDB = " hq, gastown ,, beads "
	got := reaperDatabaseNames()
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
	oldDelay := reaperDBDelay
	t.Cleanup(func() { reaperDBDelay = oldDelay })

	reaperDBDelay = "0s"
	if err := waitBeforeReaperDatabase(0); err != nil {
		t.Fatalf("first database wait returned error: %v", err)
	}
	if err := waitBeforeReaperDatabase(1); err != nil {
		t.Fatalf("zero-delay wait returned error: %v", err)
	}

	reaperDBDelay = "not-a-duration"
	if err := waitBeforeReaperDatabase(1); err == nil {
		t.Fatal("invalid delay should return an error")
	}
}

func TestDefaultReaperEndpointIgnoresStaleBeadsAliases(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("GT_DOLT_HOST", "")
	t.Setenv("GT_DOLT_PORT", "")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale-host")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")
	t.Setenv("BEADS_DOLT_PORT", "9999")

	host, port := defaultReaperEndpoint()
	if host != "127.0.0.1" || port != 3307 {
		t.Fatalf("defaultReaperEndpoint() = %s:%d, want 127.0.0.1:3307", host, port)
	}
}

func TestDefaultReaperEndpointUsesTownConfig(t *testing.T) {
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
	t.Chdir(townRoot)
	t.Setenv("GT_DOLT_IGNORE_CONFIG", "")
	t.Setenv("GT_DOLT_HOST", "")
	t.Setenv("GT_DOLT_PORT", "")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale-host")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")
	t.Setenv("BEADS_DOLT_PORT", "9999")

	host, port := defaultReaperEndpoint()
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
