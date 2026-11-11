package daemon

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/imagevalues"
)

const (
	// actionsTestRunsBase is the run listing and delete path of the repository
	// the fake serves, sloan/gastown, under the API root the client is given.
	actionsTestRunsBase = "/api/v1/repos/sloan/gastown/actions/runs"

	// actionsTestCommit is the commit the installed pair names. Its green ref is
	// actionsTestGreenRef, and a run on that branch is never deleted.
	actionsTestCommit   = "0123456789abcdef0123456789abcdef01234567"
	actionsTestGreenRef = "green/0123456789ab"
)

// actionsOld is a run created forty days before the test clock, past the
// thirty-day window; actionsYoung is one created a day before it.
var (
	actionsOld   = diskMaintNow.Add(-40 * 24 * time.Hour).Format(time.RFC3339)
	actionsYoung = diskMaintNow.Add(-24 * time.Hour).Format(time.RFC3339)
)

// fakeActionsForgejo answers the two calls the retention step makes over one
// repository: a paged run listing, newest first, and a delete. A run it deletes
// leaves the listing, as it would on the server.
type fakeActionsForgejo struct {
	mu sync.Mutex
	// runs is the repository's runs, keyed by ID.
	runs map[int64]forgejo.ActionRun
	// refuseDelete answers the delete of a run with the given status, leaving
	// the run in place.
	refuseDelete map[int64]int
	// failListPage answers the listing of that page with a server error; zero
	// fails none.
	failListPage int
	// deleted is the IDs the fake removed, in the order it removed them.
	deleted   []int64
	listCalls int
}

func newFakeActionsForgejo(runs []forgejo.ActionRun) *fakeActionsForgejo {
	f := &fakeActionsForgejo{runs: make(map[int64]forgejo.ActionRun, len(runs)), refuseDelete: map[int64]int{}}
	for _, r := range runs {
		f.runs[r.ID] = r
	}
	return f
}

func (f *fakeActionsForgejo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == actionsTestRunsBase:
		f.listCalls++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if page == f.failListPage {
			http.Error(w, "the listing failed", http.StatusInternalServerError)
			return
		}
		sorted := f.newestFirst()
		start := min((page-1)*limit, len(sorted))
		end := min(start+limit, len(sorted))
		_ = json.NewEncoder(w).Encode(forgejo.RunList{TotalCount: len(sorted), Runs: sorted[start:end]})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, actionsTestRunsBase+"/"):
		id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, actionsTestRunsBase+"/"), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if status, refused := f.refuseDelete[id]; refused {
			w.WriteHeader(status)
			_, _ = fmt.Fprint(w, `{"message":"refused"}`)
			return
		}
		if _, ok := f.runs[id]; !ok {
			http.NotFound(w, r)
			return
		}
		delete(f.runs, id)
		f.deleted = append(f.deleted, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

// newestFirst is the listing order the server answers in.
func (f *fakeActionsForgejo) newestFirst() []forgejo.ActionRun {
	out := make([]forgejo.ActionRun, 0, len(f.runs))
	for _, r := range f.runs {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b forgejo.ActionRun) int { return cmp.Compare(b.ID, a.ID) })
	return out
}

// deletedIDs returns the deleted IDs, sorted, so an assertion does not depend on
// the order the step made the calls in.
func (f *fakeActionsForgejo) deletedIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.deleted)
	slices.Sort(out)
	return out
}

// remaining is how many runs the fake still holds.
func (f *fakeActionsForgejo) remaining() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

// listings is how many listing requests the fake answered.
func (f *fakeActionsForgejo) listings() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCalls
}

// actionsFixtureRuns is 300 runs, IDs 1 to 300. The newest 200 (101 to 300) are
// kept by count whatever their age. Of the 100 beyond them, IDs 10 (running),
// 20 (on the installed green ref), 30 (younger than the window) and 40 (waiting)
// must stay, and the other 96 are the plan.
func actionsFixtureRuns() []forgejo.ActionRun {
	runs := make([]forgejo.ActionRun, 0, 300)
	for id := int64(1); id <= 300; id++ {
		run := forgejo.ActionRun{ID: id, Status: "success", PrettyRef: "main", Created: actionsOld}
		switch id {
		case 10:
			run.Status = "running"
		case 20:
			run.PrettyRef = actionsTestGreenRef
		case 30:
			run.Created = actionsYoung
		case 40:
			run.Status = "waiting"
		case 50:
			run.Status = "failure"
		}
		runs = append(runs, run)
	}
	return runs
}

