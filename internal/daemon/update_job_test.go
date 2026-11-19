package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	updatejob "github.com/steveyegge/gastown/deploy/k8s/update-job"
	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/landworker"
)

// testUpdateJobBound is the create deadline the tests shorten to. The daemon's
// clock is a fake, so no test waits on it: a test moves the clock past it.
const testUpdateJobBound = time.Minute

// fakeUpdateJob is the cluster: the Jobs it holds by name, and every create the
// daemon made, with the request timeout each one carried. refuse, when set, is
// what every create returns instead. hang, when set, makes each create signal
// entered and then wait: for its context to end, or with ignoreCtx for release,
// the way a cluster that never answers would. A hung create always ends when the
// test releases the cluster at cleanup, so a failed test leaves no goroutine.
type fakeUpdateJob struct {
	calls    []string
	timeouts []time.Duration
	made     map[string][]byte
	refuse   error
	hang     bool
	// ignoreCtx makes a hung create wait for release instead of its context.
	ignoreCtx bool
	entered   chan struct{}
	release   chan struct{}
	// active is the update Jobs the cluster reports as not yet finished; listErr,
	// when set, is what a list returns instead.
	active  []string
	listErr error
	// existsCalls records the Job names the daemon asked the cluster about; jobGetErr,
	// when set, is what every existence read returns instead.
	existsCalls []string
	jobGetErr   error
}

func (f *fakeUpdateJob) ActiveJobs(context.Context) ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.active, nil
}

func (f *fakeUpdateJob) JobExists(_ context.Context, name string) (bool, error) {
	f.existsCalls = append(f.existsCalls, name)
	if f.jobGetErr != nil {
		return false, f.jobGetErr
	}
	_, ok := f.made[name]
	return ok, nil
}

func (f *fakeUpdateJob) CreateJob(ctx context.Context, name string, manifest []byte, requestTimeout time.Duration) error {
	f.calls = append(f.calls, name)
	f.timeouts = append(f.timeouts, requestTimeout)
	if f.hang {
		f.entered <- struct{}{}
		if f.ignoreCtx {
			<-f.release
			return errors.New("released after the bound")
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-f.release:
			return errors.New("released by the test")
		}
	}
	if f.refuse != nil {
		return f.refuse
	}
	if _, ok := f.made[name]; ok {
		return errUpdateJobExists
	}
	f.made[name] = manifest
	return nil
}

// fakeMainState is the rig's durable main state as a test sets it: every load
// answers st, or err when set. Save is never reached by a roll's read.
type fakeMainState struct {
	st  landworker.MainState
	err error
}

func (f *fakeMainState) Load() (landworker.MainState, error) { return f.st, f.err }
func (f *fakeMainState) Save(landworker.MainState) error     { return nil }

// updateJobRig is a pod daemon over a town whose gastown bare clone holds three
// commits on main, the running commit being the middle one. main's tip is
// commits[2] as origin reports it (remote), and its verdict is what main holds.
type updateJobRig struct {
	d        *Daemon
	logs     *bytes.Buffer
	cluster  *fakeUpdateJob
	launches *int
	town     string
	bare     string
	git      pairReadyGit
	commits  []string
	clock    *clockwork.FakeClock
	remote   *updateRemoteFake
	main     *fakeMainState
}

// newUpdateJobRig builds the rig with cfg as the update block and the pod switch
// on. The running images name commits[1].
func newUpdateJobRig(t *testing.T, cfg *config.UpdateConfig) *updateJobRig {
	t.Helper()
	d, logs, launches, _ := newUpdateTriggerTestDaemon(t, cfg)
	setUpdateSource(t, d)
	clock := clockwork.NewFakeClockAt(updateTriggerTestTime)
	d.clock = clock
	town := d.config.TownRoot
	bare := constants.RigBareRepoPath(filepath.Join(town, "gastown"))
	require.NoError(t, os.MkdirAll(bare, 0o755))
	world := gitfake.New()
	world.InitBare(t, bare)
	var commits []string
	for i, msg := range []string{"first", "second", "third"} {
		commits = append(commits, world.Commit(t, bare, "main", msg, map[string]string{"f": strconv.Itoa(i)}))
	}
	d.openGitFn = func(dir string) daemonGit { return world.Open(dir).(daemonGit) }
	d.podMode = true
	// Nothing in this rig reads the host's environment or a resolver: the gateway
	// URL is empty unless a test sets one, and a lookup is a test's fake.
	d.getenv = func(string) string { return "" }
	d.lookupHost = func(_ context.Context, host string) ([]string, error) {
		return nil, errors.New("no resolver in this rig: " + host)
	}
	cluster := &fakeUpdateJob{made: map[string][]byte{}, entered: make(chan struct{}, 8), release: make(chan struct{})}
	t.Cleanup(func() { close(cluster.release) })
	d.updateJob = cluster
	// The rig's main tip is commits[2] and its verdict is green unless a test says
	// otherwise: the last run reached a verdict at the tip and passed.
	remote := &updateRemoteFake{output: commits[2] + "\trefs/heads/main\n"}
	d.updateRemoteRun = remote.run
	main := &fakeMainState{st: landworker.MainState{LastRun: commits[2], LastGreen: commits[2]}}
	d.postLandRunners.set("gastown", &landworker.PostLandRunner{Rig: "gastown", State: main})
	// The town has no beads repository, so the skew guard's reads are wired to
	// agree: these tests are about the roll, not the guard.
	d.pairReady.skew = pairAgreeingSkewReads(pairSchemaLevel)
	writeRunningImages(t, town, commits[1])
	return &updateJobRig{
		d: d, logs: logs, cluster: cluster, launches: launches, town: town,
		bare: bare, git: world.Open(bare).(pairReadyGit), commits: commits, clock: clock,
		remote: remote, main: main,
	}
}

// writeRunningImages writes the values file the running commit and the running
// daemon image are read from.
func writeRunningImages(t *testing.T, town, commit string) {
	t.Helper()
	reg := imagevalues.Registry{Scheme: "https", Host: "registry.test:5000", Owner: "town"}
	require.NoError(t, imagevalues.Write(imagevalues.PathIn(filepath.Join(town, "gastown")), imagevalues.Values{
		Daemon:  reg.Ref(imagevalues.DaemonImage, digestOf([]byte("running daemon"))),
		Polecat: reg.Ref(imagevalues.PolecatImage, digestOf([]byte("running polecat"))),
		Commit:  commit,
	}))
}

// pairFor runs the production pair check over the rig's clone for green, with
// the registry holding the images reg names, so the daemon holds the items the
// check raises for that read.
func (r *updateJobRig) pairFor(t *testing.T, green string, daemonPublished, polecatPublished bool) {
	t.Helper()
	r.pairForAt(t, green, updateTriggerTestTime, daemonPublished, polecatPublished)
}

// pairForAt is pairFor at now, so a test can raise a newer pair after the read
// the check already made at an earlier time.
func (r *updateJobRig) pairForAt(t *testing.T, green string, now time.Time, daemonPublished, polecatPublished bool) {
	t.Helper()
	digests := map[string]string{}
	if daemonPublished {
		digests[imagevalues.DaemonImage+"@"+green[:12]] = digestOf([]byte("daemon manifest"))
	}
	if polecatPublished {
		digests[imagevalues.PolecatImage+"@"+green[:12]] = digestOf([]byte("polecat manifest"))
	}
	api := &fakePairAPI{branches: []forgejo.Branch{pairBranch(green)}, compares: map[string]int{}}
	reg := &fakePairRegistry{digests: digests}
	rigs := func() ([]pairReadyRig, error) {
		return []pairReadyRig{{
			Rig: "gastown", Owner: pairOwner, Repo: pairRepo, API: api, Registry: reg, Git: r.git,
			Pins: func(string) (string, error) { return pairImagePins(pairBdPin), nil },
		}}, nil
	}
	r.d.pairReady.run(context.Background(), r.town, now, rigs, func(string, ...any) {})
}

