package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
		if strings.Contains(l, "seat-refill.hold") {
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

// countContaining returns how many lines contain every fragment.
func countContaining(lines []string, fragments ...string) int {
	n := 0
	for _, l := range lines {
		ok := true
		for _, f := range fragments {
			if !strings.Contains(l, f) {
				ok = false
				break
			}
		}
		if ok {
			n++
		}
	}
	return n
}

// lineLogger is a *log.Logger whose output is split into lines on read.
func lineLogger() (*log.Logger, func() []string) {
	var buf bytes.Buffer
	var mu sync.Mutex
	w := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	})
	return log.New(w, "", 0), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Split(strings.TrimSpace(buf.String()), "\n")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// stubGTOnPath puts a `gt` on PATH that logs each invocation and exits 0, for
// callers that exec "gt" by name rather than through a configured path.
func stubGTOnPath(t *testing.T) (logPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub")
	}
	bin := t.TempDir()
	logPath = filepath.Join(bin, "gt.log")
	script := "#!/bin/sh\necho \"$*\" >> \"" + logPath + "\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "gt"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// TestDispatchQueuedWork_OperatorHold_SkipsSchedulerRunAndLogsOnce covers
// heartbeat step 14: `gt scheduler run` slings queued beads (executeSling)
// and must stand down under the hold, logging once per hold-state change.
func TestDispatchQueuedWork_OperatorHold_SkipsSchedulerRunAndLogsOnce(t *testing.T) {
	gtLog := stubGTOnPath(t)
	t.Setenv("GT_SEAT_REFILL_HOLD", "")
	townRoot := t.TempDir()
	writeOperatorHold(t, townRoot)
	logger, lines := lineLogger()
	d := &Daemon{config: &Config{TownRoot: townRoot}, logger: logger}

	for i := 0; i < 3; i++ {
		d.dispatchQueuedWork()
	}
	if data, err := os.ReadFile(gtLog); err == nil {
		t.Fatalf("gt scheduler run invoked during an operator hold: %s", data)
	}
	if n := countContaining(lines(), "scheduler dispatch", "seat-refill.hold"); n != 1 {
		t.Errorf("hold logged %d times over 3 ticks, want once; log: %v", n, lines())
	}

	if err := os.Remove(filepath.Join(townRoot, "seat-refill.hold")); err != nil {
		t.Fatal(err)
	}
	d.dispatchQueuedWork()
	data, err := os.ReadFile(gtLog)
	if err != nil || !strings.Contains(string(data), "scheduler run") {
		t.Errorf("after the hold lifted, gt scheduler run was not invoked (log %q, err %v)", data, err)
	}
	if n := countContaining(lines(), "scheduler dispatch", "hold lifted"); n != 1 {
		t.Errorf("hold lift logged %d times, want once; log: %v", n, lines())
	}
}

func TestRunScheduledSlings_OperatorHold_LogsOncePerStateChange(t *testing.T) {
	t.Setenv("GT_SEAT_REFILL_HOLD", "")
	f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
	d, _ := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry}, f)
	logger, lines := lineLogger()
	d.logger = logger
	writeOperatorHold(t, d.config.TownRoot)

	for i := 0; i < 4; i++ {
		d.runScheduledSlings()
	}
	if n := countContaining(lines(), "scheduled_slings", "seat-refill.hold"); n != 1 {
		t.Errorf("hold logged %d times over 4 ticks, want once; log: %v", n, lines())
	}
}

// A rig ESTOP holds that rig's scheduled entries and nothing else.
func TestRunScheduledSlings_RigEstop_SkipsOnlyThatRig(t *testing.T) {
	t.Setenv("GT_SEAT_REFILL_HOLD", "")
	f := &fakeScheduledRunner{createID: "gt-run1", now: time.Now()}
	omEntry := ScheduledSlingEntry{Name: "om-audit", Rig: "om", Formula: "mol-doc-audit", IntervalStr: "168h"}
	d, esc := newScheduledTestDaemon(t, []ScheduledSlingEntry{docAuditEntry, omEntry}, f)
	if err := os.WriteFile(filepath.Join(d.config.TownRoot, "ESTOP.gastown"), []byte("manual\t2026-09-24T00:00:00Z\tt\n"), 0644); err != nil {
		t.Fatal(err)
	}

	d.runScheduledSlings()

	if len(f.slung) != 1 || f.slung[0].Rig != "om" {
		t.Errorf("slung %v, want only the om entry", f.slung)
	}
	if n := d.scheduledSlingFailures["doc-audit"]; n != 0 || len(*esc) != 0 {
		t.Errorf("a rig ESTOP counted as a failure (%d) or escalated (%v)", n, *esc)
	}
}

func TestFeedFirstReady_OperatorHold_LogsOncePerStateChange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	t.Setenv("GT_SEAT_REFILL_HOLD", "")
	townRoot, gtPath, _ := backpressureFeedRig(t, filepath.Join(t.TempDir(), "never-refuse"))
	writeOperatorHold(t, townRoot)
	var logged []string
	logger := func(format string, args ...interface{}) { logged = append(logged, fmt.Sprintf(format, args...)) }
	m := NewConvoyManager(townRoot, logger, gtPath, 10*time.Minute, nil, nil, nil)

	for i := 0; i < 3; i++ {
		m.feedFirstReady(strandedConvoyInfo{ID: "hq-cv-held", ReadyCount: 1, ReadyIssues: []string{"gt-issue1"}})
	}
	if n := countContaining(logged, "seat-refill.hold"); n != 1 {
		t.Errorf("hold logged %d times over 3 scans, want once; log: %v", n, logged)
	}
}

func TestFeedFirstReady_RigEstop_SkipsThatRigsIssue(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows")
	}
	t.Setenv("GT_SEAT_REFILL_HOLD", "")
	townRoot, gtPath, slingLogPath := backpressureFeedRig(t, filepath.Join(t.TempDir(), "never-refuse"))
	// routes map gt- to rig "gt".
	if err := os.WriteFile(filepath.Join(townRoot, "ESTOP.gt"), []byte("manual\t2026-09-24T00:00:00Z\tt\n"), 0644); err != nil {
		t.Fatal(err)
	}
	var logged []string
	logger := func(format string, args ...interface{}) { logged = append(logged, fmt.Sprintf(format, args...)) }
	m := NewConvoyManager(townRoot, logger, gtPath, 10*time.Minute, nil, nil, nil)

	m.feedFirstReady(strandedConvoyInfo{ID: "hq-cv-rig", ReadyCount: 1, ReadyIssues: []string{"gt-issue1"}})

	if data, err := os.ReadFile(slingLogPath); err == nil {
		t.Errorf("slung into a rig under ESTOP: %s", data)
	}
	if countContaining(logged, "gt-issue1", "ESTOP.gt") == 0 {
		t.Errorf("no log line naming the rig ESTOP; got %v", logged)
	}
}
