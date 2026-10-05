package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// infraNow is the clock every landing-infrastructure test pass runs on.
var infraNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// fakeProber answers the landing-infrastructure probes from scripted results
// and records what it was asked, so no test reaches the network.
type fakeProber struct {
	// versions maps an API base to the error its version probe returns.
	versions map[string]error
	// pickups maps "owner/repo" to the run queue answer.
	pickups map[string]fakeRunPickup
	// tokens is every token handed to a run queue probe, in order.
	tokens []string
	// bases is every API base the version probe was asked, in order.
	bases []string
}

// fakeRunPickup is one repository's run queue: the oldest run waiting on a
// runner, or the read that failed.
type fakeRunPickup struct {
	waiting bool
	since   time.Time
	runID   int64
	err     error
}

func (f *fakeProber) Version(_ context.Context, apiBase string) error {
	f.bases = append(f.bases, apiBase)
	return f.versions[apiBase]
}

func (f *fakeProber) Runs(_ context.Context, apiBase, token, owner, repo string) (forgejo.RunPickup, error) {
	f.tokens = append(f.tokens, token)
	a := f.pickups[owner+"/"+repo]
	if a.err != nil {
		return forgejo.RunPickup{}, a.err
	}
	return forgejo.RunPickup{Waiting: a.waiting, Since: a.since, ID: a.runID}, nil
}

// infraDaemon is a daemon whose alerts go to a recorder and whose probe is
// driven by the targets and prober a test supplies.
func infraDaemon(t *testing.T, tgts []landingInfraTarget, prober infraProber) (*Daemon, *fakeProber, *notifyfake.Recorder) {
	t.Helper()
	d, rec := daemonWithRecorder(t)
	fp, _ := prober.(*fakeProber)
	d.landingInfraRigs = func() []landingInfraTarget { return tgts }
	d.landingInfraProber = prober
	return d, fp, rec
}