// rollGitStub judges ancestry from a fixed table: the pairs it lists are
// ancestors, and the pairs in errs fail.
type rollGitStub struct {
	ancestors map[[2]string]bool
	errs      map[[2]string]error
}

func (s rollGitStub) HasObject(string) (bool, error) { return true, nil }

func (s rollGitStub) IsAncestor(ancestor, descendant string) (bool, error) {
	k := [2]string{ancestor, descendant}
	if err := s.errs[k]; err != nil {
		return false, err
	}
	return s.ancestors[k], nil
}

// TestPairReadySHAForRoll: the roll goes to the pair-ready commit ahead of the
// running one, and to the newest of several that line up. Every refusal names why.
func TestPairReadySHAForRoll(t *testing.T) {
	t.Parallel()
	const (
		c0 = "1000000000000000000000000000000000000000"
		c1 = "2000000000000000000000000000000000000000"
		c2 = "3000000000000000000000000000000000000000"
		c3 = "4000000000000000000000000000000000000000"
	)
	anc := func(pairs ...[2]string) map[[2]string]bool {
		m := map[[2]string]bool{}
		for _, p := range pairs {
			m[p] = true
		}
		return m
	}
	item := func(rig, sha string) attention.Item {
		return attention.Item{Kind: attention.KindDeployPairReady, Rig: rig, SHA: sha}
	}
	tests := []struct {
		name       string
		items      []attention.Item
		running    string
		git        rollGitStub
		wantSHA    string
		wantReason string
	}{
		{
			name:    "one pair ahead of running",
			items:   []attention.Item{item("gastown", c2)},
			running: c1,
			git:     rollGitStub{ancestors: anc([2]string{c1, c2})},
			wantSHA: c2,
		},
		{
			name:    "the newest of a line of pairs",
			items:   []attention.Item{item("gastown", c2), item("gastown", c3)},
			running: c0,
			git: rollGitStub{ancestors: anc(
				[2]string{c0, c2}, [2]string{c0, c3}, [2]string{c2, c3})},
			wantSHA: c3,
		},
		{
			name:       "a pair equal to running is not ahead",
			items:      []attention.Item{item("gastown", c1)},
			running:    c1,
			git:        rollGitStub{},
			wantReason: "no pair-ready green commit is ahead of the running commit",
		},
		{
			name:       "a pair behind running is not ahead",
			items:      []attention.Item{item("gastown", c0)},
			running:    c1,
			git:        rollGitStub{ancestors: anc([2]string{c0, c1})},
			wantReason: "no pair-ready green commit is ahead of the running commit",
		},
		{
			name:       "a rig with no local clone refuses the decision",
			items:      []attention.Item{item("other", c2)},
			running:    c1,
			git:        rollGitStub{ancestors: anc([2]string{c1, c2})},
			wantReason: "clone missing: the local clone of other is missing",
		},
		{
			name:    "a clone that cannot judge the pair refuses the decision",
			items:   []attention.Item{item("gastown", c2)},
			running: c1,
			git: rollGitStub{errs: map[[2]string]error{
				{c1, c2}: errors.New("git failed"),
			}},
			wantReason: "ancestry check failed: judging pair-ready " + c2[:12] + " against the running commit",
		},
		{
			name:    "pairs that do not form one line refuse the decision",
			items:   []attention.Item{item("gastown", c1), item("gastown", c2)},
			running: c0,
			git: rollGitStub{ancestors: anc(
				[2]string{c0, c1}, [2]string{c0, c2})},
			wantReason: "do not form one line",
		},
		{
			name:    "a comparison between two pairs that fails refuses the decision",
			items:   []attention.Item{item("gastown", c1), item("gastown", c2)},
			running: c0,
			git: rollGitStub{
				ancestors: anc([2]string{c0, c1}, [2]string{c0, c2}),
				errs:      map[[2]string]error{{c2, c1}: errors.New("git failed")},
			},
			wantReason: "pair not judgeable: judging pair-ready " + c2[:12] + " against " + c1[:12],
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clone := func(rig string) pairReadyGit {
				if rig != "gastown" {
					return nil
				}
				return tt.git
			}
			gotSHA, gotReason := pairReadySHAForRoll(tt.items, tt.running, clone)
			assert.Equal(t, tt.wantSHA, gotSHA)
			if tt.wantReason == "" {
				assert.Empty(t, gotReason)
			} else {
				assert.Contains(t, gotReason, tt.wantReason)
			}
		})
	}
}

// TestUpdateJobOffCreatesNothingAndLogsOnce: a pod whose switch is off, with a
// pair ready that would roll, creates no Job and says it is off once per start.
func TestUpdateJobOffCreatesNothingAndLogsOnce(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  *config.UpdateConfig
	}{
		{name: "absent block", cfg: nil},
		{name: "enabled false", cfg: &config.UpdateConfig{Enabled: false, Window: "03:00"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newUpdateJobRig(t, tt.cfg)
			r.pairFor(t, r.commits[2], true, true)

			r.d.triggerUpdate()
			r.d.triggerUpdate()

			assert.Empty(t, r.cluster.calls, "a Job was created with the switch off")
			assert.Equal(t, 0, *r.launches)
			assert.Equal(t, 1, strings.Count(r.logs.String(), "off in this pod"), "the off line is not logged once")
		})
	}
}

// TestUpdateJobCreatesOneJobForThePairReadyCommit: with the switch on inside the
// window and a pair ready ahead of the running commit, exactly one Job is created,
// named for the commit, and the child is not started.
func TestUpdateJobCreatesOneJobForThePairReadyCommit(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	green := r.commits[2]
	r.pairFor(t, green, true, true)

	r.d.triggerUpdate()
	r.d.triggerUpdate()

	name := "gt-update-" + green[:12]
	assert.Equal(t, []string{name}, r.cluster.calls, "the Job was not created exactly once")
	manifest, ok := r.cluster.made[name]
	require.True(t, ok, "no Job named for the commit")
	assert.Contains(t, string(manifest), "name: "+name)
	assert.Contains(t, string(manifest), "namespace: gt")
	assert.Contains(t, string(manifest), green, "the Job does not roll to the pair-ready commit")
	assert.Equal(t, 0, *r.launches, "a pod started the detached child")
	assert.Contains(t, r.logs.String(), "created Job "+name)
}

// TestUpdateJobCreatesNothingForAPairThatIsNotAhead: the check raises no pair for
// a green commit equal to or older than the running one, so nothing is created.
func TestUpdateJobCreatesNothingForAPairThatIsNotAhead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		green func(r *updateJobRig) string
	}{
		{name: "equal to the running commit", green: func(r *updateJobRig) string { return r.commits[1] }},
		{name: "older than the running commit", green: func(r *updateJobRig) string { return r.commits[0] }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
			r.pairFor(t, tt.green(r), true, true)

			r.d.triggerUpdate()

			assert.Empty(t, r.cluster.calls)
			assert.Contains(t, r.logs.String(), "no pair-ready green commit is ahead of the running commit")
		})
	}
}

