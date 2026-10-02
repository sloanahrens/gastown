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
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/slot"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// beadRead is one List call: the label asked for and the options it carried,
// so a test can check which bead plane the daemon read.
type beadRead struct {
	label string
	opts  beads.ListOptions
}

// labelBeads answers a List the way bd does, out of the plane the options
// name: Ephemeral reads the wisps table alone, IncludeInfra reads wisps and
// issues together, and a bare read sees the issues table.
type labelBeads struct {
	issuesByLabel map[string][]*beads.Issue
	wispsByLabel  map[string][]*beads.Issue
	failFor       string
	opened        int
	reads         []beadRead
}

func (b *labelBeads) open([]string) workBeadReader { b.opened++; return b }
func (b *labelBeads) Show(id string) (*beads.Issue, error) {
	return nil, errors.New("not used")
}
func (b *labelBeads) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	b.reads = append(b.reads, beadRead{label: opts.Label, opts: opts})
	if opts.Label == b.failFor {
		return nil, errors.New("bd: timed out")
	}
	switch {
	case opts.Ephemeral:
		return b.wispsByLabel[opts.Label], nil
	case opts.IncludeInfra:
		both := append([]*beads.Issue{}, b.issuesByLabel[opts.Label]...)
		return append(both, b.wispsByLabel[opts.Label]...), nil
	default:
		return b.issuesByLabel[opts.Label], nil
	}
}

