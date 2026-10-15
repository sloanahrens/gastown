package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/forgejo"
)

// The tests drive the watch with a fake Forgejo: the API's own answers matter
// to the watch, so the fake is the seam and no test opens a socket (except
// TestDeployWatch_TokenNeverReachesTheAlert, whose transport answers in
// process).

var deployTestNow = time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)

const (
	deployTestOwner   = "acme"
	deployTestName    = "fractals-nextjs"
	deployTestRepo    = deployTestOwner + "/" + deployTestName
	deployTestAPIBase = "http://forgejo.test/api/v1"
)

// fakeDeployForgejo is one repository's runs and jobs, as the API answers.
type fakeDeployForgejo struct {
	runs    []forgejo.ActionRun
	jobs    map[int64][]forgejo.ActionRunJob
	listErr error
	jobErr  error
	lists   int
	limits  []int
	pages   []int
}

// ListRuns answers a page of the repository's runs, newest first, which is the
// order the API pages them in.
func (f *fakeDeployForgejo) ListRuns(_ context.Context, _, _ string, filter forgejo.RunFilter) (*forgejo.RunList, error) {
	f.lists++
	f.limits = append(f.limits, filter.Limit)
	f.pages = append(f.pages, filter.Page)
	if f.listErr != nil {
		return nil, f.listErr
	}
	if filter.Limit <= 0 {
		return &forgejo.RunList{TotalCount: len(f.runs), Runs: f.runs}, nil
	}
	start := max(filter.Page-1, 0) * filter.Limit
	if start >= len(f.runs) {
		return &forgejo.RunList{TotalCount: len(f.runs)}, nil
	}
	return &forgejo.RunList{
		TotalCount: len(f.runs),
		Runs:       f.runs[start:min(start+filter.Limit, len(f.runs))],
	}, nil
}

func (f *fakeDeployForgejo) ListRunJobs(_ context.Context, _, _ string, runID int64) ([]forgejo.ActionRunJob, error) {
	if f.jobErr != nil {
		return nil, f.jobErr
	}
	return f.jobs[runID], nil
}

// deployRecorder records what one watch did, and can fail a delivery so a
// test can pin what the watch retries.
type deployRecorder struct {
	mu       sync.Mutex
	raised   []deployRaise
	cleared  []deployClear
	pushed   []deployPush
	raiseErr error
	clearErr error
	pushErr  error
}

type deployRaise struct{ Severity, Key, Message string }
type deployClear struct{ Key, Reason string }
type deployPush struct{ Title, Body string }

func (r *deployRecorder) raise(severity, key, message string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.raiseErr != nil {
		return r.raiseErr
	}
	r.raised = append(r.raised, deployRaise{severity, key, message})
	return nil
}

func (r *deployRecorder) clear(key, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.clearErr != nil {
		return r.clearErr
	}
	r.cleared = append(r.cleared, deployClear{key, reason})
	return nil
}

func (r *deployRecorder) push(title, body string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pushErr != nil {
		return r.pushErr
	}
	r.pushed = append(r.pushed, deployPush{title, body})
	return nil
}

func (r *deployRecorder) raisedAlerts() []deployRaise {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]deployRaise(nil), r.raised...)
}

func (r *deployRecorder) clearedAlerts() []deployClear {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]deployClear(nil), r.cleared...)
}

func (r *deployRecorder) pushes() []deployPush {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]deployPush(nil), r.pushed...)
}

// newTestDeployWatch builds a watch over api for one repository, with the
// recorder wired to every seam and the state file under a temp dir.
func newTestDeployWatch(t *testing.T, api deployRunAPI) (*deployWatch, *deployRecorder) {
	t.Helper()
	rec := &deployRecorder{}
	w := newDeployWatch()
	w.API = func(string) (deployRunAPI, error) { return api, nil }
	w.Repos = func() []deployRepo {
		return []deployRepo{{Rig: "fractals", Owner: deployTestOwner, Name: deployTestName, APIBase: deployTestAPIBase}}
	}
	w.Raise = rec.raise
	w.Clear = rec.clear
	w.Push = rec.push
	w.State = filepath.Join(t.TempDir(), deployWatchStateFile)
	w.Now = func() time.Time { return deployTestNow }
	return w, rec
}

// seedDeployState gives the watch the state file a daemon that has run before
// leaves, so it is not the watch whose state file was never written: that one
// clears an unknown key once, which its own test pins (clearIfOpen).
func seedDeployState(t *testing.T, w *deployWatch) {
	t.Helper()
	if err := os.WriteFile(w.State, []byte("{}"), 0o600); err != nil {
		t.Fatalf("seeding the state file: %v", err)
	}
}

// deployTestRun is one run of the fake repository's history.
func deployTestRun(id int64, workflow, ref, status string, created time.Time, title string) forgejo.ActionRun {
	return forgejo.ActionRun{
		ID:         id,
		WorkflowID: ".forgejo/workflows/" + workflow,
		Status:     status,
		PrettyRef:  ref,
		Title:      title,
		Created:    created.Format(time.RFC3339),
		HTMLURL:    fmt.Sprintf("http://forgejo.test/%s/actions/runs/%d", deployTestRepo, id),
	}
}

// failingJob is one job of a run, in the state the API reports.
func deployTestJob(runID int64, name, status string) forgejo.ActionRunJob {
	return forgejo.ActionRunJob{ID: runID * 10, RunID: runID, Name: name, Status: status}
}

func TestDeployWatch_StagingFailureOnMainRaisesOneMediumAlert(t *testing.T) {
	t.Parallel()
	run := deployTestRun(1153, deployWorkflowStaging, "main", deployRunFailed, deployTestNow.Add(-time.Hour), "fix: guard the retry (gt-abc)")
	api := &fakeDeployForgejo{
		runs: []forgejo.ActionRun{run},
		jobs: map[int64][]forgejo.ActionRunJob{1153: {deployTestJob(1153, "staging-integration", deployRunFailed)}},
	}
	w, rec := newTestDeployWatch(t, api)

	w.poll(context.Background())

	raised := rec.raisedAlerts()
	if len(raised) != 1 {
		t.Fatalf("raised = %+v, want one escalation", raised)
	}
	got := raised[0]
	if got.Key != "deploy-failed:"+deployTestRepo+":"+deployWorkflowStaging {
		t.Errorf("key = %q, want deploy-failed:%s:%s", got.Key, deployTestRepo, deployWorkflowStaging)
	}
	if got.Severity != config.SeverityMedium {
		t.Errorf("severity = %q, want %q", got.Severity, config.SeverityMedium)
	}
	for _, want := range []string{
		deployTestRepo, deployWorkflowStaging, "run 1153", "main",
		"fix: guard the retry (gt-abc)", "staging-integration",
		"http://forgejo.test/" + deployTestRepo + "/actions/runs/1153",
	} {
		if !strings.Contains(got.Message, want) {
			t.Errorf("message %q does not name %q", got.Message, want)
		}
	}
	if pushes := rec.pushes(); len(pushes) != 0 {
		t.Errorf("pushes = %+v, want none for a medium severity", pushes)
	}
	if len(api.limits) != 1 || api.limits[0] != deployWatchRunLimit {
		t.Errorf("list limits = %v, want one list at %d", api.limits, deployWatchRunLimit)
	}
}

