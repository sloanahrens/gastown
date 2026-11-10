package cmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	gtevents "github.com/steveyegge/gastown/internal/events"
)

func TestDashboardClassName(t *testing.T) {
	t.Parallel()
	for c, want := range map[tailClass]string{
		tailClassFailure: "failure", tailClassWarning: "warning", tailClassSuccess: "success",
		tailClassLanding: "landing", tailClassDispatch: "dispatch", tailClassRestart: "restart", tailClassPlain: "plain",
	} {
		if got := dashboardClassName(c); got != want {
			t.Errorf("class %d = %q, want %q", c, got, want)
		}
	}
}

func TestResolveSpendCmdPrefersFlag(t *testing.T) {
	t.Parallel()
	if got := resolveSpendCmd("/bin/echo {}"); len(got) != 2 || got[0] != "/bin/echo" {
		t.Errorf("flag not honored: %v", got)
	}
}

func TestDashboardEntryDropsClockAndKeepsTitle(t *testing.T) {
	t.Parallel()
	v := tailView{Trim: true}
	e := dashboardEntry(v, tailLine{Rig: "gastown", Kind: tailKindDaemon, Text: "landed gt-x on main", Title: "Fix the thing"})
	if e.Text != "landed gt-x on main · Fix the thing" || e.Class != "success" || e.Rig != "gastown" {
		t.Errorf("entry = %+v", e)
	}
}

// dashWhy is the reason the page is sent: the error's first line, every path
// cut to its file name, and held to dashWhyMax runes (gt-6qc3nw.2).
func TestDashWhyNamesTheFirstLineWithNoPath(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"nil error":            {nil, ""},
		"first line only":      {errors.New("bd list failed\n  stack line"), "bd list failed"},
		"path cut to its file": {errors.New("open /sandbox/gt/town/.beads/routes.jsonl: no such file"), "open routes.jsonl: no such file"},
		"held to the limit":    {errors.New(strings.Repeat("é", dashWhyMax+50)), strings.Repeat("é", dashWhyMax)},
	} {
		if got := dashWhy(c.err); got != c.want {
			t.Errorf("%s: dashWhy = %q, want %q", name, got, c.want)
		}
	}
}

// A failed read's reason is logged once per change: the same failure on every
// poll is one line, a different failure logs again, and a read that comes back
// logs again. The questions reader logs through the same log (gt-6qc3nw.2).
func TestDashboardWhyLoggedOncePerChange(t *testing.T) {
	t.Parallel()
	var lines []string
	why := newDashWhyLog(func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) })
	town := t.TempDir()
	poll := func(err error) {
		var s dashboard.Summary
		dashboardPolecatsWith(dashPolecatReaders{
			kept: func(string) (map[string]time.Time, error) { return nil, nil },
			seats: func(_ string, _ []string, _ bool, _ *dashSeatCache, _ *gtevents.DispatchReader, _ sessionLister, _ map[string]time.Time) ([]dashSeat, error) {
				return nil, err
			},
		}, &s, town, nil, nil, true, nil, newDashLandings(town), newDashSeatCache(), gtevents.NewDispatchReader(town), fakeSessionLister{})
		why.summary(&s)
	}
	breaker := errors.New("dolt circuit breaker is open: server appears down, failing fast")
	for range 3 {
		poll(breaker)
	}
	if len(lines) != 1 {
		t.Fatalf("three polls of one failure logged %d lines %q, want 1", len(lines), lines)
	}
	poll(errors.New("bd list: connection refused"))
	if len(lines) != 2 {
		t.Fatalf("a different failure logged %d lines total %q, want 2", len(lines), lines)
	}
	poll(nil)
	if len(lines) != 3 {
		t.Fatalf("a read that came back logged %d lines total %q, want 3", len(lines), lines)
	}

	questions := &dashQuestionsReader{
		list: func() ([]*beads.Issue, error) { return nil, breaker },
		why:  why,
	}
	for range 3 {
		questions.read(questionsNow)
	}
	if len(lines) != 4 || !strings.Contains(lines[3], "questions") {
		t.Errorf("three questions polls of one failure logged %q, want one questions line after the seats lines", lines)
	}
}
