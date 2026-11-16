package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/stretchr/testify/require"
)

// imagesRetryCase is one state and images outcome the due rule is asked about.
type imagesRetryCase struct {
	name         string
	lastGreenSHA string
	imagesFailed bool
	retries      int    // recorded against retrySHA
	retrySHA     string // the tip the recorded retries belong to
	sinceLastRun time.Duration
	wantDue      bool
}

func TestTierSweepImagesRetryDue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	cases := []imagesRetryCase{
		{
			name: "failed with a green tip and no retries yet is due", lastGreenSHA: "green0",
			imagesFailed: true, sinceLastRun: tierSweepImagesRetryGap, wantDue: true,
		},
		{
			name: "a green images run is not due however old the sweep", lastGreenSHA: "green0",
			imagesFailed: false, sinceLastRun: time.Hour, wantDue: false,
		},
		{
			name: "no green tip recorded is not due", lastGreenSHA: "",
			imagesFailed: true, sinceLastRun: time.Hour, wantDue: false,
		},
		{
			name: "two retries for this tip leave one more", lastGreenSHA: "green0",
			imagesFailed: true, retries: 2, retrySHA: "green0", sinceLastRun: tierSweepImagesRetryGap, wantDue: true,
		},
		{
			name: "the max retries for this tip stops the loop", lastGreenSHA: "green0",
			imagesFailed: true, retries: tierSweepImagesRetryMax, retrySHA: "green0", sinceLastRun: time.Hour, wantDue: false,
		},
		{
			name: "retries recorded for another green sha do not count", lastGreenSHA: "green0",
			imagesFailed: true, retries: tierSweepImagesRetryMax, retrySHA: "older1", sinceLastRun: tierSweepImagesRetryGap, wantDue: true,
		},
		{
			name: "just short of the gap waits", lastGreenSHA: "green0",
			imagesFailed: true, sinceLastRun: tierSweepImagesRetryGap - time.Second, wantDue: false,
		},
		{
			name: "a retry inside the gap waits however few are recorded", lastGreenSHA: "green0",
			imagesFailed: true, retries: 1, retrySHA: "green0", sinceLastRun: 5 * time.Minute, wantDue: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			st := tierSweepState{
				LastRun:        now.Add(-c.sinceLastRun),
				LastGreenSHA:   c.lastGreenSHA,
				ImagesRetries:  c.retries,
				ImagesRetrySHA: c.retrySHA,
			}
			if got := tierSweepImagesRetryDue(st, now, c.imagesFailed); got != c.wantDue {
				t.Fatalf("tierSweepImagesRetryDue(%+v, imagesFailed=%v) = %v, want %v", st, c.imagesFailed, got, c.wantDue)
			}
		})
	}
}

// A record written before the retry fields existed has neither key, so it must
// read as zero retries and let the rule retry its failed images run.
func TestTierSweepImagesRetryLegacyStateReadsZero(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := tierSweepStatePath(townRoot, "gastown")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"last_run":"2026-11-16T11:30:00Z","last_sha":"green0","last_green_sha":"green0"}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := readTierSweepState(townRoot, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if st.ImagesRetries != 0 || st.ImagesRetrySHA != "" {
		t.Fatalf("legacy record read retries=%d for %q, want zero for empty", st.ImagesRetries, st.ImagesRetrySHA)
	}
	if !tierSweepImagesRetryDue(st, st.LastRun.Add(tierSweepImagesRetryGap), true) {
		t.Fatal("a legacy record with a failed images run must read as zero retries and be due")
	}
}