func TestDeployWatch_TheSameFailureRaisesNothingNew(t *testing.T) {
	t.Parallel()
	run := deployTestRun(1153, deployWorkflowStaging, "main", deployRunFailed, deployTestNow.Add(-time.Hour), "fix: guard the retry")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{run}}
	w, rec := newTestDeployWatch(t, api)

	w.poll(context.Background())
	w.poll(context.Background())
	w.poll(context.Background())

	if raised := rec.raisedAlerts(); len(raised) != 1 {
		t.Errorf("raised = %+v, want the one escalation the first poll filed", raised)
	}
}

func TestDeployWatch_TagReleaseFailureIsHighAndPushes(t *testing.T) {
	t.Parallel()
	run := deployTestRun(77, deployWorkflowDeploy, "v0.1.3", deployRunFailed, deployTestNow.Add(-30*time.Minute), "chore: release v0.1.3")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{run}}
	w, rec := newTestDeployWatch(t, api)

	w.poll(context.Background())
	w.poll(context.Background())

	raised := rec.raisedAlerts()
	if len(raised) != 1 {
		t.Fatalf("raised = %+v, want one escalation", raised)
	}
	if raised[0].Severity != config.SeverityHigh {
		t.Errorf("severity = %q, want %q", raised[0].Severity, config.SeverityHigh)
	}
	pushes := rec.pushes()
	if len(pushes) != 1 {
		t.Fatalf("pushes = %+v, want exactly one for a high severity", pushes)
	}
	if !strings.Contains(pushes[0].Title, deployTestRepo) || !strings.Contains(pushes[0].Body, "v0.1.3") {
		t.Errorf("push = %+v, want the repository and the ref", pushes[0])
	}
}

func TestDeployWatch_ALaterGreenRunClearsTheEscalation(t *testing.T) {
	t.Parallel()
	failed := deployTestRun(1153, deployWorkflowStaging, "main", deployRunFailed, deployTestNow.Add(-time.Hour), "fix: guard the retry")
	green := deployTestRun(1154, deployWorkflowStaging, "main", deployRunSucceeded, deployTestNow.Add(-time.Minute), "fix: guard the retry")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{failed}}
	w, rec := newTestDeployWatch(t, api)

	w.poll(context.Background())

	api.runs = []forgejo.ActionRun{failed, green}
	w.poll(context.Background())

	cleared := rec.clearedAlerts()
	if len(cleared) != 1 {
		t.Fatalf("cleared = %+v, want the one alert the failure raised", cleared)
	}
	if want := "deploy-failed:" + deployTestRepo + ":" + deployWorkflowStaging; cleared[0].Key != want {
		t.Errorf("cleared key = %q, want %q", cleared[0].Key, want)
	}
	if !strings.Contains(cleared[0].Reason, "1154") {
		t.Errorf("clear reason = %q, want the run that cleared it", cleared[0].Reason)
	}
	if failures := w.failures(); len(failures) != 0 {
		t.Errorf("failures = %+v, want none once the deploy is green", failures)
	}

	// A failure after the green run is a new alert: the escalation was closed.
	again := deployTestRun(1155, deployWorkflowStaging, "main", deployRunFailed, deployTestNow.Add(-time.Second), "fix: another try")
	api.runs = []forgejo.ActionRun{failed, green, again}
	w.poll(context.Background())

	if raised := rec.raisedAlerts(); len(raised) != 2 {
		t.Errorf("raised = %+v, want a second escalation for the run that failed after the green one", raised)
	}
}

func TestDeployWatch_LandRefsAndOldRunsAreNotDeploys(t *testing.T) {
	t.Parallel()
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{
		// A candidate branch: the land/* gate covers it, not this watch.
		deployTestRun(1, deployWorkflowStaging, "land/gt-abc", deployRunFailed, deployTestNow.Add(-time.Hour), "candidate"),
		// A release workflow on main: not a deploy this watch follows.
		deployTestRun(2, deployWorkflowDeploy, "main", deployRunFailed, deployTestNow.Add(-time.Hour), "deploy main"),
		// A feature branch named like a tag.
		deployTestRun(3, deployWorkflowDeploy, "verify", deployRunFailed, deployTestNow.Add(-time.Hour), "verify"),
		// A staging failure older than the window.
		deployTestRun(4, deployWorkflowStaging, "main", deployRunFailed, deployTestNow.Add(-48*time.Hour), "old"),
		// A workflow the watch does not follow at all.
		deployTestRun(5, "gate.yml", "main", deployRunFailed, deployTestNow.Add(-time.Hour), "gate"),
	}}
	w, rec := newTestDeployWatch(t, api)

	w.poll(context.Background())

	if raised := rec.raisedAlerts(); len(raised) != 0 {
		t.Errorf("raised = %+v, want none: land/*, a deploy on main, a 48h-old run and gate.yml are not deploys", raised)
	}
	if failures := w.failures(); len(failures) != 0 {
		t.Errorf("failures = %+v, want an empty picture", failures)
	}
}

func TestDeployWatch_HealthAlertsOncePerEnvironment(t *testing.T) {
	t.Parallel()
	prod := deployTestRun(9, deployWorkflowHealth, "v0.1.3", deployRunFailed, deployTestNow.Add(-time.Minute), "release smoke")
	staging := deployTestRun(8, deployWorkflowHealth, "main", deployRunFailed, deployTestNow.Add(-2*time.Minute), "staging check")
	api := &fakeDeployForgejo{
		runs: []forgejo.ActionRun{prod, staging},
		jobs: map[int64][]forgejo.ActionRunJob{
			9: {deployTestJob(9, "smoke-runners", deployRunSucceeded), deployTestJob(9, deployHealthProdJob, deployRunFailed)},
			8: {deployTestJob(8, "staging-integration", deployRunFailed)},
		},
	}
	w, rec := newTestDeployWatch(t, api)

	w.poll(context.Background())

	raised := rec.raisedAlerts()
	if len(raised) != 2 {
		t.Fatalf("raised = %+v, want one per environment: the released artifact and the staging check", raised)
	}
	byKey := map[string]deployRaise{}
	for _, r := range raised {
		byKey[r.Key] = r
	}
	prodKey := "deploy-failed:" + deployTestRepo + ":" + deployWorkflowHealth + ":" + deployRefTag
	stagingKey := "deploy-failed:" + deployTestRepo + ":" + deployWorkflowHealth + ":" + deployRefMain
	if byKey[prodKey].Severity != config.SeverityHigh {
		t.Errorf("the release tag alert = %+v, want %q for a failed %s", byKey[prodKey], config.SeverityHigh, deployHealthProdJob)
	}
	if byKey[stagingKey].Severity != config.SeverityMedium {
		t.Errorf("the main alert = %+v, want %q for the staging check", byKey[stagingKey], config.SeverityMedium)
	}
	if !strings.Contains(byKey[prodKey].Message, deployHealthProdJob) {
		t.Errorf("message %q does not name the failing job", byKey[prodKey].Message)
	}
	if pushes := rec.pushes(); len(pushes) != 1 {
		t.Errorf("pushes = %+v, want one for the failed prod smoke", pushes)
	}
}