// TestUpdateJobCreatesNothingWithOneImageMissing: a commit with only one of its
// two images published is not a pair, so the check raises nothing and no Job is
// created.
func TestUpdateJobCreatesNothingWithOneImageMissing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		daemonPublished  bool
		polecatPublished bool
	}{
		{name: "daemon image missing", daemonPublished: false, polecatPublished: true},
		{name: "polecat image missing", daemonPublished: true, polecatPublished: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
			r.pairFor(t, r.commits[2], tt.daemonPublished, tt.polecatPublished)

			r.d.triggerUpdate()

			assert.Empty(t, r.cluster.calls)
			assert.Contains(t, r.logs.String(), "no pair-ready green commit is ahead of the running commit")
		})
	}
}

// TestUpdateJobAlreadyExistsIsAlreadyLaunched: a Job the cluster already holds for
// the commit is the answer already launched. It is not a failure, and the next
// heartbeat waits out the suppression rather than asking again.
func TestUpdateJobAlreadyExistsIsAlreadyLaunched(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	green := r.commits[2]
	name := "gt-update-" + green[:12]
	r.cluster.made[name] = []byte("earlier")
	r.pairFor(t, green, true, true)

	r.d.triggerUpdate()
	r.d.triggerUpdate()

	assert.Equal(t, []string{name}, r.cluster.calls, "the existing Job was asked for again")
	assert.Contains(t, r.logs.String(), "Job "+name+" is already launched")
	assert.NotContains(t, r.logs.String(), "launch of the update Job failed")
	assert.Equal(t, []byte("earlier"), r.cluster.made[name], "the existing Job was replaced")
}

// TestUpdateJobExistingPairIsNotDueAgain: a partial roll leaves the running values
// naming the same pair, so the pair's Job is in the cluster but no new roll is due.
// Past the suppression the launch bought, still inside the window that keeps the
// roll due, the patrol reports that the roll already ran rather than asking the
// cluster for the same Job and logging a roll that keeps starting.
func TestUpdateJobExistingPairIsNotDueAgain(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// heldBefore is a Job in the cluster before the patrol's first cycle; when
		// false the patrol creates it, and the pair is due again after the roll.
		heldBefore bool
	}{
		{name: "the Job was in the cluster before the patrol", heldBefore: true},
		{name: "the patrol created the Job", heldBefore: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
			green := r.commits[2]
			name := "gt-update-" + green[:12]
			if tt.heldBefore {
				r.cluster.made[name] = []byte("earlier")
			}
			r.pairFor(t, green, true, true)

			r.d.triggerUpdate()
			if tt.heldBefore {
				// The launch that found the Job records the roll as already run,
				// rather than leaving the next cycle to report it.
				require.Contains(t, r.logs.String(), "not due — the roll for "+green[:12]+" already ran")
			}
			r.clock.Advance(updateLaunchSuppression + time.Minute)
			r.d.triggerUpdate()

			assert.Equal(t, []string{name}, r.cluster.calls, "a later cycle asked the cluster for the pair's Job")
			assert.Equal(t, 1, strings.Count(r.logs.String(), "starting the update Job"), "a later cycle logged the roll as starting")
			assert.Contains(t, r.logs.String(), "not due — the roll for "+green[:12]+" already ran")
			assert.Equal(t, 0, *r.launches, "a pod started the detached child")
		})
	}
}

// TestUpdateJobRemovedFromTheClusterIsDueAgain: the record that a pair's Job
// exists is checked against the cluster, since the roll's Job carries a TTL after
// it finishes and an operator can delete it. A pair whose Job has left the
// cluster is due again: no Job in the cluster is no evidence the roll ran.
func TestUpdateJobRemovedFromTheClusterIsDueAgain(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	green := r.commits[2]
	name := "gt-update-" + green[:12]
	r.pairFor(t, green, true, true)

	r.d.triggerUpdate()
	require.Equal(t, []string{name}, r.cluster.calls, "the patrol did not create the pair's Job")
	require.NotContains(t, r.logs.String(), "already ran", "a created Job was reported not due")

	// The cluster drops the Job: the sweep after ttlSecondsAfterFinished, or an
	// operator's delete.
	delete(r.cluster.made, name)
	r.clock.Advance(updateLaunchSuppression + time.Minute)
	r.d.triggerUpdate()

	assert.Equal(t, []string{name, name}, r.cluster.calls, "a pair whose Job left the cluster was held not due")
	assert.Equal(t, []string{name}, r.cluster.existsCalls, "the recorded Job was not read from the cluster")
	assert.Equal(t, 2, strings.Count(r.logs.String(), "created Job "+name), "the removed Job was not created again")
}

// TestUpdateJobJobReadFailureHoldsTheRoll: a recorded Job the cluster cannot be
// asked about is not taken as gone. The cycle holds with a reason naming the
// read, and the pair is not launched.
func TestUpdateJobJobReadFailureHoldsTheRoll(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	green := r.commits[2]
	name := "gt-update-" + green[:12]
	r.pairFor(t, green, true, true)

	r.d.triggerUpdate()
	require.Equal(t, []string{name}, r.cluster.calls, "the patrol did not create the pair's Job")

	r.cluster.jobGetErr = errors.New("kubectl cannot get jobs")
	r.clock.Advance(updateLaunchSuppression + time.Minute)
	r.d.triggerUpdate()

	assert.Equal(t, []string{name}, r.cluster.calls, "a pair was launched without the recorded Job's read")
	assert.Contains(t, r.logs.String(),
		"not due — the update Job for "+green[:12]+" cannot be read: kubectl cannot get jobs; not starting a roll")
}

// TestUpdateJobPresentRefusesNoClient: the existence read refuses a daemon with
// no cluster client instead of calling into a nil one. The roll hold refuses the
// same daemon before this read runs, so the guard is exercised here directly.
func TestUpdateJobPresentRefusesNoClient(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	r.d.updateJob = nil

	held, err := r.d.updateJobPresent(r.commits[2])

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no cluster client is wired into this daemon")
	assert.False(t, held)
}

// TestUpdateJobNewerPairLaunchesItsOwnJob: the recorded pair stays quiet only
// while it is the roll's target. A newer pair-ready commit is a new roll, so its
// own Job is created.
func TestUpdateJobNewerPairLaunchesItsOwnJob(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	// Both pair-ready commits are ahead of the running one, so the newest is the
	// roll's target.
	writeRunningImages(t, r.town, r.commits[0])
	older := r.commits[1]
	olderName := "gt-update-" + older[:12]
	r.cluster.made[olderName] = []byte("earlier")
	r.pairFor(t, older, true, true)

	r.d.triggerUpdate()
	require.Equal(t, []string{olderName}, r.cluster.calls, "the older pair's existing Job was not found first")

	// A pair read past the check's own interval, and a heartbeat past the
	// suppression the earlier launch bought, both still inside the 03:00 window.
	later := pairReadyInterval + time.Minute
	r.pairForAt(t, r.commits[2], updateTriggerTestTime.Add(later), true, true)
	r.clock.Advance(later)
	r.d.triggerUpdate()

	newName := "gt-update-" + r.commits[2][:12]
	assert.Equal(t, []string{olderName, newName}, r.cluster.calls, "a newer pair did not launch its own Job")
	assert.Contains(t, r.logs.String(), "created Job "+newName)
}