// The default answer is the pair check's failed-run map, keyed by the commit's
// 12-character stamp: the green commit the check read a failed images run for
// answers true, and every other rig, commit or incomplete sha answers false.
func TestTierSweepImagesRetryAnswersThePairChecksFailedRun(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	// A failed build leaves no images behind, so the check reads the run.
	fx.reg = &fakePairRegistry{}
	fx.setImagesRun(pairGreen, deployRunFailed)
	fx.run(pairT0)
	require.NotEmpty(t, fx.check.failed, "the pair check held no failed run to answer from")

	d := &Daemon{}
	d.pairReady.failed = fx.check.failed

	cases := []struct {
		name string
		rig  string
		sha  string
		want bool
	}{
		{name: "the failed run's rig and commit", rig: "gastown", sha: pairGreen, want: true},
		{name: "the same rig at another commit", rig: "gastown", sha: pairOther},
		{name: "another rig at the same commit", rig: "devops", sha: pairGreen},
		{name: "a sha too short to carry a stamp", rig: "gastown", sha: pairGreen[:8]},
		{name: "no sha at all", rig: "gastown", sha: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := d.tierSweepImagesFailedFor(c.rig, c.sha); got != c.want {
				t.Fatalf("tierSweepImagesFailedFor(%q, %q) = %v, want %v", c.rig, c.sha, got, c.want)
			}
		})
	}
}

// Before the pair check has read, it holds no failed run, so the seam answers
// false: an absent answer is not failed.
func TestTierSweepImagesRetryAnswersFalseBeforeThePairCheckRuns(t *testing.T) {
	t.Parallel()
	d := &Daemon{}
	for _, sha := range []string{pairGreen, pairGreen[:12], ""} {
		if d.tierSweepImagesFailedFor("gastown", sha) {
			t.Fatalf("a daemon whose pair check never ran answered failed for %q", sha)
		}
	}
}

// A test that sets the seam steers the answer: the pair check, which holds a
// failed run here, is not consulted.
func TestTierSweepImagesRetryAnswersThroughItsOverride(t *testing.T) {
	t.Parallel()
	d := &Daemon{}
	d.pairReady.failed = map[string]pairReadyFailedRun{
		imagesFailedKey("gastown", pairGreen12): {},
	}
	var gotRig, gotSHA string
	d.tierSweepSeams.imagesFailedFor = func(rig, sha string) bool {
		gotRig, gotSHA = rig, sha
		return false
	}
	if d.tierSweepImagesFailedFor("gastown", pairGreen) {
		t.Fatal("the pair check's failed run answered over the seam that said not failed")
	}
	if gotRig != "gastown" || gotSHA != pairGreen {
		t.Fatalf("the seam was called with (%q, %q), want (%q, %q)", gotRig, gotSHA, "gastown", pairGreen)
	}
}

// The two fields survive the record's own write and read.
func TestTierSweepImagesRetryStateRoundTrip(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	want := tierSweepState{
		LastRun:        time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC),
		LastGreenSHA:   "green0",
		ImagesRetries:  2,
		ImagesRetrySHA: "green0",
	}
	if err := writeTierSweepState(townRoot, "gastown", want); err != nil {
		t.Fatal(err)
	}
	got, err := readTierSweepState(townRoot, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if got.ImagesRetries != want.ImagesRetries || got.ImagesRetrySHA != want.ImagesRetrySHA {
		t.Fatalf("round trip = %d retries for %q, want %d for %q",
			got.ImagesRetries, got.ImagesRetrySHA, want.ImagesRetries, want.ImagesRetrySHA)
	}
}

// imagesRetryListing is one record and images answer the early candidate list
// is asked about. The green tip is long enough to carry the pair check's stamp.
type imagesRetryListing struct {
	name         string
	lastSHA      string
	lastGreenSHA string
	sinceLastRun time.Duration
	retries      int
	retrySHA     string
	imagesFailed bool
	mainSHA      string
	mainErr      error
	noRepo       bool
	wantListed   bool
}

const (
	// imagesRetryGreen is a green tip whose first 12 characters key the pair
	// check's failed-run map.
	imagesRetryGreen = "green0abcdef"
	imagesRetryTIP   = "newtip0abcde"
)