// TestDeployWatch_AGreenHealthRunOnlyClearsItsOwnEnvironment: the staging
// check and the released artifact are one workflow and two deploys, so a green
// run of either is no answer about the other (gt-hcbft).
func TestDeployWatch_AGreenHealthRunOnlyClearsItsOwnEnvironment(t *testing.T) {
	t.Parallel()
	prod := deployTestRun(9, deployWorkflowHealth, "v0.1.3", deployRunFailed, deployTestNow.Add(-time.Hour), "release smoke")
	greenStaging := deployTestRun(10, deployWorkflowHealth, "main", deployRunSucceeded, deployTestNow.Add(-time.Minute), "staging check")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{prod}}
	w, rec := newTestDeployWatch(t, api)
	seedDeployState(t, w)

	w.poll(context.Background())

	api.runs = []forgejo.ActionRun{prod, greenStaging}
	w.poll(context.Background())

	if cleared := rec.clearedAlerts(); len(cleared) != 0 {
		t.Errorf("cleared = %+v, want nothing: a green staging check says nothing about the released artifact", cleared)
	}
	if failures := w.failures(); len(failures) != 1 {
		t.Errorf("failures = %+v, want the prod failure still standing", failures)
	}

	greenProd := deployTestRun(11, deployWorkflowHealth, "v0.1.4", deployRunSucceeded, deployTestNow.Add(-time.Second), "release smoke")
	api.runs = []forgejo.ActionRun{prod, greenStaging, greenProd}
	w.poll(context.Background())

	cleared := rec.clearedAlerts()
	if len(cleared) != 1 {
		t.Fatalf("cleared = %+v, want the green release smoke to close it", cleared)
	}
	if want := "deploy-failed:" + deployTestRepo + ":" + deployWorkflowHealth + ":" + deployRefTag; cleared[0].Key != want {
		t.Errorf("cleared key = %q, want %q", cleared[0].Key, want)
	}
}

// TestDeployWatch_SeverityDoesNotDependOnTheJobsRead: the jobs call is
// best-effort, so the level of the alarm is decided by the workflow and the
// ref class alone — a read that fails cannot quietly downgrade a release — and
// a job name can only raise it.
func TestDeployWatch_SeverityDoesNotDependOnTheJobsRead(t *testing.T) {
	t.Parallel()
	prod := deployTestRun(9, deployWorkflowHealth, "v0.1.3", deployRunFailed, deployTestNow.Add(-time.Minute), "release smoke")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{prod}, jobErr: fmt.Errorf("dial tcp: connection refused")}
	w, rec := newTestDeployWatch(t, api)

	w.poll(context.Background())

	raised := rec.raisedAlerts()
	if len(raised) != 1 || raised[0].Severity != config.SeverityHigh {
		t.Fatalf("raised = %+v, want one high escalation even with the jobs unreadable", raised)
	}
	if strings.Contains(raised[0].Message, "first failing job") {
		t.Errorf("message %q names a job the watch could not read", raised[0].Message)
	}
	if pushes := rec.pushes(); len(pushes) != 1 {
		t.Errorf("pushes = %+v, want the push a high severity is worth", pushes)
	}

	// A staging check whose job list names no prod smoke stays medium.
	staging := deployTestRun(8, deployWorkflowHealth, "main", deployRunFailed, deployTestNow.Add(-time.Minute), "staging check")
	api.runs = []forgejo.ActionRun{staging}
	api.jobErr = nil
	api.jobs = map[int64][]forgejo.ActionRunJob{8: {deployTestJob(8, "staging-integration", deployRunFailed)}}
	api.listErr = nil
	w2, rec2 := newTestDeployWatch(t, api)
	w2.poll(context.Background())
	second := rec2.raisedAlerts()
	if len(second) != 1 || second[0].Severity != config.SeverityMedium {
		t.Fatalf("raised = %+v, want one medium escalation for the staging check", second)
	}
}

// TestDeployWatch_AReRaiseCorrectsTheSeverity: a run whose job list names a
// prod smoke after a poll that could not read the jobs is raised again, at the
// higher severity, so the bead the key names holds the reading that holds.
func TestDeployWatch_AReRaiseCorrectsTheSeverity(t *testing.T) {
	t.Parallel()
	run := deployTestRun(9, deployWorkflowHealth, "main", deployRunFailed, deployTestNow.Add(-time.Minute), "release smoke")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{run}, jobErr: fmt.Errorf("dial tcp: connection refused")}
	w, rec := newTestDeployWatch(t, api)

	w.poll(context.Background())
	if raised := rec.raisedAlerts(); len(raised) != 1 || raised[0].Severity != config.SeverityMedium {
		t.Fatalf("raised = %+v, want a medium escalation: the class is a staging check and the jobs are unread", raised)
	}

	api.jobErr = nil
	api.jobs = map[int64][]forgejo.ActionRunJob{9: {deployTestJob(9, deployHealthProdJob, deployRunFailed)}}
	w.poll(context.Background())

	raised := rec.raisedAlerts()
	if len(raised) != 2 {
		t.Fatalf("raised = %+v, want the corrected re-raise", raised)
	}
	if raised[1].Severity != config.SeverityHigh {
		t.Errorf("re-raise severity = %q, want %q", raised[1].Severity, config.SeverityHigh)
	}
	if raised[1].Key != raised[0].Key {
		t.Errorf("re-raise key = %q, want the key already open, %q", raised[1].Key, raised[0].Key)
	}
}

// TestDeployWatch_AnOpenAlertKeepsItsItemWhenTheRunAgesOut: the run scrolls off
// the list a day after it failed and the escalation outlives it, so the
// dashboard item has to outlive the run with it.
func TestDeployWatch_AnOpenAlertKeepsItsItemWhenTheRunAgesOut(t *testing.T) {
	t.Parallel()
	run := deployTestRun(1153, deployWorkflowStaging, "main", deployRunFailed, deployTestNow.Add(-time.Hour), "fix: guard the retry")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{run}}
	w, rec := newTestDeployWatch(t, api)
	now := deployTestNow
	w.Now = func() time.Time { return now }

	w.poll(context.Background())
	first := w.failures()
	if len(first) != 1 {
		t.Fatalf("failures = %+v, want the failure that was just raised", first)
	}

	// The same run, a day and a half later: outside the window the watch
	// reads, so no verdict covers it any more.
	now = deployTestNow.Add(36 * time.Hour)
	w.poll(context.Background())

	if raised := rec.raisedAlerts(); len(raised) != 1 {
		t.Errorf("raised = %+v, want nothing new for a run already escalated", raised)
	}
	if cleared := rec.clearedAlerts(); len(cleared) != 0 {
		t.Errorf("cleared = %+v, want nothing: aging out is not a green run", cleared)
	}
	held := w.failures()
	if len(held) != 1 {
		t.Fatalf("failures = %+v, want the open alert's item kept", held)
	}
	if held[0].Key != first[0].Key || held[0].RunID != first[0].RunID ||
		held[0].Severity != first[0].Severity || held[0].Summary != first[0].Summary {
		t.Errorf("item = %+v, want the one the alert was raised with: %+v", held[0], first[0])
	}

	// And the state file carries it, so a restarted daemon still shows it.
	next, _ := newTestDeployWatch(t, api)
	next.State, next.Now = w.State, func() time.Time { return now }
	next.poll(context.Background())
	if got := next.failures(); len(got) != 1 {
		t.Errorf("failures after a restart = %+v, want the open alert's item", got)
	}
}

