package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/land"
)

// landWaitEpoch is the instant every fixture clock starts at.
var landWaitEpoch = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

// landWaitErrTestRead stands in for a failed read of the log or the bead.
var landWaitErrTestRead = landWaitTestError("the read failed")

type landWaitTestError string

func (e landWaitTestError) Error() string { return string(e) }

// landWaitClock is a clock whose sleep advances it, so a test drives the wait
// to its timeout without a real sleep.
type landWaitClock struct {
	t     time.Time
	slept []time.Duration
}

func newLandWaitClock() *landWaitClock { return &landWaitClock{t: landWaitEpoch} }

func (c *landWaitClock) now() time.Time { return c.t }

func (c *landWaitClock) sleep(d time.Duration) {
	c.slept = append(c.slept, d)
	c.t = c.t.Add(d)
}

// landWaitFixtureDetail answers from fixture notes through the production note
// reader, so a poll test exercises the attribution it asserts.
func landWaitFixtureDetail(notes map[string]string) func(landWaitEvent) (landWaitOutcome, bool, error) {
	return func(ev landWaitEvent) (landWaitOutcome, bool, error) {
		text, ok := notes[ev.Bead]
		if !ok {
			return landWaitOutcome{}, false, nil
		}
		outcome, found := landWaitNoteOutcome(text, ev)
		return outcome, found, nil
	}
}

// landWaitFixtureSource answers the same events on every poll, as the daemon log
// does.
func landWaitFixtureSource(events ...landWaitEvent) func(time.Time) ([]landWaitEvent, error) {
	return func(time.Time) ([]landWaitEvent, error) { return events, nil }
}

// landWaitRun drives one wait with a fake clock, source and detail, and returns
// what it printed.
func landWaitRun(t *testing.T, clock *landWaitClock, deps landWaitDeps) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	deps.now, deps.sleep = clock.now, clock.sleep
	deps.out, deps.errOut = &out, &errOut
	_, err = landWaitPoll(deps)
	return out.String(), errOut.String(), err
}

func TestParseLandWaitLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		text string
		want landWaitEvent
		ok   bool
	}{
		{
			name: "a landing",
			text: "landing_worker: [land] gt-abc: landed 1a2b3c4d on origin/main (patch-id 9f9f9f9f)",
			want: landWaitEvent{Bead: "gt-abc", Landed: true, Commit: "1a2b3c4d"},
			ok:   true,
		},
		{
			name: "a review refusal",
			text: "landing_worker: [land] gt-abc: rejected (review): om requested changes (score 0.55, 3 finding(s))",
			want: landWaitEvent{Bead: "gt-abc", Kind: "review"},
			ok:   true,
		},
		{
			name: "a gate refusal",
			text: "landing_worker: [land] gt-abc: rejected (gate): the candidate gate make gate failed on the merged tree",
			want: landWaitEvent{Bead: "gt-abc", Kind: "gate"},
			ok:   true,
		},
		{
			name: "a merge is not an outcome",
			text: "landing_worker: [land] gt-abc: merged 77aa77aa onto origin/main (55bb55bb) as 66cc66cc; reviewing the merge candidate",
		},
		{
			name: "a stage line is not an outcome",
			text: "landing_worker: [land] gt-abc: stages: lint 4s, test 210s, om 12s",
		},
		{
			name: "the worker's duplicate refusal line",
			text: "landing_worker: gastown: gt-abc: landing rejected (review): om requested changes (score 0.55, 3 finding(s))",
		},
		{
			name: "the worker's own landed line",
			text: "landing_worker: gastown: gt-abc: landed 1a2b3c4d on main (patch-id 9f9f9f9f)",
		},
		{
			name: "another component's line",
			text: "gt mail inbox: 2 messages",
		},
		{
			name: "a landed line with no commit",
			text: "landing_worker: [land] gt-abc: landed on origin/main",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseLandWaitLine(tc.text)
			if ok != tc.ok {
				t.Fatalf("parseLandWaitLine(%q) ok = %v, want %v", tc.text, ok, tc.ok)
			}
			if got != tc.want {
				t.Errorf("parseLandWaitLine(%q) = %+v, want %+v", tc.text, got, tc.want)
			}
		})
	}
}