func TestTierSweepImagesRetryCandidates(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	cases := []imagesRetryListing{
		{
			name:    "a failed images build at the green tip is listed once main moved and the gap passed",
			lastSHA: imagesRetryGreen, lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
			imagesFailed: true, mainSHA: imagesRetryTIP, wantListed: true,
		},
		{
			name:    "a green images answer is not listed however far main moved",
			lastSHA: imagesRetryGreen, lastGreenSHA: imagesRetryGreen, sinceLastRun: time.Hour,
			mainSHA: imagesRetryTIP, wantListed: false,
		},
		{
			name:    "origin/main at the last green sha is not listed",
			lastSHA: imagesRetryGreen, lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
			imagesFailed: true, mainSHA: imagesRetryGreen, wantListed: false,
		},
		{
			name:    "an unreadable origin/main is listed so the run's own read decides",
			lastSHA: imagesRetryGreen, lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
			imagesFailed: true, mainErr: errors.New("the remote did not answer"), wantListed: true,
		},
		{
			name:    "a retry inside the 15 minute gap is not listed",
			lastSHA: imagesRetryGreen, lastGreenSHA: imagesRetryGreen, sinceLastRun: 5 * time.Minute,
			imagesFailed: true, mainSHA: imagesRetryTIP, wantListed: false,
		},
		{
			name:         "no green tip recorded is not listed",
			sinceLastRun: tierSweepImagesRetryGap,
			imagesFailed: true, mainSHA: imagesRetryTIP, wantListed: false,
		},
		{
			name:    "the max retries for this tip is not listed",
			lastSHA: imagesRetryGreen, lastGreenSHA: imagesRetryGreen, sinceLastRun: time.Hour,
			retries: tierSweepImagesRetryMax, retrySHA: imagesRetryGreen,
			imagesFailed: true, mainSHA: imagesRetryTIP, wantListed: false,
		},
		{
			name:    "retries recorded for another sha do not stop the listing",
			lastSHA: imagesRetryGreen, lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
			retries: tierSweepImagesRetryMax, retrySHA: "older1",
			imagesFailed: true, mainSHA: imagesRetryTIP, wantListed: true,
		},
		{
			name:    "a rig whose bare repo is gone is not listed",
			lastSHA: imagesRetryGreen, lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
			imagesFailed: true, mainSHA: imagesRetryTIP, noRepo: true, wantListed: false,
		},
		{
			name:    "a red sweep is still listed with a green images answer",
			lastSHA: "red1", lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
			mainSHA: imagesRetryTIP, wantListed: true,
		},
		{
			name:    "a red sweep inside its own gap is not listed",
			lastSHA: "red1", lastGreenSHA: imagesRetryGreen, sinceLastRun: 5 * time.Minute,
			imagesFailed: true, mainSHA: imagesRetryTIP, wantListed: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d, _, _, _ := newTierSweepDaemon(t, now, "gastown")
			d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) {
				return c.mainSHA, c.mainErr
			}
			if c.imagesFailed {
				d.pairReady.failed = map[string]pairReadyFailedRun{
					imagesFailedKey("gastown", greenRefShortSHA(c.lastGreenSHA)): {},
				}
			}
			if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
				LastRun: now.Add(-c.sinceLastRun), LastSHA: c.lastSHA, LastGreenSHA: c.lastGreenSHA,
				ImagesRetries: c.retries, ImagesRetrySHA: c.retrySHA,
			}); err != nil {
				t.Fatal(err)
			}
			if c.noRepo {
				if err := os.RemoveAll(filepath.Join(d.config.TownRoot, "gastown", ".repo.git")); err != nil {
					t.Fatal(err)
				}
			}

			listed := slices.Contains(d.tierSweepEarlyCandidates(now), "gastown")
			if listed != c.wantListed {
				t.Fatalf("listed = %v, want %v", listed, c.wantListed)
			}
		})
	}
}

// A red record is settled by the record alone: neither the images answer nor
// origin/main is asked for it, so a red sweep costs the heartbeat no git read.
func TestTierSweepImagesRetryDoesNotAskForARedRecord(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, _, _, _ := newTierSweepDaemon(t, now, "gastown")
	d.tierSweepSeams.imagesFailedFor = func(string, string) bool {
		t.Error("a red record's images answer was asked for")
		return false
	}
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) {
		t.Error("a red record's origin/main was read")
		return "", nil
	}
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
		LastRun: now.Add(-15 * time.Minute), LastSHA: "red1", LastGreenSHA: imagesRetryGreen,
	}); err != nil {
		t.Fatal(err)
	}

	if !slices.Contains(d.tierSweepEarlyCandidates(now), "gastown") {
		t.Fatal("a red record with its gap passed was not listed")
	}
}

