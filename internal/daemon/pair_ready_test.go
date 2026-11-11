package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/imagevalues"
)

const (
	pairRunning = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pairGreen   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	pairOther   = "cccccccccccccccccccccccccccccccccccccccc"
	pairGreen12 = "bbbbbbbbbbbb"
	pairOther12 = "cccccccccccc"
	pairUser    = "viewer-bot"
	pairToken   = "viewer-token-never-logged"
	pairOwner   = "town"
	pairRepo    = "gastown"
)

// pairT0 is the first read's time; the check is due every pairReadyInterval
// after it. The clock is a value the test passes, so no test waits on a timer.
var pairT0 = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

// digestOf is the digest a registry reports for body.
func digestOf(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fakePairAPI is the viewer's Forgejo read: the branches a listing returns, the
// commit counts a comparison returns (keyed "base...head"), and the images
// workflow runs with their jobs and logs. A comparison with no entry is an
// error, so a test that drops one exercises that refusal.
type fakePairAPI struct {
	branches  []forgejo.Branch
	branchErr error
	// pageErrAt makes the listing fail on that page, after the pages before it
	// answered: a scan that ended early, not one that never started.
	pageErrAt int
	// endless makes every listing page a full page of branches that are not
	// green, so the listing never ends.
	endless  bool
	compares map[string]int
	// runs is what a run listing returns, filtered by the commit and workflow
	// the filter names. runErr fails every run listing, and the listing an
	// ignored filter returns is modelled by filling runs with another commit's.
	runs   []forgejo.ActionRun
	runErr error
	// ignoreFilter makes the run listing answer runs.runs as they are: a
	// server that honours none of the filter's fields. nilList answers no list
	// and no error, which no Forgejo does and every caller must still survive.
	ignoreFilter bool
	nilList      bool
	// jobs and logs answer by run id and job id, as the API does. jobsErr and
	// logErr fail those reads.
	jobs    map[int64][]forgejo.ActionRunJob
	jobsErr error
	logs    map[int64]string
	logErr  error
	// runsQueries records the filter each run listing was asked with, and
	// jobReads counts the job list and log reads made.
	runsQueries []forgejo.RunFilter
	jobReads    int
}

func (f *fakePairAPI) ListBranches(_ context.Context, _, _ string, page, limit int) ([]forgejo.Branch, error) {
	if f.branchErr != nil {
		return nil, f.branchErr
	}
	if f.pageErrAt != 0 && page == f.pageErrAt {
		return nil, errors.New("forgejo answered 500 on that page")
	}
	if f.endless {
		out := make([]forgejo.Branch, limit)
		for i := range out {
			out[i].Name = "feature/" + strconv.Itoa(page*limit+i)
		}
		return out, nil
	}
	if page > 1 {
		return nil, nil
	}
	return f.branches, nil
}

func (f *fakePairAPI) CompareCommits(_ context.Context, _, _, base, head string) (*forgejo.CompareInfo, error) {
	total, ok := f.compares[base+"..."+head]
	if !ok {
		return nil, fmt.Errorf("no comparison fixture for %s...%s", base, head)
	}
	return &forgejo.CompareInfo{TotalCommits: total}, nil
}

func (f *fakePairAPI) ListRuns(_ context.Context, _, _ string, filter forgejo.RunFilter) (*forgejo.RunList, error) {
	f.runsQueries = append(f.runsQueries, filter)
	if f.runErr != nil {
		return nil, f.runErr
	}
	if f.nilList {
		return nil, nil
	}
	out := forgejo.RunList{}
	for _, run := range f.runs {
		if !f.ignoreFilter && filter.HeadSHA != "" && run.CommitSHA != filter.HeadSHA {
			continue
		}
		if !f.ignoreFilter && filter.WorkflowID != "" && run.WorkflowID != filter.WorkflowID {
			continue
		}
		out.Runs = append(out.Runs, run)
	}
	return &out, nil
}

func (f *fakePairAPI) ListRunJobs(_ context.Context, _, _ string, runID int64) ([]forgejo.ActionRunJob, error) {
	f.jobReads++
	if f.jobsErr != nil {
		return nil, f.jobsErr
	}
	return f.jobs[runID], nil
}

func (f *fakePairAPI) JobLogs(_ context.Context, _, _ string, jobID int64) (string, error) {
	f.jobReads++
	if f.logErr != nil {
		return "", f.logErr
	}
	return f.logs[jobID], nil
}

// fakePairRegistry is the registry a test fills with the tags it holds, keyed
// "image@tag". err makes the reads fail: every read, or only the reads of
// failImage when it is set.
type fakePairRegistry struct {
	digests   map[string]string
	err       error
	failImage string
}

func (f *fakePairRegistry) TagDigest(_ context.Context, image, tag string) (string, bool, error) {
	if f.err != nil && (f.failImage == "" || f.failImage == image) {
		return "", false, f.err
	}
	d, ok := f.digests[image+"@"+tag]
	return d, ok, nil
}

// pairBranch is a green branch fixture whose name carries sha's stamp.
func pairBranch(sha string) forgejo.Branch {
	b := forgejo.Branch{Name: "green/" + sha[:12]}
	b.Commit.ID = sha
	return b
}

// pairFixture is one rig's check: a town whose values file names the running
// commit, a viewer API and a registry. Its logs are what the check said.
type pairFixture struct {
	town string
	api  *fakePairAPI
	// apiOverride, when set, is the rig's API instead of the fake: the
	// production client, so a test can drive the wiring a rig resolves to.
	apiOverride pairReadyAPI
	reg         pairReadyRegistry
	rigErr      error
	rigsErr     error
	check       pairReadyCheck
	logs        []string
	daemon      string
	polecat     string
}

// newPairFixture is the case that raises: running at pairRunning, green at
// pairGreen three commits ahead of it, and both images at the green stamp.
func newPairFixture(t *testing.T) *pairFixture {
	t.Helper()
	fx := &pairFixture{
		town:    t.TempDir(),
		daemon:  digestOf([]byte("daemon manifest")),
		polecat: digestOf([]byte("polecat manifest")),
	}
	fx.api = &fakePairAPI{
		branches: []forgejo.Branch{pairBranch(pairGreen)},
		compares: map[string]int{
			pairGreen + "..." + pairRunning: 0,
			pairRunning + "..." + pairGreen: 3,
		},
	}
	fx.reg = &fakePairRegistry{digests: map[string]string{
		imagevalues.DaemonImage + "@" + pairGreen12:  fx.daemon,
		imagevalues.PolecatImage + "@" + pairGreen12: fx.polecat,
	}}
	fx.writeRunning(t, pairRunning)
	return fx
}

// writeRunning writes the values file the running commit is read from.
func (fx *pairFixture) writeRunning(t *testing.T, commit string) {
	t.Helper()
	reg := imagevalues.Registry{Scheme: "https", Host: "registry.test:5000", Owner: "town"}
	v := imagevalues.Values{
		Daemon:  reg.Ref(imagevalues.DaemonImage, digestOf([]byte("running daemon"))),
		Polecat: reg.Ref(imagevalues.PolecatImage, digestOf([]byte("running polecat"))),
		Commit:  commit,
	}
	require.NoError(t, imagevalues.Write(imagevalues.PathIn(filepath.Join(fx.town, "gastown")), v))
}

// writeRunningRaw writes the values file with commitLine as its commit line, so
// a test can write a commit imagevalues.Write would refuse.
func (fx *pairFixture) writeRunningRaw(t *testing.T, commitLine string) {
	t.Helper()
	reg := imagevalues.Registry{Scheme: "https", Host: "registry.test:5000", Owner: "town"}
	body := fmt.Sprintf("daemon: %s\npolecat: %s\ncommit: %s\n",
		reg.Ref(imagevalues.DaemonImage, digestOf([]byte("running daemon"))),
		reg.Ref(imagevalues.PolecatImage, digestOf([]byte("running polecat"))),
		commitLine)
	require.NoError(t, os.WriteFile(imagevalues.PathIn(filepath.Join(fx.town, "gastown")), []byte(body), 0o600))
}

// run makes the check at now, with the fixture's one rig.
func (fx *pairFixture) run(now time.Time) {
	rigs := func() ([]pairReadyRig, error) {
		if fx.rigsErr != nil {
			return nil, fx.rigsErr
		}
		api := pairReadyAPI(fx.api)
		if fx.apiOverride != nil {
			api = fx.apiOverride
		}
		return []pairReadyRig{{
			Rig: "gastown", Owner: pairOwner, Repo: pairRepo,
			API: api, Registry: fx.reg, Err: fx.rigErr,
		}}, nil
	}
	logf := func(format string, args ...any) { fx.logs = append(fx.logs, fmt.Sprintf(format, args...)) }
	fx.check.run(context.Background(), fx.town, now, rigs, logf)
}

// TestPairReadyRaisesOneItemForBothImages: both images published for a green
// commit newer than the running one raise one item, which names the stamp, both
// digests and how many commits it is ahead.
func TestPairReadyRaisesOneItemForBothImages(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)

	fx.run(pairT0)

	items := fx.check.observed()
	require.Len(t, items, 1)
	it := items[0]
	assert.Equal(t, attention.KindDeployPairReady, it.Kind)
	assert.Equal(t, "deploy-pair-ready:gastown:"+pairGreen12, it.Key)
	assert.Equal(t, "gastown", it.Rig)
	assert.Equal(t, pairGreen, it.SHA)
	assert.Contains(t, it.Summary, "green/"+pairGreen12)
	assert.Contains(t, it.Summary, fx.daemon)
	assert.Contains(t, it.Summary, fx.polecat)
	assert.Contains(t, it.Summary, "3 commits ahead")
	assert.Empty(t, fx.logs)
}