// TestDeployWatch_A404IsSaidOutLoudAndKeepsThePicture pins the two answers a
// read can give on a repository the rig's own config names: a 404 raises and
// clears nothing — a repository that cannot be read is not a green deploy —
// and it is logged once, because a viewer that cannot read a configured
// repository is a watch that sees nothing.
func TestDeployWatch_A404IsSaidOutLoudAndKeepsThePicture(t *testing.T) {
	t.Parallel()
	run := deployTestRun(1153, deployWorkflowStaging, "main", deployRunFailed, deployTestNow.Add(-time.Hour), "fix: guard the retry")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{run}}
	w, rec := newTestDeployWatch(t, api)
	var logged []string
	w.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	w.poll(context.Background())
	if raised := rec.raisedAlerts(); len(raised) != 1 {
		t.Fatalf("raised = %+v, want the one escalation for the failed run", raised)
	}

	api.listErr = &forgejo.APIError{Method: "GET", Path: "/repos", StatusCode: 404, Body: "not found"}
	logged = nil
	w.poll(context.Background())

	if raised := rec.raisedAlerts(); len(raised) != 1 {
		t.Errorf("raised = %+v, want no second escalation from a repository that answered nothing", raised)
	}
	if cleared := rec.clearedAlerts(); len(cleared) != 0 {
		t.Errorf("cleared = %+v, want no clear from a repository that answered nothing", cleared)
	}
	if failures := w.failures(); len(failures) != 1 {
		t.Errorf("failures = %+v, want the last picture kept", failures)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "404") {
		t.Errorf("log = %v, want one line naming the 404", logged)
	}

	// The same answer is not news again.
	w.poll(context.Background())
	if len(logged) != 1 {
		t.Errorf("log = %v, want the one line, not one per poll", logged)
	}
}

// TestDeployWatch_AReadFailureKeepsTheOpenAlert is the other half: a Forgejo
// that is down answers nothing, which is not a green deploy.
func TestDeployWatch_AReadFailureKeepsTheOpenAlert(t *testing.T) {
	t.Parallel()
	run := deployTestRun(1153, deployWorkflowStaging, "main", deployRunFailed, deployTestNow.Add(-time.Hour), "fix: guard the retry")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{run}}
	w, rec := newTestDeployWatch(t, api)

	w.poll(context.Background())

	api.listErr = fmt.Errorf("dial tcp: connection refused")
	w.poll(context.Background())

	if cleared := rec.clearedAlerts(); len(cleared) != 0 {
		t.Errorf("cleared = %+v, want no clear from a repository that answered nothing", cleared)
	}
	if failures := w.failures(); len(failures) != 1 {
		t.Errorf("failures = %+v, want the last picture kept", failures)
	}
}

// TestDeployWatch_ARigThatLeavesTheWatchLosesItsItem: a rig the town no longer
// lists is polled by nobody, so its item would sit in the dashboard forever.
// The escalation stays open — a repository that vanished is not a green deploy
// — but the picture is this poll's.
func TestDeployWatch_ARigThatLeavesTheWatchLosesItsItem(t *testing.T) {
	t.Parallel()
	run := deployTestRun(1153, deployWorkflowStaging, "main", deployRunFailed, deployTestNow.Add(-time.Hour), "fix: guard the retry")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{run}}
	w, rec := newTestDeployWatch(t, api)

	w.poll(context.Background())
	if failures := w.failures(); len(failures) != 1 {
		t.Fatalf("failures = %+v, want the failure that was just raised", failures)
	}

	w.Repos = func() []deployRepo { return nil }
	w.poll(context.Background())

	if failures := w.failures(); len(failures) != 0 {
		t.Errorf("failures = %+v, want none for a rig the watch no longer polls", failures)
	}
	if cleared := rec.clearedAlerts(); len(cleared) != 0 {
		t.Errorf("cleared = %+v, want nothing: leaving the watch is not a green run", cleared)
	}
}

// TestDeployWatch_AFailureBeyondTheFirstRunPageIsStillSeen: the run list is
// paged, and a page full of runs the watch does not follow must not hide the
// deploy that failed behind them.
func TestDeployWatch_AFailureBeyondTheFirstRunPageIsStillSeen(t *testing.T) {
	t.Parallel()
	var runs []forgejo.ActionRun
	for i := range deployWatchRunLimit {
		runs = append(runs, deployTestRun(int64(2000+i), "gate.yml", fmt.Sprintf("land/gt-%03d", i),
			deployRunSucceeded, deployTestNow.Add(-time.Duration(i)*time.Minute), "candidate"))
	}
	runs = append(runs, deployTestRun(1153, deployWorkflowStaging, "main", deployRunFailed, deployTestNow.Add(-time.Hour), "fix: guard the retry"))
	api := &fakeDeployForgejo{runs: runs}
	w, rec := newTestDeployWatch(t, api)

	w.poll(context.Background())

	raised := rec.raisedAlerts()
	if len(raised) != 1 {
		t.Fatalf("raised = %+v, want the failure the second page holds", raised)
	}
	if want := deployFailedKeyPrefix + deployTestRepo + ":" + deployWorkflowStaging; raised[0].Key != want {
		t.Errorf("key = %q, want %q", raised[0].Key, want)
	}
	if api.lists != 2 {
		t.Errorf("list calls = %d (%v), want the second page read", api.lists, api.pages)
	}
}

// TestDeployWatch_AWindowWiderThanThePageBudgetIsSaid: paging stops at a
// budget, and a repository that fills it with runs inside the window is a
// repository the watch cannot see all of. It says so rather than passing for a
// quiet one.
func TestDeployWatch_AWindowWiderThanThePageBudgetIsSaid(t *testing.T) {
	t.Parallel()
	var runs []forgejo.ActionRun
	for i := range deployWatchRunLimit * deployWatchRunPages {
		runs = append(runs, deployTestRun(int64(3000+i), "gate.yml", fmt.Sprintf("land/gt-%03d", i),
			deployRunSucceeded, deployTestNow.Add(-time.Duration(i)*time.Minute), "candidate"))
	}
	api := &fakeDeployForgejo{runs: runs}
	w, _ := newTestDeployWatch(t, api)
	var logged []string
	w.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	w.poll(context.Background())

	if api.lists != deployWatchRunPages {
		t.Errorf("list calls = %d (%v), want the page budget spent once", api.lists, api.pages)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "24h0m0s") {
		t.Errorf("log = %v, want one line naming the window the pages reach back", logged)
	}
}

