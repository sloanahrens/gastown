package daemon

import (
	"fmt"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/events"
)

// recordingLogger keeps every formatted line.
type recordingLogger struct{ lines []string }

func (l *recordingLogger) Printf(format string, args ...interface{}) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

// feedCapture records the feed events a dog cycle writes.
type feedCapture struct {
	types    []string
	payloads []map[string]interface{}
	err      error
}

func (f *feedCapture) record(eventType string, payload map[string]interface{}) error {
	f.types = append(f.types, eventType)
	f.payloads = append(f.payloads, payload)
	return f.err
}

func newTestDogCycle(job string) (*dogCycle, *recordingLogger, *feedCapture) {
	logger := &recordingLogger{}
	feed := &feedCapture{}
	return &dogCycle{job: job, logger: logger, feed: feed.record}, logger, feed
}

func TestDogCycle_CleanCycleLogsOneOutcomeAndNoFeedEvent(t *testing.T) {
	t.Parallel()
	cycle, logger, feed := newTestDogCycle("wisp_reaper")

	cycle.closeStep("scan")
	cycle.skipStep("auto-close", "disarmed")
	cycle.closeStep("report")
	cycle.close()

	if len(logger.lines) != 1 {
		t.Fatalf("log lines = %q, want exactly one outcome line", logger.lines)
	}
	line := logger.lines[0]
	for _, want := range []string{"wisp_reaper", "outcome=ran", "scan=done", "auto-close=skipped", "report=done"} {
		if !strings.Contains(line, want) {
			t.Errorf("outcome line %q lacks %q", line, want)
		}
	}
	if len(feed.types) != 0 {
		t.Errorf("a clean cycle wrote feed events %v; a skipped step is not a failure", feed.types)
	}
}

func TestDogCycle_FailedStepIsReportedToTheFeed(t *testing.T) {
	t.Parallel()
	cycle, logger, feed := newTestDogCycle("jsonl_git_backup")

	cycle.closeStep("export")
	cycle.failStep("push", "remote rejected")
	cycle.failStep("report", "no summary")
	cycle.close()

	if len(logger.lines) != 1 || !strings.Contains(logger.lines[0], "outcome=failed") {
		t.Fatalf("log lines = %q, want one outcome=failed line", logger.lines)
	}
	if len(feed.types) != 1 || feed.types[0] != events.TypeDogCycleOutcome {
		t.Fatalf("feed events = %v, want one %s", feed.types, events.TypeDogCycleOutcome)
	}
	got := feed.payloads[0]
	if got["job"] != "jsonl_git_backup" || got["outcome"] != string(dogCycleFailed) {
		t.Errorf("payload = %v, want job jsonl_git_backup outcome failed", got)
	}
	if got["reason"] != "push: remote rejected; report: no summary" {
		t.Errorf("reason = %q, want every failed step with its reason", got["reason"])
	}
}

func TestDogCycle_FeedFailureIsLogged(t *testing.T) {
	t.Parallel()
	cycle, logger, feed := newTestDogCycle("doctor_dog")
	feed.err = fmt.Errorf("disk full")

	cycle.failStep("inspect", "latency 9s")
	cycle.close()

	if len(logger.lines) != 2 || !strings.Contains(logger.lines[1], "disk full") {
		t.Errorf("log lines = %q, want the outcome line then the feed error", logger.lines)
	}
}

func TestStartDogCycle_UsesTheDaemonFeedSeam(t *testing.T) {
	t.Parallel()
	feed := &feedCapture{}
	d := &Daemon{config: &Config{}, logger: log.New(io.Discard, "", 0), dogFeedFn: feed.record}

	cycle := d.startDogCycle("doctor_dog")
	cycle.failStep("inspect", "orphans 12")
	cycle.close()

	if len(feed.payloads) != 1 || feed.payloads[0]["job"] != "doctor_dog" {
		t.Errorf("feed payloads = %v, want one doctor_dog event through dogFeedFn", feed.payloads)
	}
}

func TestReportDoltWarnings_IsAFailedDoctorCycle(t *testing.T) {
	t.Parallel()
	feed := &feedCapture{}
	d := &Daemon{config: &Config{}, logger: log.New(io.Discard, "", 0), dogFeedFn: feed.record}

	d.reportDoltWarnings([]string{"latency 3s", "connections 90%"})

	if len(feed.payloads) != 1 {
		t.Fatalf("feed payloads = %v, want one", feed.payloads)
	}
	if got := feed.payloads[0]["reason"]; got != "dolt-health: latency 3s; connections 90%" {
		t.Errorf("reason = %q", got)
	}
}
