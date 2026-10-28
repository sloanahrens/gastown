package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
	"github.com/steveyegge/gastown/internal/testutil"
)

// The command reads the town config for each rig's Forgejo remote; the
// hermetic harness keeps the test binary off the live town's state, as every
// package that can reach it must.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

const (
	gastownCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	devopsCommit  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var pinsFile = []byte("# <rig> <commit>\n" +
	"gastown " + gastownCommit + "\ndevops " + devopsCommit + "\n")

// fake is the command's outside world: a pins document, one remote per rig, the
// Job the cluster holds, and every kubectl call it was handed, with the
// manifest on its stdin.
type fake struct {
	pins      []byte
	readErr   error
	remotes   map[string]string
	remoteErr map[string]error
	live      []byte // the Job the cluster holds; empty means it holds none
	liveErr   error  // the read of that Job fails
	failVerb  string // the kubectl verb whose call fails; empty fails none
	calls     []kubectlCall
}

type kubectlCall struct {
	args  []string
	stdin string
}

func (f *fake) deps() deps {
	return deps{
		readFile: func(string) ([]byte, error) {
			if f.readErr != nil {
				return nil, f.readErr
			}
			return f.pins, nil
		},
		remoteURL: func(rigName string) (string, error) {
			if err, ok := f.remoteErr[rigName]; ok {
				return "", err
			}
			if u, ok := f.remotes[rigName]; ok {
				return u, nil
			}
			return "", errors.New("rig " + rigName + " has no merge_queue.forgejo.remote_url")
		},
		kubectl: func(stdin io.Reader, args ...string) ([]byte, error) {
			call := kubectlCall{args: args}
			if stdin != nil {
				var buf bytes.Buffer
				if _, err := io.Copy(&buf, stdin); err != nil {
					return nil, err
				}
				call.stdin = buf.String()
			}
			f.calls = append(f.calls, call)
			if len(args) == 0 {
				return nil, errors.New("kubectl called with no verb")
			}
			if f.failVerb == args[0] {
				return nil, errors.New("kubectl failed")
			}
			if args[0] == "get" {
				if f.liveErr != nil {
					return nil, f.liveErr
				}
				return f.live, nil
			}
			return []byte("ok\n"), nil
		},
	}
}

func newFake() *fake {
	return &fake{
		pins: pinsFile,
		remotes: map[string]string{
			"gastown": "http://host.docker.internal:3000/sloan/gastown.git",
			"devops":  "http://host.docker.internal:3000/sloan/devops.git",
		},
	}
}

// run is the command's one path, with the fakes in place of the town, the pins
// file and the cluster.
func run(t *testing.T, f *fake, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errw bytes.Buffer
	code = runWith(args, &out, &errw, f.deps())
	return code, out.String(), errw.String()
}

// baseArgs are the flags every successful run needs.
func baseArgs(extra ...string) []string {
	return append([]string{"-town", "/town", "-image", "gastown-polecat:dev"}, extra...)
}

// verbs is the kubectl verb of each call, in order: the run reads the live Job,
// and only deletes and applies when the read does not leave it alone.
func verbs(f *fake) string {
	var vs []string
	for _, c := range f.calls {
		if len(c.args) > 0 {
			vs = append(vs, c.args[0])
		}
	}
	return strings.Join(vs, " ")
}

// appliedJob decodes the document the command handed to kubectl apply.
func appliedJob(t *testing.T, f *fake) cachevol.SeedJob {
	t.Helper()
	var manifest string
	for _, c := range f.calls {
		if strings.Join(c.args, " ") == "apply -f -" {
			manifest = c.stdin
		}
	}
	if manifest == "" {
		t.Fatalf("no kubectl apply carried a manifest; calls = %s", verbs(f))
	}
	var job cachevol.SeedJob
	if err := json.Unmarshal([]byte(manifest), &job); err != nil {
		t.Fatalf("the manifest kubectl applied is not a Job document: %v", err)
	}
	return job
}

// clusterJob is the document a run applied as the cluster stores it: the API
// server adds a status and defaults to the pod template, so the run's read must
// still recognize the Job as the one its manifest describes.
func clusterJob(t *testing.T, applied []byte, succeeded int) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(applied, &doc); err != nil {
		t.Fatalf("the applied manifest is not a document: %v", err)
	}
	doc["status"] = map[string]any{"succeeded": succeeded}
	tmpl := child(t, child(t, doc, "spec"), "template")
	child(t, tmpl, "metadata")["creationTimestamp"] = nil
	pod := child(t, tmpl, "spec")
	pod["dnsPolicy"] = "ClusterFirst"
	pod["schedulerName"] = "default-scheduler"
	pod["serviceAccountName"] = "default"
	pod["terminationGracePeriodSeconds"] = 30
	for _, key := range []string{"initContainers", "containers"} {
		containers, ok := pod[key].([]any)
		if !ok {
			t.Fatalf("the applied manifest's %s is not a list: %T", key, pod[key])
		}
		for _, c := range containers {
			container, ok := c.(map[string]any)
			if !ok {
				t.Fatalf("an entry of the applied manifest's %s is not an object: %T", key, c)
			}
			container["imagePullPolicy"] = "IfNotPresent"
			container["terminationMessagePath"] = "/dev/termination-log"
			container["terminationMessagePolicy"] = "File"
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("the cluster-shaped document does not marshal: %v", err)
	}
	return out
}

// child is the object under key in m, failing the test when it is not one.
func child(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	c, ok := m[key].(map[string]any)
	if !ok {
		t.Fatalf("%q is not an object in the manifest: %T", key, m[key])
	}
	return c
}

// appliedDocument is the manifest of the last apply, as the cluster would have
// taken it before adding its own fields.
func appliedDocument(t *testing.T, f *fake) []byte {
	t.Helper()
	for i := len(f.calls) - 1; i >= 0; i-- {
		if strings.Join(f.calls[i].args, " ") == "apply -f -" {
			return []byte(f.calls[i].stdin)
		}
	}
	t.Fatalf("no kubectl apply carried a manifest; calls = %s", verbs(f))
	return nil
}

// TestRunAppliesTheSeedJobManifest pins what reaches kubectl into an empty
// cluster: the read, then the apply of a root Job that fills the claim with the
// named image. There is nothing to delete when no Job is there.
func TestRunAppliesTheSeedJobManifest(t *testing.T) {
	t.Parallel()
	f := newFake()
	code, stdout, stderr := run(t, f, baseArgs()...)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	if got := verbs(f); got != "get apply" {
		t.Fatalf("kubectl calls = %q, want the read then the apply", got)
	}
	if got := strings.Join(f.calls[0].args, " "); got != "get job gt-cache-seed --ignore-not-found -o json" {
		t.Errorf("first kubectl call = %q, want the live Job read", got)
	}

	job := appliedJob(t, f)
	if job.APIVersion != "batch/v1" || job.Kind != "Job" || job.Metadata.Name != cachevol.DefaultSeedJobName {
		t.Errorf("manifest = %s %s %q, want batch/v1 Job %s", job.APIVersion, job.Kind, job.Metadata.Name, cachevol.DefaultSeedJobName)
	}
	// -1 is a field the pod leaves unset, so a nil security context or a nil
	// field reads as a failure instead of a dereference.
	user, group := int64(-1), int64(-1)
	if sc := job.Spec.Template.Spec.SecurityContext; sc != nil {
		if sc.RunAsUser != nil {
			user = *sc.RunAsUser
		}
		if sc.RunAsGroup != nil {
			group = *sc.RunAsGroup
		}
	}
	if user != 0 || group != 0 {
		t.Errorf("the Job's pod runs as user=%d group=%d, want root: the seed hands the claim to SeedUserID", user, group)
	}
	var claim string
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claim = v.PersistentVolumeClaim.ClaimName
		}
	}
	if claim != "gt-cache" {
		t.Errorf("the Job mounts claim %q, want gt-cache", claim)
	}
	if len(job.Spec.Template.Spec.Containers) != 1 || job.Spec.Template.Spec.Containers[0].Image != "gastown-polecat:dev" {
		t.Errorf("the seed container = %+v, want the polecat image given on -image", job.Spec.Template.Spec.Containers)
	}
	if !strings.Contains(stdout, "applied Job gt-cache-seed filling gt-cache from 2 pin(s): gastown devops") {
		t.Errorf("stdout = %q, want the applied Job, claim and rigs reported", stdout)
	}
}