// The images answer decides only a record its own conditions already allow: a
// record inside the gap, one with no green tip and one at the retry cap do not
// ask for it.
func TestTierSweepImagesRetryDoesNotAskTheImagesAnswerWhenTheRecordRefuses(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	cases := []struct {
		name string
		st   tierSweepState
	}{
		{name: "inside the gap", st: tierSweepState{LastRun: now.Add(-time.Minute), LastSHA: imagesRetryGreen, LastGreenSHA: imagesRetryGreen}},
		{name: "no green tip", st: tierSweepState{LastRun: now.Add(-time.Hour)}},
		{
			name: "at the retry cap",
			st: tierSweepState{
				LastRun: now.Add(-time.Hour), LastSHA: imagesRetryGreen, LastGreenSHA: imagesRetryGreen,
				ImagesRetries: tierSweepImagesRetryMax, ImagesRetrySHA: imagesRetryGreen,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d, _, _, _ := newTierSweepDaemon(t, now, "gastown")
			d.tierSweepSeams.imagesFailedFor = func(string, string) bool {
				t.Error("the images answer was asked for a record the rule already refuses")
				return false
			}
			if err := writeTierSweepState(d.config.TownRoot, "gastown", c.st); err != nil {
				t.Fatal(err)
			}
			if len(d.tierSweepEarlyCandidates(now)) != 0 {
				t.Fatal("a record the retry rule refuses was listed")
			}
		})
	}
}

// imagesRetryTriggerCase is one trigger scenario: the rig's record, the images
// answer, where origin/main is, the sweep beads still open and how long ago the
// last cycle ran.
type imagesRetryTriggerCase struct {
	name         string
	lastSHA      string
	lastGreenSHA string
	sinceLastRun time.Duration
	imagesFailed bool
	mainSHA      string
	openTitles   []string
	wantListed   bool
	wantRun      bool
}

// setupImagesRetry builds a daemon whose gastown record is as c says and whose
// hourly patrol clock was last set 20 minutes ago, so the interval is not due.
func setupImagesRetry(t *testing.T, c imagesRetryTriggerCase) (*Daemon, *tierSweepRunRecorder, *tierSweepFakeBeads, *bytes.Buffer) {
	t.Helper()
	now := atHour(time.Now(), 15)
	d, rec, bd, logs := newTierSweepDaemon(t, now, "gastown")
	rec.green()
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return c.mainSHA, nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	for _, title := range c.openTitles {
		bd.open = append(bd.open, &beads.Issue{ID: "gt-red", Title: title})
	}
	if err := savePatrolLastRun(d.config.TownRoot, "tier_sweep", now.Add(-20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if c.imagesFailed {
		d.pairReady.failed = map[string]pairReadyFailedRun{
			imagesFailedKey("gastown", greenRefShortSHA(c.lastGreenSHA)): {},
		}
	}
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
		LastRun: now.Add(-c.sinceLastRun), LastSHA: c.lastSHA, LastGreenSHA: c.lastGreenSHA,
	}); err != nil {
		t.Fatal(err)
	}
	return d, rec, bd, logs
}

// tierSweepImagesRetryLine is the line a cycle started by an images failure
// logs. It must not be the red line: no red bead closed for this reason.
const tierSweepImagesRetryLine = "tier_sweep: gastown: images build failed at the green tip and main moved; re-running now"