// TestPairReadyOneImageAloneRaisesNothing: a green commit with only one of its
// two images in the registry is not a pair, so nothing is raised and nothing is
// logged as a failure.
func TestPairReadyOneImageAloneRaisesNothing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		missing string
	}{
		{name: "daemon image alone", missing: imagevalues.PolecatImage},
		{name: "polecat image alone", missing: imagevalues.DaemonImage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newPairFixture(t)
			delete(fx.reg.(*fakePairRegistry).digests, tc.missing+"@"+pairGreen12)

			fx.run(pairT0)

			assert.Empty(t, fx.check.observed())
			assert.Empty(t, fx.logs)
		})
	}
}

// TestPairReadyClearsWhenRunningReachesTheGreenSha: once the running commit is
// the green commit, the item clears on the next read.
func TestPairReadyClearsWhenRunningReachesTheGreenSha(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.run(pairT0)
	require.Len(t, fx.check.observed(), 1)

	fx.writeRunning(t, pairGreen)
	fx.run(pairT0.Add(pairReadyInterval))

	assert.Empty(t, fx.check.observed())
}

// TestPairReadyOlderOrEqualGreenRaisesNothing: a green commit that is the
// running one, that the running one is ahead of, or that no commit separates
// from it is not newer, so the check raises nothing for it.
func TestPairReadyOlderOrEqualGreenRaisesNothing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		running string
		compare map[string]int
	}{
		{
			name:    "green is the running commit",
			running: pairGreen,
		},
		{
			name:    "running is ahead of green",
			running: pairRunning,
			compare: map[string]int{
				pairGreen + "..." + pairRunning: 2,
				pairRunning + "..." + pairGreen: 0,
			},
		},
		{
			// Both sides have commits the other lacks: the green commit is on a
			// branch that split from running, so it is not newer than running.
			name:    "green diverged from running",
			running: pairRunning,
			compare: map[string]int{
				pairGreen + "..." + pairRunning: 2,
				pairRunning + "..." + pairGreen: 3,
			},
		},
		{
			name:    "no commit separates green from running",
			running: pairRunning,
			compare: map[string]int{
				pairGreen + "..." + pairRunning: 0,
				pairRunning + "..." + pairGreen: 0,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newPairFixture(t)
			fx.writeRunning(t, tc.running)
			if tc.compare != nil {
				fx.api.compares = tc.compare
			}

			fx.run(pairT0)

			assert.Empty(t, fx.check.observed())
			assert.Empty(t, fx.logs)
		})
	}
}