// TestRunBuildsAPinForEachPinnedRig pins the pins the Job clones: one per row
// of the pins file, in its order, at its commit and Forgejo URL.
func TestRunBuildsAPinForEachPinnedRig(t *testing.T) {
	t.Parallel()
	f := newFake()
	if code, _, stderr := run(t, f, baseArgs()...); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	want := []cachevol.Pin{
		{Rig: "gastown", Commit: gastownCommit, URL: "http://host.docker.internal:3000/sloan/gastown.git"},
		{Rig: "devops", Commit: devopsCommit, URL: "http://host.docker.internal:3000/sloan/devops.git"},
	}
	pins := seedPins(t, appliedJob(t, f))
	if len(pins) != len(want) {
		t.Fatalf("the Job seeds %d pins, want both rows of the pins file: %+v", len(pins), pins)
	}
	for i, p := range want {
		if pins[i] != p {
			t.Errorf("pin %d = %+v, want %+v", i, pins[i], p)
		}
	}
}

// TestRunSeedsOnlyTheRigsNamedOnTheCommandLine keeps `make polecat-caches
// RIGS=devops` seeding one rig, the way the target it replaces took rigs.
func TestRunSeedsOnlyTheRigsNamedOnTheCommandLine(t *testing.T) {
	t.Parallel()
	f := newFake()
	if code, _, stderr := run(t, f, baseArgs("devops")...); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	pins := seedPins(t, appliedJob(t, f))
	if len(pins) != 1 || pins[0].Rig != "devops" {
		t.Errorf("the Job seeds %+v, want devops alone", pins)
	}
}