func TestTierSweepImagesRetryEarlyReRun(t *testing.T) {
	t.Parallel()
	cases := []imagesRetryTriggerCase{
		{
			name:    "main moved after a failed images build re-runs the rig",
			lastSHA: imagesRetryGreen, lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
			imagesFailed: true, mainSHA: imagesRetryTIP, wantListed: true, wantRun: true,
		},
		{
			name:    "a green images answer does not re-run however far main moved",
			lastSHA: imagesRetryGreen, lastGreenSHA: imagesRetryGreen, sinceLastRun: time.Hour,
			mainSHA: imagesRetryTIP, wantListed: false, wantRun: false,
		},
		{
			name:    "origin/main at the green tip does not re-run the rig",
			lastSHA: imagesRetryGreen, lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
			imagesFailed: true, mainSHA: imagesRetryGreen, wantListed: false, wantRun: false,
		},
		{
			name:    "an open red bead holds the retry",
			lastSHA: imagesRetryGreen, lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
			imagesFailed: true, mainSHA: imagesRetryTIP,
			openTitles: []string{tierSweepBeadPrefix("gastown", "shell") + "TestFoo"},
			wantListed: true, wantRun: false,
		},
		{
			name:    "five minutes since the last run waits",
			lastSHA: imagesRetryGreen, lastGreenSHA: imagesRetryGreen, sinceLastRun: 5 * time.Minute,
			imagesFailed: true, mainSHA: imagesRetryTIP, wantListed: false, wantRun: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d, rec, _, logs := setupImagesRetry(t, c)
			if listed := slices.Contains(d.tierSweepEarlyCandidates(d.clk().Now()), "gastown"); listed != c.wantListed {
				t.Fatalf("listed = %v, want %v", listed, c.wantListed)
			}

			d.triggerTierSweep()
			waitTierSweepIdle(t, d)

			ran := len(rec.stages) > 0
			if ran != c.wantRun {
				t.Fatalf("ran = %v, want %v\n%s", ran, c.wantRun, logs.String())
			}
			logged := strings.Contains(logs.String(), tierSweepImagesRetryLine)
			if logged != c.wantRun {
				t.Fatalf("images retry line logged = %v, want %v\n%s", logged, c.wantRun, logs.String())
			}
			if strings.Contains(logs.String(), tierSweepEarlyLine) {
				t.Fatalf("the red sweep line claimed beads closed for an images retry\n%s", logs.String())
			}
			if !c.wantListed && !strings.Contains(logs.String(), "no early re-run candidate") {
				t.Fatalf("a rig that was not listed did not say so\n%s", logs.String())
			}
		})
	}
}

// An early re-run it starts leaves the record green at the tip it swept and
// the patrol's hourly clock where it was, like the red reason's re-run.
func TestTierSweepImagesRetryEarlyReRunRecordsTheCycle(t *testing.T) {
	t.Parallel()
	d, rec, _, _ := setupImagesRetry(t, imagesRetryTriggerCase{
		lastSHA: imagesRetryGreen, lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
		imagesFailed: true, mainSHA: imagesRetryTIP,
	})
	before, _, err := loadPatrolLastRun(d.config.TownRoot, "tier_sweep")
	if err != nil {
		t.Fatal(err)
	}

	d.triggerTierSweep()
	waitTierSweepIdle(t, d)

	if len(rec.stages) == 0 {
		t.Fatal("no stage ran for a failed images build whose tip moved")
	}
	st := tierSweepTestState(t, d, "gastown")
	if st.LastSHA != imagesRetryTIP || st.LastGreenSHA != imagesRetryTIP {
		t.Fatalf("record = %+v, want the retry to land green on %s", st, imagesRetryTIP)
	}
	after, _, err := loadPatrolLastRun(d.config.TownRoot, "tier_sweep")
	if err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Fatalf("hourly last-run moved from %v to %v; an early re-run must not consume the hour", before, after)
	}
}

// imagesRetryClock is the daemon's fake clock, so a test moves the 15 minute
// gap without sleeping.
func imagesRetryClock(t *testing.T, d *Daemon) *clockwork.FakeClock {
	t.Helper()
	clk, ok := d.clock.(*clockwork.FakeClock)
	if !ok {
		t.Fatalf("test clock = %T, want a fake clock to advance", d.clock)
	}
	return clk
}