// TestUpdateJobRefusedLaunchWaitsOutTheBackoff: a cluster that refuses the create
// logs the refusal and is not asked again inside the first backoff.
func TestUpdateJobRefusedLaunchWaitsOutTheBackoff(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	r.cluster.refuse = errors.New("cluster unreachable")
	r.pairFor(t, r.commits[2], true, true)

	r.d.triggerUpdate()
	r.d.triggerUpdate()

	assert.Len(t, r.cluster.calls, 1, "a refused launch was retried inside its backoff")
	assert.Contains(t, r.logs.String(), "launch of the update Job failed: cluster unreachable")
	assert.NotContains(t, r.logs.String(), "created Job")
}

// TestUpdateJobBlockingCreateTimesOut: a cluster that never answers holds the
// create until its context ends. The heartbeat returns at the daemon's deadline
// with no launch recorded, the create carried kubectl's bound less the slack, and
// the timeout is logged as a timeout.
func TestUpdateJobBlockingCreateTimesOut(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	r.cluster.hang = true
	r.d.updateTrigger.jobBound = testUpdateJobBound
	r.pairFor(t, r.commits[2], true, true)
	clock := r.clock

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.d.triggerUpdate()
	}()
	awaitOrFail(t, r.cluster.entered, "the create to reach the cluster") // in flight, its deadline already on the clock
	clock.Advance(testUpdateJobBound)
	awaitOrFail(t, done, "the heartbeat to return at the create's deadline")

	name := "gt-update-" + r.commits[2][:12]
	assert.Equal(t, []string{name}, r.cluster.calls)
	assert.Equal(t, []time.Duration{testUpdateJobBound - updateJobRequestSlack}, r.cluster.timeouts,
		"kubectl's request timeout is not the deadline less the slack")
	assert.True(t, r.d.lastUpdateLaunch().IsZero(), "a timed-out create was recorded as launched")
	assert.Contains(t, r.logs.String(), "launch of the update Job timed out after "+testUpdateJobBound.String())
	assert.NotContains(t, r.logs.String(), "created Job")
}

// TestUpdateJobHungCreateIsNotStacked: a create that ignores its context is
// abandoned at the deadline, and the next attempt, past its backoff, does not
// start a second create while the first is still out. That attempt is a wait on
// the outstanding create: it is logged as one and does not count as a failed
// launch, so no backoff is noted for it.
func TestUpdateJobHungCreateIsNotStacked(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	r.cluster.hang = true
	r.cluster.ignoreCtx = true
	r.d.updateTrigger.jobBound = testUpdateJobBound
	r.pairFor(t, r.commits[2], true, true)
	clock := r.clock

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.d.triggerUpdate()
	}()
	awaitOrFail(t, r.cluster.entered, "the first create to reach the cluster")
	clock.Advance(testUpdateJobBound)
	awaitOrFail(t, done, "the heartbeat to return at the first create's deadline")

	// Past the backoff, the next attempt must return at once with the outstanding
	// refusal. A second create would hang on the cluster until the test released
	// it, and then the wait on second fails naming the step.
	clock.Advance(updateJobBackoff(1))
	before := r.logs.Len()
	second := make(chan struct{})
	go func() {
		defer close(second)
		r.d.triggerUpdate()
	}()
	awaitOrFail(t, second, "the second attempt to return without a create while the first is out")

	secondLog := r.logs.String()[before:]
	assert.Len(t, r.cluster.calls, 1, "a second create started while the first was outstanding")
	assert.Contains(t, secondLog, "waiting on an outstanding Job create")
	assert.NotContains(t, secondLog, "not retrying for", "an outstanding create was counted as a failed launch")
}

// TestUpdateJobRefusalsBackOff: each consecutive refusal doubles the wait before
// the next attempt, from one minute to the five-minute cap, and no attempt comes
// early.
func TestUpdateJobRefusalsBackOff(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	r.cluster.refuse = errors.New("cluster unreachable")
	r.pairFor(t, r.commits[2], true, true)
	clock := r.clock

	steps := []struct {
		after       time.Duration
		wantAttempt bool
	}{
		// The first refusal waits 1m, the second 2m, the third 4m; the fourth
		// would wait 8m and is capped at 5m.
		{after: 0, wantAttempt: true},
		{after: 59 * time.Second, wantAttempt: false},
		{after: time.Second, wantAttempt: true},
		{after: 119 * time.Second, wantAttempt: false},
		{after: time.Second, wantAttempt: true},
		{after: 239 * time.Second, wantAttempt: false},
		{after: time.Second, wantAttempt: true},
		{after: 299 * time.Second, wantAttempt: false},
		{after: time.Second, wantAttempt: true},
	}
	for i, st := range steps {
		clock.Advance(st.after)
		before := len(r.cluster.calls)
		r.d.triggerUpdate()
		attempted := len(r.cluster.calls) > before
		require.Equal(t, st.wantAttempt, attempted, "step %d", i)
	}
	assert.Contains(t, r.logs.String(), "not retrying for 5m0s")
}

// TestUpdateJobBackoffDoublesAndCaps: the wait after the nth consecutive failure
// is one minute doubled for each failure after the first, never past the launch
// suppression.
func TestUpdateJobBackoffDoublesAndCaps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		failures int
		want     time.Duration
	}{
		{failures: 1, want: time.Minute},
		{failures: 2, want: 2 * time.Minute},
		{failures: 3, want: 4 * time.Minute},
		{failures: 4, want: updateLaunchSuppression},
		{failures: 9, want: updateLaunchSuppression},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.failures), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, updateJobBackoff(tt.failures))
		})
	}
}

// TestUpdateJobRefusalsLogTheirReason: a clone that is missing, a clone whose
// ancestry check fails, and a running commit that cannot be read each stop the
// launch with its own logged reason, so an operator can tell a refusal from a wait.
func TestUpdateJobRefusalsLogTheirReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		setup      func(t *testing.T, r *updateJobRig)
		wantReason string
	}{
		{
			name: "clone missing",
			setup: func(t *testing.T, r *updateJobRig) {
				require.NoError(t, os.RemoveAll(r.bare))
			},
			wantReason: "clone missing: the local clone of gastown is missing",
		},
		{
			name: "ancestry check failed",
			setup: func(_ *testing.T, r *updateJobRig) {
				open := r.d.openGitFn
				r.d.openGitFn = func(dir string) daemonGit { return ancestryErrGit{open(dir)} }
			},
			wantReason: "ancestry check failed: judging pair-ready",
		},
		{
			name: "running commit unreadable",
			setup: func(t *testing.T, r *updateJobRig) {
				require.NoError(t, os.WriteFile(imagevalues.PathIn(filepath.Join(r.town, "gastown")), []byte("daemon: [\n"), 0o600))
			},
			wantReason: "running commit unreadable: ",
		},
		{
			name: "gateway host unresolvable",
			setup: func(_ *testing.T, r *updateJobRig) {
				r.d.getenv = gatewayURLEnv("http://gateway-host.example:7440")
				r.d.lookupHost = func(context.Context, string) ([]string, error) { return nil, errors.New("no such host") }
			},
			wantReason: "launch of the update Job failed: resolve the gateway host gateway-host.example",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
			r.pairFor(t, r.commits[2], true, true)
			tt.setup(t, r)

			r.d.triggerUpdate()

			assert.Contains(t, r.logs.String(), tt.wantReason)
			assert.Empty(t, r.cluster.calls, "a refused launch reached the cluster")
		})
	}
}