// TestRunSwapsTheCloneHost points the Job's clones at the host it can reach
// from the cluster, which is what -forgejo-host exists for.
func TestRunSwapsTheCloneHost(t *testing.T) {
	t.Parallel()
	f := newFake()
	if code, _, stderr := run(t, f, baseArgs("-forgejo-host", "forgejo.openshell:3000")...); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	for _, p := range seedPins(t, appliedJob(t, f)) {
		if !strings.HasPrefix(p.URL, "http://forgejo.openshell:3000/sloan/") {
			t.Errorf("pin %s clones from %q, want the -forgejo-host authority", p.Rig, p.URL)
		}
	}
}

// TestRunPassesTheNamespaceToEveryCall keeps the read, the delete that replaces
// a changed Job and the manifest in one namespace, so a run never looks at
// another namespace's Job.
func TestRunPassesTheNamespaceToEveryCall(t *testing.T) {
	t.Parallel()
	f := newFake()
	// A Job seeded from one pin is one this run replaces: the template all the
	// pins produce is a different template.
	if code, _, stderr := run(t, f, baseArgs("-namespace", "openshell", "devops")...); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	f.live = clusterJob(t, appliedDocument(t, f), 1)
	f.calls = nil
	if code, _, stderr := run(t, f, baseArgs("-namespace", "openshell")...); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	if got := verbs(f); got != "get delete apply" {
		t.Fatalf("kubectl calls = %q, want the read, the delete and the apply", got)
	}
	for _, c := range f.calls[:2] {
		if got := strings.Join(c.args, " "); !strings.Contains(got, "--namespace openshell") {
			t.Errorf("kubectl call = %q, want the namespace", got)
		}
	}
	if job := appliedJob(t, f); job.Metadata.Namespace != "openshell" {
		t.Errorf("manifest namespace = %q, want openshell", job.Metadata.Namespace)
	}
}

