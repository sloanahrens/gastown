package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/imagevalues"
)

// fakeUpdateJob is the cluster: the Jobs it holds by name, and every create the
// daemon made. refuse, when set, is what every create returns instead.
type fakeUpdateJob struct {
	calls  []string
	made   map[string][]byte
	refuse error
}

func (f *fakeUpdateJob) CreateJob(_ context.Context, name string, manifest []byte) error {
	f.calls = append(f.calls, name)
	if f.refuse != nil {
		return f.refuse
	}
	if _, ok := f.made[name]; ok {
		return errUpdateJobExists
	}
	f.made[name] = manifest
	return nil
}

// updateJobRig is a pod daemon over a town whose gastown bare clone holds three
// commits on main, the running commit being the middle one.
type updateJobRig struct {
	d        *Daemon
	logs     *bytes.Buffer
	cluster  *fakeUpdateJob
	launches *int
	town     string
	bare     string
	git      pairReadyGit
	commits  []string
}

// newUpdateJobRig builds the rig with cfg as the update block and the pod switch
// on. The running images name commits[1].
func newUpdateJobRig(t *testing.T, cfg *config.UpdateConfig) *updateJobRig {
	t.Helper()
	d, logs, launches, _ := newUpdateTriggerTestDaemon(t, cfg)
	setUpdateSource(t, d)
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
	cluster := &fakeUpdateJob{made: map[string][]byte{}}
	d.updateJob = cluster
	writeRunningImages(t, town, commits[1])
	return &updateJobRig{
		d: d, logs: logs, cluster: cluster, launches: launches, town: town,
		bare: bare, git: world.Open(bare).(pairReadyGit), commits: commits,
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
		return []pairReadyRig{{Rig: "gastown", Owner: pairOwner, Repo: pairRepo, API: api, Registry: reg, Git: r.git}}, nil
	}
	r.d.pairReady.run(context.Background(), r.town, updateTriggerTestTime, rigs, func(string, ...any) {})
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
			wantReason: "the local clone of other is missing",
		},
		{
			name:    "a clone that cannot judge the pair refuses the decision",
			items:   []attention.Item{item("gastown", c2)},
			running: c1,
			git: rollGitStub{errs: map[[2]string]error{
				{c1, c2}: errors.New("git failed"),
			}},
			wantReason: "judging pair-ready " + c2[:12] + " against the running commit",
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
			wantReason: "judging pair-ready " + c2[:12] + " against " + c1[:12],
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

// TestUpdateJobRefusedLaunchWaitsOutTheSuppression: a cluster that refuses the
// create logs the failure and is not asked again inside the suppression window.
func TestUpdateJobRefusedLaunchWaitsOutTheSuppression(t *testing.T) {
	t.Parallel()
	r := newUpdateJobRig(t, &config.UpdateConfig{Enabled: true, Window: "03:00"})
	r.cluster.refuse = errors.New("cluster unreachable")
	r.pairFor(t, r.commits[2], true, true)

	r.d.triggerUpdate()
	r.d.triggerUpdate()

	assert.Len(t, r.cluster.calls, 1, "a refused launch was retried inside the suppression window")
	assert.Contains(t, r.logs.String(), "launch of the update Job failed: cluster unreachable")
	assert.NotContains(t, r.logs.String(), "created Job")
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

// TestKubectlUpdateJobMapsTheAnswer: kubectl's exit status and its AlreadyExists
// answer are the outcome, the manifest is written where kubectl reads it, and a
// refusal names the exit without echoing kubectl's output.
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
			err := kubectlUpdateJob{dir: dir, run: run}.CreateJob(context.Background(), name, manifest)

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
			name:     "job cannot be rendered",
			settings: config.UpdateSettings{MaxRuntime: 1500 * time.Millisecond},
			wantMsg:  "render the update Job",
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

			err := r.d.launchUpdateJob(ctx, tt.settings, r.commits[2])

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
			err := kubectlUpdateJob{dir: dir, run: run}.CreateJob(context.Background(), name, []byte("kind: Job\n"))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantMsg)
		})
	}
}

// TestInClusterFromEnv: a pod is a process the kubelet gave KUBERNETES_SERVICE_HOST.
func TestInClusterFromEnv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{name: "unset", env: map[string]string{}, want: false},
		{name: "set", env: map[string]string{kubernetesServiceHostEnv: "10.0.0.1"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, inClusterFromEnv(func(k string) string { return tt.env[k] }))
		})
	}
}