// TestPairReadyFailedReadAlarmsTheQueue: every way a read can fail keeps the
// pair the rig held (an unanswered read is not an absent pair), logs why, and
// raises the check's own unreadable item. A failed read that only logged would
// leave a broken check reading in the queue exactly like a town with no pair.
func TestPairReadyFailedReadAlarmsTheQueue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		apply   func(t *testing.T, fx *pairFixture)
		wantKey string
		want    string // what the unreadable item's summary says failed
	}{
		{
			name: "the registry read fails",
			apply: func(_ *testing.T, fx *pairFixture) {
				fx.reg = &fakePairRegistry{err: errors.New("the registry answered HTTP 503")}
			},
			wantKey: "deploy-pair-unreadable:rig:gastown",
			want:    "HTTP 503",
		},
		{
			name: "the branch listing fails",
			apply: func(_ *testing.T, fx *pairFixture) {
				fx.api.branchErr = errors.New("forgejo answered 500")
			},
			wantKey: "deploy-pair-unreadable:rig:gastown",
			want:    "forgejo answered 500",
		},
		{
			name: "a comparison fails",
			apply: func(_ *testing.T, fx *pairFixture) {
				delete(fx.api.compares, pairRunning+"..."+pairGreen)
			},
			wantKey: "deploy-pair-unreadable:rig:gastown",
			want:    "green/" + pairGreen12,
		},
		{
			name: "the comparison that says green is newer fails",
			apply: func(_ *testing.T, fx *pairFixture) {
				delete(fx.api.compares, pairGreen+"..."+pairRunning)
			},
			wantKey: "deploy-pair-unreadable:rig:gastown",
			want:    "green/" + pairGreen12,
		},
		{
			name: "the polecat image read fails",
			apply: func(_ *testing.T, fx *pairFixture) {
				fx.reg = &fakePairRegistry{
					digests:   fx.reg.(*fakePairRegistry).digests,
					err:       errors.New("the registry answered HTTP 503"),
					failImage: imagevalues.PolecatImage,
				}
			},
			wantKey: "deploy-pair-unreadable:rig:gastown",
			want:    "HTTP 503",
		},
		{
			name: "the branch listing never ends",
			apply: func(_ *testing.T, fx *pairFixture) {
				fx.api.endless = true
			},
			wantKey: "deploy-pair-unreadable:rig:gastown",
			want:    "runs past",
		},
		{
			name: "the values file does not parse",
			apply: func(t *testing.T, fx *pairFixture) {
				path := imagevalues.PathIn(filepath.Join(fx.town, "gastown"))
				require.NoError(t, os.WriteFile(path, []byte("commit: [unclosed\n"), 0o600))
			},
			wantKey: "deploy-pair-unreadable:town:running-commit",
			want:    "values file",
		},
		{
			name: "the commit line is not a full commit",
			apply: func(t *testing.T, fx *pairFixture) {
				fx.writeRunningRaw(t, pairGreen12)
			},
			wantKey: "deploy-pair-unreadable:town:running-commit",
			want:    "not a full 40-character lowercase commit",
		},
		{
			name: "a green branch names another commit",
			apply: func(_ *testing.T, fx *pairFixture) {
				fx.api.branches[0].Commit.ID = pairOther
			},
			wantKey: "deploy-pair-unreadable:rig:gastown",
			want:    "which its name does not carry",
		},
		{
			name: "the rig cannot be resolved",
			apply: func(_ *testing.T, fx *pairFixture) {
				fx.rigErr = errors.New("rig gastown names no merge_queue.forgejo remote_url")
			},
			wantKey: "deploy-pair-unreadable:rig:gastown",
			want:    "names no merge_queue.forgejo remote_url",
		},
		{
			name: "the values file is gone",
			apply: func(t *testing.T, fx *pairFixture) {
				require.NoError(t, os.Remove(imagevalues.PathIn(filepath.Join(fx.town, "gastown"))))
			},
			wantKey: "deploy-pair-unreadable:town:running-commit",
			want:    "no values file",
		},
		{
			name: "the image rigs cannot be named",
			apply: func(_ *testing.T, fx *pairFixture) {
				fx.rigsErr = errors.New("image rig ghost is not a known rig")
			},
			wantKey: "deploy-pair-unreadable:town:rigs",
			want:    "is not a known rig",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newPairFixture(t)
			fx.run(pairT0)
			require.Len(t, fx.check.observed(), 1)

			tc.apply(t, fx)
			fx.logs = nil
			fx.run(pairT0.Add(pairReadyInterval))

			items := fx.check.observed()
			require.Len(t, items, 2, "a failed read must keep the pair and raise its own item")
			assert.Equal(t, "deploy-pair-ready:gastown:"+pairGreen12, items[0].Key, "a failed read cleared the pair the rig held")
			unreadable := items[1]
			assert.Equal(t, tc.wantKey, unreadable.Key)
			assert.Equal(t, attention.KindDeployPairUnreadable, unreadable.Kind)
			assert.Equal(t, attention.SeverityLow, unreadable.Severity)
			assert.Contains(t, unreadable.Summary, tc.want, "the unreadable item does not say what failed")
			assert.NotContains(t, unreadable.Summary, pairToken)
			assert.NotEmpty(t, fx.logs, "a failed read was not logged")
		})
	}
}