// TestRunLeavesASeededJobAlone keeps a run from restarting a Job that already
// finished these pins: the cluster holds a Job the last run applied, so this
// run reads it, finds the template and the pins unchanged and touches nothing.
func TestRunLeavesASeededJobAlone(t *testing.T) {
	t.Parallel()
	f := newFake()
	if code, _, stderr := run(t, f, baseArgs()...); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	f.live = clusterJob(t, appliedDocument(t, f), 1)
	f.calls = nil

	code, stdout, stderr := run(t, f, baseArgs()...)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	if got := verbs(f); got != "get" {
		t.Errorf("kubectl calls = %q, want the read alone: a finished Job with the same pins is not replaced", got)
	}
	if !strings.Contains(stdout, "already holds the 2 pinned commit(s); nothing applied") {
		t.Errorf("stdout = %q, want the run reporting it left the Job alone", stdout)
	}
}

// TestRunReplacesASeedJobWhosePinsChanged replaces a Job built from other pins:
// a finished Job is no reason to keep a template that would seed something else.
func TestRunReplacesASeedJobWhosePinsChanged(t *testing.T) {
	t.Parallel()
	f := newFake()
	if code, _, stderr := run(t, f, baseArgs("devops")...); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	f.live = clusterJob(t, appliedDocument(t, f), 1)
	f.calls = nil

	if code, _, stderr := run(t, f, baseArgs()...); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	if got := verbs(f); got != "get delete apply" {
		t.Fatalf("kubectl calls = %q, want the read, the delete and the apply", got)
	}
	if got := strings.Join(f.calls[1].args, " "); got != "delete job gt-cache-seed --ignore-not-found" {
		t.Errorf("delete call = %q, want the Job the changed pins replace", got)
	}
	if pins := seedPins(t, appliedJob(t, f)); len(pins) != 2 {
		t.Errorf("the Job seeds %+v, want both pins", pins)
	}
}

// TestRunReplacesASeedJobThatDidNotSucceed replaces a Job that never finished:
// its template matches, but only a succeeded Job wrote the claim's marker, so
// leaving it alone would leave the claim unseeded behind a failed Job.
func TestRunReplacesASeedJobThatDidNotSucceed(t *testing.T) {
	t.Parallel()
	f := newFake()
	if code, _, stderr := run(t, f, baseArgs()...); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	f.live = clusterJob(t, appliedDocument(t, f), 0)
	f.calls = nil

	if code, _, stderr := run(t, f, baseArgs()...); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	if got := verbs(f); got != "get delete apply" {
		t.Errorf("kubectl calls = %q, want the read, the delete and the apply", got)
	}
}

// TestRunForceReseedsAFinishedJob is the operator's reseed: -force replaces a
// Job that holds these pins, which is how a claim emptied under a finished Job
// is filled again.
func TestRunForceReseedsAFinishedJob(t *testing.T) {
	t.Parallel()
	f := newFake()
	if code, _, stderr := run(t, f, baseArgs()...); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	f.live = clusterJob(t, appliedDocument(t, f), 1)
	f.calls = nil

	if code, _, stderr := run(t, f, baseArgs("-force")...); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
	}
	if got := verbs(f); got != "get delete apply" {
		t.Errorf("kubectl calls = %q, want the read, the delete and the apply", got)
	}
}

