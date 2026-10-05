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
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/promote"
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
	writeJSONFile(t, StateFile(town), State{StartedAt: now.Add(-3 * time.Hour), LastHeartbeat: now.Add(-3 * time.Minute), HeartbeatCount: 12})
	for _, p := range []string{"git_hygiene", "events_prune"} {
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
			"gt:ready-to-land": {
				{ID: "gt-1", Status: "open", Notes: "READY TO LAND\nBranch: polecat/opal/gt-1\nHead: aaaa\nTarget: main\nWorker: opal\nSubmitted: " + now.Add(-time.Minute).Format(time.RFC3339)},
				{ID: "gt-2", Status: "closed"},
			},
			"gt:needs-human": {{ID: "gt-3", Status: "open", CreatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339)}},
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
		townHealthSources: func(s *healthSources) { stubHealthProbes(s, town, now) },
	}
	return d, bd
}

// stubHealthProbes answers the probes the unit tier cannot run: a Dolt ping, a
// program exec, the backup root, and the slot pool. A test that needs a probe
// of its own calls this first, so it replaces one source rather than all five.
func stubHealthProbes(s *healthSources, town string, now time.Time) {
	s.ping = func() (time.Duration, error) { return 4 * time.Millisecond, nil }
	// The real probe writes and execs programs; the unit tier runs no
	// external tool, so a test that computes health answers it.
	s.execTax = func(context.Context) (time.Duration, error) { return 9 * time.Millisecond, nil }
	s.backupRoot = func() (string, error) { return filepath.Join(town, "no-backups"), nil }
	s.slots = func() (slot.Report, error) {
		return slot.Report{Slots: []slot.SlotState{{Index: 0, Held: true, Owner: &slot.Owner{Role: "refinery", AcquiredAt: now.Add(-40 * time.Minute)}}}}, nil
	}
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
		"landing/gastown":           {townhealth.Recorded, townhealth.Green, "1 pending, oldest 1m"},
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

// A rig with no post_land_command never runs a post-landing check, so its
// lack of a verdict is n/a rather than UNKNOWN; configuring the command makes
// the missing verdict an open question again. Both read the rig's own
// settings/config.json, the file the landing worker reads (gt-fn9e6.34).
func TestWriteTownHealth_NoPostLandCommandIsNotApplicable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, _ := healthTown(t, now)
	// The rig has never run a post-landing check: no verdict is recorded.
	writeJSONFile(t, RedMainStatePath(d.config.TownRoot, "gastown"), landworker.MainState{})
	d.writeTownHealth()

	first, err := townhealth.Read(d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	got := healthField(t, first, "main/gastown")
	if got.Tag != townhealth.Recorded || got.Verdict != townhealth.Green || got.Value != "n/a" {
		t.Errorf("main/gastown with no post_land_command = %+v, want a recorded green n/a", got)
	}
	if !strings.Contains(got.Detail, "no post_land_command") {
		t.Errorf("main/gastown detail = %q, want it to name the missing command", got.Detail)
	}

	// The rig configures the check: no verdict is now an unanswered question.
	writeTownFile(t, d.config.TownRoot, filepath.Join("gastown", "settings", "config.json"),
		`{"type":"rig-settings","version":1,"merge_queue":{"post_land_command":"make test-slow"}}`)
	d.writeTownHealth()

	second, err := townhealth.Read(d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got := healthField(t, second, "main/gastown"); got.Verdict != townhealth.VerdictUnknown {
		t.Errorf("main/gastown with a post_land_command and no verdict = %+v, want UNKNOWN", got)
	}
}

// A landing the worker keeps failing and backing off from degrades its rig's
// landing field, which the queue wait alone cannot see, and the field names
// the bead, the stage, the run of failures and the error (gt-fn9e6.44).
func TestWriteTownHealth_FailingLandingDegradesTheLandingField(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, _ := healthTown(t, now)
	backoff, err := landings.BackoffPath(d.config.TownRoot, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	writeJSONFile(t, backoff, landings.BackoffState{
		Rig: "gastown", At: now,
		Beads: []landings.BackoffRecord{{
			Bead: "gt-failing", Stage: "push", Failures: 2,
			NextTry: now.Add(2 * time.Minute), Error: "the pre-push hook refused land/gt-failing",
		}},
	})

	d.writeTownHealth()
	r, err := townhealth.Read(d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	got := healthField(t, r, "landing/gastown")
	if got.Verdict != townhealth.Degraded {
		t.Fatalf("landing/gastown = %+v, want degraded", got)
	}
	if got.Value != "1 pending, oldest 1m, 1 failing" {
		t.Errorf("value = %q, want the failing count beside the queue", got.Value)
	}
	for _, want := range []string{"gt-failing", "2 times in a row", "push", "the pre-push hook refused land/gt-failing"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail %q; want it to name %q", got.Detail, want)
		}
	}
}

// The promote field reads the rig's promotion record out of the main state the
// red-main owner writes, counts the lag in the rig's repository, and stays out
// of a rig that names no promote_target (gt-fn9e6.39).
func TestWriteTownHealth_PromotionField(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	d, _ := healthTown(t, now)
	d.townHealthSources = func(s *healthSources) {
		stubHealthProbes(s, d.config.TownRoot, now)
		// The count is git's; the unit tier answers it.
		s.behind = func(rigName, promoted, green string) (int, error) {
			if rigName != "gastown" || promoted != "aaaa" || green != "bbbb" {
				t.Errorf("behind(%q, %q, %q), want the rig and the two commits of its record", rigName, promoted, green)
			}
			return 3, nil
		}
	}
	d.writeTownHealth()

	first, err := townhealth.Read(d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range first.Fields {
		if f.Name == townhealth.FieldPromote {
			t.Fatalf("a rig with no promote_target grew a promote field: %+v", f)
		}
	}

	// The rig cuts over: its config names the target, and its promotion
	// record carries the last promotion, the green candidate, and the verdict
	// time the candidate's wait is judged by.
	writeDaemonRigConfigFile(t, filepath.Join(d.config.TownRoot, "gastown"), `{"type":"rig","version":1,"name":"gastown","default_branch":"main",
		"merge_queue":{"forgejo":{"promote_target":"git@github.com:acme/gastown.git"}}}`)
	writeJSONFile(t, RedMainStatePath(d.config.TownRoot, "gastown"), landworker.MainState{
		LastGreen: "bbbb", LastRun: "bbbb", LastGreenAt: now.Add(-time.Hour),
		State: promote.State{LastPromoted: "aaaa", LastPromotedAt: now.Add(-2 * time.Hour)},
	})
	d.writeTownHealth()

	second, err := townhealth.Read(d.config.TownRoot)
	if err != nil {
		t.Fatal(err)
	}
	got := healthField(t, second, "promote/gastown")
	if got.Verdict != townhealth.Green || got.Tag != townhealth.Recorded || got.Value != "3 behind" {
		t.Errorf("promote/gastown = %+v, want the recorded 3 commits of lag", got)
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

// The dispatch field reads the daemon's own records — the spec_dispatch
// patrol switch, the operator hold file, and the tick decisions the ticker
// recorded — so an operator learns from gt status --line that nothing is
// filling the free seats (gt-xiw7o).
func TestWriteTownHealth_DispatchField(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T, d *Daemon)
		tag     townhealth.Tag
		verdict townhealth.Verdict
		value   string
		line    string
	}{
		{
			"spec_dispatch turned off",
			func(t *testing.T, d *Daemon) {
				d.patrolConfig.Patrols.SpecDispatch = &SpecDispatchConfig{Enabled: false}
			},
			townhealth.Recorded, townhealth.Red, "off", "dispatch=off[R]",
		},
		{
			"operator hold",
			func(t *testing.T, d *Daemon) {
				if err := os.WriteFile(dispatch.HoldFilePath(d.config.TownRoot), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			townhealth.Recorded, townhealth.Green, "held", "",
		},
		{
			"stalled with work waiting and a free seat",
			func(t *testing.T, d *Daemon) {
				d.recordDispatchTick(tickReport(t, `{"roster": "claude-sonnet 1/2", "candidates": 3, "dispatched": null}`), now.Add(-2*time.Minute))
			},
			townhealth.Recorded, townhealth.Red, "stalled", "dispatch=stalled[R]",
		},
		{
			"no candidates",
			func(t *testing.T, d *Daemon) {
				d.recordDispatchTick(tickReport(t, `{"roster": "claude-sonnet 0/2", "candidates": 0, "dispatched": null}`), now.Add(-2*time.Minute))
			},
			townhealth.Recorded, townhealth.Green, "idle", "",
		},
		{
			"every seat at its cap",
			func(t *testing.T, d *Daemon) {
				d.recordDispatchTick(tickReport(t, `{"roster": "claude-sonnet 2/2", "candidates": 3, "dispatched": null}`), now.Add(-2*time.Minute))
			},
			townhealth.Recorded, townhealth.Green, "full", "",
		},
		{
			"a dispatch in the window",
			func(t *testing.T, d *Daemon) {
				d.recordDispatchTick(tickReport(t, `{"roster": "claude-sonnet 1/2", "candidates": 3, "dispatched": [{"line": "gt-a"}]}`), now.Add(-2*time.Minute))
			},
			townhealth.Recorded, townhealth.Green, "ok", "",
		},
		{
			"a tick that refused its candidates is not a stall",
			func(t *testing.T, d *Daemon) {
				d.recordDispatchTick(tickReport(t, `{"roster": "claude-sonnet 1/2", "candidates": 3, "dispatched": null, "refused": [{"line": "gt-a"}]}`), now.Add(-2*time.Minute))
			},
			townhealth.Recorded, townhealth.Green, "ok", "",
		},
		{
			"a daemon that has not ticked since it started is silent",
			nil,
			townhealth.Recorded, townhealth.Red, "silent", "dispatch=silent[R]",
		},
		{
			"a roster the daemon cannot read is unknown, never a full town",
			func(t *testing.T, d *Daemon) {
				d.recordDispatchTick(tickReport(t, `{"roster": "claude-sonnet x/2", "candidates": 3, "dispatched": null}`), now.Add(-2*time.Minute))
			},
			townhealth.Unknown, townhealth.VerdictUnknown, "?", "dispatch[?]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := healthTown(t, now)
			if tc.setup != nil {
				tc.setup(t, d)
			}
			d.writeTownHealth()

			r, err := townhealth.Read(d.config.TownRoot)
			if err != nil {
				t.Fatal(err)
			}
			wantTag := tc.tag
			if wantTag == "" {
				wantTag = townhealth.Recorded
			}
			got := healthField(t, r, "dispatch")
			if got.Verdict != tc.verdict || got.Value != tc.value || got.Tag != wantTag {
				t.Errorf("dispatch = %+v, want %s %q %s", got, tc.verdict, tc.value, wantTag)
			}
			line := townhealth.Line(r, now, townhealth.DefaultStaleAfter)
			if tc.line == "" {
				if strings.Contains(line, "dispatch") {
					t.Errorf("line = %q, want no dispatch token for a working dispatcher", line)
				}
				return
			}
			if !strings.Contains(line, tc.line) {
				t.Errorf("line = %q, want it to carry %q", line, tc.line)
			}
		})
	}
}

// tickReport parses one tick's JSON the way the ticker's output is read.
func tickReport(t *testing.T, json string) specDispatchTickReport {
	t.Helper()
	r, err := parseSpecDispatchReport([]byte(json))
	if err != nil {
		t.Fatalf("parse %s: %v", json, err)
	}
	return r
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
