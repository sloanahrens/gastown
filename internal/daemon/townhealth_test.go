package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// labelBeads answers a List by its label filter and records the env of
// every client opened.
type labelBeads struct {
	byLabel map[string][]*beads.Issue
	failFor string
	opened  int
}

func (b *labelBeads) open([]string) workBeadReader { b.opened++; return b }
func (b *labelBeads) Show(id string) (*beads.Issue, error) {
	return nil, errors.New("not used")
}
func (b *labelBeads) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	if opts.Label == b.failFor {
		return nil, errors.New("bd: timed out")
	}
	return b.byLabel[opts.Label], nil
}

func writeJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// healthTown is a town with one landing rig and every record the health
// report reads, at now.
func healthTown(t *testing.T, now time.Time) (*Daemon, *labelBeads) {
	t.Helper()
	town := t.TempDir()
	writeJSONFile(t, filepath.Join(town, "mayor", "rigs.json"), map[string]any{"version": 1, "rigs": map[string]any{"gastown": map[string]any{}}})
	writeJSONFile(t, StateFile(town), State{LastHeartbeat: now.Add(-3 * time.Minute), HeartbeatCount: 12})
	for _, p := range []string{"mayor_dispatch", "git_hygiene", "events_prune"} {
		if err := savePatrolLastRun(town, p, now.Add(-50*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	lpath, _ := landings.Path(town, "gastown")
	if err := os.MkdirAll(filepath.Dir(lpath), 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, at := range []time.Time{now.Add(-30 * time.Hour), now.Add(-5 * time.Hour), now.Add(-time.Hour)} {
		b, _ := json.Marshal(landings.Record{Bead: "gt-x", Rig: "gastown", LandedAt: at})
		lines = append(lines, string(b))
	}
	if err := os.WriteFile(lpath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, RedMainStatePath(town, "gastown"), landworker.MainState{LastGreen: "aaaa", LastRun: "bbbb"})
	seat := func(rig, role, name string, rec intent.Record) {
		writeJSONFile(t, intent.Seat{Rig: rig, Role: role, Name: name}.Path(town), rec)
	}
	seat("gastown", "polecat", "opal", intent.Record{Progress: &intent.Progress{SampledAt: now.Add(-time.Minute), ChangedAt: now.Add(-45 * time.Minute)}})
	seat("gastown", "polecat", "gone", intent.Record{Progress: &intent.Progress{SampledAt: now.Add(-48 * time.Hour)}})
	seat("", "mayor", "", intent.Record{Frozen: true})

	bd := &labelBeads{byLabel: map[string][]*beads.Issue{
		"gt:ready-to-land": {{ID: "gt-1", Status: "open"}, {ID: "gt-2", Status: "closed"}},
		"gt:needs-human":   {{ID: "gt-3", Status: "open", CreatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339)}},
		"gt:escalation":    {{ID: "hq-9", Status: "open", CreatedAt: now.Add(-90 * time.Minute).Format(time.RFC3339)}},
	}}
	d := &Daemon{
		config:        &Config{TownRoot: town},
		logger:        log.New(io.Discard, "", 0),
		clock:         clockwork.NewFakeClockAt(now),
		openWorkBeads: bd.open,
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			LandingWorker: &LandingWorkerConfig{Enabled: true},
		}},
		townHealthSources: func(s *healthSources) {
			s.ping = func() (time.Duration, error) { return 4 * time.Millisecond, nil }
			// The real probe writes and execs programs; the unit tier runs
			// no external tool, so a test that computes health answers it.
			s.execTax = func(context.Context) (time.Duration, error) { return 9 * time.Millisecond, nil }
			s.backupRoot = func() (string, error) { return filepath.Join(town, "no-backups"), nil }
			s.slots = func() (slot.Report, error) {
				return slot.Report{Slots: []slot.SlotState{{Index: 0, Held: true, Owner: &slot.Owner{Role: "refinery", AcquiredAt: now.Add(-40 * time.Minute)}}}}, nil
			}
		},
	}
	return d, bd
}

func healthField(t *testing.T, r townhealth.Report, key string) townhealth.Field {
	t.Helper()
	for _, f := range r.Fields {
		if f.Key() == key {
			return f
		}
	}
	t.Fatalf("no field %s in %+v", key, r.Fields)
	return townhealth.Field{}
}

func TestWriteTownHealth_WritesTheReportFromTheTownsRecords(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, _ := healthTown(t, now)

	d.writeTownHealth()

	r, err := townhealth.Read(d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !r.At.Equal(now) || r.HeartbeatCount != 12 {
		t.Errorf("At %v count %d, want the tick time and the state's count", r.At, r.HeartbeatCount)
	}
	if r.Landed == nil || *r.Landed != 2 {
		t.Errorf("Landed = %v, want the 2 landings of the last 24h", r.Landed)
	}
	for key, want := range map[string]struct {
		tag     townhealth.Tag
		verdict townhealth.Verdict
		value   string
	}{
		"dolt":                      {townhealth.Live, townhealth.Green, "p50 4ms"},
		"daemon":                    {townhealth.Recorded, townhealth.Green, "heartbeat 3m ago"},
		"tick:mayor_dispatch":       {townhealth.Recorded, townhealth.Green, "50m/30m"},
		"landing/gastown":           {townhealth.Recorded, townhealth.Green, "1 pending, last 1h ago"},
		"main/gastown":              {townhealth.Recorded, townhealth.Red, "red"},
		"escalation":                {townhealth.Live, townhealth.Degraded, "oldest 1h"},
		"needs-human":               {townhealth.Live, townhealth.Degraded, "1"},
		"slot":                      {townhealth.Recorded, townhealth.Degraded, "refinery 40m"},
		"backup":                    {townhealth.Recorded, townhealth.Red, "none"},
		"seat/gastown:polecat/opal": {townhealth.Recorded, townhealth.Degraded, "stalled 44m"},
		"seat:mayor":                {townhealth.Recorded, townhealth.Red, "frozen"},
	} {
		got := healthField(t, r, key)
		if got.Tag != want.tag || got.Verdict != want.verdict || got.Value != want.value {
			t.Errorf("%s = %+v, want %s %s %q", key, got, want.tag, want.verdict, want.value)
		}
	}
	for _, f := range r.Fields {
		if strings.Contains(f.Subject, "gone") {
			t.Errorf("a seat the daemon no longer samples was judged: %+v", f)
		}
	}
	if got := healthField(t, r, "config"); got.Verdict != townhealth.Red || got.Tag != townhealth.Live {
		t.Errorf("config of a town with no town.json = %+v, want LIVE red", got)
	}
	if r.Verdict != townhealth.Red {
		for _, f := range r.Fields {
			t.Logf("%s %s %s %q", f.Key(), f.Tag, f.Verdict, f.Detail)
		}
		t.Errorf("verdict = %s, want red", r.Verdict)
	}
}

// The exec-tax line the daemon logs is its field's state changing, and a
// beat that reports the same state says nothing (gt-2ycne.1).
func TestWriteTownHealth_LogsTheExecTaxOnlyWhenItChanges(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, _ := healthTown(t, now)
	var logs bytes.Buffer
	d.logger = log.New(&logs, "", 0)
	median := 9 * time.Millisecond
	d.townHealthSources = func(s *healthSources) {
		s.ping = func() (time.Duration, error) { return 4 * time.Millisecond, nil }
		s.execTax = func(context.Context) (time.Duration, error) { return median, nil }
	}

	// Every beat logs the health line itself; only the exec-tax line is
	// conditional, so the test reads that one out of the log.
	taxLines := func() []string {
		var out []string
		for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			if strings.Contains(l, "exec-tax") {
				out = append(out, l)
			}
		}
		return out
	}
	d.writeTownHealth()
	if got := taxLines(); len(got) != 0 {
		t.Errorf("first beat logged %q, want nothing: there is no baseline to change from", got)
	}
	d.writeTownHealth()
	if got := taxLines(); len(got) != 0 {
		t.Errorf("an unchanged beat logged %q, want nothing (one line per change, not per beat)", got)
	}
	median = 180 * time.Millisecond
	d.writeTownHealth()
	if got := logs.String(); !strings.Contains(got, "180ms") || !strings.Contains(got, "gt-2ycne.1") {
		t.Errorf("the transition to taxed logged %q, want the median and the bead", got)
	}
	r, err := townhealth.Read(d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got := healthField(t, r, "exec-tax"); got.Verdict != townhealth.Red || r.ExecTaxMS == nil || *r.ExecTaxMS != 180 {
		t.Errorf("exec-tax = %+v with ExecTaxMS %v, want RED and 180", got, r.ExecTaxMS)
	}

	logs.Reset()
	median = 9 * time.Millisecond
	d.writeTownHealth()
	if got := logs.String(); !strings.Contains(got, "clear") {
		t.Errorf("the transition back to clear logged %q, want the clear line", got)
	}
}

// The second report checks the heartbeat count against the first: the
// daemon field becomes LIVE once there is a baseline.
func TestWriteTownHealth_HeartbeatAdvanceIsLiveAfterTheFirstTick(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, _ := healthTown(t, now)
	d.writeTownHealth()
	writeJSONFile(t, StateFile(d.config.TownRoot), State{LastHeartbeat: now, HeartbeatCount: 13})
	d.clock.(*clockwork.FakeClock).Advance(3 * time.Minute)
	d.writeTownHealth()

	r, err := townhealth.Read(d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got := healthField(t, r, "daemon"); got.Tag != townhealth.Live || got.Verdict != townhealth.Green {
		t.Errorf("daemon = %+v, want LIVE green", got)
	}
}

func TestWriteTownHealth_FailedReadsAreUnknown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, bd := healthTown(t, now)
	bd.failFor = "gt:ready-to-land"
	if err := os.WriteFile(filepath.Join(d.config.TownRoot, ".runtime", "agents", "gastown", "polecat.opal.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	d.townHealthSources = func(s *healthSources) {
		s.ping = func() (time.Duration, error) { return 0, errors.New("connection refused") }
		s.execTax = func(context.Context) (time.Duration, error) { return 9 * time.Millisecond, nil }
		s.backupRoot = func() (string, error) { return "", errors.New("no home") }
		s.slots = func() (slot.Report, error) { return slot.Report{}, nil }
	}

	d.writeTownHealth()

	r, err := townhealth.Read(d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got := healthField(t, r, "dolt"); got.Value != "unreachable" || got.Verdict != townhealth.Red {
		t.Errorf("dolt = %+v, want red unreachable", got)
	}
	for _, key := range []string{"landing/gastown", "backup", "seat"} {
		if got := healthField(t, r, key); got.Tag != townhealth.Unknown {
			t.Errorf("%s = %+v, want UNKNOWN", key, got)
		}
	}
	if r.Landed != nil {
		t.Errorf("Landed = %d with a rig unread, want nil", *r.Landed)
	}
}
