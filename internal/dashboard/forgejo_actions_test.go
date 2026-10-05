package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// stamp is an RFC3339 time n minutes past the tests' base instant, which is
// what the API sends for a run's created, started and stopped.
func stamp(n int) string {
	return time.Date(2026, 10, 5, 10, n, 0, 0, time.UTC).Format(time.RFC3339)
}

// testRun builds a run carrying the fields the Actions section reads. The URL
// names it, since the row the page gets is not the API's run.
func testRun(id int64, title, status string, created, stopped int, d time.Duration) forgejo.ActionRun {
	run := forgejo.ActionRun{
		ID:       id,
		Title:    title,
		Status:   status,
		Event:    "push",
		Created:  stamp(created),
		Duration: int64(d),
		HTMLURL:  "https://forgejo.test/runs/" + strconv.FormatInt(id, 10),
	}
	if stopped >= 0 {
		run.Stopped = stamp(stopped)
	}
	return run
}

// doneRepo is a repo whose runs were read.
func doneRepo(repo string, current, completed []forgejo.ActionRun) repoRuns {
	return repoRuns{repo: repo, current: current, completed: completed}
}

// urls names the rows the way the tests address them, the run each one is.
func urls(rows []ForgejoAction) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.URL)
	}
	return out
}

func TestMergeForgejoRunsOrdersCurrentOldestFirst(t *testing.T) {
	t.Parallel()
	// The API serves a filtered page newest first, so the merge has to turn the
	// current runs the other way round: a reader wants the run that has been
	// waiting longest at the top.
	got := mergeForgejoRuns([]repoRuns{
		doneRepo("sloan/beads",
			[]forgejo.ActionRun{
				testRun(3, "land: c (dddd4444) onto main", "waiting", 20, -1, 0),
				testRun(1, "land: a (aaaa1111) onto main", "running", 5, -1, 0),
			}, nil),
		doneRepo("sloan/mango",
			[]forgejo.ActionRun{testRun(2, "land: b (bbbb2222) onto main", "blocked", 10, -1, 0)}, nil),
	})
	require.NotNil(t, got)
	require.Len(t, got.Current, 3)
	assert.Equal(t, []string{"https://forgejo.test/runs/1", "https://forgejo.test/runs/2", "https://forgejo.test/runs/3"},
		urls(got.Current), "oldest first, across repos")
	assert.Equal(t, ForgejoActionStats{Running: 1, Queued: 2}, got.Stats)
}

func TestMergeForgejoRunsKeepsTheNewestFiveCompleted(t *testing.T) {
	t.Parallel()
	var runs []forgejo.ActionRun
	for i := 0; i < 8; i++ {
		runs = append(runs, testRun(int64(i+1),
			"land: polecat/mica/gt-fn9e6.4"+strconv.Itoa(i)+"+muvk2gi0 (aaaa1111) onto main",
			"success", i, i+1, time.Duration(i+1)*time.Second))
	}
	got := mergeForgejoRuns([]repoRuns{doneRepo("sloan/beads", nil, runs)})
	require.NotNil(t, got)
	require.Len(t, got.Recent, forgejoRecentKept, "the section lists five")
	assert.Equal(t, []string{
		"https://forgejo.test/runs/8", "https://forgejo.test/runs/7", "https://forgejo.test/runs/6",
		"https://forgejo.test/runs/5", "https://forgejo.test/runs/4",
	}, urls(got.Recent), "the newest five, newest first")
	assert.Equal(t, "gt-fn9e6.47", got.Recent[0].Bead, "the bead is parsed out of the run title")
	assert.Equal(t, 8.0, got.Recent[0].DurationSecs, "the API's nanoseconds become seconds")
}

// The stats window is the newest twenty completed runs, and a run that was
// cancelled counts against the window without being an ok or a failure — only
// success and failure are verdicts.
func TestMergeForgejoRunsTakesStatsOverTheNewestTwentyCompleted(t *testing.T) {
	t.Parallel()
	var runs []forgejo.ActionRun
	for i := 0; i < 21; i++ {
		var (
			status string
			d      time.Duration
		)
		switch {
		case i < 11:
			status, d = "success", time.Duration(i+1)*time.Second
		case i < 13:
			status, d = "cancelled", 3*time.Second
		default:
			status, d = "failure", 60*time.Second
		}
		runs = append(runs, testRun(int64(i+1), "land: polecat/mica/gt-abc (aaaa1111) onto main", status, i, i+1, d))
	}
	got := mergeForgejoRuns([]repoRuns{doneRepo("sloan/beads", nil, runs)})
	require.NotNil(t, got)
	assert.Equal(t, 10, got.Stats.OK, "the oldest success falls outside the twenty")
	assert.Equal(t, 8, got.Stats.Failed)
	assert.Equal(t, 9.5, got.Stats.MedianSecs, "2..11 and eight 60s: the mean of the two middle values")
}