// landWaitRejectionNotes is a bead's notes carrying a refusal block the landing
// worker writes, in the writer's own format. A nil score is a refusal om never
// scored, as a gate refusal on a waived review is.
func landWaitRejectionNotes(attempt int, kind, branch string, findings int, score *float64) string {
	found := make([]land.Finding, findings)
	for i := range found {
		found[i] = land.Finding{ID: "f" + string(rune('1'+i)), Severity: "major", Path: "a.go", Line: i + 1, Title: "a finding"}
	}
	note := land.RejectionNote{
		Attempt: attempt, Kind: kind, Reason: "the work was refused", Branch: branch,
		Target: "main", MR: "gt-a", Findings: found,
	}
	if score != nil {
		note.Receipt = &land.Receipt{Score: *score}
	}
	return land.FormatRejectionNote(note)
}

func landWaitScore(v float64) *float64 { return &v }

func TestLandWaitNoteOutcome(t *testing.T) {
	t.Parallel()
	landed := land.LandingRecord{BeadID: "gt-a", Rig: "gastown", Branch: "crew/dunlin/gt-a-x",
		Head: "1a2b3c4d5e6f", Target: "main", LandedCommit: "1a2b3c4d5e6f", LandedAt: landWaitEpoch}.NoteBlock()
	for _, tc := range []struct {
		name  string
		notes string
		ev    landWaitEvent
		want  landWaitOutcome
		ok    bool
	}{
		{
			name:  "a landing reads its commit and branch",
			notes: landed,
			ev:    landWaitEvent{Bead: "gt-a", Landed: true, Commit: "1a2b3c4d"},
			want:  landWaitOutcome{Bead: "gt-a", Branch: "crew/dunlin/gt-a-x", Landed: true, Commit: "1a2b3c4d5e6f", Target: "main"},
			ok:    true,
		},
		{
			name:  "a landing block for another commit is no answer",
			notes: landed,
			ev:    landWaitEvent{Bead: "gt-a", Landed: true, Commit: "ffffffff"},
		},
		{
			name:  "a refusal reads its kind, score, findings and round",
			notes: landWaitRejectionNotes(2, "review", "crew/dunlin/gt-a-x", 3, landWaitScore(0.42)),
			ev:    landWaitEvent{Bead: "gt-a", Kind: "review"},
			want:  landWaitOutcome{Bead: "gt-a", Branch: "crew/dunlin/gt-a-x", Kind: "review", Score: 0.42, HasScore: true, Findings: 3, Attempt: 2},
			ok:    true,
		},
		{
			name:  "a refusal of another kind is no answer",
			notes: landWaitRejectionNotes(1, "gate", "crew/dunlin/gt-a-x", 0, nil),
			ev:    landWaitEvent{Bead: "gt-a", Kind: "review"},
		},
		{
			name:  "a bead with no record",
			notes: "READY TO LAND\nsubmitted by: crew/dunlin",
			ev:    landWaitEvent{Bead: "gt-a", Landed: true, Commit: "1a2b3c4d"},
		},
		{
			name:  "a gate refusal with no om score",
			notes: landWaitRejectionNotes(1, "gate", "crew/dunlin/gt-a-x", 0, nil),
			ev:    landWaitEvent{Bead: "gt-a", Kind: "gate"},
			want:  landWaitOutcome{Bead: "gt-a", Branch: "crew/dunlin/gt-a-x", Kind: "gate", Findings: 0, Attempt: 1},
			ok:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := landWaitNoteOutcome(tc.notes, tc.ev)
			if ok != tc.ok {
				t.Fatalf("landWaitNoteOutcome() ok = %v, want %v", ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Errorf("landWaitNoteOutcome() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLandWaitPrintsOwnOutcomes(t *testing.T) {
	t.Parallel()
	// Another agent's landing shares the poll and must print nothing.
	notes := map[string]string{
		"gt-landed": land.LandingRecord{BeadID: "gt-landed", Branch: "crew/dunlin/gt-landed-x",
			Target: "main", LandedCommit: "1a2b3c4d5e6f", LandedAt: landWaitEpoch}.NoteBlock(),
		"gt-refused": landWaitRejectionNotes(2, "review", "crew/dunlin/gt-refused-x", 3, landWaitScore(0.42)),
		"gt-other": land.LandingRecord{BeadID: "gt-other", Branch: "crew/egret/gt-other-x",
			Target: "main", LandedCommit: "aaaa1111bbbb", LandedAt: landWaitEpoch}.NoteBlock(),
	}
	events := []landWaitEvent{
		{At: landWaitEpoch.Add(time.Second), Bead: "gt-landed", Landed: true, Commit: "1a2b3c4d"},
		{At: landWaitEpoch.Add(2 * time.Second), Bead: "gt-refused", Kind: "review"},
		{At: landWaitEpoch.Add(3 * time.Second), Bead: "gt-other", Landed: true, Commit: "aaaa1111"},
	}
	stdout, stderr, err := landWaitRun(t, newLandWaitClock(), landWaitDeps{
		agent: "dunlin", timeout: time.Minute, interval: time.Second,
		source: landWaitFixtureSource(events...), detail: landWaitFixtureDetail(notes),
	})
	if err != nil {
		t.Fatalf("landWaitPoll() error = %v", err)
	}
	want := "landed gt-landed as 1a2b3c4d5e6f on main (crew/dunlin/gt-landed-x)\n" +
		"rejected gt-refused (review) on crew/dunlin/gt-refused-x: score 0.42, 3 finding(s), refusal 2 of 3; run `gt land verdict gt-refused` for the findings\n"
	if stdout != want {
		t.Errorf("printed:\n%s\nwant:\n%s", stdout, want)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want nothing", stderr)
	}
}

// A refusal at the rework ceiling ends the loop: the bird is told the overseer
// has the bead and not to push (gt-hmxex).
func TestLandWaitThirdRefusalNamesTheOverseer(t *testing.T) {
	t.Parallel()
	notes := map[string]string{"gt-a": landWaitRejectionNotes(land.MaxReworkAttempts, "review", "crew/dunlin/gt-a-x", 1, landWaitScore(0.2))}
	stdout, _, err := landWaitRun(t, newLandWaitClock(), landWaitDeps{
		agent: "dunlin", timeout: time.Minute, interval: time.Second,
		source: landWaitFixtureSource(landWaitEvent{At: landWaitEpoch, Bead: "gt-a", Kind: "review"}),
		detail: landWaitFixtureDetail(notes),
	})
	if err != nil {
		t.Fatalf("landWaitPoll() error = %v", err)
	}
	for _, want := range []string{"refusal 3 of 3", "the overseer has this bead now", "do not push it again"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("line %q does not say %q", stdout, want)
		}
	}
}

// The landing worker logs the landing before it writes the bead's record, so a
// poll can read the line early. The wait retries, and reports the line it could
// never attribute rather than waiting out the timeout silently.
func TestLandWaitReportsAnOutcomeWithNoRecord(t *testing.T) {
	t.Parallel()
	clock := newLandWaitClock()
	stdout, stderr, err := landWaitRun(t, clock, landWaitDeps{
		agent: "dunlin", timeout: 5 * time.Minute, interval: time.Second,
		source: landWaitFixtureSource(landWaitEvent{At: landWaitEpoch, Bead: "gt-a", Landed: true, Commit: "1a2b3c4d"}),
		detail: landWaitFixtureDetail(nil),
	})
	if !isLandWaitNoOutcome(err) {
		t.Fatalf("landWaitPoll() error = %v, want the run-again answer", err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want nothing", stdout)
	}
	if !strings.Contains(stderr, "skipped the outcome of gt-a (landed): the bead's notes carry no matching record") {
		t.Errorf("stderr = %q, want the unattributed line named", stderr)
	}
	if got, want := len(clock.slept), 5*60; got != want {
		t.Errorf("slept %d times, want %d: one line costs one retry, not a poll of its own", got, want)
	}
}

// An outcome that is nobody's own branch prints nothing and, on its own, is no
// reason to stop: the wait still ends in the run-again answer.
func TestLandWaitIgnoresOtherBranches(t *testing.T) {
	t.Parallel()
	notes := map[string]string{"gt-a": land.LandingRecord{BeadID: "gt-a", Branch: "crew/egret/gt-a-x",
		Target: "main", LandedCommit: "1a2b3c4d5e6f", LandedAt: landWaitEpoch}.NoteBlock()}
	stdout, stderr, err := landWaitRun(t, newLandWaitClock(), landWaitDeps{
		agent: "dunlin", timeout: 3 * time.Second, interval: time.Second,
		source: landWaitFixtureSource(landWaitEvent{At: landWaitEpoch, Bead: "gt-a", Landed: true, Commit: "1a2b3c4d"}),
		detail: landWaitFixtureDetail(notes),
	})
	if !isLandWaitNoOutcome(err) {
		t.Fatalf("landWaitPoll() error = %v, want the run-again answer", err)
	}
	if stdout != "" || stderr != "" {
		t.Errorf("printed stdout %q stderr %q, want nothing", stdout, stderr)
	}
}

func TestLandWaitTimesOutWithTheRunAgainLine(t *testing.T) {
	t.Parallel()
	clock := newLandWaitClock()
	timeout := 30 * time.Second
	var out bytes.Buffer
	_, err := landWaitPoll(landWaitDeps{
		agent: "dunlin", timeout: timeout, interval: 5 * time.Second,
		now: clock.now, sleep: clock.sleep, out: &out, errOut: &out,
		source: landWaitFixtureSource(), detail: landWaitFixtureDetail(nil),
	})
	if got, want := len(clock.slept), int(timeout/(5*time.Second)); got != want {
		t.Errorf("slept %d times, want %d: the wait polls once per interval and no faster", got, want)
	}
	if !isLandWaitNoOutcome(err) {
		t.Fatalf("landWaitPoll() error = %v, want the run-again answer", err)
	}
	if err := landWaitAnswer(&out, err); err == nil {
		t.Fatal("landWaitAnswer() = nil, want the run-again exit")
	} else if code, ok := IsSilentExit(err); !ok || code != 3 {
		t.Errorf("landWaitAnswer() error = %v (silent %v), want exit 3", err, ok)
	}
	if !strings.Contains(out.String(), "no outcome yet, run again") {
		t.Errorf("output = %q, want the run-again line", out.String())
	}
}

// The run-again answer is the exit code, so cobra must not print it as a
// failure; an error that is not the answer is still the command's to report.
func TestLandWaitFinish(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	idle := &cobra.Command{}
	idle.SetOut(&out)
	if err := landWaitFinish(idle, errLandWaitNoOutcome); err == nil {
		t.Fatal("landWaitFinish() = nil, want exit 3")
	} else if code, ok := IsSilentExit(err); !ok || code != 3 {
		t.Errorf("landWaitFinish() error = %v (silent %v), want exit 3", err, ok)
	}
	if !idle.SilenceUsage || !idle.SilenceErrors {
		t.Error("landWaitFinish() left cobra to print the silent exit as an error")
	}
	if !strings.Contains(out.String(), "no outcome yet, run again") {
		t.Errorf("output = %q, want the run-again line", out.String())
	}

	var failed bytes.Buffer
	loud := &cobra.Command{}
	loud.SetOut(&failed)
	if err := landWaitFinish(loud, landWaitErrTestRead); err != landWaitErrTestRead {
		t.Errorf("landWaitFinish() = %v, want the read's own error", err)
	}
	if loud.SilenceUsage || loud.SilenceErrors {
		t.Error("landWaitFinish() silenced a failure the bird has to read")
	}
	if failed.Len() != 0 {
		t.Errorf("output = %q, want the error left to cobra", failed.String())
	}
}

func TestLandWaitRefusesBadInputs(t *testing.T) {
	t.Parallel()
	failing := landWaitFixtureSource(landWaitEvent{Bead: "gt-a", Kind: "review"})
	failingDetail := func(landWaitEvent) (landWaitOutcome, bool, error) {
		return landWaitOutcome{}, false, landWaitErrTestRead
	}
	failingSource := func(time.Time) ([]landWaitEvent, error) { return nil, landWaitErrTestRead }
	for _, tc := range []struct {
		name string
		deps landWaitDeps
		want string
	}{
		{
			name: "no poll interval",
			deps: landWaitDeps{timeout: time.Minute, source: landWaitFixtureSource(), detail: landWaitFixtureDetail(nil)},
			want: "poll interval must be positive",
		},
		{
			name: "no timeout",
			deps: landWaitDeps{timeout: 0, interval: time.Second, source: landWaitFixtureSource(), detail: landWaitFixtureDetail(nil)},
			want: "wait must be longer than none",
		},
		{
			name: "an unreadable log",
			deps: landWaitDeps{timeout: time.Minute, interval: time.Second, source: failingSource, detail: landWaitFixtureDetail(nil)},
			want: "the read failed",
		},
		{
			name: "an unreadable bead",
			deps: landWaitDeps{timeout: time.Minute, interval: time.Second, source: failing, detail: failingDetail},
			want: "the read failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := landWaitRun(t, newLandWaitClock(), tc.deps)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("landWaitPoll() error = %v, want it to name %q", err, tc.want)
			}
			if isLandWaitNoOutcome(err) {
				t.Errorf("a failed read came back as the run-again answer")
			}
		})
	}
}

func TestLandWaitAgentFrom(t *testing.T) {
	t.Parallel()
	env := func(gtRole string) func(string) string {
		return func(key string) string {
			if key == "GT_ROLE" {
				return gtRole
			}
			return ""
		}
	}
	if got, err := landWaitAgentFrom("dunlin", env("")); err != nil || got != "dunlin" {
		t.Errorf("flag agent = %q, %v; want dunlin", got, err)
	}
	if got, err := landWaitAgentFrom("", env("gastown/crew/dunlin")); err != nil || got != "dunlin" {
		t.Errorf("GT_ROLE agent = %q, %v; want dunlin", got, err)
	}
	for _, tc := range []struct{ name, flag, role string }{
		{name: "no role", flag: "", role: ""},
		{name: "a polecat", flag: "", role: "gastown/polecats/coma"},
		{name: "a daemon job", flag: "", role: "daemon/spec-dispatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := landWaitAgentFrom(tc.flag, env(tc.role)); err == nil {
				t.Errorf("landWaitAgentFrom(%q, %q) = nil error, want a refusal", tc.flag, tc.role)
			}
		})
	}
}

func TestLandWaitParseTimeout(t *testing.T) {
	t.Parallel()
	if got, err := landWaitParseTimeout("9m"); err != nil || got != 9*time.Minute {
		t.Errorf("9m = %v, %v; want 9m", got, err)
	}
	for _, bad := range []string{"", "soon", "0s", "-1m"} {
		if _, err := landWaitParseTimeout(bad); err == nil {
			t.Errorf("landWaitParseTimeout(%q) = nil error, want a refusal", bad)
		}
	}
}

func TestLandWaitOurs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		branch string
		want   bool
	}{
		{branch: "crew/dunlin", want: true},
		{branch: "crew/dunlin/gt-a-x", want: true},
		{branch: "crew/dunlin2/gt-a-x", want: false},
		{branch: "crew/egret/gt-a-x", want: false},
		{branch: "polecat/dunlin/gt-a+mvy", want: false},
		{branch: "main", want: false},
	} {
		t.Run(tc.branch, func(t *testing.T) {
			t.Parallel()
			if got := landWaitOurs("dunlin", tc.branch); got != tc.want {
				t.Errorf("landWaitOurs(dunlin, %q) = %v, want %v", tc.branch, got, tc.want)
			}
		})
	}
}