// TestDeployWatch_StateSurvivesARestart pins what the state file is for: a
// daemon that restarts while a deploy is red neither re-raises the alert nor
// pushes it to a person again.
func TestDeployWatch_StateSurvivesARestart(t *testing.T) {
	t.Parallel()
	run := deployTestRun(77, deployWorkflowDeploy, "v0.1.3", deployRunFailed, deployTestNow.Add(-time.Minute), "chore: release v0.1.3")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{run}}
	w, _ := newTestDeployWatch(t, api)
	w.poll(context.Background())

	// The next daemon reads the same state file.
	next, nextRec := newTestDeployWatch(t, api)
	next.State = w.State
	next.poll(context.Background())

	if raised := nextRec.raisedAlerts(); len(raised) != 0 {
		t.Errorf("raised = %+v, want nothing: the alert state file says this run is already escalated", raised)
	}
	if pushes := nextRec.pushes(); len(pushes) != 0 {
		t.Errorf("pushes = %+v, want nothing: the push for this run already went out", pushes)
	}
	// And a green run still clears what the earlier daemon raised.
	api.runs = append(api.runs, deployTestRun(78, deployWorkflowDeploy, "v0.1.3", deployRunSucceeded, deployTestNow.Add(-time.Second), "release"))
	next.poll(context.Background())
	if cleared := nextRec.clearedAlerts(); len(cleared) != 1 {
		t.Errorf("cleared = %+v, want the alert a previous daemon raised", cleared)
	}
}

// TestDeployWatch_ARaiseThatDidNotLandIsRetried: the state records a
// delivery, so a dropped escalation is raised again by the next poll instead
// of being remembered as sent.
func TestDeployWatch_ARaiseThatDidNotLandIsRetried(t *testing.T) {
	t.Parallel()
	run := deployTestRun(1153, deployWorkflowStaging, "main", deployRunFailed, deployTestNow.Add(-time.Hour), "fix: guard the retry")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{run}}
	w, rec := newTestDeployWatch(t, api)
	rec.raiseErr = fmt.Errorf("gt escalate: exit 1")

	w.poll(context.Background())

	rec.mu.Lock()
	rec.raiseErr = nil
	rec.mu.Unlock()
	w.poll(context.Background())

	if raised := rec.raisedAlerts(); len(raised) != 1 {
		t.Errorf("raised = %+v, want the retry that landed", raised)
	}
}

// TestDeployWatch_AClearThatDidNotLandIsRetried: the clear is the alert's
// whole purpose, so one that did not land leaves the alert open and the next
// poll tries again.
func TestDeployWatch_AClearThatDidNotLandIsRetried(t *testing.T) {
	t.Parallel()
	failed := deployTestRun(1153, deployWorkflowStaging, "main", deployRunFailed, deployTestNow.Add(-time.Hour), "fix: guard the retry")
	green := deployTestRun(1154, deployWorkflowStaging, "main", deployRunSucceeded, deployTestNow.Add(-time.Minute), "fix: guard the retry")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{failed}}
	w, rec := newTestDeployWatch(t, api)
	w.poll(context.Background())

	rec.mu.Lock()
	rec.clearErr = fmt.Errorf("gt escalate clear: exit 1")
	rec.mu.Unlock()
	api.runs = []forgejo.ActionRun{failed, green}
	w.poll(context.Background())

	if cleared := rec.clearedAlerts(); len(cleared) != 0 {
		t.Fatalf("cleared = %+v, want the dropped clear recorded nowhere", cleared)
	}
	if failures := w.failures(); len(failures) != 1 {
		t.Errorf("failures = %+v, want the item kept while the alert it belongs to is still open", failures)
	}

	rec.mu.Lock()
	rec.clearErr = nil
	rec.mu.Unlock()
	w.poll(context.Background())

	if cleared := rec.clearedAlerts(); len(cleared) != 1 {
		t.Errorf("cleared = %+v, want the retry that landed", cleared)
	}
	if failures := w.failures(); len(failures) != 0 {
		t.Errorf("failures = %+v, want the picture green once the clear landed", failures)
	}
}

// TestDeployWatch_APushThatDidNotLandIsRetried: the push goes out once per
// run, and a push that failed was not delivered, so it is owed again.
func TestDeployWatch_APushThatDidNotLandIsRetried(t *testing.T) {
	t.Parallel()
	run := deployTestRun(77, deployWorkflowDeploy, "v0.1.3", deployRunFailed, deployTestNow.Add(-time.Minute), "chore: release v0.1.3")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{run}}
	w, rec := newTestDeployWatch(t, api)
	rec.pushErr = fmt.Errorf("notify: exit 1")

	w.poll(context.Background())
	if raised := rec.raisedAlerts(); len(raised) != 1 {
		t.Fatalf("raised = %+v, want the escalation the push rides on", raised)
	}

	rec.mu.Lock()
	rec.pushErr = nil
	rec.mu.Unlock()
	w.poll(context.Background())

	if pushes := rec.pushes(); len(pushes) != 1 {
		t.Errorf("pushes = %+v, want the retry that landed", pushes)
	}
	if raised := rec.raisedAlerts(); len(raised) != 1 {
		t.Errorf("raised = %+v, want no second escalation for a push retry", raised)
	}
}

// TestDeployWatch_AnUnreadableStateFileStillWatches: a state file the watch
// cannot parse is logged and treated as empty, so the deploy that fails while
// it is corrupt is still raised.
func TestDeployWatch_AnUnreadableStateFileStillWatches(t *testing.T) {
	t.Parallel()
	run := deployTestRun(1153, deployWorkflowStaging, "main", deployRunFailed, deployTestNow.Add(-time.Hour), "fix: guard the retry")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{run}}
	w, rec := newTestDeployWatch(t, api)
	w.State = filepath.Join(t.TempDir(), deployWatchStateFile)
	if err := os.WriteFile(w.State, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("writing the state file: %v", err)
	}
	var logged []string
	w.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	w.poll(context.Background())

	if raised := rec.raisedAlerts(); len(raised) != 1 {
		t.Errorf("raised = %+v, want the failure raised over the unreadable state", raised)
	}
	if len(logged) == 0 || !strings.Contains(strings.Join(logged, "\n"), "reading") {
		t.Errorf("log = %v, want the unreadable state file said once", logged)
	}
}

// TestDeployWatch_ATitleWithControlCharactersIsSanitised: a commit title is
// whatever its author wrote, and it becomes a bead title.
func TestDeployWatch_ATitleWithControlCharactersIsSanitised(t *testing.T) {
	t.Parallel()
	run := deployTestRun(1153, deployWorkflowStaging, "main", deployRunFailed, deployTestNow.Add(-time.Hour), "fix:\x00 the \x1b[31mred\x1b[0m retry\n(gt-abc)")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{run}}
	w, rec := newTestDeployWatch(t, api)

	w.poll(context.Background())

	raised := rec.raisedAlerts()
	if len(raised) != 1 {
		t.Fatalf("raised = %+v, want one escalation", raised)
	}
	if strings.ContainsAny(raised[0].Message, "\x00\x1b") || strings.Contains(raised[0].Message, "\n(gt-abc)") {
		t.Errorf("message = %q, want the control characters gone and the newline folded into the line", raised[0].Message)
	}
	if !strings.Contains(raised[0].Message, "retry (gt-abc)") {
		t.Errorf("message = %q, want the title's text kept", raised[0].Message)
	}
}