// A repo the viewer cannot read costs the section that repo's runs and nothing
// else, the same bargain the feed strikes (gt-faml5).
func TestMergeForgejoRunsNamesTheRepoItCouldNotRead(t *testing.T) {
	t.Parallel()
	got := mergeForgejoRuns([]repoRuns{
		doneRepo("sloan/beads", []forgejo.ActionRun{testRun(1, "land: a (aaaa1111) onto main", "running", 1, -1, 0)}, nil),
		{repo: "sloan/mango", err: io.EOF},
	})
	require.NotNil(t, got)
	assert.Equal(t, []string{"sloan/mango"}, got.Errors)
	assert.Len(t, got.Current, 1, "the readable repo still appears")
	assert.Equal(t, 1, got.Stats.Running)
}

// No repo answered, so there is no fresh value at all. The reader turns the nil
// into the section's last good read, exactly as it does for the events.
func TestMergeForgejoRunsIsNilWhenEveryRepoFails(t *testing.T) {
	t.Parallel()
	assert.Nil(t, mergeForgejoRuns(nil))
	assert.Nil(t, mergeForgejoRuns([]repoRuns{{repo: "sloan/beads", err: io.EOF}}))
}

// A repo that answered with no runs at all is an answer: the section is empty
// rather than absent, and the page draws its idle line.
func TestMergeForgejoRunsIsEmptyNotNilForAQuietRepo(t *testing.T) {
	t.Parallel()
	got := mergeForgejoRuns([]repoRuns{doneRepo("sloan/beads", nil, nil)})
	require.NotNil(t, got)
	require.NotNil(t, got.Current)
	require.NotNil(t, got.Recent)
	assert.Empty(t, got.Current)
	assert.Empty(t, got.Recent)
	assert.Equal(t, ForgejoActionStats{}, got.Stats)
}

// shortSHA is the eight characters the Actions page shows: a full sha is cut,
// and one already short enough is left as it came.
func TestShortSHA(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, sha, want string
	}{
		{"a full sha is cut to eight characters", "645edaf6a1b2c3d4e5f60718293a4b5c6d7e8f90", "645edaf6"},
		{"a sha of exactly eight characters is left alone", "645edaf6", "645edaf6"},
		{"a shorter sha is left alone", "645eda", "645eda"},
		{"no sha stays empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, shortSHA(tc.sha))
		})
	}
}

