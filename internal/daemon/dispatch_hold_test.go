package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The daemon's automatic dispatchers — the stranded-convoy feeder and the
// scheduled_slings patrol — stand down while the operator's town-wide hold
// file exists (gt-ifijm). Each test uses a temp town root; the real
// ~/gt/seat-refill.hold is never read.

func writeOperatorHold(t *testing.T, townRoot string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(townRoot, "seat-refill.hold"), nil, 0644); err != nil {
		t.Fatalf("write hold: %v", err)
	}
}

func TestFeedFirstReady_OperatorHold_SlingsNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	// A gt that would succeed: only the hold can stop the sling.
	townRoot, gtPath, slingLogPath := backpressureFeedRig(t, filepath.Join(t.TempDir(), "never-refuse"))
	writeOperatorHold(t, townRoot)

	var logged []string
	logger := func(format string, args ...interface{}) { logged = append(logged, fmt.Sprintf(format, args...)) }
	m := NewConvoyManager(townRoot, logger, gtPath, 10*time.Minute, nil, nil, nil)

	m.feedFirstReady(strandedConvoyInfo{ID: "hq-cv-held", ReadyCount: 1, ReadyIssues: []string{"gt-issue1"}})

	if data, err := os.ReadFile(slingLogPath); err == nil {
		t.Errorf("gt invoked during an operator hold: %s", data)
	}
	found := false
	for _, l := range logged {
		if strings.Contains(l, "hq-cv-held") && strings.Contains(l, "seat-refill.hold") {
			found = true
		}
	}
	if !found {
		t.Errorf("no log line naming the hold; got %v", logged)
	}
}

func TestRunScheduledSlings_OperatorHold_DispatchesNothingAndCountsNoFailure(t *testing.T) {
	f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
	d, esc := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	writeOperatorHold(t, d.config.TownRoot)

	for i := 0; i < 3; i++ {
		d.runScheduledSlings()
	}

	if len(f.created) != 0 || len(f.slungIDs) != 0 {
		t.Errorf("scheduled sling ran during an operator hold: create=%v sling=%v", f.created, f.slungIDs)
	}
	if n := d.scheduledSlingFailures["doc-audit"]; n != 0 {
		t.Errorf("a hold counted as %d failure(s); it is not a failure", n)
	}
	if len(*esc) != 0 {
		t.Errorf("a hold escalated: %v", *esc)
	}

	// Lifting the hold resumes dispatch on the next tick.
	if err := os.Remove(filepath.Join(d.config.TownRoot, "seat-refill.hold")); err != nil {
		t.Fatal(err)
	}
	if err := d.runScheduledSlingEntry(context.Background(), docAuditEntry, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(f.slungIDs) != 1 {
		t.Errorf("after the hold lifted: sling calls = %v, want one", f.slungIDs)
	}
}