// ancestryErrGit is a clone whose ancestry check fails; everything else is read
// from the real clone under it.
type ancestryErrGit struct{ daemonGit }

func (ancestryErrGit) IsAncestor(string, string) (bool, error) {
	return false, errors.New("ancestry unreadable")
}

// TestUpdateJobPodAndContainerModeCreatesTheJobOnly: a daemon that is both a pod
// and in container mode, the likely production shape, creates the Job and starts
// no detached child, since the child is what the pod's replacement takes down.
func TestUpdateJobPodAndContainerModeCreatesTheJobOnly(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	r.d.containerMode = true
	r.pairFor(t, r.commits[2], true, true)

	r.d.triggerUpdate()

	assert.Len(t, r.cluster.calls, 1, "the pod with container mode did not create its Job")
	assert.Equal(t, 0, *r.launches, "a pod with container mode started the detached child")
}

// TestUpdateJobContainerModeKeepsTheDetachedChild: a daemon in the supervisor
// container, not a pod, still launches the detached child and creates no Job.
func TestUpdateJobContainerModeKeepsTheDetachedChild(t *testing.T) {
	t.Parallel()
	d, _, launches, _ := newUpdateTriggerTestDaemon(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	setUpdateSource(t, d)
	d.containerMode = true
	cluster := &fakeUpdateJob{made: map[string][]byte{}}
	d.updateJob = cluster

	d.triggerUpdate()

	assert.Equal(t, 1, *launches, "the container daemon did not launch the detached child")
	assert.Empty(t, cluster.calls, "a container daemon created a Job")
}

// testRequestTimeout is the kubectl request timeout the kubectl tests pass in.
const testRequestTimeout = 25 * time.Second

// TestKubectlUpdateJobMapsTheAnswer: kubectl's exit status and its AlreadyExists
// answer are the outcome, the manifest is written where kubectl reads it with the
// request timeout set, and a refusal names the exit without echoing kubectl's output.
func TestKubectlUpdateJobMapsTheAnswer(t *testing.T) {
	t.Parallel()
	const name = "gt-update-abcdef012345"
	manifest := []byte("kind: Job\n")
	tests := []struct {
		name    string
		out     string
		exit    int
		runErr  error
		wantErr error
		wantMsg string
	}{
		{name: "created", out: "job created", exit: 0},
		{name: "already exists", out: `Error from server (AlreadyExists): jobs.batch "x" already exists`, exit: 1, wantErr: errUpdateJobExists},
		{name: "refused", out: "boom secret-token", exit: 1, wantMsg: "exited 1"},
		{name: "run failed", runErr: errors.New("no kubectl"), wantMsg: "running kubectl create"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var gotArgs []string
			run := func(_ context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
				gotArgs = args
				return tt.out, tt.exit, tt.runErr
			}
			err := kubectlUpdateJob{dir: dir, run: run}.CreateJob(context.Background(), name, manifest, testRequestTimeout)

			path := filepath.Join(dir, name+".yaml")
			assert.Equal(t, []string{"create", "-f", path}, gotArgs)
			got, readErr := os.ReadFile(path)
			require.NoError(t, readErr)
			assert.Equal(t, manifest, got)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			if tt.wantMsg == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantMsg)
			assert.NotContains(t, err.Error(), "secret-token", "kubectl's output reached the error")
		})
	}
}

// TestKubectlUpdateJobJobExists: the named Job is read from the update
// namespace. kubectl's NotFound answer is the Job being gone; any other refusal is
// an error naming the exit, and no refusal echoes kubectl's output.
func TestKubectlUpdateJobJobExists(t *testing.T) {
	t.Parallel()
	const name = "gt-update-abcdef012345"
	tests := []struct {
		name    string
		out     string
		exit    int
		runErr  error
		want    bool
		wantMsg string
	}{
		{name: "the Job is in the cluster", out: "job.batch/" + name, exit: 0, want: true},
		{name: "the Job is gone", out: `Error from server (NotFound): jobs.batch "` + name + `" not found`, exit: 1},
		{name: "a refusal", out: "boom secret-token", exit: 1, wantMsg: "kubectl get job " + name + " exited 1"},
		{name: "a read that cannot run", runErr: errors.New("no kubectl"), wantMsg: "running kubectl get job " + name},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var gotArgs []string
			run := func(_ context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
				gotArgs = args
				return tt.out, tt.exit, tt.runErr
			}
			got, err := kubectlUpdateJob{dir: t.TempDir(), run: run}.JobExists(context.Background(), name)

			assert.Equal(t, []string{"get", "job", name, "-n", updatejob.DefaultNamespace}, gotArgs)
			assert.Equal(t, tt.want, got)
			if tt.wantMsg == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantMsg)
			assert.NotContains(t, err.Error(), "secret-token", "kubectl's output reached the error")
		})
	}
}

// TestLaunchUpdateJobRefusesWhatItCannotLaunch: each refusal names what the
// launch could not read, render or send, and creates no Job.
func TestLaunchUpdateJobRefusesWhatItCannotLaunch(t *testing.T) {
	t.Parallel()
	valuesPath := func(r *updateJobRig) string {
		return imagevalues.PathIn(filepath.Join(r.town, "gastown"))
	}
	tests := []struct {
		name     string
		canceled bool
		// sha is the green commit to launch; empty is the rig's third commit.
		sha      string
		settings config.UpdateSettings
		setup    func(t *testing.T, r *updateJobRig)
		wantMsg  string
	}{
		{
			name:     "canceled context",
			canceled: true,
			settings: config.UpdateSettings{MaxRuntime: time.Hour},
			wantMsg:  context.Canceled.Error(),
		},
		{
			name:     "no cluster client",
			settings: config.UpdateSettings{MaxRuntime: time.Hour},
			setup:    func(_ *testing.T, r *updateJobRig) { r.d.updateJob = nil },
			wantMsg:  "no cluster client is wired",
		},
		{
			name:     "values file unreadable",
			settings: config.UpdateSettings{MaxRuntime: time.Hour},
			setup: func(t *testing.T, r *updateJobRig) {
				require.NoError(t, os.WriteFile(valuesPath(r), []byte("daemon: [\n"), 0o600))
			},
			wantMsg: "read the running images",
		},
		{
			name:     "no values file",
			settings: config.UpdateSettings{MaxRuntime: time.Hour},
			setup: func(t *testing.T, r *updateJobRig) {
				require.NoError(t, os.Remove(valuesPath(r)))
			},
			wantMsg: "no values file names the running daemon image",
		},
		{
			name:     "green commit is not a full sha",
			sha:      "4a01ba03",
			settings: config.UpdateSettings{MaxRuntime: time.Hour},
			wantMsg:  "render the update Job: Values.GreenCommit",
		},
		{
			name:     "job cannot be encoded",
			settings: config.UpdateSettings{MaxRuntime: time.Hour},
			setup: func(_ *testing.T, r *updateJobRig) {
				r.d.updateJobEncode = func(io.Writer, any) error { return errors.New("encode refused") }
			},
			wantMsg: "encode the update Job: encode refused",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
			if tt.setup != nil {
				tt.setup(t, r)
			}
			ctx := context.Background()
			if tt.canceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			sha := r.commits[2]
			if tt.sha != "" {
				sha = tt.sha
			}

			err := r.d.launchUpdateJob(ctx, tt.settings, sha)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantMsg)
			assert.Empty(t, r.cluster.calls, "a refused launch reached the cluster")
		})
	}
}