// TestPairReadyFailedReadsForgetWhatItHeld: a read that keeps failing stops
// vouching for the pair it last raised. Before the bound the pair is kept and
// the unreadable item explains why it is not being judged; at the bound the
// stale pair is dropped, so an item that is never confirmed does not outlive
// the check that raised it.
func TestPairReadyFailedReadsForgetWhatItHeld(t *testing.T) {
	t.Parallel()
	t.Run("a rig's failed reads", func(t *testing.T) {
		t.Parallel()
		fx := newPairFixture(t)
		fx.run(pairT0)
		require.Len(t, fx.check.observed(), 1)
		fx.reg = &fakePairRegistry{err: errors.New("the registry answered HTTP 503")}

		for i := 1; i < pairReadyForgetAfter; i++ {
			fx.run(pairT0.Add(time.Duration(i) * pairReadyInterval))
			items := fx.check.observed()
			require.Len(t, items, 2, "the pair was dropped before the failure bound")
			assert.Equal(t, "deploy-pair-ready:gastown:"+pairGreen12, items[0].Key)
		}

		fx.run(pairT0.Add(pairReadyForgetAfter * pairReadyInterval))
		items := fx.check.observed()
		require.Len(t, items, 1, "a stale pair outlived the failure bound")
		assert.Equal(t, "deploy-pair-unreadable:rig:gastown", items[0].Key)
	})

	t.Run("the town's failed reads", func(t *testing.T) {
		t.Parallel()
		fx := newPairFixture(t)
		fx.run(pairT0)
		require.Len(t, fx.check.observed(), 1)
		require.NoError(t, os.Remove(imagevalues.PathIn(filepath.Join(fx.town, "gastown"))))

		for i := 1; i < pairReadyForgetAfter; i++ {
			fx.run(pairT0.Add(time.Duration(i) * pairReadyInterval))
			assert.Len(t, fx.check.observed(), 2, "the pair was dropped before the failure bound")
		}

		fx.run(pairT0.Add(pairReadyForgetAfter * pairReadyInterval))
		items := fx.check.observed()
		require.Len(t, items, 1, "a stale pair outlived the failure bound")
		assert.Equal(t, "deploy-pair-unreadable:town:running-commit", items[0].Key)
	})

	// A read that answers resets the count: failures on either side of a success
	// do not add up to the bound.
	t.Run("a success resets the count", func(t *testing.T) {
		t.Parallel()
		fx := newPairFixture(t)
		good := fx.reg
		fx.run(pairT0)
		fx.reg = &fakePairRegistry{err: errors.New("the registry answered HTTP 503")}
		fx.run(pairT0.Add(pairReadyInterval))
		fx.run(pairT0.Add(2 * pairReadyInterval))
		require.Len(t, fx.check.observed(), 2)

		fx.reg = good
		fx.run(pairT0.Add(3 * pairReadyInterval))
		require.Len(t, fx.check.observed(), 1, "a successful read did not clear the unreadable item")

		fx.reg = &fakePairRegistry{err: errors.New("the registry answered HTTP 503")}
		fx.run(pairT0.Add(4 * pairReadyInterval))
		fx.run(pairT0.Add(5 * pairReadyInterval))
		assert.Len(t, fx.check.observed(), 2, "the failure count did not reset on a successful read")
	})
}

// TestPairReadyPartialScanRaisesWhatItRead: a listing that fills its first page
// and then fails leaves the scan incomplete, not failed. The pairs it read are
// still raised, so a slow listing cannot discard the whole rig's scan.
func TestPairReadyPartialScanRaisesWhatItRead(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	pages := make([]forgejo.Branch, pairReadyBranchLimit)
	pages[0] = fx.api.branches[0]
	for i := 1; i < len(pages); i++ {
		pages[i].Name = "feature/" + strconv.Itoa(i)
	}
	fx.api.branches = pages
	fx.api.pageErrAt = 2

	fx.run(pairT0)

	items := fx.check.observed()
	require.Len(t, items, 2)
	assert.Equal(t, "deploy-pair-ready:gastown:"+pairGreen12, items[0].Key, "a pair read before the listing failed was discarded")
	assert.Equal(t, attention.KindDeployPairUnreadable, items[1].Kind)
	assert.Contains(t, items[1].Summary, "listing page 2")
	require.NotEmpty(t, fx.logs)
	assert.Contains(t, fx.logs[0], "listing page 2")
}

// TestPairReadyPartialScanKeepsThePairItDidNotReach: an incomplete scan says
// nothing about a branch it never read, so the pair last raised for that branch
// stays until a scan reaches it.
func TestPairReadyPartialScanKeepsThePairItDidNotReach(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.run(pairT0)
	require.Len(t, fx.check.observed(), 1)

	// The next listing reads one full page of non-green branches and never
	// reaches the green one.
	pages := make([]forgejo.Branch, pairReadyBranchLimit)
	for i := range pages {
		pages[i].Name = "feature/" + strconv.Itoa(i)
	}
	fx.api.branches = pages
	fx.api.pageErrAt = 2
	fx.run(pairT0.Add(pairReadyInterval))

	items := fx.check.observed()
	require.Len(t, items, 2)
	assert.Equal(t, "deploy-pair-ready:gastown:"+pairGreen12, items[0].Key, "a pair the partial scan did not reach was cleared")
	assert.Equal(t, attention.KindDeployPairUnreadable, items[1].Kind)
}

// TestPairReadyPartialScanClearsThePairItReached: a branch the incomplete scan
// did reach is judged. Its polecat image gone, the pair item clears even though
// the listing ended early, because the scan looked at that branch.
func TestPairReadyPartialScanClearsThePairItReached(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.run(pairT0)
	require.Len(t, fx.check.observed(), 1)

	pages := make([]forgejo.Branch, pairReadyBranchLimit)
	pages[0] = fx.api.branches[0]
	for i := 1; i < len(pages); i++ {
		pages[i].Name = "feature/" + strconv.Itoa(i)
	}
	fx.api.branches = pages
	fx.api.pageErrAt = 2
	delete(fx.reg.(*fakePairRegistry).digests, imagevalues.PolecatImage+"@"+pairGreen12)
	fx.run(pairT0.Add(pairReadyInterval))

	items := fx.check.observed()
	require.Len(t, items, 1, "a pair the incomplete scan reached was not cleared")
	assert.Equal(t, attention.KindDeployPairUnreadable, items[0].Kind)
}