// A row carries the commit the run tested and the run's number, so a reader can
// match it to a commit on main and open it: the hash is the API's sha cut to
// eight characters, the whole sha rides along for the row's tooltip, and the
// number is the run's index in its repo. The fields are on the JSON under their
// own names, for a run still going and for a finished one alike (gt-qes2r).
func TestForgejoActionCarriesTheCommitAndNumber(t *testing.T) {
	t.Parallel()
	const running = "645edaf6a1b2c3d4e5f60718293a4b5c6d7e8f90"

	current := testRun(7, "land: polecat/mica/gt-qes2r+muvob35k (aaaa1111) onto main", "running", 1, -1, 0)
	current.CommitSHA, current.Index = running, 20
	finished := testRun(5, "fix: the thing (gt-abc)", "success", 0, 2, 27*time.Second)
	finished.CommitSHA, finished.Index = "0123456789abcdef0123456789abcdef01234567", 19

	got := mergeForgejoRuns([]repoRuns{doneRepo("sloan/gastown",
		[]forgejo.ActionRun{current}, []forgejo.ActionRun{finished})})
	require.NotNil(t, got)
	require.Len(t, got.Current, 1)
	require.Len(t, got.Recent, 1)

	assert.Equal(t, "645edaf6", got.Current[0].Hash, "a current run's hash is its sha cut to eight")
	assert.Equal(t, running, got.Current[0].SHA)
	assert.Equal(t, int64(20), got.Current[0].Number)
	assert.Equal(t, "01234567", got.Recent[0].Hash, "a finished run carries them too")
	assert.Equal(t, int64(19), got.Recent[0].Number)

	b, err := json.Marshal(got)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"hash":"645edaf6"`)
	assert.Contains(t, string(b), `"sha":"`+running+`"`)
	assert.Contains(t, string(b), `"number":20`)
}

// A run the API sent no commit for carries no hash and no number rather than a
// zero one: the page reads an absent hash as no link, and a "0" beside it would
// name a run that does not exist (gt-qes2r).
func TestForgejoActionWithoutACommitLeavesTheFieldsEmpty(t *testing.T) {
	t.Parallel()
	got := newRunRow("sloan/beads", testRun(1, "gate: the suite", "success", 0, 1, 5*time.Second)).action()
	assert.Empty(t, got.Hash)
	assert.Empty(t, got.SHA)
	assert.Zero(t, got.Number)

	b, err := json.Marshal(got)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "hash")
	assert.NotContains(t, string(b), "number")
}

// The run's stamps reach the page as RFC3339, and a stamp the API never set is
// absent rather than the zero time: the page reads an absent start as "not
// started", and rendering year 1 would tick elapsed from the wrong epoch.
func TestForgejoActionStamps(t *testing.T) {
	t.Parallel()
	got := newRunRow("sloan/beads", forgejo.ActionRun{
		Created: stamp(1), Started: stamp(2), Stopped: stamp(3), Duration: int64(30 * time.Second),
	}).action()
	assert.Equal(t, stamp(2), got.Started)
	assert.Equal(t, stamp(3), got.Stopped)
	assert.Equal(t, 30.0, got.DurationSecs)

	queued := newRunRow("sloan/beads", forgejo.ActionRun{
		Created: stamp(1), Started: "0001-01-01T00:00:00Z", Stopped: "0001-01-01T00:00:00Z",
	}).action()
	assert.Empty(t, queued.Started, "Forgejo's zero time is not a start")
	assert.Empty(t, queued.Stopped)
}

func TestForgejoRunBead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		title string
		want  string
	}{
		{
			"a landing title names the bead in the branch it tested",
			"land: polecat/obsidian/gt-fn9e6.39+muvj72nn (c7210be8) onto main (dd809ca6)",
			"gt-fn9e6.39",
		},
		{
			"a push run's title is the commit subject, whose id is in parentheses",
			"fix: the dashboard's Forgejo pane refreshes on every tick (gt-faml5)",
			"gt-faml5",
		},
		{
			"a dashed word in the subject does not win over the id in parentheses",
			"feat: forgejo-cutover connects a rig by promotion, mirror behind --mirror (gt-fn9e6.40)",
			"gt-fn9e6.40",
		},
		{
			"a title with no id falls back to its head, cut to length",
			"gate: run the whole suite",
			"gate: run the whole suit",
		},
		{
			"the fallback is twenty-four characters",
			"rework: a title with no bead id in it at all",
			"rework: a title with no ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, forgejoRunBead(tc.title))
		})
	}
}

// forgejoActionsStub is a stand-in Forgejo serving activities and workflow runs
// so the reader can be driven without a socket. repos are the repos the viewer
// can read; every other repo answers 404, as Forgejo does for a repository the
// token cannot see. runs holds each readable repo's runs newest first, as the
// API serves them, and fail names a repo whose every runs request answers with
// that status.
type forgejoActionsStub struct {
	mu    sync.Mutex
	repos []string
	runs  map[string][]forgejo.ActionRun
	fail  map[string]int
	asked []string
}

// readable reports whether the viewer's token can see slug.
func (s *forgejoActionsStub) readable(slug string) bool {
	for _, repo := range s.repos {
		if repo == slug {
			return true
		}
	}
	return false
}

func (s *forgejoActionsStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	asked := r.URL.Path
	if r.URL.RawQuery != "" {
		asked += "?" + r.URL.RawQuery
	}
	s.asked = append(s.asked, asked)
	if strings.HasSuffix(r.URL.Path, "/activities/feeds") {
		if slug, ok := repoSlug(r.URL.Path); !ok || !s.readable(slug) {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "[]")
		return
	}
	slug, ok := repoSlug(r.URL.Path)
	if !ok || !s.readable(slug) {
		http.NotFound(w, r)
		return
	}
	if status := s.fail[slug]; status != 0 {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"message":"boom"}`)
		return
	}
	want := map[string]bool{}
	for _, st := range r.URL.Query()["status"] {
		want[st] = true
	}
	limit := forgejoRunPage
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = n
	}
	list := forgejo.RunList{Runs: []forgejo.ActionRun{}}
	for _, run := range s.runs[slug] {
		if want[run.Status] {
			list.Runs = append(list.Runs, run)
		}
	}
	if len(list.Runs) > limit {
		list.Runs = list.Runs[:limit]
	}
	list.TotalCount = len(list.Runs)
	b, err := json.Marshal(list)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(b)
}