// TestKubectlUpdateJobRefusesAnUnwritableDirectory: a manifest the daemon cannot
// write is refused before kubectl runs, so kubectl never reads a partial file.
func TestKubectlUpdateJobRefusesAnUnwritableDirectory(t *testing.T) {
	t.Parallel()
	const name = "gt-update-abcdef012345"
	tests := []struct {
		name    string
		prepare func(t *testing.T, dir string) string
		wantMsg string
	}{
		{
			name: "update directory is a file",
			prepare: func(t *testing.T, dir string) string {
				file := filepath.Join(dir, "not-a-dir")
				require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
				return file
			},
			wantMsg: "create the update directory",
		},
		{
			name: "manifest path is a directory",
			prepare: func(t *testing.T, dir string) string {
				require.NoError(t, os.Mkdir(filepath.Join(dir, name+".yaml"), 0o700))
				return dir
			},
			wantMsg: "write the manifest of " + name,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := tt.prepare(t, t.TempDir())
			run := func(context.Context, string, []string, string, ...string) (string, int, error) {
				t.Error("kubectl ran for a manifest that was not written")
				return "", 0, nil
			}
			err := kubectlUpdateJob{dir: dir, run: run}.CreateJob(context.Background(), name, []byte("kind: Job\n"), testRequestTimeout)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantMsg)
		})
	}
}

// TestKubectlUpdateJobRefusesNoRequestTimeout: kubectl reads a zero request
// timeout as no bound, so a create with none is refused before kubectl runs.
func TestKubectlUpdateJobRefusesNoRequestTimeout(t *testing.T) {
	t.Parallel()
	const name = "gt-update-abcdef012345"
	dir := t.TempDir()
	run := func(context.Context, string, []string, string, ...string) (string, int, error) {
		t.Error("kubectl ran with no request timeout")
		return "", 0, nil
	}
	err := kubectlUpdateJob{dir: dir, run: run}.CreateJob(context.Background(), name, []byte("kind: Job\n"), 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no request timeout")
	_, statErr := os.Stat(filepath.Join(dir, name+".yaml"))
	assert.ErrorIs(t, statErr, os.ErrNotExist, "the manifest was written for a create that was refused")
}

// TestUpdateJobCreateSendsNoRequestTimeout: the create's args carry no request
// timeout; the bound is the context deadline (see CreateJob).
func TestUpdateJobCreateSendsNoRequestTimeout(t *testing.T) {
	t.Parallel()
	const name = "gt-update-abcdef012345"
	dir := t.TempDir()
	var gotArgs []string
	run := func(_ context.Context, _ string, _ []string, _ string, args ...string) (string, int, error) {
		gotArgs = args
		return "", 0, nil
	}
	require.NoError(t, kubectlUpdateJob{dir: dir, run: run}.CreateJob(context.Background(), name, []byte("kind: Job\n"), testRequestTimeout))
	path := filepath.Join(dir, name+".yaml")
	assert.Equal(t, []string{"create", "-f", path}, gotArgs)
}

// TestUpdateJobCreateBoundedByContext: kubectl's context carries a deadline of
// requestTimeout from the call and no later.
func TestUpdateJobCreateBoundedByContext(t *testing.T) {
	t.Parallel()
	const name = "gt-update-abcdef012345"
	dir := t.TempDir()
	var deadline time.Time
	var hasDeadline bool
	run := func(ctx context.Context, _ string, _ []string, _ string, _ ...string) (string, int, error) {
		deadline, hasDeadline = ctx.Deadline()
		return "", 0, nil
	}
	before := time.Now()
	require.NoError(t, kubectlUpdateJob{dir: dir, run: run}.CreateJob(context.Background(), name, []byte("kind: Job\n"), testRequestTimeout))
	after := time.Now()
	require.True(t, hasDeadline, "kubectl ran with no deadline")
	assert.False(t, deadline.Before(before.Add(testRequestTimeout)), "the deadline is shorter than the request timeout")
	assert.False(t, deadline.After(after.Add(testRequestTimeout)), "the deadline is later than the request timeout")
}

// TestLaunchUpdateJobBoundedNeverFindsItsOwnFinishedCreateOutstanding: a create
// whose client has returned is not outstanding. The goroutine closes the
// outstanding channel before it sends the answer, so the next attempt, which
// can start the moment the caller has the answer, never gets errUpdateJobOutstanding
// for a create that is over. The gate failed on the ordering under load
// (TestUpdateJobRefusalsBackOff, gt-wot51s).
func TestLaunchUpdateJobBoundedNeverFindsItsOwnFinishedCreateOutstanding(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	r.cluster.refuse = errors.New("cluster unreachable")
	settings := config.UpdateSettings{MaxRuntime: time.Hour}
	for i := 0; i < 3000; i++ {
		err := r.d.launchUpdateJobBounded(settings, r.commits[2])
		if errors.Is(err, errUpdateJobOutstanding) {
			t.Fatalf("attempt %d found the earlier, finished create outstanding", i)
		}
	}
}

// gatewayURLEnv answers BD_GATEWAY_URL with raw and every other name with "".
func gatewayURLEnv(raw string) func(string) string {
	return func(key string) string {
		if key == k8sdaemon.BdGatewayURLEnv {
			return raw
		}
		return ""
	}
}

// decodeUpdateJob reads the manifest the daemon created as the Job it rendered.
func decodeUpdateJob(t *testing.T, manifest []byte) updatejob.Job {
	t.Helper()
	require.NotEmpty(t, manifest, "the daemon created no Job")
	var job updatejob.Job
	require.NoError(t, yaml.Unmarshal(manifest, &job), "the created manifest is not a Job")
	return job
}

// jobEnvValue is the value of the environment entry named name in c, and whether
// c carries it.
func jobEnvValue(c updatejob.Container, name string) (string, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// jobGatewayTokenVolume is the pod volume that carries the daemon-class gateway
// token Secret, or nil when the Job mounts none.
func jobGatewayTokenVolume(job updatejob.Job) *updatejob.Volume {
	for i, v := range job.Spec.Template.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == k8sdaemon.BdGatewayDaemonSecretName {
			return &job.Spec.Template.Spec.Volumes[i]
		}
	}
	return nil
}

// TestUpdateJobGatewayURLResolvesName: a BD_GATEWAY_URL whose host is a name
// reaches the Job with the address the daemon resolves in the host, keeping the
// scheme, the port and the path, and the Job mounts the daemon-class token Secret
// both containers read. The first IPv4 answer is the one used.
func TestUpdateJobGatewayURLResolvesName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		raw   string
		addrs []string
		want  string
	}{
		{
			name:  "a name with a port",
			raw:   "http://gateway-host.example:7440",
			addrs: []string{"192.168.5.2"},
			want:  "http://192.168.5.2:7440",
		},
		{
			name:  "a name with a port and a path",
			raw:   "http://gateway-host.example:7440/bd",
			addrs: []string{"192.168.5.2"},
			want:  "http://192.168.5.2:7440/bd",
		},
		{
			name:  "the first IPv4 answer, past an IPv6 one",
			raw:   "http://gateway-host.example:7440",
			addrs: []string{"fd00::1", "192.168.5.2", "192.168.5.3"},
			want:  "http://192.168.5.2:7440",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
			r.d.getenv = gatewayURLEnv(tt.raw)
			var asked []string
			r.d.lookupHost = func(_ context.Context, host string) ([]string, error) {
				asked = append(asked, host)
				return tt.addrs, nil
			}
			sha := r.commits[2]

			require.NoError(t, r.d.launchUpdateJob(context.Background(), config.UpdateSettings{MaxRuntime: time.Hour}, sha))

			assert.Equal(t, []string{"gateway-host.example"}, asked, "the host name was not resolved once")
			job := decodeUpdateJob(t, r.cluster.made["gt-update-"+sha[:12]])
			tokenVolume := jobGatewayTokenVolume(job)
			require.NotNil(t, tokenVolume, "the Job mounts no daemon-class gateway token Secret")
			pod := job.Spec.Template.Spec
			require.Len(t, pod.InitContainers, 1)
			require.Len(t, pod.Containers, 1)
			for _, c := range []updatejob.Container{pod.InitContainers[0], pod.Containers[0]} {
				got, ok := jobEnvValue(c, k8sdaemon.BdGatewayURLEnv)
				require.True(t, ok, "container %s carries no %s", c.Name, k8sdaemon.BdGatewayURLEnv)
				assert.Equal(t, tt.want, got, "container %s carries the wrong gateway URL", c.Name)
				tokenFile, ok := jobEnvValue(c, k8sdaemon.BdGatewayTokenFileEnv)
				require.True(t, ok, "container %s carries no %s", c.Name, k8sdaemon.BdGatewayTokenFileEnv)
				assert.Equal(t, k8sdaemon.BdGatewayTokenPath, tokenFile)
				mounted := false
				for _, m := range c.VolumeMounts {
					if m.Name == tokenVolume.Name && m.MountPath == k8sdaemon.BdGatewayTokenDir && m.ReadOnly {
						mounted = true
					}
				}
				assert.True(t, mounted, "container %s mounts no read-only gateway token", c.Name)
			}
		})
	}
}

