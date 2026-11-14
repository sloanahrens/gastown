package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/omclass"
	"github.com/steveyegge/gastown/internal/land"
)

func TestJevOMClassPatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "jev_omclass") {
		t.Error("jev_omclass must be off when there is no patrol config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "jev_omclass") {
		t.Error("jev_omclass must be off when daemon.json does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevOMClass: &JevOMClassConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "jev_omclass") {
		t.Error("an explicit enabled=false must keep jev_omclass off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevOMClass: &JevOMClassConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "jev_omclass") {
		t.Error("an explicit enabled=true must turn jev_omclass on")
	}
}

// With the patrol off the trigger runs no pass: no last-run record, no guard
// taken, and no log line.
func TestJevOMClassTriggerRunsNothingWhenOff(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		clock:  clockwork.NewFakeClockAt(time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)),
	}

	d.triggerJevOMClass()

	if d.jevOMClassRunning.Load() {
		t.Error("the single-flight guard was taken with the patrol off")
	}
	if _, found, _ := loadPatrolLastRun(town, "jev_omclass"); found {
		t.Error("a last-run record was written with the patrol off")
	}
	if logs.Len() != 0 {
		t.Errorf("the trigger logged with the patrol off: %s", logs)
	}
}

// An enabled patrol with no price refuses to run and says why, before it
// reaches Jev or the review log.
func TestJevOMClassRunNeedsAPrice(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevOMClass: &JevOMClassConfig{Enabled: true}},
		},
	}

	d.runJevOMClass()

	if !strings.Contains(logs.String(), "jev_omclass: error:") ||
		!strings.Contains(logs.String(), "price per billion tokens") {
		t.Errorf("log = %q, want the missing price named", logs)
	}
}

// omclassAsker answers every finding with choice and counts the calls.
type omclassAsker struct {
	choice string
	calls  int
}

func (a *omclassAsker) Ask(context.Context, any, map[string]jev.Question) (*jev.Answers, error) {
	a.calls++
	return &jev.Answers{Model: "jev-test", Choice: map[string]jev.ChoiceAnswer{
		omclass.QRuleClass: {Choice: a.choice, Probabilities: map[string]float64{a.choice: 1}},
	}}, nil
}

// omclassFailingAsker fails every ask with no reply.
type omclassFailingAsker struct{}

func (omclassFailingAsker) Ask(context.Context, any, map[string]jev.Question) (*jev.Answers, error) {
	return nil, errors.New("jev unreachable")
}

// omclassReviews hands the pass one review's findings.
type omclassReviews struct{ recs []land.ReviewRecord }

func (r omclassReviews) Since(time.Time) ([]land.ReviewRecord, error) { return r.recs, nil }

// TestOMClassRunWritesOneLinePerFinding runs the daemon's pass on a fixture
// review with two findings: each gets one logged choice, labeled with the
// keyword class of its finding. The settings the seam receives are the ones
// the patrol config names, so the config reaches the pass.
func TestOMClassRunWritesOneLinePerFinding(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	asker := &omclassAsker{choice: "fail-open"}
	at := time.Date(2026, 11, 14, 11, 0, 0, 0, time.UTC)
	reviews := omclassReviews{recs: []land.ReviewRecord{{
		BeadID: "gt-fixture", Rig: "gastown", Attempt: 1, At: at.Add(time.Minute),
		Verdict: land.VerdictRequestChanges,
		FindingText: []land.ReviewFindingText{
			{File: "a.go", Title: "reads as success on a failed read", Claim: "the error is dropped"},
			{File: "b.go", Title: "the grid shows blue", Claim: "the color is off"},
		},
	}}}
	logPath := omclass.LogPath(town)
	cursorPath := omclass.CursorPath(town)
	if err := battery.WriteCursor(cursorPath, battery.Cursor{Time: at}); err != nil {
		t.Fatal(err)
	}

	var got omclass.Settings
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevOMClass: &JevOMClassConfig{Enabled: true, MaxUSDPerDay: 3, USDPerBillionTokens: 4}},
		},
		jevOMClassOpen: func(_ string, s omclass.Settings, _ io.Writer) (*omclass.Pass, error) {
			got = s
			return &omclass.Pass{
				Asker:      asker,
				Reviews:    reviews,
				LogPath:    logPath,
				CursorPath: cursorPath,
				Allows:     func() (bool, error) { return true, nil },
			}, nil
		},
	}

	d.runJevOMClass()

	if got.MaxUSDPerDay != 3 || got.USDPerBillionTokens != 4 {
		t.Errorf("settings = %+v, want the patrol's cap and price", got)
	}
	var lines []omclass.Line
	if err := battery.ReadRecent(logPath, 10, &lines); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || asker.calls != 2 {
		t.Fatalf("log has %d lines for %d calls, want one line per finding", len(lines), asker.calls)
	}
	if lines[0].Keyword != "fail-open" || lines[1].Keyword != omclass.NoneLabel {
		t.Errorf("keyword labels = %q, %q; want fail-open, none", lines[0].Keyword, lines[1].Keyword)
	}
	if !strings.Contains(logs.String(), "jev_omclass: asked 2 findings") {
		t.Errorf("log = %q, want the pass's count", logs)
	}
}