// TestDeployWatch_NoClientIsLoggedOnceAndRaisesNothing: a Forgejo instance the
// watch has no client for is a read it cannot make, which is not a green
// deploy and not a failure either.
func TestDeployWatch_NoClientIsLoggedOnceAndRaisesNothing(t *testing.T) {
	t.Parallel()
	w, rec := newTestDeployWatch(t, &fakeDeployForgejo{})
	w.API = func(string) (deployRunAPI, error) { return nil, fmt.Errorf("no token") }
	var logged []string
	w.Logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }

	w.poll(context.Background())
	w.poll(context.Background())

	if raised := rec.raisedAlerts(); len(raised) != 0 {
		t.Errorf("raised = %+v, want nothing from a repository no client can read", raised)
	}
	if len(logged) != 1 {
		t.Errorf("log = %v, want the one reason, not one per poll", logged)
	}
}

// TestDeployWatch_StartsOnALaterPassWhenItCouldNotBuildOne: the landing worker
// manager calls the starter on every pass, and the first passes are exactly
// when a town's rigs or viewer token may not be there yet. A watch that gave
// up there would be an alarm that is off, so the next pass tries again.
func TestDeployWatch_StartsOnALaterPassWhenItCouldNotBuildOne(t *testing.T) {
	t.Parallel()
	d, _ := daemonWithRecorder(t)
	d.ctx = t.Context()
	var attempts int
	built := make(chan struct{}, 1)
	d.deployWatchBuild = func() (*deployWatch, string) {
		attempts++
		if attempts < 3 {
			return nil, "no rig names a Forgejo repository"
		}
		w := newDeployWatch()
		w.Repos = func() []deployRepo { return nil }
		w.State = filepath.Join(t.TempDir(), deployWatchStateFile)
		w.Logf = func(string, ...any) {}
		built <- struct{}{}
		return w, ""
	}

	d.startDeployWatch()
	d.startDeployWatch()
	if d.watchDeploy() != nil {
		t.Fatal("the watch started from a pass that could not build one")
	}
	d.startDeployWatch()
	waitForSignal(t, built, "the third pass never built the watch")

	if attempts != 3 {
		t.Errorf("build attempts = %d, want one per pass until one succeeds", attempts)
	}
	if d.watchDeploy() == nil {
		t.Error("watchDeploy = nil, want the watch the third pass built")
	}
	// A later pass starts nothing further.
	d.startDeployWatch()
	if attempts != 3 {
		t.Errorf("build attempts = %d, want no fourth attempt once a watch runs", attempts)
	}
}

// TestProductionDeployWatch_WiresTheTown pins the builder the daemon runs: the
// repositories are the landing worker's rigs read from their own Forgejo
// blocks, the state file lives under <town>/.runtime, and the viewer client
// comes from the configured token dir. A town it cannot build one for comes
// back with the reason the starter logs and retries on.
func TestProductionDeployWatch_WiresTheTown(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	const rigName = "fractals"
	writeKnownRigs(t, townRoot, rigName)
	forgejoRigConfig(t, townRoot, rigName)
	tokenDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tokenDir, "forgejo-viewer.env"), []byte("FORGEJO_TOKEN=secret\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	newDaemon := func(root, dir string) *Daemon {
		return &Daemon{logger: discardLogger, config: &Config{TownRoot: root},
			patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{LandingWorker: &LandingWorkerConfig{
				Forgejo: &config.ForgejoWorkerConfig{TokenDir: dir}}}}}
	}

	d := newDaemon(townRoot, tokenDir)
	w, hold := d.productionDeployWatch()
	if w == nil {
		t.Fatalf("productionDeployWatch = nil, %q; want the watch for a rig with a Forgejo repository", hold)
	}
	if hold != "" {
		t.Errorf("hold = %q, want none beside a watch", hold)
	}
	if want := filepath.Join(townRoot, ".runtime", deployWatchStateFile); w.State != want {
		t.Errorf("state = %q, want %q", w.State, want)
	}
	repos := w.Repos()
	want := deployRepo{Rig: rigName, Owner: "gastown", Name: "gastown", APIBase: "https://forgejo.example/api/v1"}
	if len(repos) != 1 || repos[0] != want {
		t.Fatalf("repos = %+v, want %+v", repos, want)
	}
	if _, err := w.API(repos[0].APIBase); err != nil {
		t.Errorf("client = %v, want the viewer client the token dir builds", err)
	}

	// The two holds the starter retries on: a town with no rig to poll, and
	// one whose viewer token is not there yet.
	if w, hold := newDaemon(t.TempDir(), tokenDir).productionDeployWatch(); w != nil || !strings.Contains(hold, "no rig names a Forgejo repository") {
		t.Errorf("productionDeployWatch = %v, %q; want no watch and the rig reason", w, hold)
	}
	if w, hold := newDaemon(townRoot, t.TempDir()).productionDeployWatch(); w != nil || !strings.Contains(hold, "token") {
		t.Errorf("productionDeployWatch = %v, %q; want no watch and the token reason", w, hold)
	}
}

// endpointTransport answers each request in process, so the test exercises the
// real client's request building without a socket.
type endpointTransport struct{ h http.Handler }