// The command is discoverable where a bird looks for it: gt land --help lists it
// with an example to copy (gt-hmxex).
func TestLandHelpListsWait(t *testing.T) {
	t.Parallel()
	if c := lookupCommand(rootCmd, []string{"land", "wait"}); c == nil {
		t.Fatal("gt land has no wait subcommand")
	}
	for _, want := range []string{"  wait", "gt land wait"} {
		if !strings.Contains(landCmd.Long, want) {
			t.Errorf("gt land --help does not carry %q:\n%s", want, landCmd.Long)
		}
	}
}

func TestLandWaitLogSourceReadsSinceTheStart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	body := strings.Join([]string{
		"2026/11/01 08:59:59 landing_worker: [land] gt-old: landed aaaa1111 on origin/main (patch-id bbbb2222)",
		"2026/11/01 09:00:01 landing_worker: [land] gt-new: rejected (gate): make gate failed",
		"2026/11/01 09:00:02 unrelated: line",
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "daemon.log"), []byte(body), 0o600); err != nil {
		t.Fatalf("writing the log: %v", err)
	}
	events, err := landWaitLogSource(dir)(landWaitEpoch)
	if err != nil {
		t.Fatalf("landWaitLogSource() error = %v", err)
	}
	if len(events) != 1 || events[0].Bead != "gt-new" || events[0].Kind != "gate" {
		t.Errorf("events = %+v, want only the gate refusal after the start", events)
	}
	if want := time.Date(2026, 11, 1, 9, 0, 1, 0, time.Local); !events[0].At.Equal(want) {
		t.Errorf("event at %s, want the line's stamp %s", events[0].At, want)
	}
}

// isLandWaitNoOutcome reports whether err is the wait's own no-outcome answer.
func isLandWaitNoOutcome(err error) bool {
	return err != nil && err.Error() == errLandWaitNoOutcome.Error()
}