// imagesRetryTrigger runs the heartbeat with the hourly clock parked twenty
// minutes short of due, so the trigger takes the early path an images retry
// runs on, and waits for the cycle it starts.
func imagesRetryTrigger(t *testing.T, d *Daemon) {
	t.Helper()
	if err := savePatrolLastRun(d.config.TownRoot, "tier_sweep", imagesRetryClock(t, d).Now().Add(-20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	d.triggerTierSweep()
	waitTierSweepIdle(t, d)
}

// imagesRetryRecording is one record and cycle outcome the recording rule is
// asked about: the record as the cycle read it, the green tip the cycle
// recorded, whether it was an early re-run, and the green tips whose images run
// the pair check holds as failed.
type imagesRetryRecording struct {
	name         string
	lastSHA      string
	lastGreenSHA string
	sinceLastRun time.Duration
	retries      int    // recorded against retrySHA
	retrySHA     string // the tip the recorded retries belong to
	greenSHA     string // the green tip the cycle recorded, "" when it recorded none
	early        bool
	imagesFailed []string
	wantRetries  int
	wantSHA      string
}

func TestTierSweepImagesRetryRecorded(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	cases := []imagesRetryRecording{
		{
			name: "an early re-run counts one for the failed sha", lastSHA: imagesRetryGreen,
			lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
			greenSHA: imagesRetryGreen, early: true, imagesFailed: []string{imagesRetryGreen},
			wantRetries: 1, wantSHA: imagesRetryGreen,
		},
		{
			name: "a second early re-run counts two", lastSHA: imagesRetryGreen,
			lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
			retries: 1, retrySHA: imagesRetryGreen,
			greenSHA: imagesRetryGreen, early: true, imagesFailed: []string{imagesRetryGreen},
			wantRetries: 2, wantSHA: imagesRetryGreen,
		},
		{
			name: "a new green sha clears the pair even when its images run failed", lastSHA: imagesRetryGreen,
			lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
			retries: 2, retrySHA: imagesRetryGreen,
			greenSHA: imagesRetryTIP, early: true, imagesFailed: []string{imagesRetryGreen, imagesRetryTIP},
			wantRetries: 0, wantSHA: "",
		},
		{
			name: "the images run no longer failed clears the pair", lastSHA: imagesRetryGreen,
			lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
			retries: 2, retrySHA: imagesRetryGreen,
			greenSHA: imagesRetryGreen, early: true,
			wantRetries: 0, wantSHA: "",
		},
		{
			name: "no green sha records nothing", lastSHA: imagesRetryGreen,
			lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
			retries: 2, retrySHA: imagesRetryGreen, early: true, imagesFailed: []string{imagesRetryGreen},
			wantRetries: 0, wantSHA: "",
		},
		{
			name: "a town-wide cycle keeps the pair", lastSHA: imagesRetryGreen,
			lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
			retries: 2, retrySHA: imagesRetryGreen,
			greenSHA: imagesRetryGreen, imagesFailed: []string{imagesRetryGreen},
			wantRetries: 2, wantSHA: imagesRetryGreen,
		},
		{
			name: "an early re-run inside the gap keeps the pair", lastSHA: "red1",
			lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap - time.Minute,
			retries: 2, retrySHA: imagesRetryGreen,
			greenSHA: imagesRetryGreen, early: true, imagesFailed: []string{imagesRetryGreen},
			wantRetries: 2, wantSHA: imagesRetryGreen,
		},
		{
			name: "an early re-run at the cap keeps it", lastSHA: "red1",
			lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
			retries: tierSweepImagesRetryMax, retrySHA: imagesRetryGreen,
			greenSHA: imagesRetryGreen, early: true, imagesFailed: []string{imagesRetryGreen},
			wantRetries: tierSweepImagesRetryMax, wantSHA: imagesRetryGreen,
		},
		{
			name: "an early re-run for a sha the record did not count writes one", lastSHA: "red1",
			lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
			retries: tierSweepImagesRetryMax, retrySHA: "older1",
			greenSHA: imagesRetryGreen, early: true, imagesFailed: []string{imagesRetryGreen},
			wantRetries: 1, wantSHA: imagesRetryGreen,
		},
		{
			name: "a count recorded for another tip stands for nothing", lastSHA: imagesRetryGreen,
			lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
			retries: 2, retrySHA: "older1",
			greenSHA: imagesRetryGreen, imagesFailed: []string{imagesRetryGreen},
			wantRetries: 0, wantSHA: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := &Daemon{}
			d.tierSweepSeams.imagesFailedFor = func(_, sha string) bool {
				return slices.Contains(c.imagesFailed, sha)
			}
			prior := tierSweepState{
				LastRun:        now.Add(-c.sinceLastRun),
				LastSHA:        c.lastSHA,
				LastGreenSHA:   c.lastGreenSHA,
				ImagesRetries:  c.retries,
				ImagesRetrySHA: c.retrySHA,
			}
			retries, sha := d.tierSweepImagesRetryRecorded("gastown", prior, c.greenSHA, c.early, now)
			if retries != c.wantRetries || sha != c.wantSHA {
				t.Fatalf("recorded %d retries for %q, want %d for %q", retries, sha, c.wantRetries, c.wantSHA)
			}
		})
	}
}

// Each early re-run an images failure leaves due counts itself in the record:
// the first writes one retry for the green sha whose images failed, the second
// writes two. The stages read RED, so the tip never advances and the count
// keeps the sha it belongs to.
func TestTierSweepImagesRetryCountsEachEarlyReRun(t *testing.T) {
	t.Parallel()
	d, rec, _, logs := setupImagesRetry(t, imagesRetryTriggerCase{
		lastSHA: imagesRetryGreen, lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
		imagesFailed: true, mainSHA: imagesRetryTIP,
	})
	// The stages judge RED: no verdict line means the cycle never goes green.
	rec.result = nil
	main := imagesRetryTIP
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return main, nil }

	imagesRetryTrigger(t, d)
	if !strings.Contains(logs.String(), tierSweepImagesRetryLine) {
		t.Fatalf("the first re-run was not the images failure's:\n%s", logs.String())
	}
	st := tierSweepTestState(t, d, "gastown")
	if st.LastGreenSHA != imagesRetryGreen {
		t.Fatalf("LastGreenSHA = %q, want the RED re-run to leave %q", st.LastGreenSHA, imagesRetryGreen)
	}
	if st.ImagesRetries != 1 || st.ImagesRetrySHA != imagesRetryGreen {
		t.Fatalf("the first re-run wrote %d retries for %q, want 1 for %q",
			st.ImagesRetries, st.ImagesRetrySHA, imagesRetryGreen)
	}

	// The tip moved again and the gap passed while the images run still stands
	// failed: the next early re-run counts against the same sha.
	imagesRetryClock(t, d).Advance(tierSweepImagesRetryGap)
	main = "secondtip0ab"
	imagesRetryTrigger(t, d)
	st = tierSweepTestState(t, d, "gastown")
	if st.ImagesRetries != 2 || st.ImagesRetrySHA != imagesRetryGreen {
		t.Fatalf("the second re-run wrote %d retries for %q, want 2 for %q",
			st.ImagesRetries, st.ImagesRetrySHA, imagesRetryGreen)
	}
}