func (tr endpointTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	tr.h.ServeHTTP(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// TestDeployWatch_TokenNeverReachesTheAlert drives the watch through the real
// viewer client: the token authenticates the read and appears nowhere else —
// not in a run's text, and so not in an escalation or a push.
func TestDeployWatch_TokenNeverReachesTheAlert(t *testing.T) {
	t.Parallel()
	const secret = "viewer-token-that-must-not-be-printed"
	var auth string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/actions/runs"):
			// A release run, so both the escalation and the push are written
			// and both are checked for the token.
			_, _ = io.WriteString(w, `{"total_count":1,"workflow_runs":[{"id":77,"status":"failure","title":"chore: release v0.1.3","prettyref":"v0.1.3","workflow_id":".forgejo/workflows/deploy.yml","created":"2026-10-15T17:00:00Z","html_url":"http://forgejo.test/acme/fractals-nextjs/actions/runs/77"}]}`)
		case strings.HasSuffix(r.URL.Path, "/jobs"):
			_, _ = io.WriteString(w, `[{"id":1,"run_id":77,"name":"deploy-production","status":"failure"}]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	client, err := forgejo.NewClient(config.ForgejoRoleViewer,
		forgejo.WithToken(secret),
		forgejo.WithBaseURL("http://forgejo.test/api/v1"),
		forgejo.WithHTTPClient(&http.Client{Transport: endpointTransport{h: handler}}))
	if err != nil {
		t.Fatalf("forgejo client: %v", err)
	}
	w, rec := newTestDeployWatch(t, client)

	w.poll(context.Background())

	if auth != "token "+secret {
		t.Errorf("Authorization = %q, want the viewer token", auth)
	}
	raised := rec.raisedAlerts()
	if len(raised) != 1 {
		t.Fatalf("raised = %+v, want one escalation", raised)
	}
	if strings.Contains(raised[0].Message, secret) {
		t.Errorf("the escalation carries the token: %q", raised[0].Message)
	}
	pushes := rec.pushes()
	if len(pushes) != 1 {
		t.Fatalf("pushes = %+v, want the high severity push this run earns", pushes)
	}
	for _, p := range pushes {
		if strings.Contains(p.Title+p.Body, secret) {
			t.Errorf("the push carries the token: %+v", p)
		}
	}
}

// TestDeployWatch_StateFileIsReadable pins the state file's shape, the one
// thing that outlives the process.
func TestDeployWatch_StateFileIsReadable(t *testing.T) {
	t.Parallel()
	run := deployTestRun(1153, deployWorkflowStaging, "main", deployRunFailed, deployTestNow.Add(-time.Hour), "fix: guard the retry")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{run}}
	w, _ := newTestDeployWatch(t, api)
	w.State = filepath.Join(t.TempDir(), "deploy-watch.json")

	w.poll(context.Background())

	data, err := os.ReadFile(w.State)
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	var st deployWatchState
	if err := json.Unmarshal([]byte(data), &st); err != nil {
		t.Fatalf("state file %s: %v", data, err)
	}
	key := "deploy-failed:" + deployTestRepo + ":" + deployWorkflowStaging
	if open, ok := st.Open[key]; !ok || open.Run != 1153 {
		t.Errorf("state = %+v, want %s open on run 1153", st.Open, key)
	}
}

// TestDeployWatch_AStateFileWithoutASummaryGainsOne: the item the attention
// queue shows is part of the record, so a poll that fills one in writes it.
// A daemon that restarted onto a state file with no summary would otherwise
// lose the item of an alert it raised and never re-raise it.
func TestDeployWatch_AStateFileWithoutASummaryGainsOne(t *testing.T) {
	t.Parallel()
	run := deployTestRun(1153, deployWorkflowStaging, "main", deployRunFailed, deployTestNow.Add(-time.Hour), "fix: guard the retry")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{run}}
	w, rec := newTestDeployWatch(t, api)
	key := deployFailedKeyPrefix + deployTestRepo + ":" + deployWorkflowStaging
	// What a state file written by an older daemon holds: the run and the
	// severity, and nothing the item shows.
	data, err := json.Marshal(deployWatchState{Open: map[string]deployOpenRun{
		key: {Run: 1153, Severity: config.SeverityMedium},
	}})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if err := os.WriteFile(w.State, data, 0o600); err != nil {
		t.Fatalf("writing the state file: %v", err)
	}

	w.poll(context.Background())

	if raised := rec.raisedAlerts(); len(raised) != 0 {
		t.Errorf("raised = %+v, want nothing: the state file names this run already", raised)
	}
	stored, err := os.ReadFile(w.State)
	if err != nil {
		t.Fatalf("state file: %v", err)
	}
	var st deployWatchState
	if err := json.Unmarshal(stored, &st); err != nil {
		t.Fatalf("state file %s: %v", stored, err)
	}
	if open := st.Open[key]; open.Summary == "" || open.Rig != "fractals" {
		t.Errorf("state = %+v, want the item the alert was raised with written alongside it", st.Open[key])
	}
}

// deployTestWorkflow is the watch's own table entry for a workflow file, so
// a test asserts against the ref classes the poll filters on rather than a
// copy of them.
func deployTestWorkflow(t *testing.T, file string) deployWorkflow {
	t.Helper()
	for _, wf := range deployWorkflows {
		if wf.File == file {
			return wf
		}
	}
	t.Fatalf("deployWorkflows has no entry for %s", file)
	return deployWorkflow{}
}

// TestDeployWatch_ACancelledRunCountsAsAFailureAndAnInFlightOneDoesNot pins
// the two statuses a run may end or hold that are not a plain failure: the
// bead counts a cancelled run as failed, and a newer run still in flight
// says nothing either way, so it clears nothing.
func TestDeployWatch_ACancelledRunCountsAsAFailureAndAnInFlightOneDoesNot(t *testing.T) {
	t.Parallel()
	cancelled := deployTestRun(70, deployWorkflowTeardown, "v0.1.3", deployRunCancelled, deployTestNow.Add(-time.Hour), "teardown v0.1.3")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{cancelled}}
	w, rec := newTestDeployWatch(t, api)

	w.poll(context.Background())

	raised := rec.raisedAlerts()
	if len(raised) != 1 || raised[0].Severity != config.SeverityHigh {
		t.Fatalf("raised = %+v, want one high escalation for a cancelled release run", raised)
	}

	running := deployTestRun(71, deployWorkflowTeardown, "v0.1.3", "running", deployTestNow.Add(-time.Minute), "teardown v0.1.3")
	api.runs = []forgejo.ActionRun{cancelled, running}
	w.poll(context.Background())

	if cleared := rec.clearedAlerts(); len(cleared) != 0 {
		t.Errorf("cleared = %+v, want nothing: a run still in flight is not a green one", cleared)
	}
	if raised := rec.raisedAlerts(); len(raised) != 1 {
		t.Errorf("raised = %+v, want no second escalation for the run in flight", raised)
	}
	// The alert the cancelled run raised is still open, so its item is still
	// in the picture: the run in flight supersedes nothing.
	if failures := w.failures(); len(failures) != 1 || failures[0].RunID != 70 {
		t.Errorf("failures = %+v, want the cancelled run's item kept", failures)
	}
}

// TestDeployWatch_WorkflowRefAndSeverity is the classification table: which
// workflows are deploys, on which refs, at which severity. A run that matches
// no row is not a deploy and raises nothing.
func TestDeployWatch_WorkflowRefAndSeverity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		workflow string
		ref      string
		jobs     []string
		wantRef  string
		wantSev  string
	}{
		{"staging on main", deployWorkflowStaging, "main", nil, deployRefMain, config.SeverityMedium},
		{"staging on a tag", deployWorkflowStaging, "v0.1.3", nil, deployRefTag, ""},
		{"deploy on a tag", deployWorkflowDeploy, "v0.1.3", nil, deployRefTag, config.SeverityHigh},
		{"deploy on main", deployWorkflowDeploy, "main", nil, deployRefMain, ""},
		{"teardown on a tag", deployWorkflowTeardown, "v1.0.0", nil, deployRefTag, config.SeverityHigh},
		{"teardown on main", deployWorkflowTeardown, "main", nil, deployRefMain, ""},
		{"health prod smoke", deployWorkflowHealth, "v0.1.3", []string{deployHealthProdJob}, deployRefTag, config.SeverityHigh},
		{"health on a tag, jobs unread", deployWorkflowHealth, "v0.1.3", nil, deployRefTag, config.SeverityHigh},
		{"health staging check", deployWorkflowHealth, "main", []string{"staging-integration"}, deployRefMain, config.SeverityMedium},
		{"health on main, jobs unread", deployWorkflowHealth, "main", nil, deployRefMain, config.SeverityMedium},
		{"health on main naming a prod smoke", deployWorkflowHealth, "main", []string{deployHealthProdJob}, deployRefMain, config.SeverityHigh},
		{"a candidate branch", deployWorkflowStaging, "land/gt-abc", nil, "", ""},
		{"a branch named like a tag", deployWorkflowDeploy, "verify", nil, "", ""},
		{"a branch named v2-migration", deployWorkflowDeploy, "v2-migration", nil, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if class := deployRefClass(tc.ref); class != tc.wantRef {
				t.Errorf("deployRefClass(%q) = %q, want %q", tc.ref, class, tc.wantRef)
			}
			if got := deploySeverity(deployTestWorkflow(t, tc.workflow), deployRefClass(tc.ref), tc.jobs); got != tc.wantSev {
				t.Errorf("%s on %s = %q, want %q", tc.workflow, tc.ref, got, tc.wantSev)
			}
		})
	}
	for workflowID, want := range map[string]string{
		".forgejo/workflows/" + deployWorkflowStaging: deployWorkflowStaging,
		deployWorkflowDeploy:                          deployWorkflowDeploy,
		".forgejo/workflows/gate.yml":                 "",
		"":                                            "",
	} {
		if got := deployRunWorkflow(workflowID); got != want {
			t.Errorf("deployRunWorkflow(%q) = %q, want %q", workflowID, got, want)
		}
	}
}

// TestDeployWatch_EachRepositoryIsReadThroughItsOwnInstance pins the seam the
// API is: a rig is read through the host its own config names, so a
// same-named repository on another Forgejo instance is never read as this
// rig's.
func TestDeployWatch_EachRepositoryIsReadThroughItsOwnInstance(t *testing.T) {
	t.Parallel()
	one := &fakeDeployForgejo{runs: []forgejo.ActionRun{deployTestRun(1, deployWorkflowStaging, "main", deployRunFailed, deployTestNow.Add(-time.Minute), "one")}}
	two := &fakeDeployForgejo{runs: []forgejo.ActionRun{deployTestRun(2, deployWorkflowStaging, "main", deployRunFailed, deployTestNow.Add(-time.Minute), "two")}}
	byBase := map[string]deployRunAPI{
		"http://one.test/api/v1": one,
		"http://two.test/api/v1": two,
	}
	w, rec := newTestDeployWatch(t, one)
	var asked []string
	w.API = func(base string) (deployRunAPI, error) {
		asked = append(asked, base)
		api, ok := byBase[base]
		if !ok {
			return nil, fmt.Errorf("no client for %s", base)
		}
		return api, nil
	}
	w.Repos = func() []deployRepo {
		return []deployRepo{
			{Rig: "one", Owner: "acme", Name: deployTestName, APIBase: "http://one.test/api/v1"},
			{Rig: "two", Owner: "acme", Name: deployTestName, APIBase: "http://two.test/api/v1"},
		}
	}

	w.poll(context.Background())

	if one.lists != 1 || two.lists != 1 {
		t.Errorf("lists = %d and %d, want each repository read once through its own instance", one.lists, two.lists)
	}
	// Two asks per repository: its runs, then the failed run's jobs.
	instances := map[string]bool{}
	for _, base := range asked {
		instances[base] = true
	}
	if len(instances) != 2 || !instances["http://one.test/api/v1"] || !instances["http://two.test/api/v1"] {
		t.Errorf("clients asked for = %v, want each repository's own API base and no other", asked)
	}
	if raised := rec.raisedAlerts(); len(raised) != 2 {
		t.Errorf("raised = %+v, want one per repository", raised)
	}
}

// TestDeployWatch_AGreenRunClearsAnAlertWhoseStateFileIsGone: the state file
// says which alerts this process raised, and a town whose file was deleted
// still has the escalation beads. A green run closes them once, then trusts
// the state it wrote.
func TestDeployWatch_AGreenRunClearsAnAlertWhoseStateFileIsGone(t *testing.T) {
	t.Parallel()
	green := deployTestRun(1154, deployWorkflowStaging, "main", deployRunSucceeded, deployTestNow.Add(-time.Minute), "fix: guard the retry")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{green}}
	w, rec := newTestDeployWatch(t, api)

	w.poll(context.Background())

	cleared := rec.clearedAlerts()
	if len(cleared) != 1 {
		t.Fatalf("cleared = %+v, want one clear for a state file that was never written", cleared)
	}
	if !strings.Contains(cleared[0].Reason, "1154") {
		t.Errorf("clear reason = %q, want the run that cleared it", cleared[0].Reason)
	}
	w.poll(context.Background())
	if again := rec.clearedAlerts(); len(again) != 1 {
		t.Errorf("cleared = %+v, want the clear once, not one per poll", again)
	}

	// The next daemon reads the state this one wrote, so it clears nothing.
	next, nextRec := newTestDeployWatch(t, api)
	next.State = w.State
	next.poll(context.Background())
	if cleared := nextRec.clearedAlerts(); len(cleared) != 0 {
		t.Errorf("cleared = %+v, want nothing once the state file names the alerts", cleared)
	}
}

// TestDeployWatch_ARecordedPushNeedsACommand pins that a town with no
// operational.overseer.silent_push owes no push: nothing is recorded as
// delivered, so configuring the command later still reaches a person.
func TestDeployWatch_ARecordedPushNeedsACommand(t *testing.T) {
	t.Parallel()
	run := deployTestRun(77, deployWorkflowDeploy, "v0.1.3", deployRunFailed, deployTestNow.Add(-time.Minute), "chore: release v0.1.3")
	api := &fakeDeployForgejo{runs: []forgejo.ActionRun{run}}
	w, rec := newTestDeployWatch(t, api)
	w.Push = nil

	w.poll(context.Background())

	if pushes := rec.pushes(); len(pushes) != 0 {
		t.Errorf("pushes = %+v, want none with no command configured", pushes)
	}

	w.Push = rec.push
	w.poll(context.Background())

	if pushes := rec.pushes(); len(pushes) != 1 {
		t.Errorf("pushes = %+v, want the push for the failure that still stands", pushes)
	}
}

// TestCollectDeployFailures_ItemsCarryTheRuleAndOwner pins the queue's half:
// the failures the watch found appear as deploy.failed items the dashboard's
// Attention panel and the wake monitor read.
func TestCollectDeployFailures_ItemsCarryTheRuleAndOwner(t *testing.T) {
	t.Parallel()
	d, _ := daemonWithRecorder(t)
	w := newDeployWatch()
	w.failing = map[string][]deployFailure{
		"fractals": {{
			Rig: "fractals", Repo: deployTestRepo, Workflow: deployWorkflowStaging,
			Key:      "deploy-failed:" + deployTestRepo + ":" + deployWorkflowStaging,
			Severity: config.SeverityMedium, RunID: 1153,
			Summary: deployTestRepo + " " + deployWorkflowStaging + " run 1153 failed on main",
		}},
	}
	d.deployW.Store(w)

	s := &attentionSources{d: d, now: deployTestNow}
	items, err := s.collectDeployFailures(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one", items)
	}
	got := items[0]
	if got.Kind != attention.KindDeployFailed {
		t.Errorf("kind = %q, want %q", got.Kind, attention.KindDeployFailed)
	}
	if got.Rig != "fractals" || !strings.Contains(got.Summary, "run 1153") {
		t.Errorf("item = %+v, want the rig and the run", got)
	}
	rule := attention.RuleFor(got)
	if rule != attention.RuleDeployFailed {
		t.Errorf("rule = %q, want %q", rule, attention.RuleDeployFailed)
	}
	if flags, ok := attention.Flags(rule); !ok || !flags.Wake {
		t.Errorf("flags = %+v, want a rule that wakes", flags)
	}
}