// TestRunReportsEveryRefusal drives each way a run stops before or during
// kubectl, so a refusal cannot regress into a partial apply.
func TestRunReportsEveryRefusal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		args      []string
		mutate    func(*testing.T, *fake)
		wantCode  int
		wantErr   string
		wantCalls string // the kubectl verbs the refusal allows, in order
	}{
		{
			name:     "no town",
			args:     []string{"-image", "img"},
			wantCode: 2,
			wantErr:  "-town is required",
		},
		{
			name:     "no image",
			args:     []string{"-town", "/town"},
			wantCode: 2,
			wantErr:  "-image is required",
		},
		{
			name:     "unreadable pins file",
			args:     baseArgs(),
			mutate:   func(_ *testing.T, f *fake) { f.readErr = errors.New("permission denied") },
			wantCode: 1,
			wantErr:  "cannot read deploy/polecat/seed-pins: permission denied",
		},
		{
			name:     "pins file naming no rig",
			args:     baseArgs(),
			mutate:   func(_ *testing.T, f *fake) { f.pins = []byte("# only comments\n") },
			wantCode: 1,
			wantErr:  "names no rig",
		},
		{
			name:     "rig with no pin line",
			args:     baseArgs("om"),
			wantCode: 1,
			wantErr:  "om names no pin in deploy/polecat/seed-pins",
		},
		{
			name: "rig with no Forgejo remote",
			args: baseArgs(),
			mutate: func(_ *testing.T, f *fake) {
				f.remoteErr = map[string]error{"devops": errors.New("rig devops has no merge_queue.forgejo.remote_url")}
			},
			wantCode: 1,
			wantErr:  "no merge_queue.forgejo.remote_url",
		},
		{
			name:      "live Job unreadable",
			args:      baseArgs(),
			mutate:    func(_ *testing.T, f *fake) { f.liveErr = errors.New("kubectl failed") },
			wantCode:  1,
			wantErr:   "kubectl failed",
			wantCalls: "get",
		},
		{
			name:      "live Job is not a Job document",
			args:      baseArgs(),
			mutate:    func(_ *testing.T, f *fake) { f.live = []byte("not a job") },
			wantCode:  1,
			wantErr:   "not a Job document",
			wantCalls: "get",
		},
		{
			name: "delete of a changed Job fails",
			args: baseArgs(),
			mutate: func(t *testing.T, f *fake) {
				if code, _, stderr := run(t, f, baseArgs("devops")...); code != 0 {
					t.Fatalf("exit = %d, want 0; stderr: %s", code, stderr)
				}
				f.live = clusterJob(t, appliedDocument(t, f), 1)
				f.calls = nil
				f.failVerb = "delete"
			},
			wantCode:  1,
			wantErr:   "kubectl failed",
			wantCalls: "get delete",
		},
		{
			name:      "apply fails",
			args:      baseArgs(),
			mutate:    func(_ *testing.T, f *fake) { f.failVerb = "apply" },
			wantCode:  1,
			wantErr:   "kubectl failed",
			wantCalls: "get apply",
		},
		{
			name:     "image the builder refuses",
			args:     []string{"-town", "/town", "-image", "not an image"},
			wantCode: 1,
			wantErr:  "the seed Job image",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake()
			if tt.mutate != nil {
				tt.mutate(t, f)
			}
			code, _, stderr := run(t, f, tt.args...)
			if code != tt.wantCode {
				t.Errorf("exit = %d, want %d; stderr: %s", code, tt.wantCode, stderr)
			}
			if !strings.Contains(stderr, tt.wantErr) {
				t.Errorf("stderr = %q, want it to name %q", stderr, tt.wantErr)
			}
			if got := verbs(f); got != tt.wantCalls {
				t.Errorf("kubectl calls = %q, want %q", got, tt.wantCalls)
			}
		})
	}
}