// A sweep that produces a new green sha clears the retries: they were spent on
// a failure the old tip owns, and no images run builds for a tip a newer green
// has passed.
func TestTierSweepImagesRetryGreenSweepResetsTheCount(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, _ := newTierSweepDaemon(t, now, "gastown")
	rec.green()
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return imagesRetryTIP, nil }
	// Both tips have a failed images run, so only the new green sha can be what
	// clears the pair.
	d.pairReady.failed = map[string]pairReadyFailedRun{
		imagesFailedKey("gastown", greenRefShortSHA(imagesRetryGreen)): {},
		imagesFailedKey("gastown", greenRefShortSHA(imagesRetryTIP)):   {},
	}
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
		LastRun: now.Add(-tierSweepImagesRetryGap), LastSHA: imagesRetryGreen, LastGreenSHA: imagesRetryGreen,
		ImagesRetries: tierSweepImagesRetryMax - 1, ImagesRetrySHA: imagesRetryGreen,
	}); err != nil {
		t.Fatal(err)
	}

	imagesRetryTrigger(t, d)

	st := tierSweepTestState(t, d, "gastown")
	if st.LastGreenSHA != imagesRetryTIP {
		t.Fatalf("LastGreenSHA = %q, want the green re-run's tip %q", st.LastGreenSHA, imagesRetryTIP)
	}
	if st.ImagesRetries != 0 || st.ImagesRetrySHA != "" {
		t.Fatalf("the new green sha left %d retries for %q, want them cleared",
			st.ImagesRetries, st.ImagesRetrySHA)
	}
}