// TestUpdateJobGatewayURLLookupFails: a host the daemon cannot resolve in time, a
// lookup that answers no IPv4 address, a value that is not a URL with a host, and an
// https URL on a name each refuse the launch, and no Job is created. A lookup
// failure names the host and a malformed value names the variable, never the value.
func TestUpdateJobGatewayURLLookupFails(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		raw     string
		lookup  lookupHostName
		wantMsg string
	}{
		{
			name:    "the resolver fails",
			raw:     "http://gateway-host.example:7440",
			lookup:  func(context.Context, string) ([]string, error) { return nil, errors.New("no such host") },
			wantMsg: "resolve the gateway host gateway-host.example for the update Job: no such host",
		},
		{
			name:    "the resolver runs past its bound",
			raw:     "http://gateway-host.example:7440",
			lookup:  func(context.Context, string) ([]string, error) { return nil, context.DeadlineExceeded },
			wantMsg: "resolve the gateway host gateway-host.example for the update Job: context deadline exceeded",
		},
		{
			name:    "the resolver answers no IPv4 address",
			raw:     "http://gateway-host.example:7440",
			lookup:  func(context.Context, string) ([]string, error) { return []string{"fd00::1"}, nil },
			wantMsg: "resolve the gateway host gateway-host.example for the update Job: the lookup answered no IPv4 address",
		},
		{
			name:    "an https URL on a name",
			raw:     "https://gateway-host.example",
			lookup:  func(context.Context, string) ([]string, error) { return []string{"192.168.5.2"}, nil },
			wantMsg: k8sdaemon.BdGatewayURLEnv + " is an https URL on the name gateway-host.example",
		},
		{
			name:    "the value is not a URL with a host",
			raw:     "gateway-host.example:7440",
			lookup:  func(context.Context, string) ([]string, error) { return []string{"192.168.5.2"}, nil },
			wantMsg: k8sdaemon.BdGatewayURLEnv + " is not a URL with a host",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
			r.d.getenv = gatewayURLEnv(tt.raw)
			r.d.lookupHost = tt.lookup

			err := r.d.launchUpdateJob(context.Background(), config.UpdateSettings{MaxRuntime: time.Hour}, r.commits[2])

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantMsg)
			assert.Empty(t, r.cluster.calls, "a refused launch reached the cluster")
		})
	}
}

// TestUpdateJobGatewayLookupHasADeadline: the gateway host lookup is given a context
// whose deadline is no more than updateJobLookupTimeout after the launch starts, so
// a stalled resolver cannot hold the heartbeat. The deadline is read from the
// context the lookup receives; nothing waits on it.
func TestUpdateJobGatewayLookupHasADeadline(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	r.d.getenv = gatewayURLEnv("http://gateway-host.example:7440")
	var deadline time.Time
	var hasDeadline bool
	r.d.lookupHost = func(ctx context.Context, host string) ([]string, error) {
		deadline, hasDeadline = ctx.Deadline()
		return []string{"192.168.5.2"}, nil
	}
	before := time.Now()

	require.NoError(t, r.d.launchUpdateJob(context.Background(), config.UpdateSettings{MaxRuntime: time.Hour}, r.commits[2]))

	after := time.Now()
	require.True(t, hasDeadline, "the gateway host lookup has no deadline")
	assert.True(t, deadline.After(before), "the lookup deadline precedes the launch")
	assert.False(t, deadline.After(after.Add(updateJobLookupTimeout)), "the lookup deadline is past its bound")
}

// TestUpdateJobGatewayURLUnsetOrIP: an unset BD_GATEWAY_URL renders the Job exactly
// as a render with no gateway value does, and a host that is already an IP is
// passed through unchanged. Neither reads a resolver.
func TestUpdateJobGatewayURLUnsetOrIP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
		// wantGateway is the gateway URL the Job carries; empty means the render
		// must equal a render with no gateway value.
		wantGateway string
	}{
		{name: "unset"},
		{name: "an IPv4 host", raw: "http://10.9.8.7:7440/bd", wantGateway: "http://10.9.8.7:7440/bd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
			r.d.getenv = gatewayURLEnv(tt.raw)
			lookups := 0
			r.d.lookupHost = func(context.Context, string) ([]string, error) {
				lookups++
				return nil, errors.New("the host is not a name")
			}
			sha := r.commits[2]
			settings := config.UpdateSettings{MaxRuntime: time.Hour}
			name := "gt-update-" + sha[:12]

			require.NoError(t, r.d.launchUpdateJob(context.Background(), settings, sha))

			assert.Zero(t, lookups, "the host was resolved")
			manifest, ok := r.cluster.made[name]
			require.True(t, ok, "the Job was not created")
			if tt.wantGateway == "" {
				running, ok, err := imagevalues.LoadIn(filepath.Join(r.town, "gastown"))
				require.NoError(t, err)
				require.True(t, ok)
				want, err := updatejob.Build(updatejob.Values{
					Image: running.Daemon, GreenCommit: sha, MaxRuntime: settings.MaxRuntime,
				})
				require.NoError(t, err)
				want.Job.Metadata.Name = name
				var buf bytes.Buffer
				require.NoError(t, updatejob.YAML(&buf, want.Job))
				assert.Equal(t, buf.String(), string(manifest), "the empty gateway changed the rendered Job")
				return
			}
			job := decodeUpdateJob(t, manifest)
			update := job.Spec.Template.Spec.Containers[0]
			got, ok := jobEnvValue(update, k8sdaemon.BdGatewayURLEnv)
			require.True(t, ok, "the Job carries no %s", k8sdaemon.BdGatewayURLEnv)
			assert.Equal(t, tt.wantGateway, got, "the IP host was not passed through unchanged")
		})
	}
}