// TestCloneURL pins the URL rule: the rig's remote as configured, rehosted
// when asked, with userinfo never carried and never echoed.
func TestCloneURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		remote  string
		host    string
		want    string
		wantErr string
	}{
		{
			name:   "no host keeps the configured remote",
			remote: "http://host.docker.internal:3000/sloan/gastown.git",
			want:   "http://host.docker.internal:3000/sloan/gastown.git",
		},
		{
			name:   "host and port replace the remote's",
			remote: "http://host.docker.internal:3000/sloan/gastown.git",
			host:   "forgejo.openshell:3001",
			want:   "http://forgejo.openshell:3001/sloan/gastown.git",
		},
		{
			name:   "a host with no port keeps the remote's port",
			remote: "https://forgejo.example:8443/sloan/gastown.git",
			host:   "forgejo.openshell",
			want:   "https://forgejo.openshell:8443/sloan/gastown.git",
		},
		{
			name:   "userinfo is dropped",
			remote: "http://bot:secret@forgejo.example/sloan/gastown.git",
			want:   "http://forgejo.example/sloan/gastown.git",
		},
		{
			name:    "an ssh remote is refused without quoting it",
			remote:  "git@forgejo.example:sloan/gastown.git",
			wantErr: "not an http(s) URL",
		},
		{
			name:    "an empty remote is refused",
			remote:  "",
			wantErr: "not an http(s) URL",
		},
		{
			name:    "a host with no name is refused",
			remote:  "http://forgejo.example/x.git",
			host:    "-bad-",
			wantErr: "want a bare host name",
		},
		{
			name:    "a port that is not a port is refused",
			remote:  "http://forgejo.example/x.git",
			host:    "forgejo.example:0",
			wantErr: "want host[:port]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := cloneURL("gastown", tt.remote, tt.host)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("cloneURL(%q, %q) = %q, %v; want an error naming %q", tt.remote, tt.host, got, err, tt.wantErr)
				}
				if strings.Contains(err.Error(), "secret") {
					t.Errorf("the error quotes the URL's userinfo: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("cloneURL(%q, %q): %v", tt.remote, tt.host, err)
			}
			if got != tt.want {
				t.Errorf("cloneURL(%q, %q) = %q, want %q", tt.remote, tt.host, got, tt.want)
			}
		})
	}
}

// TestProdDepsRunsKubectl drives the wrapper the command really uses: prodDeps
// hands the kubectl call to execKubectl, so a stub in place of that wiring, or
// an execKubectl that never reaches kubectl, fails here. PATH holds no kubectl,
// so the lookup fails and no process starts.
//
//testpolicy:allow parallel — PATH is process-wide, so no other test may run while this one holds it
func TestProdDepsRunsKubectl(t *testing.T) {
	//testpolicy:allow no-env — the wrapper resolves kubectl on PATH, and a PATH with no kubectl is what keeps this test off the cluster
	t.Setenv("PATH", t.TempDir())
	_, err := prodDeps().kubectl(nil, "apply", "-f", "-")
	if err == nil {
		t.Fatal("prodDeps().kubectl with no kubectl on PATH = nil error, want the wrapper's failure")
	}
	if !strings.Contains(err.Error(), "kubectl apply -f -") {
		t.Errorf("prodDeps().kubectl error = %v, want the command named", err)
	}
}

// TestForgejoRemoteRefusesARigWithNoBlock pins the resolution rule: a rig with
// no merge_queue.forgejo block has no URL for the Job to clone, and the error
// names the setting rather than failing later in the cluster.
func TestForgejoRemoteRefusesARigWithNoBlock(t *testing.T) {
	t.Parallel()
	_, err := forgejoRemote(t.TempDir(), "gastown")
	if err == nil || !strings.Contains(err.Error(), "merge_queue.forgejo.remote_url") {
		t.Fatalf("forgejoRemote on a town that configures no rig = %v, want the missing block named", err)
	}
}

// seedPins decodes the pins document the Job's containers take as argv.
func seedPins(t *testing.T, job cachevol.SeedJob) []cachevol.Pin {
	t.Helper()
	for _, c := range job.Spec.Template.Spec.Containers {
		for i, a := range c.Args {
			if a != "--pins" || i+1 >= len(c.Args) {
				continue
			}
			var pins []cachevol.Pin
			if err := json.Unmarshal([]byte(c.Args[i+1]), &pins); err != nil {
				t.Fatalf("container %s: the pins argument is not a pins document: %v", c.Name, err)
			}
			return pins
		}
	}
	t.Fatalf("no container carries a --pins argument: %+v", job.Spec.Template.Spec.Containers)
	return nil
}