// TestPairReadyOneBranchsFailedReadKeepsThePass: a comparison or image read
// that fails on one branch makes the pass incomplete, not failed. The pairs it
// already confirmed are still raised, and a branch it could not judge keeps the
// pair it last held: one branch's failure cannot discard the pass.
func TestPairReadyOneBranchsFailedReadKeepsThePass(t *testing.T) {
	t.Parallel()
	t.Run("the pairs the pass confirmed are kept", func(t *testing.T) {
		t.Parallel()
		fx := newPairFixture(t)
		// A second green commit whose first comparison has no fixture, so it
		// fails after the first branch's pair was confirmed.
		fx.api.branches = append(fx.api.branches, pairBranch(pairOther))

		fx.run(pairT0)

		items := fx.check.observed()
		require.Len(t, items, 2, "one branch's failed read discarded the pass's confirmed pair")
		assert.Equal(t, "deploy-pair-ready:gastown:"+pairGreen12, items[0].Key)
		assert.Equal(t, attention.KindDeployPairUnreadable, items[1].Kind)
		assert.Contains(t, items[1].Summary, "green/"+pairOther12)
	})

	t.Run("a branch the pass did not judge keeps the pair it held", func(t *testing.T) {
		t.Parallel()
		fx := newPairFixture(t)
		fx.api.branches = append(fx.api.branches, pairBranch(pairOther))
		fx.api.compares[pairOther+"..."+pairRunning] = 0
		fx.api.compares[pairRunning+"..."+pairOther] = 7
		fx.reg.(*fakePairRegistry).digests[imagevalues.DaemonImage+"@"+pairOther12] = digestOf([]byte("other daemon"))
		fx.reg.(*fakePairRegistry).digests[imagevalues.PolecatImage+"@"+pairOther12] = digestOf([]byte("other polecat"))
		fx.run(pairT0)
		require.Len(t, fx.check.observed(), 2)

		// The second branch's comparison now fails: its held pair is not judged,
		// so it stays, beside the unreadable item.
		delete(fx.api.compares, pairRunning+"..."+pairOther)
		fx.run(pairT0.Add(pairReadyInterval))

		items := fx.check.observed()
		require.Len(t, items, 3, "a pair the pass could not judge was cleared")
		assert.Equal(t, "deploy-pair-ready:gastown:"+pairGreen12, items[0].Key)
		assert.Equal(t, "deploy-pair-ready:gastown:"+pairOther12, items[1].Key)
		assert.Equal(t, attention.KindDeployPairUnreadable, items[2].Kind)
		assert.Contains(t, items[2].Summary, "green/"+pairOther12)
	})
}

// TestPairReadyKindsHaveTheirOwnCollector: the pair items and the check's
// unreadable items are two kinds from one read, so each has a collector of its
// own. Each collector returns only its kind, and a failed collector keeps only
// the previous items of its kind (gt-4nlpw8).
func TestPairReadyKindsHaveTheirOwnCollector(t *testing.T) {
	t.Parallel()
	ready := attention.Item{Key: "deploy-pair-ready:gastown:" + pairGreen12, Kind: attention.KindDeployPairReady}
	unreadable := attention.Item{Key: "deploy-pair-unreadable:rig:gastown", Kind: attention.KindDeployPairUnreadable}
	s := &attentionSources{pairReady: func() []attention.Item {
		return []attention.Item{ready, unreadable}
	}}

	var collectors []attentionCollector
	for _, c := range s.collectors() {
		if c.kind == attention.KindDeployPairReady || c.kind == attention.KindDeployPairUnreadable {
			collectors = append(collectors, c)
		}
	}
	require.Len(t, collectors, 2, "each kind the pair check raises needs a collector of its own")

	want := map[attention.Kind]attention.Item{
		attention.KindDeployPairReady:      ready,
		attention.KindDeployPairUnreadable: unreadable,
	}
	prev := attention.State{Items: []attention.Item{ready, unreadable}}
	for _, c := range collectors {
		items, err := c.collect(context.Background())
		require.NoError(t, err)
		require.Len(t, items, 1, "collector %s returned %d items, want only its own kind", c.kind, len(items))
		assert.Equal(t, want[c.kind], items[0])

		kept := c.kept(prev)
		require.Len(t, kept, 1, "collector %s kept %d items, want only its own kind", c.kind, len(kept))
		assert.Equal(t, c.kind, kept[0].Kind)
	}
}

// TestPairReadyRunningCommitRefusesAnInvalidCommit: the commit line is compared
// against green branch names, so a value that is not a full lowercase commit
// matches nothing and would raise silence. imagevalues refuses such a file at
// parse time, and the check raises its unreadable item rather than comparing
// the value.
func TestPairReadyRunningCommitRefusesAnInvalidCommit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		commitLine string
		want       string
	}{
		{name: "a short commit", commitLine: pairGreen12, want: "not a full 40-character lowercase commit"},
		{name: "an upper-case commit", commitLine: strings.ToUpper(pairGreen), want: "not a full 40-character lowercase commit"},
		{name: "an empty commit", commitLine: `""`, want: "has no commit line"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newPairFixture(t)
			fx.writeRunningRaw(t, tc.commitLine)

			_, err := pairReadyRunningCommit(fx.town)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)

			fx.run(pairT0)
			items := fx.check.observed()
			require.Len(t, items, 1)
			assert.Equal(t, "deploy-pair-unreadable:town:running-commit", items[0].Key)
			assert.Equal(t, attention.KindDeployPairUnreadable, items[0].Kind)
		})
	}
}

// TestPairReadyIsDueOnTheIntervalOnly: between reads the check does nothing, so
// a registry that has since lost the pair is not read until the interval is up.
func TestPairReadyIsDueOnTheIntervalOnly(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.run(pairT0)
	require.Len(t, fx.check.observed(), 1)

	fx.reg = &fakePairRegistry{}
	fx.run(pairT0.Add(time.Minute))
	assert.Len(t, fx.check.observed(), 1, "the check read before its interval was up")

	fx.run(pairT0.Add(pairReadyInterval))
	assert.Empty(t, fx.check.observed(), "the check did not read once its interval was up")
}