// TestUpdateJobGatewayServicePassesThrough: a BD_GATEWAY_URL whose host is the
// gateway's in-cluster Service name reaches the Job exactly as the daemon was given
// it, and no resolver is read: the Job reaches the name through cluster DNS. The
// fixed address the admission policy also admits is passed through the same way. Every
// URL here is one the Job policy admits (deploy/k8s/update-admission.yaml).
func TestUpdateJobGatewayServicePassesThrough(t *testing.T) {
	t.Parallel()
	serviceURL := "http://" + bdGatewayServiceHost + ":" + strconv.Itoa(k8sdaemon.BdGatewayPort)
	tests := []struct {
		name string
		raw  string
	}{
		{name: "the Service name with its port", raw: serviceURL},
		{name: "the fixed address the policy admits", raw: "http://192.168.5.2:7440"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
			r.d.getenv = gatewayURLEnv(tt.raw)
			lookups := 0
			r.d.lookupHost = func(context.Context, string) ([]string, error) {
				lookups++
				return nil, errors.New("the host is not resolved")
			}
			sha := r.commits[2]

			require.NoError(t, r.d.launchUpdateJob(context.Background(), config.UpdateSettings{MaxRuntime: time.Hour}, sha))

			assert.Zero(t, lookups, "the host was resolved")
			job := decodeUpdateJob(t, r.cluster.made["gt-update-"+sha[:12]])
			pod := job.Spec.Template.Spec
			require.Len(t, pod.InitContainers, 1)
			require.Len(t, pod.Containers, 1)
			for _, c := range []updatejob.Container{pod.InitContainers[0], pod.Containers[0]} {
				got, ok := jobEnvValue(c, k8sdaemon.BdGatewayURLEnv)
				require.True(t, ok, "container %s carries no %s", c.Name, k8sdaemon.BdGatewayURLEnv)
				assert.Equal(t, tt.raw, got, "container %s does not carry the gateway URL unchanged", c.Name)
			}
		})
	}
}

// TestUpdateJobGatewayServiceOtherNamesResolve: only the gateway's own Service name
// in the Job's namespace is passed on. Any other name, including one that only ends
// in that suffix, is resolved to the address the daemon's resolver answers.
func TestUpdateJobGatewayServiceOtherNamesResolve(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
	}{
		{name: "the gateway name in another namespace", raw: "http://bd-gateway.other.svc:7440"},
		{name: "a name that only ends in the Service suffix", raw: "http://evil-bd-gateway.gt.svc:7440"},
		{name: "a plain host name", raw: "http://gateway-host.example:7440"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
			r.d.getenv = gatewayURLEnv(tt.raw)
			var asked []string
			r.d.lookupHost = func(_ context.Context, host string) ([]string, error) {
				asked = append(asked, host)
				return []string{"192.168.5.2"}, nil
			}
			sha := r.commits[2]

			require.NoError(t, r.d.launchUpdateJob(context.Background(), config.UpdateSettings{MaxRuntime: time.Hour}, sha))

			u, err := url.Parse(tt.raw)
			require.NoError(t, err)
			assert.Equal(t, []string{u.Hostname()}, asked, "the host name was not resolved once")
			job := decodeUpdateJob(t, r.cluster.made["gt-update-"+sha[:12]])
			got, ok := jobEnvValue(job.Spec.Template.Spec.Containers[0], k8sdaemon.BdGatewayURLEnv)
			require.True(t, ok, "the Job carries no %s", k8sdaemon.BdGatewayURLEnv)
			assert.Equal(t, "http://192.168.5.2:7440", got, "the name was not replaced by its address")
		})
	}
}

// TestUpdateJobGatewayServicePolicyPinsBothURLs reads the admission policy the cluster
// enforces on the update Job and holds the BD_GATEWAY_URL pin of each container
// validation to exactly two URLs: the address the daemon resolves a name to, and the
// in-cluster Service URL the daemon passes on. The Service URL is accepted, and any
// other host, port or address is refused, since the pin is an exact set.
func TestUpdateJobGatewayServicePolicyPinsBothURLs(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "k8s", "update-admission.yaml"))
	require.NoError(t, err)
	var expressions []string
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc updateAdmissionDoc
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err, "the admission manifest holds a document that does not parse")
		if doc.Kind != "ValidatingAdmissionPolicy" {
			continue
		}
		for _, v := range doc.Spec.Validations {
			expressions = append(expressions, v.Expression)
		}
	}
	require.NotEmpty(t, expressions, "the admission manifest holds no ValidatingAdmissionPolicy")

	clause := regexp.MustCompile(`e\.name == '` + regexp.QuoteMeta(k8sdaemon.BdGatewayURLEnv) + `' && has\(e\.value\) && \(([^()]*)\)`)
	quoted := regexp.MustCompile(`'([^']*)'`)
	var pinned [][]string
	for _, expr := range expressions {
		if !strings.Contains(expr, k8sdaemon.BdGatewayURLEnv) {
			continue
		}
		clauses := clause.FindAllStringSubmatch(expr, -1)
		require.Len(t, clauses, 1, "a validation pins %s in %d clauses, want 1", k8sdaemon.BdGatewayURLEnv, len(clauses))
		var urls []string
		for _, q := range quoted.FindAllStringSubmatch(clauses[0][1], -1) {
			urls = append(urls, q[1])
		}
		slices.Sort(urls)
		pinned = append(pinned, urls)
	}
	require.Len(t, pinned, 2, "the policy pins %s in %d validations, want the two container validations", k8sdaemon.BdGatewayURLEnv, len(pinned))

	want := []string{"http://192.168.5.2:7440", "http://" + bdGatewayServiceHost + ":" + strconv.Itoa(k8sdaemon.BdGatewayPort)}
	slices.Sort(want)
	for i, urls := range pinned {
		assert.Equal(t, want, urls, "container validation %d admits other gateway URLs than the daemon hands the Job", i+1)
	}
}

// TestUpdateJobGatewayServiceHostIsJobNamespace holds the Service host the daemon hands
// the Job to the namespace the Job it creates lands in, so the name the Job resolves is
// the gateway Service in the Job's own namespace.
func TestUpdateJobGatewayServiceHostIsJobNamespace(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	r.d.getenv = gatewayURLEnv("http://bd-gateway.gt.svc:7440")
	sha := r.commits[2]

	require.NoError(t, r.d.launchUpdateJob(context.Background(), config.UpdateSettings{MaxRuntime: time.Hour}, sha))

	job := decodeUpdateJob(t, r.cluster.made["gt-update-"+sha[:12]])
	got, ok := jobEnvValue(job.Spec.Template.Spec.Containers[0], k8sdaemon.BdGatewayURLEnv)
	require.True(t, ok, "the Job carries no %s", k8sdaemon.BdGatewayURLEnv)
	u, err := url.Parse(got)
	require.NoError(t, err)
	assert.Equal(t, k8sdaemon.BdGatewayName+"."+job.Metadata.Namespace+".svc", u.Hostname(),
		"the host handed to the Job is not the gateway Service in the Job's namespace %q", job.Metadata.Namespace)
}

// updateAdmissionDoc is the part of one document of deploy/k8s/update-admission.yaml
// the gateway policy test reads: its kind and the expressions of a policy.
type updateAdmissionDoc struct {
	Kind string `yaml:"kind"`
	Spec struct {
		Validations []struct {
			Expression string `yaml:"expression"`
		} `yaml:"validations"`
	} `yaml:"spec"`
}