// actionsWantDeleted is the fixture's plan: IDs 1 to 100 less the four kept.
func actionsWantDeleted() []int64 {
	var out []int64
	for id := int64(1); id <= 100; id++ {
		if !slices.Contains([]int64{10, 20, 30, 40}, id) {
			out = append(out, id)
		}
	}
	return out
}

// writeActionsPair records the installed image pair at actionsTestCommit in the
// town's gastown directory, where the step reads it.
func writeActionsPair(t *testing.T, townRoot string) {
	t.Helper()
	reg := imagevalues.Registry{Scheme: "https", Host: "registry.test:5000", Owner: "town"}
	digest := "sha256:" + strings.Repeat("1", 64)
	v := imagevalues.Values{
		Daemon:  reg.Ref(imagevalues.DaemonImage, digest),
		Polecat: reg.Ref(imagevalues.PolecatImage, digest),
		Commit:  actionsTestCommit,
	}
	require.NoError(t, imagevalues.Write(filepath.Join(townRoot, "gastown", imagevalues.DefaultRelPath), v))
}

// actionsTestDaemon is the disk maintenance harness with the retention block
// set to ret, the patrol in mode, the installed pair recorded, and api serving
// the one repository the step plans for.
func actionsTestDaemon(t *testing.T, ret *DiskMaintenanceActionsRetention, mode string, api actionsRunAPI) (*Daemon, *bytes.Buffer) {
	t.Helper()
	d, buf, _ := diskMaintTestDaemon(t)
	cfg := diskMaintEnabled("", mode)
	cfg.Patrols.DiskMaintenance.ActionsRetention = ret
	d.patrolConfig = cfg
	d.diskMaint.actionsRepos = func() ([]actionsRetentionRepo, error) {
		return []actionsRetentionRepo{{Rig: "gastown", Owner: "sloan", Repo: "gastown", API: api}}, nil
	}
	writeActionsPair(t, d.config.TownRoot)
	return d, buf
}

// handlerTransport answers each request by calling a handler in-process, so the
// fake is reached without a socket: a unit test opens no network connection.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// newActionsServer returns a Forgejo client whose requests the fake answers
// in-process.
func newActionsServer(t *testing.T, fake *fakeActionsForgejo) *forgejo.Client {
	t.Helper()
	api, err := forgejo.NewClient("landing",
		forgejo.WithBaseURL("http://forgejo.test/api/v1"),
		forgejo.WithToken("test-token"),
		forgejo.WithHTTPClient(&http.Client{Transport: handlerTransport{fake}}))
	require.NoError(t, err)
	return api
}

// TestActionsRetentionDeletesExactlyThePlannedRuns drives the real daily pass
// over 300 runs served in pages: the step deletes the runs older than the window
// beyond the newest 200, and leaves the in-progress, green-ref and young runs.
func TestActionsRetentionDeletesExactlyThePlannedRuns(t *testing.T) {
	t.Parallel()
	fake := newFakeActionsForgejo(actionsFixtureRuns())
	d, buf := actionsTestDaemon(t, &DiskMaintenanceActionsRetention{Enabled: true},
		agentconfig.DiskMaintenanceModeApply, newActionsServer(t, fake))

	d.runDiskMaintenance()

	assert.Equal(t, actionsWantDeleted(), fake.deletedIDs())
	assert.Equal(t, 204, fake.remaining(), "the newest 200, the four kept runs and nothing else remain")
	assert.Contains(t, buf.String(), "actions_retention: gastown apply: deleted 96 of 96 planned, 204 kept")
}

// TestActionsRetentionDeletesOnlyWhenEnabledAndApply: a delete needs both the
// block's enabled and the patrol's apply mode. Every other combination logs the
// plan and deletes nothing.
func TestActionsRetentionDeletesOnlyWhenEnabledAndApply(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		enabled bool
		mode    string
	}{
		{"enabled false in apply", false, agentconfig.DiskMaintenanceModeApply},
		{"enabled true in dry run", true, agentconfig.DiskMaintenanceModeDryRun},
		{"enabled false in dry run", false, agentconfig.DiskMaintenanceModeDryRun},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeActionsForgejo(actionsFixtureRuns())
			d, buf := actionsTestDaemon(t, &DiskMaintenanceActionsRetention{Enabled: tc.enabled},
				tc.mode, newActionsServer(t, fake))

			d.runDiskMaintenance()

			assert.Empty(t, fake.deletedIDs(), "a run the plan names is not deleted outside enabled and apply")
			assert.Equal(t, 300, fake.remaining())
			assert.Positive(t, fake.listings(), "the plan is still built, so the log names what would go")
			assert.Contains(t, buf.String(), "actions_retention: gastown dry_run: 96 to delete, 204 kept")
		})
	}
}