// A pass whose ask gets no reply stops with that error, and the daemon logs it
// as the pass's error through the logger it injects, not as a skip.
func TestOMClassRunLogsAPassFailure(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	at := time.Date(2026, 11, 14, 11, 0, 0, 0, time.UTC)
	reviews := omclassReviews{recs: []land.ReviewRecord{{
		BeadID: "gt-fixture", Rig: "gastown", Attempt: 1, At: at.Add(time.Minute),
		Verdict:     land.VerdictRequestChanges,
		FindingText: []land.ReviewFindingText{{File: "a.go", Title: "reads as success on a failed read", Claim: "the error is dropped"}},
	}}}
	cursorPath := omclass.CursorPath(town)
	if err := battery.WriteCursor(cursorPath, battery.Cursor{Time: at}); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevOMClass: &JevOMClassConfig{Enabled: true, USDPerBillionTokens: 4}},
		},
		jevOMClassOpen: func(string, omclass.Settings, io.Writer) (*omclass.Pass, error) {
			return &omclass.Pass{
				Asker:      omclassFailingAsker{},
				Reviews:    reviews,
				LogPath:    omclass.LogPath(town),
				CursorPath: cursorPath,
				Allows:     func() (bool, error) { return true, nil },
			}, nil
		},
	}

	d.runJevOMClass()

	want := "jev_omclass: error: asking about gt-fixture#1/0000: jev unreachable"
	if !strings.Contains(logs.String(), want) {
		t.Errorf("log = %q, want the pass failure %q", logs, want)
	}
}

// The pass is single-flight: while the lock is held by another pass the daemon
// logs it as skipped, not as an error.
func TestOMClassRunSkipsWhenAnotherPassHoldsTheLock(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	cursorPath := omclass.CursorPath(town)
	release, err := battery.Lock(cursorPath + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevOMClass: &JevOMClassConfig{Enabled: true, USDPerBillionTokens: 4}},
		},
		jevOMClassOpen: func(string, omclass.Settings, io.Writer) (*omclass.Pass, error) {
			return &omclass.Pass{Asker: &omclassAsker{choice: "none"}, Reviews: omclassReviews{}, LogPath: filepath.Join(town, "log"), CursorPath: cursorPath}, nil
		},
	}

	d.runJevOMClass()

	if !strings.Contains(logs.String(), "jev_omclass: skipped:") {
		t.Errorf("log = %q, want the busy pass skipped", logs)
	}
}

// A first pass says it only set the cursor, and a run that reaches the cap says
// the findings past it wait for the next pass.
func TestOMClassRunLogsTheStartAndTheCap(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 14, 11, 0, 0, 0, time.UTC)
	var findings []land.ReviewFindingText
	for i := 0; i < omclass.MaxFindingsPerRun+1; i++ {
		findings = append(findings, land.ReviewFindingText{Title: fmt.Sprintf("finding %d", i), Claim: "claim"})
	}
	recs := []land.ReviewRecord{{BeadID: "gt-big", Rig: "gastown", Attempt: 1, At: at.Add(time.Minute), FindingText: findings}}
	cases := []struct {
		name      string
		cursorSet bool
		want      string
	}{
		{"first pass names its start", false, "first pass set the cursor"},
		{"a run at the cap names the rest", true, "stopped at 300 findings this run"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if c.cursorSet {
				if err := battery.WriteCursor(omclass.CursorPath(town), battery.Cursor{Time: at}); err != nil {
					t.Fatal(err)
				}
			}
			logs := &bytes.Buffer{}
			d := &Daemon{
				config: &Config{TownRoot: town},
				logger: log.New(logs, "", 0),
				patrolConfig: &DaemonPatrolConfig{
					Patrols: &PatrolsConfig{JevOMClass: &JevOMClassConfig{Enabled: true, USDPerBillionTokens: 4}},
				},
				jevOMClassOpen: func(string, omclass.Settings, io.Writer) (*omclass.Pass, error) {
					return &omclass.Pass{
						Asker:      &omclassAsker{choice: omclass.NoneLabel},
						Reviews:    omclassReviews{recs: recs},
						LogPath:    omclass.LogPath(town),
						CursorPath: omclass.CursorPath(town),
						Allows:     func() (bool, error) { return true, nil },
					}, nil
				},
			}

			d.runJevOMClass()

			if !strings.Contains(logs.String(), c.want) {
				t.Errorf("log = %q, want %q", logs, c.want)
			}
		})
	}
}
