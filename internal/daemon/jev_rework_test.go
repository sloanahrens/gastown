package daemon

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/rework"
	"github.com/steveyegge/gastown/internal/land"
)

func TestJevReworkPatrolIsOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "jev_rework") {
		t.Error("jev_rework must be off when there is no patrol config")
	}
	if IsPatrolEnabled(&DaemonPatrolConfig{Patrols: &PatrolsConfig{}}, "jev_rework") {
		t.Error("jev_rework must be off when daemon.json does not mention it")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevRework: &JevReworkConfig{Enabled: false}}}
	if IsPatrolEnabled(off, "jev_rework") {
		t.Error("an explicit enabled=false must keep jev_rework off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevRework: &JevReworkConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "jev_rework") {
		t.Error("an explicit enabled=true must turn jev_rework on")
	}
}

// With the patrol off, the trigger runs no pass: it makes no last-run record,
// so a later enable is due at once, and it logs nothing.
func TestJevReworkTriggerRunsNothingWhenOff(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(logs, "", 0),
		clock:  clockwork.NewFakeClockAt(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)),
	}

	d.triggerJevRework()

	if d.jevReworkRunning.Load() {
		t.Error("the single-flight guard was taken with the patrol off")
	}
	if _, found, _ := loadPatrolLastRun(town, "jev_rework"); found {
		t.Error("a last-run record was written with the patrol off")
	}
	if logs.Len() != 0 {
		t.Errorf("the trigger logged with the patrol off: %s", logs)
	}
}

// An enabled patrol with no price refuses to run and says why, without
// reaching Jev, git or the rig's beads.
func TestJevReworkRunNeedsAPrice(t *testing.T) {
	t.Parallel()
	logs := &bytes.Buffer{}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logs, "", 0),
		patrolConfig: &DaemonPatrolConfig{
			Patrols: &PatrolsConfig{JevRework: &JevReworkConfig{Enabled: true}},
		},
	}

	d.runJevRework()

	if !strings.Contains(logs.String(), "jev_rework: error:") ||
		!strings.Contains(logs.String(), "price per billion tokens") {
		t.Errorf("log = %q, want the missing price named", logs)
	}
}