// TestActionsRetentionRefusesAFailedListing: a listing that fails on any page
// refuses the repository's plan. A partial listing would name the wrong newest
// runs, so nothing is deleted from it.
func TestActionsRetentionRefusesAFailedListing(t *testing.T) {
	t.Parallel()
	fake := newFakeActionsForgejo(actionsFixtureRuns())
	fake.failListPage = 3
	d, buf := actionsTestDaemon(t, &DiskMaintenanceActionsRetention{Enabled: true},
		agentconfig.DiskMaintenanceModeApply, newActionsServer(t, fake))

	d.runDiskMaintenance()

	assert.Empty(t, fake.deletedIDs())
	assert.Equal(t, 300, fake.remaining())
	assert.Contains(t, buf.String(), "actions_retention: gastown: refused, the runs could not be listed: forgejo answered 500")
}

// TestActionsRetentionRefusesWithoutAnInstalledPair: with no recorded pair there
// is no green ref to protect, so the step refuses before it lists anything.
func TestActionsRetentionRefusesWithoutAnInstalledPair(t *testing.T) {
	t.Parallel()
	fake := newFakeActionsForgejo(actionsFixtureRuns())
	d, buf := actionsTestDaemon(t, &DiskMaintenanceActionsRetention{Enabled: true},
		agentconfig.DiskMaintenanceModeApply, newActionsServer(t, fake))
	require.NoError(t, os.Remove(filepath.Join(d.config.TownRoot, "gastown", imagevalues.DefaultRelPath)))

	d.runDiskMaintenance()

	assert.Zero(t, fake.listings())
	assert.Empty(t, fake.deletedIDs())
	assert.Contains(t, buf.String(), "actions_retention: not running: no installed image pair is recorded")
}

// TestActionsRetentionCountsAFailedDelete: one refused delete leaves its run in
// place and does not stop the others.
func TestActionsRetentionCountsAFailedDelete(t *testing.T) {
	t.Parallel()
	fake := newFakeActionsForgejo(actionsFixtureRuns())
	fake.refuseDelete[50] = http.StatusConflict
	d, buf := actionsTestDaemon(t, &DiskMaintenanceActionsRetention{Enabled: true},
		agentconfig.DiskMaintenanceModeApply, newActionsServer(t, fake))

	d.runDiskMaintenance()

	assert.Equal(t, slices.DeleteFunc(actionsWantDeleted(), func(id int64) bool { return id == 50 }), fake.deletedIDs())
	assert.Equal(t, 205, fake.remaining(), "the refused run stays in the listing")
	assert.Contains(t, buf.String(), "actions_retention: gastown: 1 of 96 deletes failed; the first: forgejo answered 409")
}

// TestActionsRetentionRefusesAnUnreadableInstalledPair: a values file that does
// not parse is an error, not a pair with nothing protected, so the step refuses
// before it lists anything.
func TestActionsRetentionRefusesAnUnreadableInstalledPair(t *testing.T) {
	t.Parallel()
	fake := newFakeActionsForgejo(actionsFixtureRuns())
	d, buf := actionsTestDaemon(t, &DiskMaintenanceActionsRetention{Enabled: true},
		agentconfig.DiskMaintenanceModeApply, newActionsServer(t, fake))
	require.NoError(t, os.WriteFile(filepath.Join(d.config.TownRoot, "gastown", imagevalues.DefaultRelPath), []byte("daemon: [\n"), 0o644))

	d.runDiskMaintenance()

	assert.Zero(t, fake.listings())
	assert.Empty(t, fake.deletedIDs())
	assert.Contains(t, buf.String(), "actions_retention: not running: the installed image pair could not be read")
}

// TestActionsRetentionRefusesAnOutOfRangeSetting: a block built past the config's
// own check still refuses by name, and lists nothing.
func TestActionsRetentionRefusesAnOutOfRangeSetting(t *testing.T) {
	t.Parallel()
	fake := newFakeActionsForgejo(actionsFixtureRuns())
	zero := 0
	d, buf := actionsTestDaemon(t, &DiskMaintenanceActionsRetention{Enabled: true, Days: &zero},
		agentconfig.DiskMaintenanceModeApply, newActionsServer(t, fake))

	d.runDiskMaintenance()

	assert.Zero(t, fake.listings())
	assert.Empty(t, fake.deletedIDs())
	assert.Contains(t, buf.String(), "actions_retention: not running: patrols.disk_maintenance.actions_retention.days = 0")
}