func (s *forgejoActionsStub) askedFor() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}

// repoSlug is the owner/name a repo path addresses, whatever endpoint sits
// under it.
func repoSlug(path string) (string, bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/api/v1/repos/"), "/")
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return parts[0] + "/" + parts[1], true
}

func newForgejoActionsReader(t *testing.T, stub *forgejoActionsStub, repos []string) *ForgejoReader {
	t.Helper()
	client, err := forgejo.NewClient("viewer",
		forgejo.WithToken("test-token"),
		forgejo.WithBaseURL("http://forgejo.test/api/v1"),
		forgejo.WithHTTPClient(&http.Client{Transport: stubTransport{stub}}),
	)
	require.NoError(t, err)
	return NewForgejoReader(client, repos)
}

// The Actions section is built by the same Read() that builds the events, from
// runs read per repo with a bounded query per status.
func TestForgejoReaderReadsActionsWithTheFeed(t *testing.T) {
	t.Parallel()
	stub := &forgejoActionsStub{repos: []string{"sloan/beads", "sloan/mango"}, runs: map[string][]forgejo.ActionRun{
		"sloan/beads": {
			testRun(9, "land: c (dddd4444) onto main", "waiting", 3, -1, 0),
			testRun(7, "land: polecat/mica/gt-fn9e6.47+muvk2gi0 (aaaa1111) onto main", "running", 1, -1, 0),
			testRun(5, "fix: the thing (gt-abc)", "success", 0, 2, 27*time.Second),
			testRun(4, "gate: the suite", "failure", 0, 1, 90*time.Second),
		},
		"sloan/mango": {
			testRun(6, "land: b (bbbb2222) onto main", "blocked", 2, -1, 0),
			testRun(3, "gate: the suite", "success", 0, 1, 5*time.Second),
		},
	}}
	feed := newForgejoActionsReader(t, stub, []string{"sloan/beads", "sloan/mango"}).Read()
	require.Empty(t, feed.Error)
	require.NotNil(t, feed.Actions)

	a := feed.Actions
	require.Len(t, a.Current, 3)
	assert.Equal(t, []string{
		"https://forgejo.test/runs/7", "https://forgejo.test/runs/6", "https://forgejo.test/runs/9",
	}, urls(a.Current), "current runs oldest first across repos")
	assert.Equal(t, []string{
		"https://forgejo.test/runs/5", "https://forgejo.test/runs/4", "https://forgejo.test/runs/3",
	}, urls(a.Recent), "the completed runs newest first")
	assert.Equal(t, ForgejoActionStats{Running: 1, Queued: 2, OK: 2, Failed: 1, MedianSecs: 27}, a.Stats)
	assert.Empty(t, a.Errors)

	assert.Equal(t, ForgejoAction{
		Repo:   "sloan/beads",
		Bead:   "gt-fn9e6.47",
		Status: "running",
		Event:  "push",
		URL:    "https://forgejo.test/runs/7",
	}, a.Current[0])
	assert.Equal(t, 27.0, a.Recent[0].DurationSecs, "27s arrives as 27000000000 nanoseconds")
	assert.Equal(t, "gt-abc", a.Recent[0].Bead, "the bead id is parsed out of the title")

	assert.Equal(t, []string{
		"/api/v1/repos/sloan/beads/activities/feeds?limit=30",
		"/api/v1/repos/sloan/beads/actions/runs?limit=20&status=waiting",
		"/api/v1/repos/sloan/beads/actions/runs?limit=20&status=blocked",
		"/api/v1/repos/sloan/beads/actions/runs?limit=20&status=running",
		"/api/v1/repos/sloan/beads/actions/runs?limit=20&status=success&status=failure&status=cancelled&status=skipped",
		"/api/v1/repos/sloan/mango/activities/feeds?limit=30",
		"/api/v1/repos/sloan/mango/actions/runs?limit=20&status=waiting",
		"/api/v1/repos/sloan/mango/actions/runs?limit=20&status=blocked",
		"/api/v1/repos/sloan/mango/actions/runs?limit=20&status=running",
		"/api/v1/repos/sloan/mango/actions/runs?limit=20&status=success&status=failure&status=cancelled&status=skipped",
	}, stub.askedFor(), "current is one bounded query per status, completed one for every finished status")
}