// The images run for the green sha no longer standing failed clears the
// retries: a later run for it succeeded, so no failure is left for a count to
// belong to.
func TestTierSweepImagesRetryImagesSuccessClearsTheCount(t *testing.T) {
	t.Parallel()
	now := atHour(time.Now(), 15)
	d, rec, _, _ := newTierSweepDaemon(t, now, "gastown")
	rec.result = nil
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return imagesRetryTIP, nil }
	if err := writeTierSweepState(d.config.TownRoot, "gastown", tierSweepState{
		LastRun: now.Add(-tierSweepImagesRetryGap), LastSHA: "red1", LastGreenSHA: imagesRetryGreen,
		ImagesRetries: 2, ImagesRetrySHA: imagesRetryGreen,
	}); err != nil {
		t.Fatal(err)
	}

	imagesRetryTrigger(t, d)

	st := tierSweepTestState(t, d, "gastown")
	if st.LastSHA != imagesRetryTIP || st.LastGreenSHA != imagesRetryGreen {
		t.Fatalf("record = %+v, want the RED cycle to leave the green tip %q", st, imagesRetryGreen)
	}
	if st.ImagesRetries != 0 || st.ImagesRetrySHA != "" {
		t.Fatalf("the succeeded images run left %d retries for %q, want them cleared",
			st.ImagesRetries, st.ImagesRetrySHA)
	}
}

// The retries cost the loop survive the process: a daemon value that ran none
// of the cycles reads the count back from the state file, and the count at the
// cap is what holds the fourth retry off.
func TestTierSweepImagesRetryCountReadByAFreshDaemon(t *testing.T) {
	t.Parallel()
	d, rec, _, _ := setupImagesRetry(t, imagesRetryTriggerCase{
		lastSHA: imagesRetryGreen, lastGreenSHA: imagesRetryGreen, sinceLastRun: tierSweepImagesRetryGap,
		imagesFailed: true, mainSHA: imagesRetryTIP,
	})
	rec.result = nil
	main := imagesRetryTIP
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return main, nil }

	for i, tip := range []string{"tipone0abcd", "tiptwo0abcd", "tipthree0ab"} {
		main = tip
		imagesRetryTrigger(t, d)
		st := tierSweepTestState(t, d, "gastown")
		if st.ImagesRetries != i+1 || st.ImagesRetrySHA != imagesRetryGreen {
			t.Fatalf("re-run %d wrote %d retries for %q, want %d for %q",
				i+1, st.ImagesRetries, st.ImagesRetrySHA, i+1, imagesRetryGreen)
		}
		imagesRetryClock(t, d).Advance(tierSweepImagesRetryGap)
	}

	fresh := &Daemon{config: &Config{TownRoot: d.config.TownRoot}}
	fresh.pairReady.failed = d.pairReady.failed
	st, err := readTierSweepState(fresh.config.TownRoot, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if st.ImagesRetries != tierSweepImagesRetryMax || st.ImagesRetrySHA != imagesRetryGreen {
		t.Fatalf("the fresh daemon read %d retries for %q, want %d for %q",
			st.ImagesRetries, st.ImagesRetrySHA, tierSweepImagesRetryMax, imagesRetryGreen)
	}
	if !fresh.tierSweepImagesFailedFor("gastown", st.LastGreenSHA) {
		t.Fatalf("the fresh daemon lost the failed images answer for %q", st.LastGreenSHA)
	}
	if tierSweepImagesRetryDue(st, imagesRetryClock(t, d).Now(), true) {
		t.Fatal("the count the fresh daemon read back did not hold the fourth retry off")
	}
}