// TestListActionRunsRefusesAListingPastItsPageCap: a listing that still has pages
// at the cap is refused, and one that ends within it is returned whole.
func TestListActionRunsRefusesAListingPastItsPageCap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		maxPages int
		want     int
		wantErr  bool
	}{
		{"the cap ends before the last page", 2, 0, true},
		{"the cap covers every page", 3, 120, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runs := make([]forgejo.ActionRun, 0, 120)
			for id := int64(1); id <= 120; id++ {
				runs = append(runs, forgejo.ActionRun{ID: id})
			}
			api := newActionsServer(t, newFakeActionsForgejo(runs))
			got, err := listActionRunsPaged(context.Background(), api, "sloan", "gastown", tc.maxPages)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "ran past 2 pages")
				return
			}
			require.NoError(t, err)
			assert.Len(t, got, tc.want)
		})
	}
}

// TestActionsRetentionReposResolvesTheLandingRigs: the step plans for the rigs
// the landing worker serves, at each remote's owner and repository. A rig outside
// the allowlist is left out.
func TestActionsRetentionReposResolvesTheLandingRigs(t *testing.T) {
	t.Parallel()
	d, _ := daemonWithRecorder(t)
	f := useGitfake(t, d)
	d.knownRigsCache, d.knownRigsCacheValid = []string{"gastown", "other"}, true
	tokenDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tokenDir, "forgejo-landing.env"), []byte("FORGEJO_TOKEN=test\n"), 0o600))
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{LandingWorker: &LandingWorkerConfig{
		Rigs:    []string{"gastown"},
		Forgejo: &agentconfig.ForgejoWorkerConfig{TokenDir: tokenDir},
	}}}
	for _, rigName := range []string{"gastown", "other"} {
		forgejoRigConfig(t, d.config.TownRoot, rigName)
		f.InitBare(t, filepath.Join(d.config.TownRoot, rigName, ".repo.git"))
	}

	repos, err := d.actionsRetentionRepos()

	require.NoError(t, err)
	require.Len(t, repos, 1)
	assert.Equal(t, "gastown", repos[0].Rig)
	assert.Equal(t, "gastown", repos[0].Owner)
	assert.Equal(t, "gastown", repos[0].Repo)
	assert.NotNil(t, repos[0].API)
}

// TestActionsRetentionReposRefusesAnUnreadableLandingToken: a landing bot token
// the step cannot read refuses the whole list, rather than planning the rest.
func TestActionsRetentionReposRefusesAnUnreadableLandingToken(t *testing.T) {
	t.Parallel()
	d, _ := daemonWithRecorder(t)
	f := useGitfake(t, d)
	d.knownRigsCache, d.knownRigsCacheValid = []string{"gastown"}, true
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{LandingWorker: &LandingWorkerConfig{
		Forgejo: &agentconfig.ForgejoWorkerConfig{TokenDir: t.TempDir()},
	}}}
	forgejoRigConfig(t, d.config.TownRoot, "gastown")
	f.InitBare(t, filepath.Join(d.config.TownRoot, "gastown", ".repo.git"))

	repos, err := d.actionsRetentionRepos()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "the landing bot token is unusable")
	assert.Nil(t, repos)
}

// TestActionsRetentionReposRefusesATokenDirThatDoesNotResolve: a token directory
// that cannot be resolved refuses the list, and the refusal names the cause.
func TestActionsRetentionReposRefusesATokenDirThatDoesNotResolve(t *testing.T) {
	t.Parallel()
	d, _ := daemonWithRecorder(t)
	d.diskMaint.actionsTokenDir = func() (string, error) { return "", errors.New("no home directory") }

	repos, err := d.actionsRetentionRepos()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "the landing bot token directory could not be resolved: no home directory")
	assert.Nil(t, repos)
}