// pairHandlerTransport answers each request in process, so the registry fixture
// opens no socket.
type pairHandlerTransport struct{ h http.Handler }

func (t pairHandlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// pairRegistryHandler serves manifests by request path. status, when set,
// answers every request with that status. auth records the credential each
// request carried.
type pairRegistryHandler struct {
	manifests map[string][]byte
	status    int
	auth      []string
}

func (h *pairRegistryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if user, token, ok := r.BasicAuth(); ok {
		h.auth = append(h.auth, user+":"+token)
	}
	if h.status != 0 {
		w.WriteHeader(h.status)
		return
	}
	body, ok := h.manifests[r.URL.Path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Docker-Content-Digest", digestOf(body))
	_, _ = w.Write(body)
}

// TestPairReadyTokenNeverReachesLogOrItem: the production registry read sends
// the viewer's credential and the check's log and items carry none of it, on a
// read that succeeds and on one that fails.
func TestPairReadyTokenNeverReachesLogOrItem(t *testing.T) {
	t.Parallel()
	daemonBody := []byte(`{"schemaVersion":2,"daemon":true}`)
	polecatBody := []byte(`{"schemaVersion":2,"polecat":true}`)
	handler := &pairRegistryHandler{manifests: map[string][]byte{
		"/v2/bot-registry/" + imagevalues.DaemonImage + "/manifests/" + pairGreen12:  daemonBody,
		"/v2/bot-registry/" + imagevalues.PolecatImage + "/manifests/" + pairGreen12: polecatBody,
	}}
	reader := pairReadyTagReader{
		reg:   imagevalues.Registry{Scheme: "http", Host: "127.0.0.1:5001", Owner: "bot-registry"},
		user:  pairUser,
		token: pairToken,
		hc:    &http.Client{Transport: pairHandlerTransport{handler}},
	}

	t.Run("a pair that reads", func(t *testing.T) {
		t.Parallel()
		fx := newPairFixture(t)
		fx.reg = reader

		fx.run(pairT0)

		items := fx.check.observed()
		require.Len(t, items, 1)
		assert.Contains(t, items[0].Summary, digestOf(daemonBody))
		assert.Contains(t, items[0].Summary, digestOf(polecatBody))
		assert.NotContains(t, items[0].Summary, pairToken)
		assert.NotContains(t, strings.Join(fx.logs, "\n"), pairToken)
		assert.Equal(t, []string{pairUser + ":" + pairToken, pairUser + ":" + pairToken}, handler.auth,
			"the registry was not read with the viewer's credential")
	})

	t.Run("a registry that fails", func(t *testing.T) {
		t.Parallel()
		fx := newPairFixture(t)
		failing := &pairRegistryHandler{status: http.StatusInternalServerError}
		fx.reg = pairReadyTagReader{
			reg:   reader.reg,
			user:  pairUser,
			token: pairToken,
			hc:    &http.Client{Transport: pairHandlerTransport{failing}},
		}

		fx.run(pairT0)

		items := fx.check.observed()
		require.Len(t, items, 1, "a failed registry read raised no unreadable item")
		assert.Equal(t, attention.KindDeployPairUnreadable, items[0].Kind)
		assert.NotContains(t, items[0].Summary, pairToken)
		require.NotEmpty(t, fx.logs, "the failed read was not logged")
		assert.NotContains(t, strings.Join(fx.logs, "\n"), pairToken)
	})
}

const (
	pairImagesRun  = int64(14757)
	pairImagesJob  = int64(311)
	pairImagesLine = "Error: rig devops pin fc7615ec3b0b is 44 commits behind main, over the 40-commit bound"
)

// setImagesRun makes the fixture's images workflow hold one run for sha: the
// run is the newest, and its only job reports the same status, so a succeeded
// run clears what a failed one raised.
func (fx *pairFixture) setImagesRun(sha, status string) {
	if fx.api.jobs == nil {
		fx.api.jobs = map[int64][]forgejo.ActionRunJob{}
	}
	if fx.api.logs == nil {
		fx.api.logs = map[int64]string{}
	}
	run := forgejo.ActionRun{
		ID: pairImagesRun, CommitSHA: sha, WorkflowID: pairReadyImagesWorkflow, Status: status,
	}
	// Runs are listed newest first, so the run this helper set is the one the
	// scan judges even when a case adds more behind it.
	fx.api.runs = append([]forgejo.ActionRun{run}, fx.api.runs...)
	fx.api.jobs[pairImagesRun] = []forgejo.ActionRunJob{
		{ID: pairImagesJob, RunID: pairImagesRun, Name: "images", Status: status},
	}
	fx.api.logs[pairImagesJob] = "[preflight] checking the pins\n" + pairImagesLine + "\n"
}

// TestImagesFailedRaisesAnItemForAFailedRun: an images run that ended failed or
// canceled for a green commit newer than the running one raises one item
// naming the commit and the first Error: line of the job it ran. The registry
// holds nothing, which is what a failed build leaves behind.
func TestImagesFailedRaisesAnItemForAFailedRun(t *testing.T) {
	t.Parallel()
	for _, status := range []string{deployRunFailed, deployRunCancelled} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			fx := newPairFixture(t)
			fx.reg = &fakePairRegistry{}
			fx.setImagesRun(pairGreen, status)

			fx.run(pairT0)

			items := fx.check.observed()
			require.Len(t, items, 1)
			it := items[0]
			assert.Equal(t, attention.KindImagesFailed, it.Kind)
			assert.Equal(t, "images-failed:gastown:"+pairGreen12, it.Key)
			assert.Equal(t, "gastown", it.Rig)
			assert.Equal(t, pairGreen, it.SHA)
			assert.Contains(t, it.Summary, pairGreen12, "the item does not name the commit")
			assert.Contains(t, it.Summary, pairImagesLine, "the item does not carry the Error line")
			assert.Equal(t, attention.RuleImagesFailed, attention.RuleFor(it))
			require.NotEmpty(t, fx.api.runsQueries, "the images runs were not read")
			assert.Equal(t, pairReadyImagesWorkflow, fx.api.runsQueries[0].WorkflowID)
			assert.Equal(t, pairGreen, fx.api.runsQueries[0].HeadSHA)
		})
	}
}