// read returns the List call that asked for label. A label read once is the
// common case; the last call wins if a test reads it twice.
func (b *labelBeads) read(t *testing.T, label string) beadRead {
	t.Helper()
	var found *beadRead
	for i := range b.reads {
		if b.reads[i].label == label {
			found = &b.reads[i]
		}
	}
	if found == nil {
		t.Fatalf("no read of %s among %+v", label, b.reads)
	}
	return *found
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
	// A polecat's record names a seat only while its directory is there.
	seat := func(rig, role, name string, rec intent.Record) {
		writeJSONFile(t, intent.Seat{Rig: rig, Role: role, Name: name}.Path(town), rec)
		if role == constants.RolePolecat {
			if err := os.MkdirAll(filepath.Join(town, rig, "polecats", name), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	seat("gastown", "polecat", "opal", intent.Record{Progress: &intent.Progress{SampledAt: now.Add(-time.Minute), ChangedAt: now.Add(-45 * time.Minute)}})
	seat("gastown", "polecat", "gone", intent.Record{Progress: &intent.Progress{SampledAt: now.Add(-48 * time.Hour)}})
	seat("", "mayor", "", intent.Record{Frozen: true})

	bd := &labelBeads{
		issuesByLabel: map[string][]*beads.Issue{
			"gt:ready-to-land": {{ID: "gt-1", Status: "open"}, {ID: "gt-2", Status: "closed"}},
			"gt:needs-human":   {{ID: "gt-3", Status: "open", CreatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339)}},
		},
		wispsByLabel: map[string][]*beads.Issue{
			// Escalations are filed as ephemeral wisps.
			"gt:escalation": {{ID: "hq-9", Status: "open", Labels: []string{"gt:escalation"}, CreatedAt: now.Add(-90 * time.Minute).Format(time.RFC3339)}},
		},
	}
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

// A parked decision is not a pending one: deferring gt-v4ssj.7 held needs-human
// at 1 and hid the beads that arrived after it (gt-tk2xd).
func TestWriteTownHealth_NeedsHumanCountsOnlyPendingBeads(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	at := now.Add(-2 * time.Hour).Format(time.RFC3339)
	bead := func(id, status string) *beads.Issue {
		return &beads.Issue{ID: id, Status: status, CreatedAt: at}
	}
	for _, tc := range []struct {
		name    string
		beads   []*beads.Issue
		want    string
		verdict townhealth.Verdict
	}{
		{"an open bead beside a deferred one", []*beads.Issue{bead("gt-3", "open"), bead("gt-4", "deferred")}, "1", townhealth.Degraded},
		{"only parked beads", []*beads.Issue{bead("gt-4", "deferred"), bead("gt-5", "pinned")}, "0", townhealth.Green},
		{"a blocked bead still waits on an unblocker", []*beads.Issue{bead("gt-6", "blocked")}, "1", townhealth.Degraded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, bd := healthTown(t, now)
			bd.issuesByLabel["gt:needs-human"] = tc.beads
			d.writeTownHealth()

			r, err := townhealth.Read(d.config.TownRoot)
			if err != nil {
				t.Fatal(err)
			}
			got := healthField(t, r, "needs-human")
			if got.Value != tc.want || got.Verdict != tc.verdict {
				t.Errorf("needs-human = %s %q, want %s %q", got.Verdict, got.Value, tc.verdict, tc.want)
			}
		})
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

// TestWriteTownHealth_EscalationReadsTheSetEscalateListShows pins the health
// field to the set `gt escalate list` displays, over the two ways the two
// readers disagreed: a delivery carrier is not an escalation, and an
// escalation outside the wisps table is one (gt-9k2bx).
func TestWriteTownHealth_EscalationReadsTheSetEscalateListShows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, bd := healthTown(t, now)
	// The mail carriers routed for an escalation carry gt:escalation so ack and
	// close can find them, and they stay open long after the escalation is
	// done. This one is two days old; counting it is what made the status line
	// read oldest_2d while `gt escalate list` showed nothing older than a day.
	bd.wispsByLabel["gt:escalation"] = append(bd.wispsByLabel["gt:escalation"], &beads.Issue{
		ID: "hq-wisp-msg", Status: "open", Labels: []string{"gt:escalation", "gt:message"},
		CreatedAt: now.Add(-48 * time.Hour).Format(time.RFC3339),
	})
	// An escalation outside the wisps table, which a wisps-only query misses.
	bd.issuesByLabel["gt:escalation"] = []*beads.Issue{{
		ID: "hq-esc", Status: "open", Labels: []string{"gt:escalation"},
		CreatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339),
	}}

	d.writeTownHealth()

	r, err := townhealth.Read(d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	// hq-esc at 2h, not hq-wisp-msg at 48h (a carrier) and not the 90m wisp
	// alone (the issues-plane escalation counts).
	if got := healthField(t, r, "escalation"); got.Value != "oldest 2h" || got.Verdict != townhealth.Degraded {
		t.Errorf("escalation = %+v, want DEGRADED oldest 2h — the carriers `gt escalate list` filters must not count", got)
	}

	read := bd.read(t, "gt:escalation")
	if read.opts.Ephemeral || !read.opts.IncludeInfra {
		t.Errorf("escalation read used Ephemeral=%v IncludeInfra=%v, want the both-plane read `gt escalate list` runs",
			read.opts.Ephemeral, read.opts.IncludeInfra)
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

// The walk drops the record of a polecat whose directory is gone, so a record
// that outlived its seat cannot report that seat dead forever (gt-u7voe).
func TestHealthSourcesSeatsIgnoreRecordsWhosePolecatIsGone(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	town := t.TempDir()
	sampled := func() *intent.Progress {
		return &intent.Progress{SampledAt: now.Add(-time.Minute), ChangedAt: now.Add(-45 * time.Minute)}
	}
	seat := func(rig, role, name string) string {
		t.Helper()
		p := intent.Seat{Rig: rig, Role: role, Name: name}.Path(town)
		writeJSONFile(t, p, intent.Record{Progress: sampled()})
		return p
	}
	removed := seat("gastown", "polecat", "jasper") // record kept, polecat removed
	seat("gastown", "polecat", "opal")              // record kept, polecat still there
	seat("", "mayor", "")                           // town-level seat, no polecat directory
	seat("gastown", "witness", "")                  // rig seat, no polecat directory
	if err := os.MkdirAll(filepath.Join(town, "gastown", "polecats", "opal"), 0o755); err != nil {
		t.Fatal(err)
	}

	src := &healthSources{d: &Daemon{config: &Config{TownRoot: town}}, evidence: time.Hour, now: now}
	got, err := src.Seats()
	if err != nil {
		t.Fatalf("Seats: %v", err)
	}
	var keys []string
	for _, s := range got {
		keys = append(keys, s.Rig+"/"+s.Name)
	}
	want := []string{"/mayor", "gastown/polecat/opal", "gastown/witness"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("Seats = %v, want %v", keys, want)
	}
	if _, err := os.Stat(removed); err != nil {
		t.Fatalf("Seats deleted the record of a removed seat: %v", err)
	}
}

// A seat the patrol tick retired to stop is where the town wants it, so its
// last sample is not stall evidence and it is not listed — a record stuck at
// desired=run is what made townhealth report an idle polecat dead forever
// (gt-613vw).
func TestHealthSourcesSeatsSkipStoppedSeats(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	town := t.TempDir()
	progress := &intent.Progress{SampledAt: now.Add(-time.Minute), DeadSamples: 796}
	stop := intent.Record{Desired: intent.DesiredStop, Progress: progress}
	run := intent.Record{Progress: progress}
	for _, s := range []struct {
		name string
		rec  intent.Record
	}{{"opal", stop}, {"jasper", run}} {
		writeJSONFile(t, intent.Seat{Rig: "gastown", Role: "polecat", Name: s.name}.Path(town), s.rec)
		if err := os.MkdirAll(filepath.Join(town, "gastown", "polecats", s.name), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	src := &healthSources{d: &Daemon{config: &Config{TownRoot: town}}, evidence: time.Hour, now: now}
	got, err := src.Seats()
	if err != nil {
		t.Fatalf("Seats: %v", err)
	}
	if len(got) != 1 || got[0].Name != "polecat/jasper" {
		t.Fatalf("Seats = %+v, want only gastown/polecat/jasper", got)
	}
}

func TestSeatHomeMapsPolecatDirectoriesOnly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		rig, stem string
		want      string
		known     bool
	}{
		{"gastown", "polecat.opal", "/town/gastown/polecats/opal", true},
		{"gastown", "witness", "", false},
		{"gastown", "refinery", "", false},
		{"gastown", "crew.moss", "", false},
		{"", "polecat.opal", "", false},
	}
	for _, c := range cases {
		got, known := seatHome("/town", c.rig, c.stem)
		if got != c.want || known != c.known {
			t.Errorf("seatHome(%q, %q) = %q, %v; want %q, %v", c.rig, c.stem, got, known, c.want, c.known)
		}
	}
}