func TestJevReworkConfigDecodesFromDaemonJSON(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(town, constants.DirMayor, "daemon.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"daemon-patrol-config","version":1,"patrols":{"jev_rework":{"enabled":true,"max_usd_per_day":2.5,"usd_per_billion_tokens":3}}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := ReadPatrolConfig(town)
	if err != nil {
		t.Fatalf("ReadPatrolConfig: %v", err)
	}
	c := cfg.Patrols.JevRework
	if c == nil || !c.Enabled || c.MaxUSDPerDay != 2.5 || c.USDPerBillionTokens != 3 {
		t.Errorf("jev_rework = %+v, want enabled with a 2.5 cap at 3 USD per billion tokens", c)
	}
}

// stubReworkRigs serves one refused bead's notes and a placeholder diff: the
// daemon test is about when a pass runs and what it logs, not about the state
// it builds.
type stubReworkRigs struct {
	notes map[string]string
}

func (s stubReworkRigs) Notes(_, bead string) (string, error) {
	notes, ok := s.notes[bead]
	if !ok {
		return "", nil
	}
	return notes, nil
}

func (stubReworkRigs) Numstat(context.Context, string, string, string) (string, error) {
	return "1\t0\tinternal/x.go\n", nil
}

// countingReworkAsker answers every Ask and counts them.
type countingReworkAsker struct{ n int }

func (c *countingReworkAsker) Ask(context.Context, any, map[string]jev.Question) (*jev.Answers, error) {
	c.n++
	return &jev.Answers{
		Model: "jev-1.13.0",
		Noul:  map[string]float64{rework.QReworkedToLand: 0.7},
		Choice: map[string]jev.ChoiceAnswer{
			rework.QCause: {Choice: "tests_missing", Probabilities: map[string]float64{"tests_missing": 0.7}},
		},
	}, nil
}

// The failed review of gt-abc, and the notes its refusal left there.
const (
	reworkFailedHead = "abc123def456"
	reworkRefusedAt  = "2026-10-20T11:00:00Z"
)

func reworkRefusalNotes(t *testing.T) string {
	t.Helper()
	return land.FormatRejectionNote(land.RejectionNote{
		Attempt: 1, Kind: string(land.RejectReview), Reason: "om requested changes (score 0.60, 1 finding(s))",
		Branch: "polecat/angharad/gt-abc", Target: "main", MR: "gt-abc", Head: reworkFailedHead,
		Findings: []land.Finding{{ID: "f1", Severity: "major", Path: "internal/x.go", Line: 1, Title: "no test"}},
		Receipt:  &land.Receipt{Score: 0.6},
	})
}

// jevReworkRig is an enabled daemon over a temp town whose passes open through
// a fake, so no pass reaches Jev, git or the rig's beads.
type jevReworkRig struct {
	d     *Daemon
	town  string
	logs  *bytes.Buffer
	pass  *rework.Pass
	asker *countingReworkAsker
}

func newJevReworkRig(t *testing.T) *jevReworkRig {
	t.Helper()
	town := t.TempDir()
	r := &jevReworkRig{town: town, logs: &bytes.Buffer{}, asker: &countingReworkAsker{}}
	clock := clockwork.NewFakeClockAt(time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	r.d = &Daemon{
		config: &Config{TownRoot: town},
		logger: log.New(r.logs, "", 0),
		clock:  clock,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{JevRework: &JevReworkConfig{
			Enabled: true, MaxUSDPerDay: 2.5, USDPerBillionTokens: 3,
		}}},
	}
	dayCap := battery.Cap{Ledger: jev.UsageLedgerPath(town), MaxUSDPerDay: 2.5, USDPerBillionTokens: 3}
	refusedAt, err := time.Parse(time.RFC3339, reworkRefusedAt)
	if err != nil {
		t.Fatal(err)
	}
	r.pass = &rework.Pass{
		Asker: r.asker,
		Reviews: stubReworkReviews{recs: []land.ReviewRecord{{
			BeadID: "gt-abc", Rig: "gastown", Attempt: 1, Score: 0.6,
			Verdict: land.VerdictRequestChanges, Rejected: true, Findings: 1, At: refusedAt,
		}}},
		Rigs:       stubReworkRigs{notes: map[string]string{"gt-abc": reworkRefusalNotes(t)}},
		LogPath:    rework.LogPath(town),
		CursorPath: rework.CursorPath(town),
		Allows:     func() (bool, error) { return dayCap.Allows(clock.Now) },
		Now:        func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) },
	}
	r.d.jevReworkOpen = func(string, rework.Settings, io.Writer) (*rework.Pass, error) {
		return r.pass, nil
	}
	// The cursor starts before the refusal, so the first run has it pending.
	if err := battery.WriteCursor(r.pass.CursorPath, battery.Cursor{
		Time: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	return r
}

// stubReworkReviews lists the review records a test hands the pass.
type stubReworkReviews struct{ recs []land.ReviewRecord }

func (s stubReworkReviews) Since(time.Time) ([]land.ReviewRecord, error) { return s.recs, nil }

// An enabled patrol asks about each refused review once: the second run finds
// the cursor past it and bills nothing.
func TestJevReworkRunAsksEachRefusalOnce(t *testing.T) {
	t.Parallel()
	r := newJevReworkRig(t)

	r.d.runJevRework()
	r.d.runJevRework()

	if r.asker.n != 1 {
		t.Errorf("made %d Jev calls over two runs, want 1", r.asker.n)
	}
	if !strings.Contains(r.logs.String(), "jev_rework: asked 1 refusals") {
		t.Errorf("log = %q, want the first run's one refusal named", r.logs)
	}
	lines, err := rework.ReadRecent(r.pass.LogPath, 5)
	if err != nil {
		t.Fatalf("ReadRecent: %v", err)
	}
	if len(lines) != 1 || lines[0].Bead != "gt-abc" || lines[0].Head != reworkFailedHead {
		t.Errorf("log lines = %+v, want one for gt-abc at %s", lines, reworkFailedHead)
	}
}