// TestImagesFailedIsOneItemAndASuccessClearsIt: the item is keyed by the
// commit, so a second scan keeps one and does not duplicate it, and a later
// green run of the same commit clears it.
func TestImagesFailedIsOneItemAndASuccessClearsIt(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.reg = &fakePairRegistry{}
	fx.setImagesRun(pairGreen, deployRunFailed)
	fx.run(pairT0)
	require.Len(t, fx.check.observed(), 1)

	fx.run(pairT0.Add(pairReadyInterval))
	items := fx.check.observed()
	require.Len(t, items, 1, "a second scan duplicated the failed build")
	assert.Equal(t, "images-failed:gastown:"+pairGreen12, items[0].Key)

	fx.setImagesRun(pairGreen, deployRunSucceeded)
	fx.run(pairT0.Add(2 * pairReadyInterval))

	assert.Empty(t, fx.check.observed(), "a green run for the commit did not clear the item")
}

// TestImagesFailedStandsWhileTheRerunIsInFlight: a new run that has not
// finished is no answer, so the failure the check last raised for the commit
// stands until the run settles. Clearing it when the re-run starts would report
// a fix nobody has seen yet.
func TestImagesFailedStandsWhileTheRerunIsInFlight(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.reg = &fakePairRegistry{}
	fx.setImagesRun(pairGreen, deployRunFailed)
	fx.run(pairT0)
	require.Len(t, fx.check.observed(), 1)

	fx.setImagesRun(pairGreen, forgejo.RunStatusRunning)
	fx.run(pairT0.Add(pairReadyInterval))

	items := fx.check.observed()
	require.Len(t, items, 1, "an in-flight re-run cleared the item it has not answered yet")
	assert.Equal(t, attention.KindImagesFailed, items[0].Kind)

	fx.setImagesRun(pairGreen, deployRunSucceeded)
	fx.run(pairT0.Add(2 * pairReadyInterval))
	assert.Empty(t, fx.check.observed())
}

// TestImagesFailedWithAnUnreadableLogStillRaises: the run ended failed, which
// the scan knows whatever the log says, so every way of not reading the log
// still raises the item and says the log could not be read rather than passing
// for a build that did not fail.
func TestImagesFailedWithAnUnreadableLogStillRaises(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		apply func(fx *pairFixture)
		want  string
	}{
		{
			name:  "the run's jobs cannot be listed",
			apply: func(fx *pairFixture) { fx.api.jobsErr = errors.New("forgejo answered 500 on the job list") },
			want:  "the job log could not be read: forgejo answered 500 on the job list",
		},
		{
			name:  "the job's log cannot be read",
			apply: func(fx *pairFixture) { fx.api.logErr = errors.New("forgejo answered 404 on the log") },
			want:  "the job log could not be read: forgejo answered 404 on the log",
		},
		{
			name:  "the run names no job",
			apply: func(fx *pairFixture) { fx.api.jobs = nil },
			want:  "no failed job found, so no log was read",
		},
		{
			name:  "the log names no Error: line",
			apply: func(fx *pairFixture) { fx.api.logs[pairImagesJob] = "step failed\n" },
			want:  "the job log names no Error: line",
		},
		{
			// The run failed but the API marked no job failed. The check does not
			// read an arbitrary job's log for the reason, so it says none was found.
			name: "the run marks no job failed",
			apply: func(fx *pairFixture) {
				fx.api.jobs[pairImagesRun] = []forgejo.ActionRunJob{
					{ID: pairImagesJob, RunID: pairImagesRun, Name: "images", Status: deployRunSucceeded},
				}
			},
			want: "no failed job found, so no log was read",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newPairFixture(t)
			fx.reg = &fakePairRegistry{}
			fx.setImagesRun(pairGreen, deployRunFailed)
			tc.apply(fx)

			fx.run(pairT0)

			items := fx.check.observed()
			require.Len(t, items, 1)
			assert.Equal(t, attention.KindImagesFailed, items[0].Kind)
			assert.Contains(t, items[0].Summary, tc.want)
			assert.Contains(t, items[0].Summary, pairGreen12)
		})
	}
}

// TestImagesFailedRefusesARunForAnotherCommit: the run list is asked for one
// commit, and a run it returns for another is not that commit's verdict. A
// filter a server ignores must not put another commit's failure in the queue.
func TestImagesFailedRefusesARunForAnotherCommit(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.reg = &fakePairRegistry{}
	fx.setImagesRun(pairOther, deployRunFailed)
	// A server that ignores the filter answers this commit's listing with the
	// other commit's failed run.
	fx.api.ignoreFilter = true

	fx.run(pairT0)

	assert.Empty(t, fx.check.observed(), "a failed run for another commit raised an item for this one")
}

// TestImagesFailedAbsentRunKeepsItsItem: a run list with no run for the commit
// is no answer, so the failure raised for it stands until a successful run
// clears it.
func TestImagesFailedAbsentRunKeepsItsItem(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.reg = &fakePairRegistry{}
	fx.setImagesRun(pairGreen, deployRunFailed)
	fx.run(pairT0)
	require.Len(t, fx.check.observed(), 1)

	fx.api.runs = nil
	fx.run(pairT0.Add(pairReadyInterval))

	items := fx.check.observed()
	require.Len(t, items, 1, "a run list with no run for the commit cleared the failure")
	assert.Equal(t, attention.KindImagesFailed, items[0].Kind)
}