// infraTokenFile writes a landing bot token file the way the client reads
// one, and returns its path.
func infraTokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "forgejo-landing.env")
	if err := os.WriteFile(path, []byte("FORGEJO_TOKEN="+token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// infraTargets is one rig landing through one Forgejo instance, with a
// readable landing bot token.
func infraTargets(t *testing.T) []landingInfraTarget {
	t.Helper()
	return []landingInfraTarget{{
		Rig:       "gastown",
		Owner:     "gastown",
		Repo:      "gastown",
		APIBase:   "https://forgejo.test/api/v1",
		TokenPath: infraTokenFile(t, "token-from-file"),
	}}
}

func TestLandingInfraProbesTheInstanceAndTheRunQueue(t *testing.T) {
	t.Parallel()
	prober := &fakeProber{
		versions: map[string]error{},
		pickups:  map[string]fakeRunPickup{"gastown/gastown": {waiting: true, since: infraNow.Add(-30 * time.Second), runID: 91}},
	}
	d, fp, _ := infraDaemon(t, infraTargets(t), prober)

	d.runLandingInfraProbe(infraNow)

	if len(fp.bases) != 1 || fp.bases[0] != "https://forgejo.test/api/v1" {
		t.Errorf("version probe asked %v, want the rig's API base once", fp.bases)
	}
	if len(fp.tokens) != 1 || fp.tokens[0] != "token-from-file" {
		t.Errorf("run queue probe tokens = %v, want the token read from the file", fp.tokens)
	}
	rep := d.landingInfra.report()
	if !rep.Configured || len(rep.Rigs) != 1 || rep.Rigs[0].Rig != "gastown" {
		t.Fatalf("report = %+v, want one configured rig", rep)
	}
	if !rep.Rigs[0].Forgejo.OK {
		t.Errorf("report = %+v, want the version probe green", rep.Rigs[0])
	}
	if got := townhealth.InfraRunnerVerdict(rep.Rigs[0].Runner, infraNow); got != townhealth.Green {
		t.Errorf("runner verdict = %s, want %s with a run only just queued", got, townhealth.Green)
	}
	if got := rep.Rigs[0].Runner.WaitingSince; !got.Equal(infraNow.Add(-30 * time.Second)) {
		t.Errorf("runner datum = %v, want the oldest waiting run's creation", got)
	}
}

// TestLandingInfraNoForgejoRigMeansNoFieldsAndNoProbe: a town where no rig
// carries merge_queue.forgejo makes no request and reports no field at all
// (gt-fn9e6.11).
func TestLandingInfraNoForgejoRigMeansNoFieldsAndNoProbe(t *testing.T) {
	t.Parallel()
	prober := &fakeProber{}
	d, fp, _ := infraDaemon(t, nil, prober)

	d.runLandingInfraProbe(infraNow)

	if len(fp.bases) != 0 || len(fp.tokens) != 0 {
		t.Errorf("probe ran with no forgejo rig: bases %v, tokens %v", fp.bases, fp.tokens)
	}
	if rep := d.landingInfra.report(); rep.Configured {
		t.Errorf("report = %+v, want no fields", rep)
	}
	if got := (&healthSources{d: d}).LandingInfra(); got.Configured {
		t.Errorf("health source = %+v, want no fields", got)
	}
}

// TestLandingInfraRaisesOneEscalationWhenForgejoTurnsRed: an instance that
// does not answer is degraded at first, escalates once when the failure
// passes the red threshold, does not escalate again while it holds, and is
// cleared when it answers again (gt-fn9e6.11).
func TestLandingInfraRaisesOneEscalationWhenForgejoTurnsRed(t *testing.T) {
	t.Parallel()
	down := errors.New("dial tcp 127.0.0.1:3000: connect: connection refused")
	prober := &fakeProber{
		versions: map[string]error{"https://forgejo.test/api/v1": down},
		pickups:  map[string]fakeRunPickup{},
	}
	d, _, rec := infraDaemon(t, infraTargets(t), prober)

	d.runLandingInfraProbe(infraNow)
	if n := len(rec.Escalations()); n != 0 {
		t.Fatalf("escalations after one failing pass = %d, want 0: the failure is young", n)
	}
	if got := infraVerdictOf(t, d, townhealth.FieldForgejo, infraNow); got != townhealth.Degraded {
		t.Fatalf("forgejo verdict = %s, want %s", got, townhealth.Degraded)
	}

	red := infraNow.Add(townhealth.InfraRedAfter)
	d.runLandingInfraProbe(red)
	got := rec.Escalations()
	if len(got) != 1 {
		t.Fatalf("escalations at the threshold = %d, want 1", len(got))
	}
	if key := got[0].Escalation.Fingerprint; key != alertKeyForgejoDown {
		t.Errorf("fingerprint = %q, want %q", key, alertKeyForgejoDown)
	}
	if !strings.Contains(got[0].Escalation.Reason, "gastown") {
		t.Errorf("escalation reason = %q, want it to name the rig", got[0].Escalation.Reason)
	}

	d.runLandingInfraProbe(red.Add(time.Minute))
	if n := len(rec.Escalations()); n != 1 {
		t.Errorf("escalations while the outage holds = %d, want 1: it must not re-raise", n)
	}

	prober.versions["https://forgejo.test/api/v1"] = nil
	d.runLandingInfraProbe(red.Add(2 * time.Minute))
	clears := rec.Clears()
	if len(clears) != 1 {
		t.Fatalf("clears after recovery = %d, want 1", len(clears))
	}
	if keys := clears[0].Fingerprints; len(keys) != 1 || keys[0] != alertKeyForgejoDown {
		t.Errorf("cleared keys = %v, want [%s]", keys, alertKeyForgejoDown)
	}
}

// TestLandingInfraRaisesAndClearsTheRunnerCondition: a run nobody picks up
// degrades at first, escalates once when its wait passes the red threshold,
// does not escalate again while it holds, and is cleared when CI picks it up.
// The escalation names the run, which is what an operator opens
// (gt-fn9e6.56).
func TestLandingInfraRaisesAndClearsTheRunnerCondition(t *testing.T) {
	t.Parallel()
	prober := &fakeProber{
		versions: map[string]error{},
		pickups:  map[string]fakeRunPickup{"gastown/gastown": {waiting: true, since: infraNow, runID: 91}},
	}
	d, _, rec := infraDaemon(t, infraTargets(t), prober)

	late := infraNow.Add(townhealth.InfraRunnerDegradedAfter)
	d.runLandingInfraProbe(late)
	if n := len(rec.Escalations()); n != 0 {
		t.Fatalf("escalations after a late run = %d, want 0: it is degraded, not red", n)
	}
	if got := infraVerdictOf(t, d, townhealth.FieldRunner, late); got != townhealth.Degraded {
		t.Fatalf("runner verdict = %s, want %s", got, townhealth.Degraded)
	}

	red := infraNow.Add(townhealth.InfraRunnerRedAfter)
	d.runLandingInfraProbe(red)
	got := rec.Escalations()
	if len(got) != 1 || got[0].Escalation.Fingerprint != alertKeyRunnerOffline {
		t.Fatalf("escalations = %+v, want one keyed %s", got, alertKeyRunnerOffline)
	}
	if !strings.Contains(got[0].Escalation.Reason, "91") {
		t.Errorf("escalation reason = %q, want it to name the waiting run", got[0].Escalation.Reason)
	}
	if n := len(rec.Clears()); n != 0 {
		t.Fatalf("clears before recovery = %d, want 0", n)
	}

	d.runLandingInfraProbe(red.Add(time.Minute))
	if n := len(rec.Escalations()); n != 1 {
		t.Errorf("escalations while the queue stays stalled = %d, want 1: it must not re-raise", n)
	}

	prober.pickups["gastown/gastown"] = fakeRunPickup{}
	d.runLandingInfraProbe(red.Add(2 * time.Minute))
	clears := rec.Clears()
	if len(clears) != 1 || clears[0].Fingerprints[0] != alertKeyRunnerOffline {
		t.Fatalf("clears = %+v, want one for %s", clears, alertKeyRunnerOffline)
	}
}

// TestLandingInfraUnreadableRunQueueIsUnknown: a run list the landing bot
// cannot read is an unanswerable question, never a runner verdict — the 403 a
// non-owner gets on the repository runner API is exactly what this signal
// replaced (gt-fn9e6.56).
func TestLandingInfraUnreadableRunQueueIsUnknown(t *testing.T) {
	t.Parallel()
	forbidden := errors.New("forgejo: GET /repos/gastown/gastown/actions/runs returned 403: user should be the owner of the repo")
	prober := &fakeProber{
		versions: map[string]error{},
		pickups:  map[string]fakeRunPickup{"gastown/gastown": {err: forbidden}},
	}
	d, _, rec := infraDaemon(t, infraTargets(t), prober)

	d.runLandingInfraProbe(infraNow)
	d.runLandingInfraProbe(infraNow.Add(24 * time.Hour))

	if n := len(rec.Escalations()); n != 0 {
		t.Errorf("escalations = %d, want 0: an unreadable queue is not a stalled runner", n)
	}
	if got := infraVerdictOf(t, d, townhealth.FieldRunner, infraNow); got != townhealth.VerdictUnknown {
		t.Errorf("runner verdict = %s, want %s", got, townhealth.VerdictUnknown)
	}
	rep := d.landingInfra.report()
	if !strings.Contains(rep.Rigs[0].Runner.Unavailable, "403") {
		t.Errorf("runner unavailable reason = %q, want it to name the error", rep.Rigs[0].Runner.Unavailable)
	}
}

// TestLandingInfraUnanswerableProbeNeverEscalates: a missing token file and a
// rig whose block names no base URL read UNKNOWN and raise nothing, however
// long they stay that way (gt-fn9e6.11).
func TestLandingInfraUnanswerableProbeNeverEscalates(t *testing.T) {
	t.Parallel()
	tgts := []landingInfraTarget{{
		Rig:       "gastown",
		TokenPath: filepath.Join(t.TempDir(), "missing.env"),
		APIBase:   "https://forgejo.test/api/v1",
		Owner:     "gastown",
		Repo:      "gastown",
	}}
	prober := &fakeProber{versions: map[string]error{}, pickups: map[string]fakeRunPickup{}}
	d, _, rec := infraDaemon(t, tgts, prober)

	d.runLandingInfraProbe(infraNow)
	d.runLandingInfraProbe(infraNow.Add(24 * time.Hour))

	if n := len(rec.Escalations()); n != 0 {
		t.Errorf("escalations = %d, want 0: an unanswerable probe is not an outage", n)
	}
	if got := infraVerdictOf(t, d, townhealth.FieldRunner, infraNow); got != townhealth.VerdictUnknown {
		t.Errorf("runner verdict = %s, want %s", got, townhealth.VerdictUnknown)
	}
	if len(prober.tokens) != 0 {
		t.Errorf("runner probe ran without a token: %v", prober.tokens)
	}
}

// TestLandingInfraRigWithoutABaseURLIsUnknown: a rig that carries
// merge_queue.forgejo but names no usable remote URL reads unknown with the
// reason, and neither probe is made (gt-fn9e6.11).
func TestLandingInfraRigWithoutABaseURLIsUnknown(t *testing.T) {
	t.Parallel()
	tgts := []landingInfraTarget{{
		Rig:       "gastown",
		BaseErr:   errors.New(`forgejo remote "" is not a URL with a scheme and host`),
		TokenPath: "/town/forgejo-landing.env",
	}}
	prober := &fakeProber{}
	d, _, rec := infraDaemon(t, tgts, prober)

	d.runLandingInfraProbe(infraNow)

	if len(prober.bases) != 0 || len(prober.tokens) != 0 {
		t.Errorf("probes ran without a base URL: %v %v", prober.bases, prober.tokens)
	}
	if n := len(rec.Escalations()); n != 0 {
		t.Errorf("escalations = %d, want 0", n)
	}
	rep := d.landingInfra.report()
	if rep.Rigs[0].Forgejo.Unavailable == "" || rep.Rigs[0].Runner.Unavailable == "" {
		t.Errorf("report = %+v, want both probes unknown with a reason", rep.Rigs[0])
	}
}

// TestLandingInfraInstanceIsProbedOncePerPass: two rigs on one instance make
// one version request between them; the runner list is per repository, so
// each rig still asks its own (gt-fn9e6.11).
func TestLandingInfraInstanceIsProbedOncePerPass(t *testing.T) {
	t.Parallel()
	tokenPath := infraTokenFile(t, "token-from-file")
	tgts := []landingInfraTarget{
		{Rig: "alpha", Owner: "gastown", Repo: "alpha", APIBase: "https://forgejo.test/api/v1", TokenPath: tokenPath},
		{Rig: "beta", Owner: "gastown", Repo: "beta", APIBase: "https://forgejo.test/api/v1", TokenPath: tokenPath},
	}
	prober := &fakeProber{
		versions: map[string]error{},
		pickups:  map[string]fakeRunPickup{"gastown/alpha": {}, "gastown/beta": {}},
	}
	d, fp, _ := infraDaemon(t, tgts, prober)

	d.runLandingInfraProbe(infraNow)

	if len(fp.bases) != 1 {
		t.Errorf("version probe calls = %d, want 1 for the shared instance", len(fp.bases))
	}
	if len(fp.tokens) != 2 {
		t.Errorf("runner probe calls = %d, want one per rig", len(fp.tokens))
	}
	if rep := d.landingInfra.report(); len(rep.Rigs) != 2 {
		t.Errorf("report = %+v, want both rigs", rep)
	}
}

// TestLandingInfraTokenNeverLeavesThePass: the landing bot's token is read
// from its file, handed to the probe, and appears in no field, no detail and
// no escalation (gt-fn9e6.11).
func TestLandingInfraTokenNeverLeavesThePass(t *testing.T) {
	t.Parallel()
	const token = "s3cr3t-landing-token"
	tgts := []landingInfraTarget{{
		Rig: "gastown", Owner: "gastown", Repo: "gastown",
		APIBase: "https://forgejo.test/api/v1", TokenPath: infraTokenFile(t, token),
	}}
	prober := &fakeProber{
		versions: map[string]error{"https://forgejo.test/api/v1": errors.New("down")},
		pickups:  map[string]fakeRunPickup{"gastown/gastown": {err: errors.New("boom")}},
	}
	d, fp, rec := infraDaemon(t, tgts, prober)

	d.runLandingInfraProbe(infraNow)
	d.runLandingInfraProbe(infraNow.Add(townhealth.InfraRedAfter))

	if len(fp.tokens) != 2 || fp.tokens[0] != token {
		t.Fatalf("probe tokens = %v, want the token read from the file", fp.tokens)
	}
	rep := d.landingInfra.report()
	if strings.Contains(rep.Rigs[0].Runner.Detail, token) {
		t.Errorf("the token reached a field detail: %q", rep.Rigs[0].Runner.Detail)
	}
	for _, c := range rec.Calls() {
		for _, s := range []string{c.Escalation.Description, c.Escalation.Reason, c.Reason} {
			if strings.Contains(s, token) {
				t.Errorf("the token reached an alert: %q", s)
			}
		}
	}
}

// TestLandingInfraEscalationClearsWhenNoRigLandsThroughForgejo: a rig the
// operator removes must not leave an alarm nothing can clear (gt-fn9e6.11).
func TestLandingInfraEscalationClearsWhenNoRigLandsThroughForgejo(t *testing.T) {
	t.Parallel()
	prober := &fakeProber{
		versions: map[string]error{"https://forgejo.test/api/v1": errors.New("down")},
		pickups:  map[string]fakeRunPickup{"gastown/gastown": {}},
	}
	d, _, rec := infraDaemon(t, infraTargets(t), prober)
	d.runLandingInfraProbe(infraNow)
	d.runLandingInfraProbe(infraNow.Add(townhealth.InfraRedAfter))
	if len(rec.Escalations()) != 1 {
		t.Fatalf("escalations = %+v, want one before the rig is removed", rec.Escalations())
	}

	d.landingInfraRigs = func() []landingInfraTarget { return nil }
	d.runLandingInfraProbe(infraNow.Add(townhealth.InfraRedAfter + time.Minute))

	if clears := rec.Clears(); len(clears) != 1 || clears[0].Fingerprints[0] != alertKeyForgejoDown {
		t.Errorf("clears = %+v, want one for %s", clears, alertKeyForgejoDown)
	}
}

// TestLandingInfraIntervalDefaultsToAMinute pins the probe cadence and its
// config key (gt-fn9e6.11).
func TestLandingInfraIntervalDefaultsToAMinute(t *testing.T) {
	t.Parallel()
	if got := landingInfraInterval(nil); got != time.Minute {
		t.Errorf("default interval = %v, want 1m", got)
	}
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{LandingInfra: &LandingInfraConfig{IntervalStr: "15s"}}}
	if got := landingInfraInterval(cfg); got != 15*time.Second {
		t.Errorf("configured interval = %v, want 15s", got)
	}
	bad := &DaemonPatrolConfig{Patrols: &PatrolsConfig{LandingInfra: &LandingInfraConfig{IntervalStr: "soon"}}}
	if got := landingInfraInterval(bad); got != time.Minute {
		t.Errorf("unparseable interval = %v, want the default", got)
	}
}

// TestIsPatrolEnabledLandingInfraDefaultsOn: the probe is on unless the
// operator turns it off; it makes no request where no rig lands through
// Forgejo, so leaving it on costs a town nothing (gt-fn9e6.11).
func TestIsPatrolEnabledLandingInfraDefaultsOn(t *testing.T) {
	t.Parallel()
	if !IsPatrolEnabled(nil, "landing_infra") {
		t.Error("landing_infra is off with no config, want on")
	}
	off := false
	cfg := &DaemonPatrolConfig{Patrols: &PatrolsConfig{LandingInfra: &LandingInfraConfig{Enabled: &off}}}
	if IsPatrolEnabled(cfg, "landing_infra") {
		t.Error("landing_infra is on with enabled:false, want off")
	}
	on := true
	cfg.Patrols.LandingInfra.Enabled = &on
	if !IsPatrolEnabled(cfg, "landing_infra") {
		t.Error("landing_infra is off with enabled:true, want on")
	}
}

// infraVerdictOf reads one field's verdict from the daemon's own state, the
// way the health computation will.
func infraVerdictOf(t *testing.T, d *Daemon, name string, now time.Time) townhealth.Verdict {
	t.Helper()
	rep := (&healthSources{d: d}).LandingInfra()
	if len(rep.Rigs) != 1 {
		t.Fatalf("report = %+v, want one rig", rep)
	}
	if name == townhealth.FieldRunner {
		return townhealth.InfraRunnerVerdict(rep.Rigs[0].Runner, now)
	}
	return townhealth.InfraVerdict(rep.Rigs[0].Forgejo, now)
}