// TestActionsRetentionReposKeepsARemoteCredentialOutOfTheRefusal: a remote that
// names no repository is refused by rig and setting. The URL it carries, which
// here holds a credential, is not echoed into the error.
func TestActionsRetentionReposKeepsARemoteCredentialOutOfTheRefusal(t *testing.T) {
	t.Parallel()
	d, _ := daemonWithRecorder(t)
	f := useGitfake(t, d)
	d.knownRigsCache, d.knownRigsCacheValid = []string{"gastown"}, true
	tokenDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(tokenDir, "forgejo-landing.env"), []byte("FORGEJO_TOKEN=test\n"), 0o600))
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{LandingWorker: &LandingWorkerConfig{
		Forgejo: &agentconfig.ForgejoWorkerConfig{TokenDir: tokenDir},
	}}}
	rigPath := filepath.Join(d.config.TownRoot, "gastown")
	require.NoError(t, os.MkdirAll(filepath.Join(rigPath, ".repo.git"), 0o755))
	writeDaemonRigConfigFile(t, rigPath, `{"type":"rig","version":1,"name":"gastown","default_branch":"main",
		"merge_queue":{"forgejo":{"remote_url":"https://bot:hunter2@forgejo.example/","gate_workflow":"gate","bots":{"landing":"gt-landing"}}}}`)
	f.InitBare(t, filepath.Join(rigPath, ".repo.git"))

	repos, err := d.actionsRetentionRepos()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "rig gastown: its merge_queue.forgejo remote_url does not name a repository")
	assert.NotContains(t, err.Error(), "hunter2")
	assert.Nil(t, repos)
}

// TestPlanActionsRetention is the planner in isolation: each row is one rule.
func TestPlanActionsRetention(t *testing.T) {
	t.Parallel()
	now := diskMaintNow
	old := now.Add(-40 * 24 * time.Hour).Format(time.RFC3339)
	young := now.Add(-24 * time.Hour).Format(time.RFC3339)
	// atCutoff is exactly thirty days old: the window is "older than", so it stays.
	atCutoff := now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	run := func(id int64, status, ref, created string) forgejo.ActionRun {
		return forgejo.ActionRun{ID: id, Status: status, PrettyRef: ref, Created: created}
	}
	cases := []struct {
		name    string
		runs    []forgejo.ActionRun
		want    []int64
		wantErr bool
	}{
		{"an old finished run beyond the keep is planned",
			[]forgejo.ActionRun{run(1, "success", "main", old), run(2, "success", "main", old), run(3, "success", "main", old)},
			[]int64{1}, false},
		{"a run that is only the newest keep is not planned",
			[]forgejo.ActionRun{run(1, "success", "main", old), run(2, "success", "main", old)},
			nil, false},
		{"a run exactly at the window is kept",
			[]forgejo.ActionRun{run(1, "success", "main", atCutoff), run(2, "success", "main", young), run(3, "success", "main", young)},
			nil, false},
		{"a failed run is finished and planned",
			[]forgejo.ActionRun{run(1, "failure", "main", old), run(2, "success", "main", young), run(3, "success", "main", young)},
			[]int64{1}, false},
		{"a cancelled run is finished and planned",
			[]forgejo.ActionRun{run(1, "cancelled", "main", old), run(2, "success", "main", young), run(3, "success", "main", young)},
			[]int64{1}, false},
		{"a skipped run is finished and planned",
			[]forgejo.ActionRun{run(1, "skipped", "main", old), run(2, "success", "main", young), run(3, "success", "main", young)},
			[]int64{1}, false},
		{"a running run is kept",
			[]forgejo.ActionRun{run(1, "running", "main", old), run(2, "success", "main", young), run(3, "success", "main", young)},
			nil, false},
		{"a waiting run is kept",
			[]forgejo.ActionRun{run(1, "waiting", "main", old), run(2, "success", "main", young), run(3, "success", "main", young)},
			nil, false},
		{"a blocked run is kept",
			[]forgejo.ActionRun{run(1, "blocked", "main", old), run(2, "success", "main", young), run(3, "success", "main", young)},
			nil, false},
		{"a status the step does not know is kept",
			[]forgejo.ActionRun{run(1, "unknown", "main", old), run(2, "success", "main", young), run(3, "success", "main", young)},
			nil, false},
		{"a run on the installed green ref is kept",
			[]forgejo.ActionRun{run(1, "success", actionsTestGreenRef, old), run(2, "success", "main", young), run(3, "success", "main", young)},
			nil, false},
		{"a run listed twice is planned once",
			[]forgejo.ActionRun{run(1, "success", "main", old), run(1, "success", "main", old), run(2, "success", "main", young), run(3, "success", "main", young)},
			[]int64{1}, false},
		{"an unreadable created time refuses the plan",
			[]forgejo.ActionRun{run(1, "success", "main", "yesterday"), run(2, "success", "main", young), run(3, "success", "main", young)},
			nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan, err := planActionsRetention(tc.runs, now, 30, 2, actionsTestGreenRef)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			var got []int64
			for _, r := range plan.Delete {
				got = append(got, r.ID)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}