// TestImagesFailedRunListErrorSkipsOnlyThatCheck: a run list that cannot be read
// says nothing about the commit. The check logs it once and keeps what it held
// for the commit; it does not mark the rig unreadable, so the pass still answers.
func TestImagesFailedRunListErrorSkipsOnlyThatCheck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		apply func(fx *pairFixture)
		want  string
	}{
		{
			name:  "the run list fails",
			apply: func(fx *pairFixture) { fx.api.runErr = errors.New("forgejo answered 500 on the run list") },
			want:  "forgejo answered 500 on the run list",
		},
		{
			name:  "the run list answers nothing",
			apply: func(fx *pairFixture) { fx.api.nilList = true },
			want:  "the list is not there",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newPairFixture(t)
			fx.reg = &fakePairRegistry{}
			tc.apply(fx)

			fx.run(pairT0)

			assert.Empty(t, fx.check.observed(), "an unreadable run list raised an item or marked the rig unreadable")
			require.Len(t, fx.logs, 1, "the unreadable run list was not logged once")
			assert.Contains(t, fx.logs[0], tc.want)
			assert.Contains(t, fx.logs[0], pairGreen12)
		})
	}
}

// TestImagesFailedRunListErrorKeepsWhatItHeld: after a failed run raised its
// item, a run list that then fails keeps that item rather than clearing it.
func TestImagesFailedRunListErrorKeepsWhatItHeld(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.reg = &fakePairRegistry{}
	fx.setImagesRun(pairGreen, deployRunFailed)
	fx.run(pairT0)
	require.Len(t, fx.check.observed(), 1)

	fx.api.runErr = errors.New("forgejo answered 500 on the run list")
	fx.run(pairT0.Add(pairReadyInterval))

	items := fx.check.observed()
	require.Len(t, items, 1, "an unreadable run list cleared the failure it had raised")
	assert.Equal(t, attention.KindImagesFailed, items[0].Kind)
}

// TestPairReadyDoesNotAskForRunsWhenThePairIsPublished: a commit whose pair is
// in the registry needs no run read, so a failed run for it raises nothing.
func TestPairReadyDoesNotAskForRunsWhenThePairIsPublished(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.setImagesRun(pairGreen, deployRunFailed)

	fx.run(pairT0)

	items := fx.check.observed()
	require.Len(t, items, 1)
	assert.Equal(t, attention.KindDeployPairReady, items[0].Kind)
	assert.Empty(t, fx.api.runsQueries, "a commit whose pair is published had its run read")
}

// TestImagesFailedReadsAFailedRunsLogOnce: a failed run keeps its item across
// scans, so its job and log are read once rather than on every scan.
func TestImagesFailedReadsAFailedRunsLogOnce(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.reg = &fakePairRegistry{}
	fx.setImagesRun(pairGreen, deployRunFailed)
	fx.run(pairT0)
	require.Len(t, fx.check.observed(), 1)
	reads := fx.api.jobReads
	require.NotZero(t, reads, "the failed run's log was not read")

	fx.run(pairT0.Add(pairReadyInterval))
	fx.run(pairT0.Add(2 * pairReadyInterval))

	assert.Equal(t, reads, fx.api.jobReads, "a failed run's job or log was read again on a later scan")
	items := fx.check.observed()
	require.Len(t, items, 1)
	assert.Contains(t, items[0].Summary, pairImagesLine)
}

// TestImagesFailedRereadsAnUnreadableLog: a log the check could not read is no
// answer to keep, so the next scan reads it again and names the Error line once
// it can.
func TestImagesFailedRereadsAnUnreadableLog(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.reg = &fakePairRegistry{}
	fx.setImagesRun(pairGreen, deployRunFailed)
	fx.api.logErr = errors.New("forgejo answered 404 on the log")
	fx.run(pairT0)
	require.Len(t, fx.check.observed(), 1)
	assert.Contains(t, fx.check.observed()[0].Summary, "the job log could not be read")

	fx.api.logErr = nil
	fx.run(pairT0.Add(pairReadyInterval))

	items := fx.check.observed()
	require.Len(t, items, 1)
	assert.Contains(t, items[0].Summary, pairImagesLine, "the log was not read again")
}

// TestImagesFailedQuotesTheLogLineAsData: the log is text a build writes, so the
// summary states its Error line quoted as CI output, with control characters
// dropped, double quotes made single, and the line capped at 160 characters.
func TestImagesFailedQuotesTheLogLineAsData(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		line string
		want string
	}{
		{
			name: "control characters and double quotes",
			line: "Error:\tthe\x07 pin \"drifted\"\x1b[0m",
			want: `log: "Error: the pin 'drifted'[0m"`,
		},
		{
			name: "a line past the cap",
			line: "Error: " + strings.Repeat("x", 300),
			want: `log: "Error: ` + strings.Repeat("x", 153) + `..."`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newPairFixture(t)
			fx.reg = &fakePairRegistry{}
			fx.setImagesRun(pairGreen, deployRunFailed)
			fx.api.logs[pairImagesJob] = "[preflight] checking the pins\n" + tc.line + "\n"

			fx.run(pairT0)

			items := fx.check.observed()
			require.Len(t, items, 1)
			assert.Contains(t, items[0].Summary, tc.want)
		})
	}
}

// TestImagesFailedHasItsOwnCollector: a failed build is a third kind from the
// pair check's one read, so it needs a collector of its own to reach the queue
// and to survive a failed collector beside it.
func TestImagesFailedHasItsOwnCollector(t *testing.T) {
	t.Parallel()
	failed := attention.Item{Key: "images-failed:gastown:" + pairGreen12, Kind: attention.KindImagesFailed}
	s := &attentionSources{pairReady: func() []attention.Item { return []attention.Item{failed} }}

	var found bool
	for _, c := range s.collectors() {
		if c.kind != attention.KindImagesFailed {
			continue
		}
		found = true
		items, err := c.collect(context.Background())
		require.NoError(t, err)
		require.Len(t, items, 1)
		assert.Equal(t, failed, items[0])
		kept := c.kept(attention.State{Items: []attention.Item{failed}})
		require.Len(t, kept, 1)
		assert.Equal(t, attention.KindImagesFailed, kept[0].Kind)
	}
	assert.True(t, found, "no collector raises the images-failed kind, so its items never reach the queue")
}
