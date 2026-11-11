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

// fakePairAPI is the viewer's Forgejo read: the branches a listing returns and
// the commit counts a comparison returns, keyed "base...head". A comparison with
// no entry is an error, so a test that drops one exercises that refusal.
type fakePairAPI struct {
	branches  []forgejo.Branch
	branchErr error
	// endless makes every listing page a full page of branches that are not
	// green, so the listing never ends.
	endless  bool
	compares map[string]int
}

func (f *fakePairAPI) ListBranches(_ context.Context, _, _ string, page, limit int) ([]forgejo.Branch, error) {
	if f.branchErr != nil {
		return nil, f.branchErr
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

// pairFixture is one rig's check: a town whose values file names the running
// commit, a viewer API and a registry. Its logs are what the check said.
type pairFixture struct {
	town    string
	api     *fakePairAPI
	reg     pairReadyRegistry
	rigErr  error
	check   pairReadyCheck
	logs    []string
	daemon  string
	polecat string
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
		branches: []forgejo.Branch{{Name: "green/" + pairGreen12, Commit: struct {
			ID string `json:"id"`
		}{ID: pairGreen}}},
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

// run makes the check at now, with the fixture's one rig.
func (fx *pairFixture) run(now time.Time) {
	rigs := func() ([]pairReadyRig, error) {
		return []pairReadyRig{{
			Rig: "gastown", Owner: pairOwner, Repo: pairRepo,
			API: fx.api, Registry: fx.reg, Err: fx.rigErr,
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

// TestPairReadyRefusalKeepsWhatTheRigHeld: a read that fails keeps the item the
// rig held, since an unanswered read says nothing about whether a pair exists.
// Each refusal is logged.
func TestPairReadyRefusalKeepsWhatTheRigHeld(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		apply func(t *testing.T, fx *pairFixture)
	}{
		{
			name: "the registry read fails",
			apply: func(_ *testing.T, fx *pairFixture) {
				fx.reg = &fakePairRegistry{err: errors.New("the registry answered HTTP 503")}
			},
		},
		{
			name: "the branch listing fails",
			apply: func(_ *testing.T, fx *pairFixture) {
				fx.api.branchErr = errors.New("forgejo answered 500")
			},
		},
		{
			name: "a comparison fails",
			apply: func(_ *testing.T, fx *pairFixture) {
				delete(fx.api.compares, pairRunning+"..."+pairGreen)
			},
		},
		{
			name: "the comparison that says green is newer fails",
			apply: func(_ *testing.T, fx *pairFixture) {
				delete(fx.api.compares, pairGreen+"..."+pairRunning)
			},
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
		},
		{
			name: "the branch listing never ends",
			apply: func(_ *testing.T, fx *pairFixture) {
				fx.api.endless = true
			},
		},
		{
			name: "the values file does not parse",
			apply: func(t *testing.T, fx *pairFixture) {
				path := imagevalues.PathIn(filepath.Join(fx.town, "gastown"))
				require.NoError(t, os.WriteFile(path, []byte("commit: [unclosed\n"), 0o600))
			},
		},
		{
			name: "a green branch names another commit",
			apply: func(_ *testing.T, fx *pairFixture) {
				fx.api.branches[0].Commit.ID = pairOther
			},
		},
		{
			name: "the rig cannot be resolved",
			apply: func(_ *testing.T, fx *pairFixture) {
				fx.rigErr = errors.New("rig gastown names no merge_queue.forgejo remote_url")
			},
		},
		{
			name: "the values file is gone",
			apply: func(t *testing.T, fx *pairFixture) {
				require.NoError(t, os.Remove(imagevalues.PathIn(filepath.Join(fx.town, "gastown"))))
			},
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
			require.Len(t, items, 1, "a failed read cleared the item the rig held")
			assert.Equal(t, "deploy-pair-ready:gastown:"+pairGreen12, items[0].Key)
			assert.NotEmpty(t, fx.logs, "a failed read was not logged")
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

// TestPairReadyKeepsWhatItHeldWhenTheRigListFails: when the image rigs cannot be
// named, no rig is read and every item the check held stays, with the failure
// logged.
func TestPairReadyKeepsWhatItHeldWhenTheRigListFails(t *testing.T) {
	t.Parallel()
	fx := newPairFixture(t)
	fx.run(pairT0)
	require.Len(t, fx.check.observed(), 1)

	var logs []string
	fx.check.run(context.Background(), fx.town, pairT0.Add(pairReadyInterval),
		func() ([]pairReadyRig, error) { return nil, errors.New("image rig ghost is not a known rig") },
		func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) })

	require.Len(t, fx.check.observed(), 1, "a failed rig list cleared the item")
	require.NotEmpty(t, logs, "the failed rig list was not logged")
	assert.Contains(t, logs[0], "is not a known rig")
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

		assert.Empty(t, fx.check.observed())
		require.NotEmpty(t, fx.logs, "the failed read was not logged")
		assert.NotContains(t, strings.Join(fx.logs, "\n"), pairToken)
	})
}