// The reader keeps the last good Actions when every repo's runs fail, the same
// bargain it strikes for the events.
func TestForgejoReaderKeepsTheLastGoodActionsWhenEveryRepoFails(t *testing.T) {
	t.Parallel()
	stub := &forgejoActionsStub{repos: []string{"sloan/beads"}, runs: map[string][]forgejo.ActionRun{
		"sloan/beads": {testRun(5, "fix: the thing (gt-abc)", "success", 0, 2, 27*time.Second)},
	}}
	reader := newForgejoActionsReader(t, stub, []string{"sloan/beads"})
	good := reader.Read()
	require.NotNil(t, good.Actions)
	require.Len(t, good.Actions.Recent, 1)

	stub.mu.Lock()
	stub.fail = map[string]int{"sloan/beads": http.StatusInternalServerError}
	stub.mu.Unlock()
	stale := reader.Read()
	require.Empty(t, stale.Error, "the feed itself was read fine")
	assert.Equal(t, good.Actions, stale.Actions, "no repo answered, so the section keeps the last good value")
}

// A repo whose runs fail is named in the section while the others still appear.
func TestForgejoReaderNamesTheRepoWhoseRunsFailed(t *testing.T) {
	t.Parallel()
	stub := &forgejoActionsStub{
		repos: []string{"sloan/beads", "sloan/mango"},
		runs: map[string][]forgejo.ActionRun{
			"sloan/beads": {testRun(5, "fix: the thing (gt-abc)", "success", 0, 2, 27*time.Second)},
		},
		fail: map[string]int{"sloan/mango": http.StatusInternalServerError},
	}
	feed := newForgejoActionsReader(t, stub, []string{"sloan/beads", "sloan/mango"}).Read()
	require.Empty(t, feed.Error)
	require.NotNil(t, feed.Actions)
	assert.Equal(t, []string{"sloan/mango"}, feed.Actions.Errors)
	assert.Equal(t, []string{"https://forgejo.test/runs/5"}, urls(feed.Actions.Recent))
}

// A repo that answered with nothing leaves an empty section, not an absent one:
// the page draws its idle line rather than hiding the section.
func TestForgejoReaderPublishesAnEmptyActionsSectionForAQuietRepo(t *testing.T) {
	t.Parallel()
	feed := newForgejoActionsReader(t, &forgejoActionsStub{repos: []string{"sloan/beads"}}, []string{"sloan/beads"}).Read()
	require.NotNil(t, feed.Actions)
	assert.Empty(t, feed.Actions.Current)
	assert.Empty(t, feed.Actions.Recent)
	assert.Equal(t, ForgejoActionStats{}, feed.Actions.Stats)

	b, err := json.Marshal(feed)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"actions":{"current":[],"recent":[],"stats":{`)
}

// With no repo answering there is nothing to say, so the section is left out of
// the snapshot entirely rather than rendered empty.
func TestForgejoReaderWithoutAFreshActionsValueLeavesActionsOut(t *testing.T) {
	t.Parallel()
	stub := &forgejoActionsStub{
		repos: []string{"sloan/beads"},
		fail:  map[string]int{"sloan/beads": http.StatusInternalServerError},
	}
	feed := newForgejoActionsReader(t, stub, []string{"sloan/beads"}).Read()
	assert.Nil(t, feed.Actions)

	b, err := json.Marshal(feed)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "actions", "the panel omits the section it has no data for")
}

// A repo the viewer's token cannot read never reaches the runs: the feed has
// already named it missing.
func TestForgejoReaderSkipsRunsForAMissingRepo(t *testing.T) {
	t.Parallel()
	stub := &forgejoActionsStub{}
	feed := newForgejoActionsReader(t, stub, []string{"sloan/beads", "sloan/mango"}).Read()
	require.Empty(t, feed.Error)
	assert.Empty(t, feed.Actions, "the stub 404s both feeds, so neither is read")

	for _, asked := range stub.askedFor() {
		assert.NotContains(t, asked, "/actions/runs", "a repo the viewer cannot read is not asked for runs")
	}
}
