package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/disks"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/polecat/cachevol"
	"github.com/steveyegge/gastown/internal/polecatimage"
)

const (
	// cacheTrimClaimName is the claim the fixtures mount.
	cacheTrimClaimName = "gt-shared-cache"
	// cacheTrimNamespace is the namespace the fixtures put that claim in.
	cacheTrimNamespace = "openshell"
	// cacheTrimJob is the Job the pass names after that claim.
	cacheTrimJob = cachevol.DefaultTrimJobName + "-" + cacheTrimClaimName
	// cacheTrimListJob is the lister Job the pass names after that claim, read
	// when no running pod mounts it.
	cacheTrimListJob = cachevol.DefaultListJobName + "-" + cacheTrimClaimName
	// cacheTrimMountPath is where a sandbox pod mounts the claim, the root the
	// pass reads the listing through. It matches the kubernetes driver's mount,
	// outside /sandbox so it does not replace the workspace.
	cacheTrimMountPath = "/tmp/gt-cache"

	// cacheTrimApplyVerb is the verb every document of a trim is applied with,
	// the argv cacheTrimApplyArgs builds: server-side, under the trim's field
	// manager, so no last-applied annotation is written (gt-gg9hv.40).
	cacheTrimApplyVerb = "apply --server-side --field-manager=gt-cache-trim --force-conflicts -f - -n " + cacheTrimNamespace

	// cacheTrimListVerb is the pass's listing read through the pod that mounts
	// the claim.
	cacheTrimListVerb = "exec -n " + cacheTrimNamespace + " sbx-a -c agent -- " + cachevol.HelperPath + " trim-list --root " + cacheTrimMountPath

	// cacheTrimNoPodsVerb is the pod read of a claim no pod mounts, the state the
	// lister Job reads the claim in.
	cacheTrimNoPodsVerb = "get pods -n " + cacheTrimNamespace + " -o json"

	// The lister Job's calls after its apply, in the order the pass makes them:
	// the wait for it to complete, the read of its log, and the delete once the
	// log is read.
	cacheTrimListerWaitVerb   = "wait --for=condition=complete job/" + cacheTrimListJob + " -n " + cacheTrimNamespace + " --timeout=120s"
	cacheTrimListerLogsVerb   = "logs job/" + cacheTrimListJob + " -n " + cacheTrimNamespace
	cacheTrimListerDeleteVerb = "delete job " + cacheTrimListJob + " -n " + cacheTrimNamespace + " --ignore-not-found --wait=true --timeout=60s"

	// cacheTrimFreeVerb is the pass's read of the claim's free space, made only
	// when the trim takes the claim route: df in the pod the listing came from,
	// not a helper subcommand, since an older image sends an unknown subcommand
	// to a full plugin run (gt-gg9hv.41).
	cacheTrimFreeVerb = "exec -n " + cacheTrimNamespace + " sbx-a -c agent -- df -Pk " + cacheTrimMountPath

	// cacheTrimDuVerb is the pass's read of the claim's own size after a
	// completed trim: du in the pod the listing came from, since df reports the
	// whole node filesystem for a local-path claim (gt-eyjgal).
	cacheTrimDuVerb = "exec -n " + cacheTrimNamespace + " sbx-a -c agent -- du -sk " + cacheTrimMountPath

	// cacheTrimLastAppliedLimit is the metadata size the API server holds
	// kubectl's last-applied-configuration annotation to, in bytes.
	cacheTrimLastAppliedLimit = 262144
)

// cacheTrimClaimsJSON is the cluster's claim listing: the fixture's claim, in
// its namespace.
const cacheTrimClaimsJSON = `{"items":[{"metadata":{"name":"` + cacheTrimClaimName + `","namespace":"` + cacheTrimNamespace + `"}}]}`

// cacheTrimPodsJSON is one running sandbox pod that mounts the claim, the pod
// a listing is read through and PlanTrim's sandbox count.
const cacheTrimPodsJSON = `{"items":[{"metadata":{"name":"sbx-a","namespace":"` + cacheTrimNamespace + `"},` +
	`"spec":{"containers":[{"name":"agent","volumeMounts":[{"name":"gt-cache","mountPath":"` + cacheTrimMountPath + `"}]}],` +
	`"volumes":[{"name":"gt-cache","persistentVolumeClaim":{"claimName":"` + cacheTrimClaimName + `"}}]},` +
	`"status":{"phase":"Running"}}]}`

// cacheTrimListingJSON is three build-cache files over the fixture's one-kilobyte
// cap, least recently used first, so PlanTrim removes two of them.
const cacheTrimListingJSON = `[` +
	`{"path":"gocache/aa/a","cache":"build","size":1000,"atime":"2026-10-29T01:00:00Z","mtime":"2026-10-29T01:00:00Z"},` +
	`{"path":"gocache/bb/b","cache":"build","size":1000,"atime":"2026-10-29T02:00:00Z","mtime":"2026-10-29T02:00:00Z"},` +
	`{"path":"gocache/cc/c","cache":"build","size":1000,"atime":"2026-10-29T03:00:00Z","mtime":"2026-10-29T03:00:00Z"}]`

// cacheTrimExistingJSON is the Job an earlier run left behind, carrying the
// trim's labels, and one shard of that run that this one does not name.
const cacheTrimExistingJSON = `{"items":[` +
	`{"kind":"Job","metadata":{"name":"` + cacheTrimJob + `","namespace":"` + cacheTrimNamespace + `",` +
	`"labels":{"app.kubernetes.io/name":"polecat-cache-trim","app.kubernetes.io/managed-by":"gt"}}},` +
	`{"kind":"ConfigMap","metadata":{"name":"` + cacheTrimJob + `-7","namespace":"` + cacheTrimNamespace + `",` +
	`"labels":{"app.kubernetes.io/name":"polecat-cache-trim","app.kubernetes.io/managed-by":"gt"}}}]}`

// cacheTrimFixture is the daemon a cache-trim test runs: the patrol on in its
// window, one kubernetes sandbox profile that mounts the claim, and kubectl
// answered in process.
type cacheTrimFixture struct {
	d   *Daemon
	cli *fakeCLI
	log *bytes.Buffer
}

// newCacheTrimFixture builds the patrol's daemon for mode ("", the dry-run
// default, or apply), answering kubectl from overrides and the standard
// replies for the calls a case does not name.
func newCacheTrimFixture(t *testing.T, mode string, overrides map[string]cliReply) *cacheTrimFixture {
	t.Helper()
	var logs bytes.Buffer
	var cli *fakeCLI
	cli = newFakeCLIFor(func(c cliCall) cliReply {
		if reply, refused := cacheTrimClientSideApplyRefusal(c); refused {
			return reply
		}
		verb := cacheTrimVerb(c.args)
		if reply, ok := overrides[verb]; ok {
			return reply
		}
		switch {
		case verb == cacheTrimPreflightVerb:
			return cliReply{stdout: diskMaintSandboxContext + "\n"}
		case verb == cacheTrimClaimsVerb:
			return cliReply{stdout: cacheTrimClaimsJSON}
		case strings.HasPrefix(verb, "get pods "):
			return cliReply{stdout: cacheTrimPodsJSON}
		case strings.Contains(verb, " "+cachevol.HelperPath+" trim-list "):
			return cliReply{stdout: cacheTrimListingJSON}
		case strings.HasPrefix(verb, "logs job/"+cachevol.DefaultListJobName+"-"):
			return cliReply{stdout: cacheTrimListingJSON}
		case strings.Contains(verb, " df -Pk "):
			return cliReply{stdout: cacheTrimDfReply(t, 1<<30)}
		case verb == cacheTrimDuVerb:
			return cliReply{stdout: cacheTrimDuReply(t, 1<<10)}
		case strings.HasPrefix(verb, "get job,configmap "):
			return cliReply{stdout: cacheTrimExistingJSON}
		}
		return cliReply{}
	})

	d := &Daemon{
		config:       &Config{TownRoot: t.TempDir()},
		logger:       log.New(&logs, "", 0),
		patrolConfig: diskMaintEnabled("", mode),
	}
	d.execCmd = cli.run
	d.maint.quiet = func(*Daemon) (bool, string) { return true, "" }
	d.diskMaint.now = func() time.Time { return diskMaintNow }
	d.diskMaint.getenv = func(key string) string {
		if key == cacheTrimCapEnv {
			return "1KB"
		}
		return ""
	}
	// A known environment with a marker the trim's kubectl must not inherit:
	// the daemon's whole environment is not what a cluster client gets.
	d.diskMaint.environ = func() []string {
		return []string{
			"PATH=/usr/local/bin:/usr/bin",
			"HOME=/home/polecat",
			"KUBECONFIG=/town/.runtime/sbx-k8s/kubeconfig",
			"GT_SECRET_MARKER=must-not-reach-kubectl",
		}
	}
	d.diskMaint.readDisks = func(context.Context) []disks.Reading { return nil }
	d.diskMaint.sandboxes = func() (map[string]agentconfig.SandboxProfile, error) {
		return map[string]agentconfig.SandboxProfile{
			"polecat-default": {
				Driver:               agentconfig.SandboxDriverKubernetes,
				Image:                "gastown-polecat:dev",
				KubernetesCacheClaim: cacheTrimClaimName,
			},
		}, nil
	}
	f := &cacheTrimFixture{d: d, cli: cli, log: &logs}
	d.diskMaint.targets = func(context.Context, *Daemon, string) (map[string]diskmaint.Target, error) {
		return map[string]diskmaint.Target{}, nil
	}
	d.diskMaint.dispatch = func(fn func()) { fn() }
	return f
}

// cacheTrimClientSideApplyRefusal is the API server's answer to a client-side
// apply whose kubectl.kubernetes.io/last-applied-configuration annotation would
// hold more than the metadata limit: client-side apply writes the whole object
// into that annotation, so an object over the limit is refused before it is
// stored. A server-side apply writes no annotation and is never refused for the
// object's size (gt-gg9hv.40).
func cacheTrimClientSideApplyRefusal(c cliCall) (cliReply, bool) {
	if !strings.HasPrefix(cacheTrimVerb(c.args), "apply ") ||
		slices.Contains(c.args, "--server-side") ||
		len(c.stdin) <= cacheTrimLastAppliedLimit {
		return cliReply{}, false
	}
	var obj struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	object := "Object"
	if err := json.Unmarshal([]byte(c.stdin), &obj); err == nil && obj.Kind != "" {
		object = obj.Kind + " " + obj.Metadata.Name
	}
	return cliReply{
		stderr: fmt.Sprintf("The %s is invalid: metadata.annotations: Too long: may not be more than %d bytes",
			object, cacheTrimLastAppliedLimit),
		code: 1,
	}, true
}

// setProfiles replaces the sandbox profiles the pass reads.
func (f *cacheTrimFixture) setProfiles(profiles map[string]agentconfig.SandboxProfile) {
	f.d.diskMaint.sandboxes = func() (map[string]agentconfig.SandboxProfile, error) { return profiles, nil }
}

// cacheTrimClaimsVerb is the pass's one cluster-wide claim read, the call that
// names the namespace every trim after it runs in.
const cacheTrimClaimsVerb = "get persistentvolumeclaim -A -o json"

// cacheTrimVerb is a kubectl call's verb and arguments, without the kubeconfig
// and the context every trim call names first. A call carrying only one of the
// two is no trim call and reads as "".
func cacheTrimVerb(args []string) string {
	if len(args) < 5 || args[0] != "--kubeconfig" || args[2] != "--context" {
		return ""
	}
	return strings.Join(args[4:], " ")
}

// verbs returns the kubectl verbs the pass ran, in call order.
func (f *cacheTrimFixture) verbs() []string {
	var out []string
	for _, call := range f.cli.recorded() {
		if verb := cacheTrimVerb(call.args); verb != "" {
			out = append(out, verb)
		}
	}
	return out
}

// indexOf returns where verb first appears in the pass's calls, and false when
// it never ran.
func (f *cacheTrimFixture) indexOf(needle string) (int, bool) {
	for i, call := range f.cli.recorded() {
		if cacheTrimVerb(call.args) == needle {
			return i, true
		}
	}
	return 0, false
}

// indexAfter is indexOf over the calls after index from: the call that follows a
// given one, when the same argv runs more than once.
func (f *cacheTrimFixture) indexAfter(needle string, from int) (int, bool) {
	for i, call := range f.cli.recorded() {
		if i > from && cacheTrimVerb(call.args) == needle {
			return i, true
		}
	}
	return 0, false
}

// applied returns the documents the pass applied, as "Kind name" in apply order.
func (f *cacheTrimFixture) applied(t *testing.T) []string {
	t.Helper()
	var applied []string
	for _, call := range f.cli.recorded() {
		if !strings.HasPrefix(cacheTrimVerb(call.args), "apply ") {
			continue
		}
		var doc struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal([]byte(call.stdin), &doc); err != nil {
			t.Fatalf("an applied document is not JSON: %v (%q)", err, call.stdin)
		}
		applied = append(applied, doc.Kind+" "+doc.Metadata.Name)
	}
	return applied
}

// appliedTrimJob is the Job document the pass applied, decoded as the builder's
// own type so a test reads the spec the cluster would.
func (f *cacheTrimFixture) appliedTrimJob(t *testing.T) cachevol.SeedJob {
	t.Helper()
	for _, call := range f.cli.recorded() {
		if !strings.HasPrefix(cacheTrimVerb(call.args), "apply ") {
			continue
		}
		var header struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(call.stdin), &header); err != nil {
			t.Fatalf("an applied document is not JSON: %v (%q)", err, call.stdin)
		}
		if header.Kind != "Job" {
			continue
		}
		var job cachevol.SeedJob
		if err := json.Unmarshal([]byte(call.stdin), &job); err != nil {
			t.Fatalf("the applied Job is not a Job document: %v", err)
		}
		return job
	}
	t.Fatal("the pass applied no Job")
	return cachevol.SeedJob{}
}

// cacheTrimValues is the image pair a test writes for the CI-built values file:
// two digest references for one commit, the pair gt update images records.
func cacheTrimValues() imagevalues.Values {
	return imagevalues.Values{
		Daemon:  "registry.internal:5000/town/gastown-daemon@sha256:" + strings.Repeat("a", 64),
		Polecat: "registry.internal:5000/town/gastown-polecat@sha256:" + strings.Repeat("b", 64),
		Commit:  strings.Repeat("c", 40),
	}
}

// writeValuesFile records v at the path the pass reads under the fixture's town
// root.
func (f *cacheTrimFixture) writeValuesFile(t *testing.T, v imagevalues.Values) {
	t.Helper()
	path := imagevalues.PathIn(filepath.Join(f.d.config.TownRoot, "gastown"))
	if err := imagevalues.Write(path, v); err != nil {
		t.Fatalf("write the values file: %v", err)
	}
}

// TestCacheTrimImageFromTheValuesFile is the bead's gate: with the CI-built
// values file present, the Job the pass applies runs the polecat image the file
// names, and its digest reference carries no pull policy; with no file the Job
// runs the claim's profile image under today's policy for its moving tag.
func TestCacheTrimImageFromTheValuesFile(t *testing.T) {
	t.Parallel()
	t.Run("the values file names the image", func(t *testing.T) {
		t.Parallel()
		f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, nil)
		values := cacheTrimValues()
		f.writeValuesFile(t, values)
		f.d.runDiskMaintenance()

		containers := f.appliedTrimJob(t).Spec.Template.Spec.Containers
		if len(containers) == 0 {
			t.Fatal("the applied Job has no container")
		}
		for _, c := range containers {
			if c.Image != values.Polecat {
				t.Errorf("the Job's container %s runs %q, want the values file's polecat image %q", c.Name, c.Image, values.Polecat)
			}
			if c.ImagePullPolicy != "" {
				t.Errorf("the Job's container %s carries the pull policy %q, want none for a digest reference", c.Name, c.ImagePullPolicy)
			}
		}
	})
	t.Run("no values file", func(t *testing.T) {
		t.Parallel()
		f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, nil)
		f.d.runDiskMaintenance()

		// The fixture's profile runs a moving tag, so the claim's image keeps the
		// policy a moving tag takes.
		const claimImage = "gastown-polecat:dev"
		containers := f.appliedTrimJob(t).Spec.Template.Spec.Containers
		if len(containers) == 0 {
			t.Fatal("the applied Job has no container")
		}
		for _, c := range containers {
			if c.Image != claimImage {
				t.Errorf("the Job's container %s runs %q, want the claim's image %q with no values file", c.Name, c.Image, claimImage)
			}
			if c.ImagePullPolicy != "Always" {
				t.Errorf("the Job's container %s carries the pull policy %q, want %q for a moving tag", c.Name, c.ImagePullPolicy, "Always")
			}
		}
	})
}

// TestCacheTrimMalformedValuesFile pins the refusal: a values file that is there
// and does not parse is logged, stops the pass before any cluster call, and is
// retried in fifteen minutes rather than at the full interval.
func TestCacheTrimMalformedValuesFile(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, nil)
	path := imagevalues.PathIn(filepath.Join(f.d.config.TownRoot, "gastown"))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the values file's directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("polecat: not a digest\n"), 0o644); err != nil {
		t.Fatalf("write the malformed values file: %v", err)
	}

	now := diskMaintNow
	f.d.diskMaint.now = func() time.Time { return now }
	trims := 0
	profiles := f.d.diskMaint.sandboxes
	f.d.diskMaint.sandboxes = func() (map[string]agentconfig.SandboxProfile, error) {
		trims++
		return profiles()
	}

	f.d.runDiskMaintenance()
	if trims != 1 {
		t.Fatalf("the trim ran %d times at the first tick, want 1", trims)
	}
	if verbs := f.verbs(); len(verbs) != 0 {
		t.Errorf("the malformed values file let the pass reach the cluster: %v", verbs)
	}
	logged := f.log.String()
	if !strings.Contains(logged, "imagevalues:") || !strings.Contains(logged, "nothing trimmed") {
		t.Errorf("the log does not name the values file's error: %s", logged)
	}

	now = now.Add(5 * time.Minute)
	f.d.runDiskMaintenance() // a failed pass is due in fifteen minutes, not now
	if trims != 1 {
		t.Fatalf("the trim ran %d times five minutes after the malformed file, want the retry at fifteen", trims)
	}
	now = now.Add(10 * time.Minute)
	f.d.runDiskMaintenance() // the retry
	if trims != 2 {
		t.Fatalf("the trim ran %d times fifteen minutes after the malformed file, want a retry", trims)
	}
}

// TestCacheTrimMalformedValuesFileWithNoClaims: a malformed values file is named
// and the pass retried in fifteen minutes even when no profile mounts a claim.
// The file read once there was a claim to trim made the fault silent in a pass
// with nothing to trim (gt-714vqw).
func TestCacheTrimMalformedValuesFileWithNoClaims(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, nil)
	// A docker profile mounts no kubernetes cache claim, so the pass has no claim
	// to trim and reaches the empty-claims return.
	f.setProfiles(map[string]agentconfig.SandboxProfile{
		"polecat-default": {Driver: agentconfig.SandboxDriverDocker, Image: "gastown-polecat:dev"},
	})
	path := imagevalues.PathIn(filepath.Join(f.d.config.TownRoot, "gastown"))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the values file's directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("polecat: not a digest\n"), 0o644); err != nil {
		t.Fatalf("write the malformed values file: %v", err)
	}

	now := diskMaintNow
	f.d.diskMaint.now = func() time.Time { return now }
	passes := 0
	profiles := f.d.diskMaint.sandboxes
	f.d.diskMaint.sandboxes = func() (map[string]agentconfig.SandboxProfile, error) {
		passes++
		return profiles()
	}

	f.d.runDiskMaintenance()
	if passes != 1 {
		t.Fatalf("the trim ran %d times at the first tick, want 1", passes)
	}
	logged := f.log.String()
	if !strings.Contains(logged, "imagevalues:") || !strings.Contains(logged, "nothing trimmed") {
		t.Errorf("the log does not name the values file's error: %s", logged)
	}

	now = now.Add(5 * time.Minute)
	f.d.runDiskMaintenance() // a failed pass is due in fifteen minutes, not now
	if passes != 1 {
		t.Fatalf("the trim ran %d times five minutes after the malformed file, want the retry at fifteen", passes)
	}
	now = now.Add(10 * time.Minute)
	f.d.runDiskMaintenance() // the retry
	if passes != 2 {
		t.Fatalf("the trim ran %d times fifteen minutes after the malformed file, want a retry", passes)
	}
}

// TestDiskMaintenanceAppliesTheShardsBeforeTheJob is the bead's gate: the
// patrol's own pass reaches cachevol.BuildTrimManifest and applies the
// manifest's documents in order, the shard ConfigMaps before the Job that
// mounts them, and the shards a finished Job read are deleted after it.
func TestDiskMaintenanceAppliesTheShardsBeforeTheJob(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, nil)
	f.d.runDiskMaintenance()

	want := []string{
		"ConfigMap " + cacheTrimShardName(0),
		"Job " + cacheTrimJob,
	}
	if applied := f.applied(t); !slices.Equal(applied, want) {
		t.Fatalf("the pass applied %v, want the shards before the Job %v", applied, want)
	}

	// The stale Job is deleted before anything is applied: a completed Job's
	// pod never runs again, and its template cannot be applied over. The delete
	// waits for the Job's pods, so the same-named Job applied next does not
	// race it.
	deleteJob, ok := f.indexOf("delete job " + cacheTrimJob + " -n " + cacheTrimNamespace + " --ignore-not-found --wait=true --timeout=60s")
	if !ok {
		t.Fatalf("the pass did not delete the earlier run's Job in the foreground; calls: %v", f.verbs())
	}
	firstApply, _ := f.indexOf(cacheTrimApplyVerb)
	if deleteJob > firstApply {
		t.Errorf("the stale Job was deleted after the apply; calls: %v", f.verbs())
	}
	// The shards are deleted once the Job has finished, so a pod never mounts
	// one that is gone.
	wait, ok := f.indexOf("wait --for=condition=complete job/" + cacheTrimJob + " -n " + cacheTrimNamespace + " --timeout=120s")
	if !ok {
		t.Fatalf("the pass did not wait for the Job; calls: %v", f.verbs())
	}
	shards, ok := f.indexOf("delete configmap -n " + cacheTrimNamespace + " -l app.kubernetes.io/managed-by=gt,app.kubernetes.io/name=polecat-cache-trim")
	if !ok {
		t.Fatalf("the pass left the Job's shards behind; calls: %v", f.verbs())
	}
	if shards < wait {
		t.Errorf("the shards were deleted before the Job finished; calls: %v", f.verbs())
	}
}

// TestDiskMaintenanceCacheTrimScopesEveryCallToTheClaimNamespace pins the
// namespace rule: the claim listing is the pass's one cluster-wide read, and
// every call after it -- the pod read, the listing exec, and the apply, delete
// and wait on the trim's objects -- names the namespace that listing gave.
func TestDiskMaintenanceCacheTrimScopesEveryCallToTheClaimNamespace(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, nil)
	f.d.runDiskMaintenance()

	calls := f.verbs()
	if len(calls) < 2 {
		t.Fatalf("the pass ran %d kubectl calls, want the preflight and then the claim listing", len(calls))
	}
	if calls[0] != cacheTrimPreflightVerb {
		t.Fatalf("the first kubectl call is %q, want the context preflight %q", calls[0], cacheTrimPreflightVerb)
	}
	if calls[1] != cacheTrimClaimsVerb {
		t.Fatalf("the second kubectl call is %q, want the claim listing %q", calls[1], cacheTrimClaimsVerb)
	}
	for _, verb := range calls[2:] {
		if !slices.Contains(strings.Fields(verb), "-n") || !strings.Contains(verb, "-n "+cacheTrimNamespace) {
			t.Errorf("the call %q does not name the claim's namespace %s", verb, cacheTrimNamespace)
		}
	}
}

// TestDiskMaintenanceCacheTrimJudgesAClaimByItsOwnNamespace pins the namespace
// against a same-named claim in another namespace: a claim in namespace A is
// trimmed from A's pods alone, and a claim of that name in B is read from B's
// pods alone, so a pod in A is never B's sandbox count or listing source.
func TestDiskMaintenanceCacheTrimJudgesAClaimByItsOwnNamespace(t *testing.T) {
	t.Parallel()
	const otherNamespace = "other"
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		cacheTrimClaimsVerb: {stdout: `{"items":[` +
			`{"metadata":{"name":"` + cacheTrimClaimName + `","namespace":"` + cacheTrimNamespace + `"}},` +
			`{"metadata":{"name":"` + cacheTrimClaimName + `","namespace":"` + otherNamespace + `"}}]}`},
		"get pods -n " + otherNamespace + " -o json": {stdout: `{"items":[]}`},
	})
	f.d.runDiskMaintenance()

	// The busy namespace's pod is neither read nor counted for the other claim:
	// the other claim has no pod of its own, so its listing comes from the lister
	// Job in its own namespace, and nothing in that namespace is exec'd.
	for _, verb := range f.verbs() {
		if strings.Contains(verb, otherNamespace) && strings.HasPrefix(verb, "exec ") {
			t.Errorf("the pass exec'd %q in the podless namespace", verb)
		}
	}
	if !slices.Contains(f.verbs(), "logs job/"+cacheTrimListJob+" -n "+otherNamespace) {
		t.Errorf("the other claim's listing was not read through its own lister Job; calls: %v", f.verbs())
	}
	if logged := f.log.String(); !strings.Contains(logged, "cache trim "+cacheTrimClaimName+" in "+otherNamespace+" applied") {
		t.Errorf("the log does not say the other claim was trimmed from its own lister's listing: %s", logged)
	}
	if logged := f.log.String(); !strings.Contains(logged, "cache trim "+cacheTrimClaimName+" in "+cacheTrimNamespace+" applied") {
		t.Errorf("the log does not say the claim was trimmed from its own namespace's pod: %s", logged)
	}
}

// cacheTrimShardName is the shard ConfigMap a manifest of the fixture's list
// names at index.
func cacheTrimShardName(index int) string {
	return cachevol.TrimShardName(cacheTrimJob, index)
}

// TestDiskMaintenanceDryRunAppliesNothing pins the default mode: the manifest
// is built and its objects are logged, and no apply, delete or wait runs.
func TestDiskMaintenanceDryRunAppliesNothing(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", nil)
	f.d.runDiskMaintenance()

	for _, verb := range f.verbs() {
		if strings.HasPrefix(verb, "apply ") || strings.HasPrefix(verb, "delete ") || strings.HasPrefix(verb, "wait ") {
			t.Errorf("a dry run ran %q, want no change", verb)
		}
	}
	logged := f.log.String()
	if !strings.Contains(logged, "cache trim "+cacheTrimClaimName+" in "+cacheTrimNamespace+" dry_run") {
		t.Errorf("the log does not name the dry run: %s", logged)
	}
	if !strings.Contains(logged, cacheTrimJob) || !strings.Contains(logged, cacheTrimShardName(0)) {
		t.Errorf("the log does not name the manifest's objects: %s", logged)
	}
}

// TestDiskMaintenanceCacheTrimRefusesAnUnreadableListing pins the fail-closed
// read: a listing the pod cannot print trims nothing and says so, rather than
// building a manifest from a listing it never got.
func TestDiskMaintenanceCacheTrimRefusesAnUnreadableListing(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		"exec -n " + cacheTrimNamespace + " sbx-a -c agent -- " + cachevol.HelperPath + " trim-list --root " + cacheTrimMountPath: {
			stderr: "trim-list: the claim at " + cacheTrimMountPath + ": no such file", code: 3,
		},
	})
	f.d.runDiskMaintenance()

	for _, verb := range f.verbs() {
		if strings.HasPrefix(verb, "apply ") {
			t.Errorf("the pass applied a trim from a listing it could not read: %q", verb)
		}
	}
	if logged := f.log.String(); !strings.Contains(logged, "the listing could not be read") {
		t.Errorf("the log does not say why nothing was trimmed: %s", logged)
	}
}

// cacheTrimLogListingJSON is a listing of one file over the fixture's cap on its
// own, so the trim removes exactly it. It is not the exec route's listing, so a
// test can tell which listing the trim was built from.
const cacheTrimLogListingJSON = `[{"path":"gocache/ll/x","cache":"build","size":3000,"atime":"2026-10-29T01:00:00Z","mtime":"2026-10-29T01:00:00Z"}]`

// cacheTrimPendingHolderJSON is the pod list of a claim whose one holder is
// pending: the claim is held, and no pod can answer an exec.
const cacheTrimPendingHolderJSON = `{"items":[{"metadata":{"name":"sbx-b","namespace":"` + cacheTrimNamespace + `"},` +
	`"spec":{"containers":[{"name":"agent","volumeMounts":[{"name":"gt-cache","mountPath":"` + cacheTrimMountPath + `"}]}],` +
	`"volumes":[{"name":"gt-cache","persistentVolumeClaim":{"claimName":"` + cacheTrimClaimName + `"}}]},` +
	`"status":{"phase":"Pending"}}]}`

// appliedContaining reports whether the pass applied a document whose bytes hold
// needle.
func (f *cacheTrimFixture) appliedContaining(needle string) bool {
	for _, call := range f.cli.recorded() {
		if strings.HasPrefix(cacheTrimVerb(call.args), "apply ") && strings.Contains(string(call.stdin), needle) {
			return true
		}
	}
	return false
}

// TestCacheTrimListerJobReadsAClaimNoPodMounts is the bead's first box: with no
// source pod the pass applies the lister Job, reads its log as the listing,
// deletes the Job once read, and builds the trim from that listing.
func TestCacheTrimListerJobReadsAClaimNoPodMounts(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		cacheTrimNoPodsVerb:     {stdout: `{"items":[]}`},
		cacheTrimListerLogsVerb: {stdout: cacheTrimLogListingJSON},
	})
	f.d.runDiskMaintenance()

	if applied := f.applied(t); !slices.Contains(applied, "Job "+cacheTrimListJob) {
		t.Fatalf("the pass applied %v, want the lister Job", applied)
	}
	for _, verb := range f.verbs() {
		if strings.HasPrefix(verb, "exec ") {
			t.Errorf("the pass ran %q with no running pod to exec in", verb)
		}
	}
	// The same delete runs before the apply, to clear an earlier pass's Job, and
	// after the read, to remove this one: the second is the one after the log.
	apply, applied := f.indexOf(cacheTrimApplyVerb)
	wait, waited := f.indexOf(cacheTrimListerWaitVerb)
	logs, read := f.indexOf(cacheTrimListerLogsVerb)
	cleared, clearedFirst := f.indexOf(cacheTrimListerDeleteVerb)
	deleted, gone := f.indexAfter(cacheTrimListerDeleteVerb, logs)
	if !applied || !waited || !read || !clearedFirst || !gone {
		t.Fatalf("the lister Job was not applied, waited for, read and deleted; calls: %v", f.verbs())
	}
	if !(cleared < apply && apply < wait && wait < logs && logs < deleted) {
		t.Errorf("the lister Job's calls ran out of order (clear %d, apply %d, wait %d, logs %d, delete %d); calls: %v",
			cleared, apply, wait, logs, deleted, f.verbs())
	}
	if !f.appliedContaining("gocache/ll/x") {
		t.Errorf("the trim was not built from the lister's log: no applied document names its one file")
	}
	if !slices.Contains(f.applied(t), "Job "+cacheTrimJob) {
		t.Errorf("the pass did not apply the trim Job after reading the listing; applied %v", f.applied(t))
	}
}

// TestCacheTrimListerJobTrimsTheClaimRouteWithoutAFitCheck pins the reads a
// claim with no running pod cannot make. The listing crosses the shard ceiling,
// so the trim takes the claim route, whose free space df would read through a
// pod; with none the room is unknown and the trim runs without the fit check its
// writer makes again. The claim is not measured after the trim, since du needs a
// pod too.
func TestCacheTrimListerJobTrimsTheClaimRouteWithoutAFitCheck(t *testing.T) {
	t.Parallel()
	// The room is never read here, so the listing needs only to cross the ceiling;
	// the KiB-aligned one costs a search this test does not need.
	listing := cacheTrimClaimRouteListing(cachevol.TrimMaxShards+1, 0)
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		cacheTrimNoPodsVerb:     {stdout: `{"items":[]}`},
		cacheTrimListerLogsVerb: {stdout: listing},
	})
	f.d.runDiskMaintenance()

	if applied := f.applied(t); !slices.Equal(applied, []string{"Job " + cacheTrimListJob, "Job " + cacheTrimJob}) {
		t.Fatalf("the pass applied %v, want the lister Job and the one trim Job; log: %s", applied, f.log.String())
	}
	for _, verb := range []string{cacheTrimFreeVerb, cacheTrimDuVerb} {
		if _, ok := f.indexOf(verb); ok {
			t.Errorf("the pass ran %q with no running pod to run it in", verb)
		}
	}
	logged := f.log.String()
	for _, want := range []string{"free space unknown", "no running pod mounts the claim", "not measured after the trim"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the log does not say %q: %s", want, logged)
		}
	}
}

// TestCacheTrimListerJobFailureTrimsNothing is the bead's second box: a lister
// Job that fails, does not finish, or whose log does not parse logs that the
// listing could not be read, trims nothing, and reports a retry. A claim with no
// image to run the Job in is refused the same way.
func TestCacheTrimListerJobFailureTrimsNothing(t *testing.T) {
	t.Parallel()
	timedOut := cliReply{stderr: "error: timed out waiting for the condition on jobs/" + cacheTrimListJob, code: 1}
	for _, tc := range []struct {
		name      string
		overrides map[string]cliReply
		noImage   bool
		why       string
	}{
		{
			name:      "the log does not parse",
			overrides: map[string]cliReply{cacheTrimListerLogsVerb: {stdout: "trim-list: no listing"}},
			why:       "decoding the trim listing",
		},
		{
			name: "the Job failed",
			overrides: map[string]cliReply{
				cacheTrimListerWaitVerb: timedOut,
				"get job " + cacheTrimListJob + " -n " + cacheTrimNamespace + " -o json": {stdout: `{"status":{"failed":1}}`},
			},
			why: "the Job failed",
		},
		{
			name:      "the Job does not finish",
			overrides: map[string]cliReply{cacheTrimListerWaitVerb: timedOut},
			why:       "has not finished within the wait",
		},
		{
			name:      "the Job cannot be applied",
			overrides: map[string]cliReply{cacheTrimApplyVerb: {stderr: "Error from server: forbidden", code: 1}},
			why:       "applying the lister Job",
		},
		{
			name:      "the Job's log cannot be read",
			overrides: map[string]cliReply{cacheTrimListerLogsVerb: {stderr: "Error from server: pod not found", code: 1}},
			why:       "reading the lister Job's log",
		},
		{
			name:      "an earlier lister Job cannot be deleted",
			overrides: map[string]cliReply{cacheTrimListerDeleteVerb: {stderr: "Error from server: forbidden", code: 1}},
			why:       "deleting an earlier lister Job",
		},
		{
			name:    "no image runs the Job",
			noImage: true,
			why:     "the trim Job image",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			overrides := map[string]cliReply{cacheTrimNoPodsVerb: {stdout: `{"items":[]}`}}
			maps.Copy(overrides, tc.overrides)
			f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, overrides)
			if tc.noImage {
				f.setProfiles(map[string]agentconfig.SandboxProfile{
					"polecat-default": {
						Driver:               agentconfig.SandboxDriverKubernetes,
						KubernetesCacheClaim: cacheTrimClaimName,
					},
				})
			}
			now := diskMaintNow
			passes := 0
			profiles := f.d.diskMaint.sandboxes
			f.d.diskMaint.sandboxes = func() (map[string]agentconfig.SandboxProfile, error) {
				passes++
				return profiles()
			}
			f.d.diskMaint.now = func() time.Time { return now }

			f.d.runDiskMaintenance()

			if logged := f.log.String(); !strings.Contains(logged, "could not be read") || !strings.Contains(logged, tc.why) {
				t.Errorf("the log does not say the listing could not be read because %q: %s", tc.why, logged)
			}
			for _, doc := range f.applied(t) {
				if doc != "Job "+cacheTrimListJob {
					t.Errorf("the pass applied %q from a listing it could not read; applied %v", doc, f.applied(t))
				}
			}
			// The failed read reports a retry: due at fifteen minutes, not five.
			now = now.Add(5 * time.Minute)
			f.d.runDiskMaintenance()
			if passes != 1 {
				t.Fatalf("the pass ran %d times five minutes after a failed read, want the retry at fifteen minutes", passes)
			}
			now = now.Add(10 * time.Minute)
			f.d.runDiskMaintenance()
			if passes != 2 {
				t.Errorf("the pass ran %d times fifteen minutes after a failed read, want 2", passes)
			}
		})
	}
}

// TestCacheTrimExecRouteWinsOverListerJob pins the order: with a running source
// pod the listing is read through exec, and no lister Job is applied or read.
func TestCacheTrimExecRouteWinsOverListerJob(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, nil)
	f.d.runDiskMaintenance()

	if !slices.Contains(f.verbs(), cacheTrimListVerb) {
		t.Fatalf("the pass did not read the listing through exec; calls: %v", f.verbs())
	}
	for _, doc := range f.applied(t) {
		if doc == "Job "+cacheTrimListJob {
			t.Errorf("the pass applied the lister Job with a running pod to read through")
		}
	}
	if _, read := f.indexOf(cacheTrimListerLogsVerb); read {
		t.Errorf("the pass read a lister Job's log with a running pod to read through")
	}
}

// TestCacheTrimListerJobIsNotAppliedInADryRun pins the dry run: it applies
// nothing, so the lister Job is not applied for a claim no running pod mounts.
func TestCacheTrimListerJobIsNotAppliedInADryRun(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeDryRun, map[string]cliReply{
		cacheTrimNoPodsVerb: {stdout: `{"items":[]}`},
	})
	f.d.runDiskMaintenance()

	for _, verb := range f.verbs() {
		if strings.HasPrefix(verb, "apply ") || strings.HasPrefix(verb, "delete ") || strings.HasPrefix(verb, "wait ") {
			t.Errorf("a dry run ran %q, want no change", verb)
		}
	}
	if logged := f.log.String(); !strings.Contains(logged, "cannot be read in a dry run") {
		t.Errorf("the dry run does not say the listing was not read: %s", logged)
	}
}

// TestCacheTrimListerJobNameIsDistinctFromTheTrimJob pins the lister's name: it
// is named for the claim, is a valid object name even for the longest claim, and
// never equals the trim Job's name, so one never touches the other.
func TestCacheTrimListerJobNameIsDistinctFromTheTrimJob(t *testing.T) {
	t.Parallel()
	if got := cacheTrimListJobName(cacheTrimClaimName); got != cacheTrimListJob {
		t.Errorf("cacheTrimListJobName(%q) = %q, want %q", cacheTrimClaimName, got, cacheTrimListJob)
	}
	for _, claim := range []string{cacheTrimClaimName, strings.Repeat("e", 63)} {
		list, trim := cacheTrimListJobName(claim), cacheTrimJobName(claim)
		if list == trim {
			t.Errorf("the lister Job and the trim Job for %d-character claim share the name %q", len(claim), list)
		}
		if len(list) > cacheTrimJobNameMax {
			t.Errorf("the lister Job name for a %d-character claim is %d characters, want at most %d", len(claim), len(list), cacheTrimJobNameMax)
		}
	}
}

// TestDiskMaintenanceCacheTrimReadsAClaimWithOnlyAPendingHolder pins the case the
// old refusal named: a pod holds the claim but none runs, so no exec can read the
// listing, and the lister Job reads it instead. The pending holder still counts
// as a sandbox.
func TestDiskMaintenanceCacheTrimReadsAClaimWithOnlyAPendingHolder(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		cacheTrimNoPodsVerb:     {stdout: cacheTrimPendingHolderJSON},
		cacheTrimListerLogsVerb: {stdout: cacheTrimLogListingJSON},
	})
	f.d.runDiskMaintenance()

	for _, verb := range f.verbs() {
		if strings.HasPrefix(verb, "exec ") {
			t.Errorf("the pass ran %q with no running pod to exec in", verb)
		}
	}
	if !slices.Contains(f.applied(t), "Job "+cacheTrimListJob) {
		t.Errorf("the pass did not apply the lister Job for a claim held by a pending pod; applied %v", f.applied(t))
	}
}

// TestCacheTrimPodsCountsEveryHolderThatHasNotFinished pins the sandbox count:
// a pod that mounts the claim and has not finished holds it, whether it runs or
// waits to start, so PlanTrim leaves the module cache alone while either does.
// A pod that has finished holds nothing, and a pod of another claim is no
// holder at all.
func TestCacheTrimPodsCountsEveryHolderThatHasNotFinished(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, "", map[string]cliReply{
		"get pods -n " + cacheTrimNamespace + " -o json": {stdout: `{"items":[` +
			cacheTrimPodJSON("sbx-run", "Running", cacheTrimClaimName) + `,` +
			cacheTrimPodJSON("sbx-wait", "Pending", cacheTrimClaimName) + `,` +
			cacheTrimPodJSON("sbx-done", "Succeeded", cacheTrimClaimName) + `,` +
			cacheTrimPodJSON("sbx-failed", "Failed", cacheTrimClaimName) + `,` +
			cacheTrimPodJSON("sbx-other", "Running", "gt-other-cache") + `]}`},
	})

	holders, sources, err := f.d.cacheTrimPods(context.Background(), "/kubeconfig", cacheTrimNamespace, cacheTrimClaimName)
	if err != nil {
		t.Fatalf("cacheTrimPods: %v", err)
	}
	if holders != 2 {
		t.Errorf("cacheTrimPods counted %d holders, want the running and the pending pod", holders)
	}
	if len(sources) != 1 || sources[0].Pod != "sbx-run" {
		t.Errorf("cacheTrimPods read the listing through %+v, want the running pod alone", sources)
	}
}

// cacheTrimPodJSON is one pod of a fixture's pod list in the claim's
// namespace, mounting claim.
func cacheTrimPodJSON(name, phase, claim string) string {
	return cacheTrimPodJSONIn(name, phase, claim, cacheTrimNamespace)
}

// cacheTrimPodJSONIn is one pod of a fixture's pod list, in namespace,
// mounting claim.
func cacheTrimPodJSONIn(name, phase, claim, namespace string) string {
	return `{"metadata":{"name":"` + name + `","namespace":"` + namespace + `"},` +
		`"spec":{"containers":[{"name":"agent","volumeMounts":[{"name":"gt-cache","mountPath":"` + cacheTrimMountPath + `"}]}],` +
		`"volumes":[{"name":"gt-cache","persistentVolumeClaim":{"claimName":"` + claim + `"}}]},` +
		`"status":{"phase":"` + phase + `"}}`
}

// TestDiskMaintenanceCacheTrimLeavesShardsWhileTheJobRuns pins the cleanup's
// guard: a Job still running keeps its shards, since deleting a mount its pod
// is reading would fail the trim it is part of.
func TestDiskMaintenanceCacheTrimLeavesShardsWhileTheJobRuns(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		"wait --for=condition=complete job/" + cacheTrimJob + " -n " + cacheTrimNamespace + " --timeout=120s": {
			stderr: "error: timed out waiting for the condition", code: 1,
		},
	})
	f.d.runDiskMaintenance()

	if _, ok := f.indexOf("delete configmap -n " + cacheTrimNamespace + " -l app.kubernetes.io/managed-by=gt,app.kubernetes.io/name=polecat-cache-trim"); ok {
		t.Errorf("the shards were deleted while the Job was still running; calls: %v", f.verbs())
	}
	if logged := f.log.String(); !strings.Contains(logged, "has not finished within the wait (timed out)") {
		t.Errorf("the log does not say why the Job kept its shards: %s", logged)
	}
}

// TestDiskMaintenanceCacheTrimLeavesARunningEarlierJobAlone pins the delete's
// guard: an earlier trim Job whose pods are still working is not deleted, and
// this pass applies nothing, since applying over it is refused by its immutable
// template and deleting it would stop the trim it is in the middle of.
func TestDiskMaintenanceCacheTrimLeavesARunningEarlierJobAlone(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		"get job,configmap -n " + cacheTrimNamespace + " -l app.kubernetes.io/managed-by=gt,app.kubernetes.io/name=polecat-cache-trim -o json": {
			stdout: `{"items":[{"kind":"Job","metadata":{"name":"` + cacheTrimJob + `","namespace":"` + cacheTrimNamespace + `",` +
				`"labels":{"app.kubernetes.io/name":"polecat-cache-trim","app.kubernetes.io/managed-by":"gt"}},` +
				`"status":{"active":1}}]}`,
		},
	})
	f.d.runDiskMaintenance()

	for _, verb := range f.verbs() {
		if strings.HasPrefix(verb, "delete ") || strings.HasPrefix(verb, "apply ") {
			t.Errorf("the pass ran %q while an earlier trim Job was running", verb)
		}
	}
	if logged := f.log.String(); !strings.Contains(logged, "the earlier trim Job "+cacheTrimJob+" is still running") {
		t.Errorf("the log does not say why nothing was applied: %s", logged)
	}
}

// TestDiskMaintenanceCacheTrimNamesTheWaitFailure pins the three ways a wait
// ends without the Job completing: a failed Job, a timeout, and a kubectl that
// could not watch the Job each get their own line, because each is a different
// repair.
func TestDiskMaintenanceCacheTrimNamesTheWaitFailure(t *testing.T) {
	t.Parallel()
	const (
		wait      = "wait --for=condition=complete job/" + cacheTrimJob + " -n " + cacheTrimNamespace + " --timeout=120s"
		jobStatus = "get job " + cacheTrimJob + " -n " + cacheTrimNamespace + " -o json"
	)
	tests := []struct {
		name     string
		override map[string]cliReply
		want     string
	}{
		{
			// The timeout text is what a failed Job's wait usually prints, so
			// the Job's own status is read first and wins.
			"the Job failed",
			map[string]cliReply{
				wait:      {stderr: "error: timed out waiting for the condition", code: 1},
				jobStatus: {stdout: `{"status":{"failed":1,"conditions":[{"type":"Failed","status":"True"}]}}`},
			},
			"the Job failed",
		},
		{
			"the wait timed out",
			map[string]cliReply{wait: {stderr: "error: timed out waiting for the condition", code: 1}},
			"the Job has not finished within the wait (timed out)",
		},
		{
			"kubectl could not watch the Job",
			map[string]cliReply{wait: {stderr: "Error from server: etcdserver: request timed out", code: 1}},
			"the wait on the Job failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, tc.override)
			f.d.runDiskMaintenance()
			if logged := f.log.String(); !strings.Contains(logged, tc.want) {
				t.Errorf("the log does not name the outcome %q: %s", tc.want, logged)
			}
		})
	}
}

// TestDiskMaintenanceCacheTrimRefusesASecondClaimInOneNamespace pins the
// guard on the trim's shared labels: a town that mounts two claims in one
// namespace has no labels to tell one trim's objects from the other's, so only
// the first claim's trim runs and the second says why it did not.
func TestDiskMaintenanceCacheTrimRefusesASecondClaimInOneNamespace(t *testing.T) {
	t.Parallel()
	const otherClaim = "gt-other-cache"
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		cacheTrimClaimsVerb: {stdout: `{"items":[` +
			`{"metadata":{"name":"` + cacheTrimClaimName + `","namespace":"` + cacheTrimNamespace + `"}},` +
			`{"metadata":{"name":"` + otherClaim + `","namespace":"` + cacheTrimNamespace + `"}}]}`},
		"get pods -n " + cacheTrimNamespace + " -o json": {stdout: `{"items":[` +
			cacheTrimPodJSON("sbx-a", "Running", cacheTrimClaimName) + `,` +
			cacheTrimPodJSON("sbx-b", "Running", otherClaim) + `]}`},
	})
	f.setProfiles(map[string]agentconfig.SandboxProfile{
		"first": {
			Driver:               agentconfig.SandboxDriverKubernetes,
			Image:                "gastown-polecat:dev",
			KubernetesCacheClaim: cacheTrimClaimName,
		},
		"second": {
			Driver:               agentconfig.SandboxDriverKubernetes,
			Image:                "gastown-polecat:dev",
			KubernetesCacheClaim: otherClaim,
		},
	})
	f.d.runDiskMaintenance()

	// Only the first claim's trim applies: its shard and its Job.
	if applied := f.applied(t); len(applied) != 2 {
		t.Errorf("the pass applied %v, want the first claim's two documents; calls: %v", applied, f.verbs())
	}
	if logged := f.log.String(); !strings.Contains(logged, "is trimmed in the same namespace") {
		t.Errorf("the log does not say why the second claim was not trimmed: %s", logged)
	}
}

// TestCacheTrimRefusedByTheConfigIsAVerdictNotARetry pins that a second claim in
// one namespace is refused by the town's config, which holds on every pass: the
// pass records a verdict and judges the claim again at the full interval, not
// in fifteen minutes.
func TestCacheTrimRefusedByTheConfigIsAVerdictNotARetry(t *testing.T) {
	t.Parallel()
	const otherClaim = "gt-other-cache"
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		cacheTrimClaimsVerb: {stdout: `{"items":[` +
			`{"metadata":{"name":"` + cacheTrimClaimName + `","namespace":"` + cacheTrimNamespace + `"}},` +
			`{"metadata":{"name":"` + otherClaim + `","namespace":"` + cacheTrimNamespace + `"}}]}`},
		"get pods -n " + cacheTrimNamespace + " -o json": {stdout: `{"items":[` +
			cacheTrimPodJSON("sbx-a", "Running", cacheTrimClaimName) + `,` +
			cacheTrimPodJSON("sbx-b", "Running", otherClaim) + `]}`},
	})
	f.setProfiles(map[string]agentconfig.SandboxProfile{
		"first": {
			Driver:               agentconfig.SandboxDriverKubernetes,
			Image:                "gastown-polecat:dev",
			KubernetesCacheClaim: cacheTrimClaimName,
		},
		"second": {
			Driver:               agentconfig.SandboxDriverKubernetes,
			Image:                "gastown-polecat:dev",
			KubernetesCacheClaim: otherClaim,
		},
	})
	now := diskMaintNow
	f.d.diskMaint.now = func() time.Time { return now }

	f.d.runDiskMaintenance() // 04:45: the second claim is refused
	reads := f.listReads()
	now = now.Add(15 * time.Minute)
	f.d.runDiskMaintenance() // 05:00: the refusal holds, so nothing is due
	if got := f.listReads(); got != reads {
		t.Errorf("the pass read the listings %d more times fifteen minutes after a config refusal, want none: the refusal is a verdict", got-reads)
	}
}

// TestCacheTrimUnderTheCapIsAVerdictNotARetry pins that a claim under the cap
// has nothing for the trim to remove. That is the claim's answer for its
// listing, so the pass judges it again at the full interval, not in fifteen
// minutes.
func TestCacheTrimUnderTheCapIsAVerdictNotARetry(t *testing.T) {
	t.Parallel()
	const underCap = `[{"path":"gocache/aa/a","cache":"build","size":500,` +
		`"atime":"2026-10-29T01:00:00Z","mtime":"2026-10-29T01:00:00Z"}]`
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		cacheTrimListVerb: {stdout: underCap},
	})
	now := diskMaintNow
	f.d.diskMaint.now = func() time.Time { return now }

	f.d.runDiskMaintenance() // 04:45: the listing is under the one-kilobyte cap
	if got := f.listReads(); got != 1 {
		t.Fatalf("the pass read the listing %d times, want 1", got)
	}
	now = now.Add(15 * time.Minute)
	f.d.runDiskMaintenance() // 05:00: a verdict spends the full interval
	if got := f.listReads(); got != 1 {
		t.Errorf("the pass read the listing %d times fifteen minutes after a clean verdict, want 1: nothing to trim is not a retry", got)
	}
	now = now.Add(105 * time.Minute)
	f.d.runDiskMaintenance() // 06:45: two hours after the verdict
	if got := f.listReads(); got != 2 {
		t.Errorf("the pass read the listing %d times two hours after the verdict, want 2", got)
	}
}

// TestCacheTrimOverTheCapPinnedByASandboxRetriesSoon pins the other side of the
// cap verdict: a claim over the cap whose only removable files are module cache
// files, while a sandbox runs, has nothing the trim may remove yet. That clears
// when the sandbox stops, so it is a failed run, retried in fifteen minutes.
func TestCacheTrimOverTheCapPinnedByASandboxRetriesSoon(t *testing.T) {
	t.Parallel()
	const pinned = `[{"path":"gomodcache/aa/a","cache":"module","size":1000,` +
		`"atime":"2026-10-29T01:00:00Z","mtime":"2026-10-29T01:00:00Z"},` +
		`{"path":"gomodcache/bb/b","cache":"module","size":1000,` +
		`"atime":"2026-10-29T02:00:00Z","mtime":"2026-10-29T02:00:00Z"}]`
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		cacheTrimListVerb: {stdout: pinned},
	})
	now := diskMaintNow
	f.d.diskMaint.now = func() time.Time { return now }

	f.d.runDiskMaintenance() // 04:45: the running sandbox pins the module cache
	if got := f.listReads(); got != 1 {
		t.Fatalf("the pass read the listing %d times, want 1", got)
	}
	now = now.Add(5 * time.Minute)
	f.d.runDiskMaintenance() // 04:50: a failed run is due in fifteen minutes, not now
	if got := f.listReads(); got != 1 {
		t.Errorf("the pass retried %d times five minutes after a pinned refusal, want 1: a retry is due at fifteen", got)
	}
	now = now.Add(10 * time.Minute)
	f.d.runDiskMaintenance() // 05:00: the short retry
	if got := f.listReads(); got != 2 {
		t.Errorf("the pass read the listing %d times fifteen minutes after a pinned refusal, want 2", got)
	}
}

// TestCacheTrimLogsEverySuccessfulTrim pins that a success is not held back by
// the once-an-hour hold on refusals: the same trim run twice in one hour says it
// applied both times.
func TestCacheTrimLogsEverySuccessfulTrim(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, nil)
	for range 2 {
		f.d.cacheTrim(context.Background(), agentconfig.DiskMaintenanceModeApply)
	}
	applied := "cache trim " + cacheTrimClaimName + " in " + cacheTrimNamespace + " applied"
	if got := strings.Count(f.log.String(), applied); got != 2 {
		t.Errorf("logged the applied trim %d times in one hour, want 2: %s", got, f.log.String())
	}
}

// TestDiskMaintenanceCacheTrimStopsAtTheReadItCannotMake pins each read the
// trim depends on: one that fails, or answers something this pass cannot
// decode, stops the trim where it happened and the log names it. A reply it
// reads as "nothing to trim" or as an empty object list would apply a manifest
// over an object it never saw.
func TestDiskMaintenanceCacheTrimStopsAtTheReadItCannotMake(t *testing.T) {
	t.Parallel()
	const (
		getClaims  = cacheTrimClaimsVerb
		getPods    = "get pods -n " + cacheTrimNamespace + " -o json"
		execList   = "exec -n " + cacheTrimNamespace + " sbx-a -c agent -- " + cachevol.HelperPath + " trim-list --root " + cacheTrimMountPath
		getObjects = "get job,configmap -n " + cacheTrimNamespace + " -l app.kubernetes.io/managed-by=gt,app.kubernetes.io/name=polecat-cache-trim -o json"
		deleteJob  = "delete job " + cacheTrimJob + " -n " + cacheTrimNamespace + " --ignore-not-found --wait=true --timeout=60s"
		apply      = cacheTrimApplyVerb
		deleteCM   = "delete configmap -n " + cacheTrimNamespace + " -l app.kubernetes.io/managed-by=gt,app.kubernetes.io/name=polecat-cache-trim"
	)
	tests := []struct {
		name     string
		override map[string]cliReply
		want     string
	}{
		{"the claim list fails", map[string]cliReply{getClaims: {stderr: "the server is unavailable", code: 1}},
			"the cluster's claims could not be read: kubectl"},
		{"the claim list is not JSON", map[string]cliReply{getClaims: {stdout: "not json"}},
			"the cluster's claims could not be read: decoding the claim list"},
		{"the claim list names no items", map[string]cliReply{getClaims: {stdout: `{}`}},
			"the cluster's claims could not be read: decoding the claim list: the reply names no item list"},
		{"a claim item names no namespace", map[string]cliReply{getClaims: {stdout: `{"items":[{"metadata":{"name":"` + cacheTrimClaimName + `"}}]}`}},
			"the cluster's claims could not be read: decoding the claim list: an item names no name or no namespace"},
		{"the pod list fails", map[string]cliReply{getPods: {stderr: "the server is unavailable", code: 1}},
			"the pods that mount it could not be read: kubectl"},
		{"the pod list is not JSON", map[string]cliReply{getPods: {stdout: "not json"}},
			"the pods that mount it could not be read: decoding the pod list"},
		{"the pod list names no items", map[string]cliReply{getPods: {stdout: `{}`}},
			"the pod list: the reply names no item list"},
		{"the pod list names a pod of another namespace", map[string]cliReply{getPods: {stdout: `{"items":[` +
			cacheTrimPodJSONIn("sbx-z", "Running", cacheTrimClaimName, "elsewhere") + `]}`}},
			"the pod list for namespace " + cacheTrimNamespace + " names the pod sbx-z in elsewhere"},
		{"the listing is not JSON", map[string]cliReply{execList: {stdout: "not json"}},
			"the listing could not be read: decoding the trim listing"},
		{"the listing is empty", map[string]cliReply{execList: {stdout: " "}},
			"the listing could not be read: decoding the trim listing"},
		{"the listing is an array of numbers", map[string]cliReply{execList: {stdout: "[1]"}},
			"the listing could not be read: decoding the trim listing"},
		{"the listing is a document, not the bare array", map[string]cliReply{execList: {stdout: `{"entries":[]}`}},
			"the listing could not be read: decoding the trim listing"},
		{"the object list fails", map[string]cliReply{getObjects: {stderr: "forbidden", code: 1}},
			"the apply failed: listing the trim's objects"},
		{"the object list is not JSON", map[string]cliReply{getObjects: {stdout: "not json"}},
			"the apply failed: decoding the trim's objects"},
		{"the object list names no items", map[string]cliReply{getObjects: {stdout: `{}`}},
			"the trim's objects: the reply names no object list"},
		{"the stale delete fails", map[string]cliReply{deleteJob: {stderr: "forbidden", code: 1}},
			"the apply failed: deleting the stale Job"},
		{"an apply fails", map[string]cliReply{apply: {stderr: "invalid manifest", code: 1}},
			"the apply failed: applying document 1 of"},
		{"the shard delete fails", map[string]cliReply{deleteCM: {stderr: "forbidden", code: 1}},
			"the apply failed: deleting the trim's shards"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, tc.override)
			f.d.runDiskMaintenance()
			if logged := f.log.String(); !strings.Contains(logged, tc.want) {
				t.Errorf("the log does not name the refusal %q: %s", tc.want, logged)
			}
		})
	}
}

// TestDiskMaintenanceCacheTrimRunsKubectlWithAMinimalEnvironment pins the
// environment the trim's kubectl runs with: the variables a binary and a
// kubeconfig need, and nothing else the daemon happens to hold.
func TestDiskMaintenanceCacheTrimRunsKubectlWithAMinimalEnvironment(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, nil)
	f.d.runDiskMaintenance()

	calls := f.cli.recorded()
	if len(calls) == 0 {
		t.Fatal("the pass ran no kubectl call")
	}
	want := []string{
		"PATH=/usr/local/bin:/usr/bin",
		"HOME=/home/polecat",
		"KUBECONFIG=/town/.runtime/sbx-k8s/kubeconfig",
	}
	for _, call := range calls {
		if !slices.Equal(call.env, want) {
			t.Errorf("kubectl ran with the environment %v, want %v", call.env, want)
		}
	}
}

// TestCacheTrimClaimsPicksTheClaimsWithAManifest pins which profiles a pass
// trims: only a kubernetes profile that mounts a claim, keyed by claim, with the
// image that profile runs; a claim two profiles name with different images is
// refused rather than trimmed with one of them.
func TestCacheTrimClaimsPicksTheClaimsWithAManifest(t *testing.T) {
	t.Parallel()
	profiles := map[string]agentconfig.SandboxProfile{
		"docker": {
			Driver:               agentconfig.SandboxDriverDocker,
			Image:                "docker-image",
			KubernetesCacheClaim: cacheTrimClaimName,
		},
		"no-claim": {
			Driver: agentconfig.SandboxDriverKubernetes,
			Image:  "gastown-polecat:dev",
		},
		"k8s-a": {
			Driver:               agentconfig.SandboxDriverKubernetes,
			Image:                "gastown-polecat:dev",
			KubernetesCacheClaim: cacheTrimClaimName,
		},
		"k8s-b": {
			Driver:               agentconfig.SandboxDriverKubernetes,
			Image:                "gastown-polecat:dev",
			KubernetesCacheClaim: "gt-other-cache",
		},
		"k8s-c": {
			Driver:               agentconfig.SandboxDriverKubernetes,
			Image:                "other-image",
			KubernetesCacheClaim: "gt-other-cache",
		},
	}

	claims, errs := cacheTrimClaims(profiles)
	want := []cacheTrimClaim{{ClaimName: cacheTrimClaimName, Image: "gastown-polecat:dev"}}
	if !reflect.DeepEqual(claims, want) {
		t.Errorf("claims = %+v, want %+v", claims, want)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "gt-other-cache") {
		t.Errorf("errs = %v, want the claim two profiles name with different images", errs)
	}
}

// TestDiskMaintenancePlacesTheTrimJobOnTheClaimsPool pins the placement wiring
// end to end: the node labels and tolerations of the profile that mounts the
// claim reach the trim Job's applied pod spec, so the pod runs where the
// claim's storage binds and not on a node that cannot mount it (gt-5paez).
func TestDiskMaintenancePlacesTheTrimJobOnTheClaimsPool(t *testing.T) {
	t.Parallel()
	profile := agentconfig.SandboxProfile{
		Driver:               agentconfig.SandboxDriverKubernetes,
		Image:                "gastown-polecat:dev",
		KubernetesCacheClaim: cacheTrimClaimName,
		NodeSelector:         map[string]string{"gt.dev/pool": "sandbox"},
		Tolerations: []agentconfig.SandboxToleration{
			{Key: "gt.dev/pool", Operator: "Equal", Value: "sandbox", Effect: "NoSchedule"},
		},
	}
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, nil)
	f.setProfiles(map[string]agentconfig.SandboxProfile{"polecat-k8s": profile})
	f.d.runDiskMaintenance()

	pod := f.appliedTrimJob(t).Spec.Template.Spec
	if !maps.Equal(pod.NodeSelector, profile.NodeSelector) {
		t.Errorf("nodeSelector = %v, want the profile's pool %v", pod.NodeSelector, profile.NodeSelector)
	}
	want := []cachevol.Toleration{{Key: "gt.dev/pool", Operator: "Equal", Value: "sandbox", Effect: "NoSchedule"}}
	if !slices.Equal(pod.Tolerations, want) {
		t.Errorf("tolerations = %+v, want the profile's %+v", pod.Tolerations, want)
	}
}

// TestCacheTrimClaimsRefusesAClaimItsProfilesPlaceDifferently pins the other
// ambiguity a claim can carry: two profiles that mount it on different pools
// are refused, because the trim Job runs on one pool and picking either
// profile's would schedule it where the other profile's storage does not bind.
func TestCacheTrimClaimsRefusesAClaimItsProfilesPlaceDifferently(t *testing.T) {
	t.Parallel()
	profile := func(pool string) agentconfig.SandboxProfile {
		return agentconfig.SandboxProfile{
			Driver:               agentconfig.SandboxDriverKubernetes,
			Image:                "gastown-polecat:dev",
			KubernetesCacheClaim: cacheTrimClaimName,
			NodeSelector:         map[string]string{"gt.dev/pool": pool},
		}
	}

	claims, errs := cacheTrimClaims(map[string]agentconfig.SandboxProfile{
		"k8s-a": profile("sandbox"),
		"k8s-b": profile("core"),
	})
	if len(claims) != 0 {
		t.Errorf("claims = %+v, want none: the profiles place the Job on two pools", claims)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "k8s-a") || !strings.Contains(errs[0].Error(), "k8s-b") {
		t.Errorf("errs = %v, want the two profiles that disagree named", errs)
	}

	// The same pool under both profiles is one placement, and the claim is
	// trimmed.
	claims, errs = cacheTrimClaims(map[string]agentconfig.SandboxProfile{
		"k8s-a": profile("sandbox"),
		"k8s-b": profile("sandbox"),
	})
	if len(errs) != 0 {
		t.Errorf("errs = %v, want none: the profiles place the Job on one pool", errs)
	}
	if len(claims) != 1 || !maps.Equal(claims[0].NodeSelector, map[string]string{"gt.dev/pool": "sandbox"}) {
		t.Errorf("claims = %+v, want the claim carrying the shared pool", claims)
	}
}

// TestCacheTrimJobNameStaysAValidObjectName pins the one place a Job name is
// built: a short claim keeps its name, and a claim too long for the 63-character
// limit is shortened into a DNS-1123 label that still differs per claim, so two
// claims sharing a long prefix get two Jobs.
func TestCacheTrimJobNameStaysAValidObjectName(t *testing.T) {
	t.Parallel()
	label := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

	if got := cacheTrimJobName(cacheTrimClaimName); got != cacheTrimJob {
		t.Errorf("cacheTrimJobName(%q) = %q, want %q", cacheTrimClaimName, got, cacheTrimJob)
	}

	// Exactly at the limit: no shortening.
	longest := strings.Repeat("c", cacheTrimJobNameMax-len(cachevol.DefaultTrimJobName)-1)
	if got := cacheTrimJobName(longest); got != cachevol.DefaultTrimJobName+"-"+longest {
		t.Errorf("cacheTrimJobName of a claim that exactly fits = %q, want it unchanged", got)
	}
	// One character over: shortened, still a valid label of at most 63.
	over := longest + "c"
	got := cacheTrimJobName(over)
	if len(got) > cacheTrimJobNameMax || !label.MatchString(got) {
		t.Errorf("cacheTrimJobName(%d characters) = %q, want a DNS-1123 label of at most %d characters", len(over), got, cacheTrimJobNameMax)
	}
	// Two claims differing only in their last character get two names.
	neighbour := over[:len(over)-1] + "d"
	if cacheTrimJobName(neighbour) == got {
		t.Errorf("two long claims sharing a prefix share the Job name %q", got)
	}
	// The longest claim a DNS-1123 label allows.
	claim63 := strings.Repeat("e", 63)
	if name := cacheTrimJobName(claim63); len(name) > cacheTrimJobNameMax || !label.MatchString(name) {
		t.Errorf("cacheTrimJobName(%q) = %q, want a DNS-1123 label of at most %d characters", claim63, name, cacheTrimJobNameMax)
	}
}

// TestCacheTrimRefusesASettingItCannotActOn pins the two settings that stop a
// trim rather than narrowing it: a cap that is not a size would trim everything
// under a near-zero reading, and a claim name that is not a DNS-1123 label
// would reach kubectl as it was written. Such a name is refused, never
// normalized into the claim it resembles.
func TestCacheTrimRefusesASettingItCannotActOn(t *testing.T) {
	t.Parallel()
	if _, err := cacheTrimCap(func(string) string { return "ten gigabytes" }); err == nil {
		t.Error("cacheTrimCap read a setting that is not a size")
	}
	if _, err := cacheTrimCap(func(string) string { return "" }); err != nil {
		t.Errorf("cacheTrimCap refused the default cap: %v", err)
	}

	for _, claim := range []string{"Gt_Cache", "gt_cache", "Gt-Cache"} {
		_, errs := cacheTrimClaims(map[string]agentconfig.SandboxProfile{
			"bad-name": {
				Driver:               agentconfig.SandboxDriverKubernetes,
				Image:                "gastown-polecat:dev",
				KubernetesCacheClaim: claim,
			},
		})
		if len(errs) != 1 || !strings.Contains(errs[0].Error(), "DNS-1123") {
			t.Errorf("the claim %q: errs = %v, want it refused", claim, errs)
		}
	}
}

// TestCacheTrimParseListingRefusesACacheItDoesNotKnow pins the fail-closed
// decode: a cache name the helper should never print refuses the listing,
// because PlanTrim picks removals by the cache and a wrong one admits a module
// file to a trim while a sandbox builds.
func TestCacheTrimParseListingRefusesACacheItDoesNotKnow(t *testing.T) {
	t.Parallel()
	listing := `[{"path":"gocache/aa/a","cache":"elsewhere","size":1,"atime":"2026-10-29T01:00:00Z","mtime":"2026-10-29T01:00:00Z"}]`
	if _, err := cacheTrimParseListing([]byte(listing)); err == nil {
		t.Fatal("cacheTrimParseListing read a cache name it does not know")
	}
}

// TestCacheTrimUsesTheHelpersKubeconfig pins that every cluster call the trim
// makes carries the kubeconfig the helper picks for the process: the container's
// file in container mode and the host's file otherwise. kubectl reads the
// cluster through the --kubeconfig flag, so that is what the test reads.
func TestCacheTrimUsesTheHelpersKubeconfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		supervisor string
		rel        string
	}{
		{"container mode", agentconfig.SupervisorContainer, ".runtime/container/kubeconfig.yaml"},
		{"host", "", ".runtime/sbx-k8s/kubeconfig"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, nil)
			f.d.diskMaint.getenv = func(key string) string {
				switch key {
				case cacheTrimCapEnv:
					return "1KB"
				case agentconfig.ContainerModeEnv:
					return tt.supervisor
				}
				return ""
			}
			f.d.runDiskMaintenance()

			want := filepath.Join(f.d.config.TownRoot, filepath.FromSlash(tt.rel))
			calls := f.cli.recorded()
			if len(calls) == 0 {
				t.Fatal("the pass ran no cluster call")
			}
			for _, call := range calls {
				if len(call.args) < 2 || call.args[0] != "--kubeconfig" || call.args[1] != want {
					t.Errorf("cluster call %v does not name the kubeconfig %q", call.args, want)
				}
			}
		})
	}
}

// TestCacheTrimNamesTheContextOnEveryCall pins the prefix every cluster call
// carries: the kubeconfig the helper picks and the sandbox context, from the
// one shared helper, so no call relies on the kubeconfig's current-context
// (gt-gg9hv.24.6). It walks a full apply pass, so every verb the trim issues is
// in the table.
func TestCacheTrimNamesTheContextOnEveryCall(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, nil)
	f.d.runDiskMaintenance()

	kubeconfig := filepath.Join(f.d.config.TownRoot, filepath.FromSlash(diskMaintHostKubeconfig))
	wantPrefix := diskmaint.KubectlArgs(kubeconfig, diskMaintSandboxContext)
	calls := f.cli.recorded()
	if len(calls) == 0 {
		t.Fatal("the pass ran no cluster call")
	}
	for _, call := range calls {
		if len(call.args) < len(wantPrefix) || !slices.Equal(call.args[:len(wantPrefix)], wantPrefix) {
			t.Errorf("cluster call %v does not start with %v", call.args, wantPrefix)
		}
	}

	wantVerbs := []string{
		cacheTrimPreflightVerb,
		cacheTrimClaimsVerb,
		"get pods -n " + cacheTrimNamespace + " -o json",
		"exec -n " + cacheTrimNamespace + " sbx-a -c agent -- " + cachevol.HelperPath + " trim-list --root " + cacheTrimMountPath,
		"delete job " + cacheTrimJob + " -n " + cacheTrimNamespace + " --ignore-not-found --wait=true --timeout=60s",
		cacheTrimApplyVerb,
		"wait --for=condition=complete job/" + cacheTrimJob + " -n " + cacheTrimNamespace + " --timeout=120s",
		"delete configmap -n " + cacheTrimNamespace + " -l app.kubernetes.io/managed-by=gt,app.kubernetes.io/name=polecat-cache-trim",
	}
	got := f.verbs()
	for _, want := range wantVerbs {
		if !slices.Contains(got, want) {
			t.Errorf("the pass never ran %q; calls: %v", want, got)
		}
	}
}

// TestCacheTrimRefusesAKubeconfigWithNoContext pins the preflight: a kubeconfig
// that names a cluster but no sandbox context stops the pass after that one
// call, with the specific line, and trims nothing. kubectl would otherwise fall
// back to the file's current-context or to localhost:8080.
func TestCacheTrimRefusesAKubeconfigWithNoContext(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		cacheTrimPreflightVerb: {stdout: ""},
	})
	f.d.runDiskMaintenance()

	kubeconfig := filepath.Join(f.d.config.TownRoot, filepath.FromSlash(diskMaintHostKubeconfig))
	want := `disk_maintenance: cache trim: kubeconfig ` + kubeconfig +
		` names no context "` + diskMaintSandboxContext + `"; nothing trimmed`
	if logged := f.log.String(); !strings.Contains(logged, want) {
		t.Errorf("log = %q, want the line %q", logged, want)
	}
	if got := f.verbs(); len(got) != 1 || got[0] != cacheTrimPreflightVerb {
		t.Errorf("the pass ran %v, want the context preflight alone", got)
	}
	for _, call := range f.cli.recorded() {
		if strings.HasPrefix(cacheTrimVerb(call.args), "apply ") {
			t.Errorf("the pass applied a trim from a kubeconfig with no context: %q", cacheTrimVerb(call.args))
		}
	}
}

// TestDiskMaintSandboxContextEndToEnd runs the complete pass behind a cluster
// client that behaves like kubectl against a kubeconfig with no current-context:
// any call that does not name the sandbox context is refused with the fallback
// to localhost:8080, and a call that names it is answered. The pass must reach
// the listing step and apply the trim.
func TestDiskMaintSandboxContextEndToEnd(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, nil)
	inner := f.cli.answer
	f.cli.answer = func(c cliCall) cliReply {
		for i := 0; i+1 < len(c.args); i++ {
			if c.args[i] == "--context" && c.args[i+1] == diskMaintSandboxContext {
				return inner(c)
			}
		}
		return cliReply{
			stderr: "The connection to the server localhost:8080 was refused - did you specify the right host or port?",
			code:   1,
		}
	}
	f.d.runDiskMaintenance()

	exec := "exec -n " + cacheTrimNamespace + " sbx-a -c agent -- " + cachevol.HelperPath + " trim-list --root " + cacheTrimMountPath
	if _, ok := f.indexOf(exec); !ok {
		t.Fatalf("the pass never read the listing; calls: %v", f.verbs())
	}
	if applied := f.applied(t); len(applied) != 2 {
		t.Errorf("the pass applied %v, want the shard and the Job", applied)
	}
}

// TestCacheTrimFailureRetriesSoonAndLogsOnce pins the schedule: a run that
// cannot start records a retry fifteen minutes later rather than the full two
// hours, the same refusal is said once in the hour, and a run that reaches a
// verdict spends the full interval.
func TestCacheTrimFailureRetriesSoonAndLogsOnce(t *testing.T) {
	t.Parallel()
	overrides := map[string]cliReply{cacheTrimPreflightVerb: {stdout: ""}}
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, overrides)
	now := diskMaintNow
	f.d.diskMaint.now = func() time.Time { return now }
	trims := 0
	profiles := f.d.diskMaint.sandboxes
	f.d.diskMaint.sandboxes = func() (map[string]agentconfig.SandboxProfile, error) {
		trims++
		return profiles()
	}
	const refusal = `names no context "` + diskMaintSandboxContext + `"`

	f.d.runDiskMaintenance() // 04:45: the trim cannot start
	if trims != 1 {
		t.Fatalf("trimmed %d times at the first tick, want 1", trims)
	}
	if got := strings.Count(f.log.String(), refusal); got != 1 {
		t.Fatalf("logged the refusal %d times, want 1: %s", got, f.log.String())
	}

	now = now.Add(5 * time.Minute)
	f.d.runDiskMaintenance() // 04:50: a failed run is due in fifteen minutes, not now
	if trims != 1 {
		t.Fatalf("trimmed %d times five minutes after a failure, want 1: a retry is due at fifteen", trims)
	}

	now = now.Add(10 * time.Minute)
	f.d.runDiskMaintenance() // 05:00: the retry
	if trims != 2 {
		t.Fatalf("trimmed %d times fifteen minutes after a failure, want 2", trims)
	}

	now = now.Add(30 * time.Minute)
	f.d.runDiskMaintenance() // 05:30: the next retry, still inside the hour
	if trims != 3 {
		t.Fatalf("trimmed %d times at the second retry, want 3", trims)
	}
	if got := strings.Count(f.log.String(), refusal); got != 1 {
		t.Errorf("logged the same refusal %d times within the hour, want once: %s", got, f.log.String())
	}

	// The cluster recovers: a verdict spends the full interval, so the next run
	// is due two hours later.
	delete(overrides, cacheTrimPreflightVerb)
	now = now.Add(15 * time.Minute)
	f.d.runDiskMaintenance() // 05:45: the retry after the last failure
	if trims != 4 {
		t.Fatalf("trimmed %d times at the retry after the cluster recovered, want 4", trims)
	}

	now = now.Add(time.Hour + 59*time.Minute)
	f.d.runDiskMaintenance()
	if trims != 4 {
		t.Fatalf("trimmed %d times a minute short of the interval, want 4", trims)
	}
	now = now.Add(time.Minute)
	f.d.runDiskMaintenance()
	if trims != 5 {
		t.Fatalf("trimmed %d times two hours after the verdict, want 5", trims)
	}
}

// TestCacheTrimRefusesAnUnreadableKubeconfig pins the preflight's other
// refusal: a kubeconfig kubectl cannot read stops the pass after that one call
// with a line naming the read, and trims nothing. It is a failure, not a
// verdict, so the patrol retries it soon.
func TestCacheTrimRefusesAnUnreadableKubeconfig(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		cacheTrimPreflightVerb: {stderr: "error: no configuration has been provided", code: 2},
	})
	f.d.runDiskMaintenance()

	if logged := f.log.String(); !strings.Contains(logged, "the kubeconfig could not be read") {
		t.Errorf("log = %q, want the preflight's read failure named", logged)
	}
	if got := f.verbs(); len(got) != 1 || got[0] != cacheTrimPreflightVerb {
		t.Errorf("the pass ran %v, want the context preflight alone", got)
	}
	for _, call := range f.cli.recorded() {
		if strings.HasPrefix(cacheTrimVerb(call.args), "apply ") {
			t.Errorf("the pass applied a trim from a kubeconfig it could not read: %q", cacheTrimVerb(call.args))
		}
	}
}

// applyCalls returns the pass's kubectl apply calls, in call order.
func (f *cacheTrimFixture) applyCalls() []cliCall {
	var out []cliCall
	for _, call := range f.cli.recorded() {
		if strings.HasPrefix(cacheTrimVerb(call.args), "apply ") {
			out = append(out, call)
		}
	}
	return out
}

// listReads is how many times the pass read the claim's listing, one read per
// trim it started.
func (f *cacheTrimFixture) listReads() int {
	n := 0
	for _, verb := range f.verbs() {
		if verb == cacheTrimListVerb {
			n++
		}
	}
	return n
}

// cacheTrimLargeListingJSON is a listing of paths, each long enough that its
// shard ConfigMap document exceeds the 262144-byte metadata limit on its own:
// one path per shard, every shard over the limit a client-side apply's
// last-applied annotation would break (gt-gg9hv.40).
func cacheTrimLargeListingJSON(shards int) string {
	// One byte past the shard bound in the path text, so the encoded entry
	// takes a shard of its own and its ConfigMap carries the file list past
	// the metadata limit.
	pathText := strings.Repeat("a", cachevol.TrimShardBytes+1)
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < shards; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b,
			`{"path":"gocache/%03d%s","cache":"build","size":%d,"atime":"2026-10-29T01:%02d:00Z","mtime":"2026-10-29T01:%02d:00Z"}`,
			i, pathText, cachevol.TrimShardBytes, i, i)
	}
	b.WriteByte(']')
	return b.String()
}

// cacheTrimClaimRouteListing is this bead's own listing, apart from the shared
// one above (whose minute field holds only the sixty entries the other tests
// use): shards entries past the shard ceiling, the minute wrapping so a listing
// of more than 59 entries still names a time the decoder reads, and pad extra
// characters on the first path so a test can put the writer's room on the
// whole-KiB grid df -Pk reports by.
func cacheTrimClaimRouteListing(shards, pad int) string {
	pathText := strings.Repeat("a", cachevol.TrimShardBytes+1)
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < shards; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		extra := ""
		if i == 0 {
			extra = strings.Repeat("a", pad)
		}
		fmt.Fprintf(&b,
			`{"path":"gocache/%03d%s%s","cache":"build","size":%d,"atime":"2026-10-29T01:%02d:00Z","mtime":"2026-10-29T01:%02d:00Z"}`,
			i, pathText, extra, cachevol.TrimShardBytes, i%60, i%60)
	}
	b.WriteByte(']')
	return b.String()
}

// cacheTrimManifestWithLargeShards is the manifest the pass builds from a
// listing whose shard ConfigMaps each exceed the metadata limit.
func cacheTrimManifestWithLargeShards(t *testing.T, shards int) *cachevol.TrimManifest {
	t.Helper()
	listing, err := cacheTrimParseListing([]byte(cacheTrimLargeListingJSON(shards)))
	if err != nil {
		t.Fatalf("the large listing does not parse: %v", err)
	}
	m, err := cachevol.BuildTrimManifest(cachevol.TrimJobInput{
		Name:             cacheTrimJob,
		Namespace:        cacheTrimNamespace,
		Image:            "gastown-polecat:dev",
		ClaimName:        cacheTrimClaimName,
		Listing:          listing,
		CapBytes:         1024,
		SandboxesRunning: 1,
	})
	if err != nil {
		t.Fatalf("the large listing builds no trim: %v", err)
	}
	return m
}

// TestCacheTrimAppliesLargeShardsServerSide is the bead's gate: a trim whose
// shard ConfigMaps each exceed the 262144-byte metadata limit applies anyway,
// because every document goes through server-side apply, which writes no
// last-applied annotation. The fake kubectl refuses a client-side apply of an
// object that large as the API server does, so a client-side apply fails the
// pass instead of passing it (gt-gg9hv.40).
func TestCacheTrimAppliesLargeShardsServerSide(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		cacheTrimListVerb: {stdout: cacheTrimLargeListingJSON(60)},
	})
	f.d.runDiskMaintenance()

	applies := f.applyCalls()
	if len(applies) != 61 {
		t.Fatalf("the pass made %d applies, want the 60 shards and the Job; log: %s", len(applies), f.log.String())
	}
	large := 0
	for _, call := range applies {
		if len(call.stdin) > cacheTrimLastAppliedLimit {
			large++
		}
		if !slices.Contains(call.args, "--server-side") {
			t.Errorf("the apply of %d bytes ran client-side: %v", len(call.stdin), call.args)
		}
		if !slices.Contains(call.args, "--field-manager=gt-cache-trim") {
			t.Errorf("the apply of %d bytes names no trim field manager: %v", len(call.stdin), call.args)
		}
	}
	if large != 60 {
		t.Fatalf("%d of the applied documents exceed the %d-byte limit, want the 60 shards", large, cacheTrimLastAppliedLimit)
	}
	if logged := f.log.String(); !strings.Contains(logged, "cache trim "+cacheTrimClaimName+" in "+cacheTrimNamespace+" applied") {
		t.Errorf("the large trim did not reach a verdict: %s", logged)
	}
}

// TestCacheTrimApplyFailureNamesTheDocument pins the apply failure's line in the
// log an operator reads: it names the document's place in the manifest, the
// object it holds, and only the first line of kubectl's stderr, which for a
// shard can be followed by the file list itself (gt-gg9hv.40). It drives the
// whole pass, so the line is checked where cacheTrim writes it, not only in the
// error the apply returns.
func TestCacheTrimApplyFailureNamesTheDocument(t *testing.T) {
	t.Parallel()
	const (
		firstLine  = "The ConfigMap is invalid: metadata.annotations: Too long: may not be more than 262144 bytes"
		secondLine = "See 'kubectl apply --help' for more information."
	)
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		cacheTrimListVerb: {stdout: cacheTrimLargeListingJSON(60)},
	})
	inner := f.cli.answer
	applies := 0
	f.cli.answer = func(c cliCall) cliReply {
		if strings.HasPrefix(cacheTrimVerb(c.args), "apply ") {
			applies++
			if applies == 7 {
				return cliReply{stderr: firstLine + "\n" + secondLine, code: 1}
			}
		}
		return inner(c)
	}

	f.d.runDiskMaintenance()

	logged := f.log.String()
	for _, want := range []string{"the apply failed: applying document 7 of 61", "ConfigMap", cacheTrimShardName(6), firstLine} {
		if !strings.Contains(logged, want) {
			t.Errorf("the log does not name %q: %s", want, logged)
		}
	}
	if strings.Contains(logged, secondLine) {
		t.Errorf("the log carries stderr's second line: %s", logged)
	}
	if applies != 7 {
		t.Errorf("the pass made %d applies, want it to stop at the seventh", applies)
	}
}

// TestCacheTrimApplyDocumentLabel pins the object an apply failure names: the
// document's kind and name, and nothing when the document names no object. The
// manifest builder renders every document as an object, so the empty form costs
// a message detail, never the report of the failure itself (gt-gg9hv.40).
func TestCacheTrimApplyDocumentLabel(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		doc  string
		want string
	}{
		"a shard":  {doc: `{"kind":"ConfigMap","metadata":{"name":"` + cacheTrimShardName(0) + `"}}`, want: " (ConfigMap " + cacheTrimShardName(0) + ")"},
		"not JSON": {doc: "not a document", want: ""},
		"no kind":  {doc: `{"metadata":{"name":"` + cacheTrimShardName(0) + `"}}`, want: ""},
		"no name":  {doc: `{"kind":"ConfigMap","metadata":{}}`, want: ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := cacheTrimDocumentLabel([]byte(tc.doc)); got != tc.want {
				t.Errorf("cacheTrimDocumentLabel(%q) = %q, want %q", tc.doc, got, tc.want)
			}
		})
	}
}

// TestCacheTrimApplyFailureRetriesSoon pins the retry rule at the apply: a run
// that fails while applying records the short retry, so the next 5-minute tick
// at or after fifteen minutes tries again rather than waiting out the full
// interval; a run that reaches a verdict still spends the interval
// (gt-gg9hv.40, gt-gg9hv.24.6).
func TestCacheTrimApplyFailureRetriesSoon(t *testing.T) {
	t.Parallel()
	overrides := map[string]cliReply{
		cacheTrimApplyVerb: {stderr: "The server is unavailable", code: 1},
	}
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, overrides)
	now := diskMaintNow
	f.d.diskMaint.now = func() time.Time { return now }

	f.d.runDiskMaintenance() // 04:45: the trim fails at its first apply
	if got := f.listReads(); got != 1 {
		t.Fatalf("trimmed %d times at the first tick, want 1", got)
	}

	now = now.Add(5 * time.Minute)
	f.d.runDiskMaintenance() // 04:50: a failed run is due in fifteen minutes, not now
	if got := f.listReads(); got != 1 {
		t.Fatalf("trimmed %d times five minutes after a failed apply, want 1: a retry is due at fifteen", got)
	}

	now = now.Add(10 * time.Minute)
	f.d.runDiskMaintenance() // 05:00: the short retry, not the two-hour interval
	if got := f.listReads(); got != 2 {
		t.Fatalf("trimmed %d times fifteen minutes after a failed apply, want 2", got)
	}

	// The cluster recovers: the run reaches a verdict, which spends the full
	// interval rather than the retry.
	delete(overrides, cacheTrimApplyVerb)
	now = now.Add(15 * time.Minute)
	f.d.runDiskMaintenance() // 05:15: the retry after the last failure
	if got := f.listReads(); got != 3 {
		t.Fatalf("trimmed %d times at the retry after the cluster recovered, want 3", got)
	}

	now = now.Add(15 * time.Minute)
	f.d.runDiskMaintenance() // 05:30: fifteen minutes after the verdict, not due
	if got := f.listReads(); got != 3 {
		t.Fatalf("trimmed %d times fifteen minutes after a verdict, want 3: a verdict spends the interval", got)
	}
}

// cacheTrimDfReply is df -Pk's reply for a claim with free bytes of room: the
// header df -Pk prints and one data row, its Available column in KiB. The room
// is a whole number of KiB, as df reports it (gt-gg9hv.41).
func cacheTrimDfReply(t *testing.T, free int64) string {
	t.Helper()
	if free%1024 != 0 {
		t.Fatalf("cacheTrimDfReply(%d): df -Pk reports whole KiB", free)
	}
	return "Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
		fmt.Sprintf("overlay 1000000 1 %d 1%% /", free/1024)
}

// cacheTrimAppliedLine is the pass's applied line from the log.
func cacheTrimAppliedLine(t *testing.T, logged string) string {
	t.Helper()
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, "cache trim "+cacheTrimClaimName+" in "+cacheTrimNamespace+" applied") {
			return line
		}
	}
	t.Fatalf("the log carries no applied line: %s", logged)
	return ""
}

// cacheTrimRemovalListBytes is the removal list a listing plans, encoded as the
// claim-carried writer encodes it.
func cacheTrimRemovalListBytes(t *testing.T, listing string) int {
	t.Helper()
	entries, err := cacheTrimParseListing([]byte(listing))
	if err != nil {
		t.Fatalf("the listing does not parse: %v", err)
	}
	plan := cachevol.PlanTrim(entries, 1024, true)
	paths := make([]string, 0, len(plan.Removals))
	for _, removal := range plan.Removals {
		paths = append(paths, removal.Path)
	}
	data, err := json.Marshal(paths)
	if err != nil {
		t.Fatalf("encoding the removal list: %v", err)
	}
	return len(data)
}

// cacheTrimListingAcrossTheCeilingOnAKiBGrid is a listing whose removal list
// packs past the shard ceiling and whose WRITER ROOM (polecatimage.TrimListRoom
// of the encoded list: the list and its digest, each on disk twice) is a whole
// number of KiB, so a df -Pk reply can name exactly the room the writer needs
// and the fit, equal and no-fit cases are each one KiB apart. It returns the
// listing and that room.
func cacheTrimListingAcrossTheCeilingOnAKiBGrid(t *testing.T) (string, int64) {
	t.Helper()
	listing := cacheTrimClaimRouteListing(cachevol.TrimMaxShards+1, 0)
	room := polecatimage.TrimListRoom(cacheTrimRemovalListBytes(t, listing))
	for pad := 0; room%1024 != 0 && pad < 1024; pad++ {
		listing = cacheTrimClaimRouteListing(cachevol.TrimMaxShards+1, pad)
		room = polecatimage.TrimListRoom(cacheTrimRemovalListBytes(t, listing))
	}
	if room%1024 != 0 {
		t.Fatalf("the writer's room is %d bytes, which a df -Pk reply cannot name", room)
	}
	return listing, room
}

// TestCacheTrimCrossesTheShardCeilingByTheClaimRoute is the bead's gate: a
// removal list that packs into more than TrimMaxShards shard ConfigMaps takes
// the claim, and the pass applies the one Job that plans its list there, with
// no ConfigMap and no shard cleanup (gt-gg9hv.41).
//
// The Job is waited on even though the route has no shards to clean: the pass
// measures the claim once the Job has finished, and a reading taken while it
// runs would show the usage the trim is about to remove (gt-gg9hv.46).
func TestCacheTrimCrossesTheShardCeilingByTheClaimRoute(t *testing.T) {
	t.Parallel()
	listing, free := cacheTrimListingAcrossTheCeilingOnAKiBGrid(t)
	// The claim reports exactly the room the encoded removal list takes: the
	// equal case fits, since only a list larger than the claim is refused.
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		cacheTrimListVerb: {stdout: listing},
		cacheTrimFreeVerb: {stdout: cacheTrimDfReply(t, free)},
	})
	f.d.runDiskMaintenance()

	if applied := f.applied(t); !slices.Equal(applied, []string{"Job " + cacheTrimJob}) {
		t.Fatalf("the pass applied %v, want the one Job and no ConfigMap; log: %s", applied, f.log.String())
	}
	if applies := f.applyCalls(); len(applies) != 1 {
		t.Fatalf("the pass made %d applies, want the one Job", len(applies))
	}
	if logged := f.log.String(); !strings.Contains(logged, "one Job, which plans its list in the claim") {
		t.Errorf("the log does not name the claim route: %s", logged)
	}
	if _, ok := f.indexOf(cacheTrimFreeVerb); !ok {
		t.Errorf("the pass never asked the pod for the claim's free space; calls: %v", f.verbs())
	}
	wait := "wait --for=condition=complete job/" + cacheTrimJob + " -n " + cacheTrimNamespace + " --timeout=120s"
	if _, ok := f.indexOf(wait); !ok {
		t.Errorf("the claim route did not wait for its Job before measuring the claim; calls: %v", f.verbs())
	}
	for _, verb := range f.verbs() {
		if strings.HasPrefix(verb, "delete configmap ") && strings.Contains(verb, " -l ") {
			t.Errorf("the claim route ran a shard cleanup: %q", verb)
		}
	}

	// The Job plans and writes the list itself, under the cap and the sandbox
	// count the pass computed.
	pod := f.appliedTrimJob(t).Spec.Template.Spec
	if len(pod.InitContainers) != 1 {
		t.Fatalf("the applied Job runs %d init containers, want the list writer", len(pod.InitContainers))
	}
	writer := pod.InitContainers[0]
	if want := []string{cachevol.HelperPath, "trim-list-write"}; !slices.Equal(writer.Command, want) {
		t.Errorf("the init container runs %v, want the image's list writer %v", writer.Command, want)
	}
	if want := []string{"--root", cachevol.SeedCacheRoot, "--cap-bytes", "1024", "--sandboxes", "1"}; !slices.Equal(writer.Args, want) {
		t.Errorf("the list writer's args = %v, want the pass's cap and sandbox count %v", writer.Args, want)
	}
}

// TestCacheTrimLogsItsRoute pins the route on the applied line: a small list
// names the shards it took, and a list past the ceiling names the claim, so a
// manual reading of the log shows which route ran (gt-gg9hv.41).
func TestCacheTrimLogsItsRoute(t *testing.T) {
	t.Parallel()
	large, free := cacheTrimListingAcrossTheCeilingOnAKiBGrid(t)
	tests := []struct {
		name    string
		listing string
		free    int64
		want    string
	}{
		{
			"a small list takes the shards",
			cacheTrimListingJSON,
			0,
			"route=shards n=1",
		},
		{
			"a list past the ceiling takes the claim",
			large,
			free,
			"route=claim",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
				cacheTrimListVerb: {stdout: tc.listing},
				cacheTrimFreeVerb: {stdout: cacheTrimDfReply(t, tc.free)},
			})
			f.d.runDiskMaintenance()

			applied := cacheTrimAppliedLine(t, f.log.String())
			if !strings.Contains(applied, tc.want) {
				t.Errorf("the applied line %q does not name %q", applied, tc.want)
			}
		})
	}
}

// TestCacheTrimRefusesAListThatDoesNotFitTheClaim pins the claim route's size
// guard: a list larger than the free space the pod reports is refused before
// any apply, with both byte counts; a list smaller than the room, and one that
// exactly fills it, apply. A refused run records the short retry rather than
// the full interval (gt-gg9hv.41).
func TestCacheTrimRefusesAListThatDoesNotFitTheClaim(t *testing.T) {
	t.Parallel()
	listing, need := cacheTrimListingAcrossTheCeilingOnAKiBGrid(t)
	for _, tc := range cacheTrimFitCases(need) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cacheTrimCheckFitCase(t, listing, need, tc)
		})
	}

	// The refusal is a failed run: the retry is due in fifteen minutes, not at
	// the full interval.
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		cacheTrimListVerb: {stdout: listing},
		cacheTrimFreeVerb: {stdout: cacheTrimDfReply(t, need-1024)},
	})
	now := diskMaintNow
	f.d.diskMaint.now = func() time.Time { return now }

	f.d.runDiskMaintenance() // 04:45: the list does not fit the claim
	if got := f.listReads(); got != 1 {
		t.Fatalf("the pass read the listing %d times, want 1", got)
	}
	now = now.Add(5 * time.Minute)
	f.d.runDiskMaintenance() // 04:50: a failed run is due in fifteen minutes, not now
	if got := f.listReads(); got != 1 {
		t.Errorf("the pass retried %d times five minutes after a refusal, want 1: a retry is due at fifteen", got)
	}
	now = now.Add(10 * time.Minute)
	f.d.runDiskMaintenance() // 05:00: the short retry
	if got := f.listReads(); got != 2 {
		t.Errorf("the pass read the listing %d times fifteen minutes after a refusal, want 2", got)
	}
}

// cacheTrimFitCase is one room-versus-list case for the claim route's size
// guard: the room df reports, and whether the pass applies the trim.
type cacheTrimFitCase struct {
	name    string
	free    int64
	applies bool
}

// cacheTrimFitCases is the size guard's three cases, each a KiB apart because
// df -Pk reports whole KiB: a list smaller than the room and one that exactly
// fills it apply, and one KiB more than the room is refused.
func cacheTrimFitCases(need int64) []cacheTrimFitCase {
	return []cacheTrimFitCase{
		{"a list smaller than the claim's room fits", need + 1024, true},
		{"a list equal to the claim's room fits", need, true},
		{"a list larger than the claim's room is refused", need - 1024, false},
	}
}

// cacheTrimCheckFitCase runs one room-versus-list case: the claim route applies
// its one Job when the list fits, and otherwise makes no apply and logs the
// refusal with both byte counts.
func cacheTrimCheckFitCase(t *testing.T, listing string, need int64, tc cacheTrimFitCase) {
	t.Helper()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		cacheTrimListVerb: {stdout: listing},
		cacheTrimFreeVerb: {stdout: cacheTrimDfReply(t, tc.free)},
	})
	f.d.runDiskMaintenance()

	applies := len(f.applyCalls())
	if tc.applies {
		if applies != 1 {
			t.Errorf("the pass applied %d documents for a list the claim has room for, want the one Job: %s", applies, f.log.String())
		}
		if logged := f.log.String(); strings.Contains(logged, "does not fit") {
			t.Errorf("the pass refused a list the claim has room for: %s", logged)
		}
		return
	}
	if applies != 0 {
		t.Errorf("the pass applied %d documents for a list the claim cannot hold", applies)
	}
	want := fmt.Sprintf("cache trim %s in %s: the removal list (%d bytes) needs %d bytes of room to be written with its digest and does not fit the claim's free space (%d); nothing trimmed",
		cacheTrimClaimName, cacheTrimNamespace, cacheTrimRemovalListBytes(t, listing), need, tc.free)
	if logged := f.log.String(); !strings.Contains(logged, want) {
		t.Errorf("the log does not carry the refusal %q: %s", want, logged)
	}
}

// TestCacheTrimDfReadingIgnoresTheUsedColumn pins what the df reading takes
// from its row: the Available column only. A Used column that is not a whole
// number is not read and does not fail the reading, so the room-versus-list
// cases behave as with a whole-KiB Used -- a too-large list is refused and one
// that fits applies. While Used was still parsed (gt-hilhxl), such a row left
// the room unknown and the trim ran without the fit check.
func TestCacheTrimDfReadingIgnoresTheUsedColumn(t *testing.T) {
	t.Parallel()
	listing, need := cacheTrimListingAcrossTheCeilingOnAKiBGrid(t)
	for _, tc := range cacheTrimFitCases(need) {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reply := cliReply{stdout: "Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
				fmt.Sprintf("overlay 1000000 not-a-number %d 1%% /", tc.free/1024)}
			f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
				cacheTrimListVerb: {stdout: listing},
				cacheTrimFreeVerb: reply,
			})
			f.d.runDiskMaintenance()

			applies := len(f.applyCalls())
			if tc.applies {
				if applies != 1 {
					t.Errorf("the pass applied %d documents for a list the claim has room for, want the one Job: %s", applies, f.log.String())
				}
				if logged := f.log.String(); strings.Contains(logged, "free space unknown") {
					t.Errorf("a df Used column that is not a number left the room unknown: %s", logged)
				}
				return
			}
			if applies != 0 {
				t.Errorf("the pass applied %d documents for a list the claim cannot hold", applies)
			}
			want := fmt.Sprintf("does not fit the claim's free space (%d); nothing trimmed", tc.free)
			if logged := f.log.String(); !strings.Contains(logged, want) {
				t.Errorf("the log does not carry the refusal %q: %s", want, logged)
			}
		})
	}
}

// TestCacheTrimOldImageFreeSpace pins the rollout case the df read exists for.
// The listing is the bare array the cycle-2 image prints; df runs in the pod,
// never through the helper, whose dispatcher sends any first argument it does
// not know to a full plugin run. Each shape an older or broken image gives
// leaves the room unknown, and that is a DELIBERATE fail-open: the claim route
// still applies its one Job, the unknown line names the reason, and no fit
// refusal is made. The same line is held to once an hour. With a good df row
// the fit, equal and no-fit cases behave as the refusal box says (gt-gg9hv.41).
func TestCacheTrimOldImageFreeSpace(t *testing.T) {
	t.Parallel()
	listing, need := cacheTrimListingAcrossTheCeilingOnAKiBGrid(t)
	tests := []struct {
		name   string
		reply  cliReply
		reason string
	}{
		{
			"df is not in the pod",
			cliReply{stderr: "df: executable file not found in $PATH", code: 127},
			"exit status 127",
		},
		{
			"df prints something that is not its table",
			cliReply{stdout: "no room here\n"},
			"df printed 1 non-empty line(s), want the header and one data row",
		},
		{
			"df's header is not the one df -Pk prints",
			cliReply{stdout: "Filesystem 1024-blocks Used Available Use% Mounted on\noverlay 1000000 1 100 1% /\n"},
			"df printed an unexpected header",
		},
		{
			"df's data row has no Available column",
			cliReply{stdout: "Filesystem 1024-blocks Used Available Capacity Mounted on\noverlay 1000000 1\n"},
			"df's data row names no Available column",
		},
		{
			"df's Available column is not a number",
			cliReply{stdout: "Filesystem 1024-blocks Used Available Capacity Mounted on\noverlay 1000000 1 lots 1% /\n"},
			"df's Available column is not a whole number of KiB",
		},
		{
			"df's Available column is too large to read as bytes",
			cliReply{stdout: "Filesystem 1024-blocks Used Available Capacity Mounted on\noverlay 1000000 1 99999999999999999 1% /\n"},
			"df's Available column is too large to read as bytes",
		},
		{
			"df's room is negative",
			cliReply{stdout: "Filesystem 1024-blocks Used Available Capacity Mounted on\noverlay 1000000 1 -1 1% /\n"},
			"df names -1 KiB available",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
				cacheTrimListVerb: {stdout: listing},
				cacheTrimFreeVerb: tc.reply,
			})
			f.d.runDiskMaintenance()

			if applied := f.applied(t); !slices.Equal(applied, []string{"Job " + cacheTrimJob}) {
				t.Fatalf("the pass applied %v, want the one Job: an unknown free space does not stop the trim; log: %s", applied, f.log.String())
			}
			logged := f.log.String()
			if got := strings.Count(logged, "free space unknown: "); got != 1 {
				t.Errorf("the log says the free space is unknown %d times, want 1: %s", got, logged)
			}
			if !strings.Contains(logged, tc.reason) {
				t.Errorf("the unknown line does not name the reason %q: %s", tc.reason, logged)
			}
			if strings.Contains(logged, "does not fit") {
				t.Errorf("the pass refused a trim it could not size: %s", logged)
			}
		})
	}

	// A good df row: the fit, equal and no-fit cases each behave as the refusal
	// box says, so the guard is exercised through the same read the old image
	// leaves unanswerable.
	for _, tc := range cacheTrimFitCases(need) {
		t.Run("a good df row: "+tc.name, func(t *testing.T) {
			t.Parallel()
			cacheTrimCheckFitCase(t, listing, need, tc)
		})
	}

	// The unknown line is held to once an hour: the trim runs on its own
	// schedule, and the same line is said once rather than on every run.
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		cacheTrimListVerb: {stdout: listing},
		cacheTrimFreeVerb: {stderr: "df: executable file not found in $PATH", code: 127},
	})
	now := diskMaintNow
	f.d.diskMaint.now = func() time.Time { return now }
	const unknown = "free space unknown: "

	f.d.runDiskMaintenance() // 04:45
	if applies := f.applyCalls(); len(applies) != 1 {
		t.Fatalf("the pass applied %d documents, want the one Job", len(applies))
	}
	if got := strings.Count(f.log.String(), unknown); got != 1 {
		t.Fatalf("the log says the free space is unknown %d times, want 1: %s", got, f.log.String())
	}

	// A second trim inside the hour is made due; the same line is held back.
	now = now.Add(15 * time.Minute)
	f.d.diskMaintSchedule.record(diskMaintCacheTrimName, now.Add(-3*time.Hour))
	f.d.runDiskMaintenance() // 05:00
	if applies := f.applyCalls(); len(applies) != 2 {
		t.Fatalf("the pass applied %d documents at the second trim, want 2", len(applies))
	}
	if got := strings.Count(f.log.String(), unknown); got != 1 {
		t.Errorf("the log said the free space is unknown %d times inside the hour, want once: %s", got, f.log.String())
	}

	// An hour after the first line, it is said again.
	now = now.Add(50 * time.Minute)
	f.d.diskMaintSchedule.record(diskMaintCacheTrimName, now.Add(-3*time.Hour))
	f.d.runDiskMaintenance() // 05:50
	if applies := f.applyCalls(); len(applies) != 3 {
		t.Fatalf("the pass applied %d documents at the third trim, want 3", len(applies))
	}
	if got := strings.Count(f.log.String(), unknown); got != 2 {
		t.Errorf("the log said the free space is unknown %d times after the hour, want 2: %s", got, f.log.String())
	}
}

// cacheTrimHelperSubcommands are the first arguments the image's helper
// dispatches, read from internal/polecatimage/program.go runProgram at
// cf0d2c17b. The daemon must never pass a helper argument outside them: the
// shipped runProgram sends any other first argument to runMain, a full plugin
// run -- image refresh and cache work -- so an older image would not answer the
// daemon's ask (gt-gg9hv.41).
var cacheTrimHelperSubcommands = []string{
	"trim-cache",
	"trim-paths",
	"trim-list",
	"trim-list-write",
	"seed-clone",
	"seed-cache",
}

// cacheTrimHelperSubcommand returns the first argument a trim verb passes the
// image's helper, and whether the verb runs the helper at all.
func cacheTrimHelperSubcommand(verb string) (string, bool) {
	fields := strings.Fields(verb)
	for i, field := range fields {
		if field != cachevol.HelperPath {
			continue
		}
		if i+1 == len(fields) {
			return "", true
		}
		return fields[i+1], true
	}
	return "", false
}

// TestCacheTrimNeverSendsAnUnknownHelperArgument pins the rollout rule the df
// read exists for: every helper argv the pass builds, and every call site in
// the daemon that can build one, names a subcommand the image dispatches. A
// new subcommand fails here before an older image meets it and starts a plugin
// run (gt-gg9hv.41).
func TestCacheTrimNeverSendsAnUnknownHelperArgument(t *testing.T) {
	t.Parallel()
	large, need := cacheTrimListingAcrossTheCeilingOnAKiBGrid(t)

	// Every helper argv the pass actually builds, on both routes.
	for _, tc := range []struct {
		name    string
		listing string
		free    int64
	}{
		{"the shard route", cacheTrimListingJSON, 0},
		{"the claim route", large, need},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
				cacheTrimListVerb: {stdout: tc.listing},
				cacheTrimFreeVerb: {stdout: cacheTrimDfReply(t, tc.free)},
			})
			f.d.runDiskMaintenance()

			helperCalls := 0
			for _, verb := range f.verbs() {
				sub, ok := cacheTrimHelperSubcommand(verb)
				if !ok {
					continue
				}
				helperCalls++
				if !slices.Contains(cacheTrimHelperSubcommands, sub) {
					t.Errorf("the pass ran %q: %q is not a subcommand the image dispatches", verb, sub)
				}
			}
			if helperCalls == 0 {
				t.Errorf("the pass ran no helper at all; calls: %v", f.verbs())
			}
		})
	}

	// And no call site in the daemon can build another: the literal beside each
	// cachevol.HelperPath in the production source is one of the six.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	sites := 0
	var problems []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		args, err := cacheTrimHelperArgs(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, arg := range args {
			sites++
			if !slices.Contains(cacheTrimHelperSubcommands, arg) {
				problems = append(problems, name+": "+arg)
			}
		}
	}
	if sites == 0 {
		problems = append(problems, "no call in the daemon passes cachevol.HelperPath, so the pin checked nothing")
	}
	if len(problems) > 0 {
		t.Fatalf("helper arguments: %s", strings.Join(problems, "; "))
	}
}

// cacheTrimHelperArgs returns the literal each argument list in path passes the
// image's helper as its first argument, for every list naming
// cachevol.HelperPath. A helper named without a following literal, or outside an
// argument list, is reported as a problem rather than skipped.
func cacheTrimHelperArgs(path string) ([]string, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	var args []string
	w := &cacheTrimWalker{visit: func(n ast.Node, ancestors []ast.Node) {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || !cacheTrimIsHelperPath(sel) {
			return
		}
		elements := cacheTrimArgList(ancestors)
		for i, element := range elements {
			if element != sel {
				continue
			}
			if i+1 >= len(elements) {
				args = append(args, "<HelperPath ends the argument list>")
				return
			}
			lit, ok := elements[i+1].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				args = append(args, "<not a string literal>")
				return
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				args = append(args, "<unquotable literal>")
				return
			}
			args = append(args, value)
			return
		}
		args = append(args, "<HelperPath outside an argument list>")
	}}
	ast.Walk(w, file)
	return args, nil
}

// cacheTrimIsHelperPath reports whether sel names cachevol.HelperPath.
func cacheTrimIsHelperPath(sel *ast.SelectorExpr) bool {
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "cachevol" && sel.Sel.Name == "HelperPath"
}

// cacheTrimArgList returns the elements of the innermost call or composite
// literal above a node.
func cacheTrimArgList(ancestors []ast.Node) []ast.Expr {
	for i := len(ancestors) - 1; i >= 0; i-- {
		switch container := ancestors[i].(type) {
		case *ast.CallExpr:
			return container.Args
		case *ast.CompositeLit:
			return container.Elts
		}
	}
	return nil
}

// cacheTrimSmallApplies are the client-side kubectl applies in the daemon and
// the sweep runner whose object cannot grow with a file list, named by the
// package, the file and the expression whose document the call applies. The
// trim's own applies carry --server-side and are not listed (gt-gg9hv.40).
var cacheTrimSmallApplies = map[string]string{
	"internal/sweeprunner/k8s.go: repoClaim.JSON()":  "a PersistentVolumeClaim: an access mode and a storage request",
	"internal/sweeprunner/k8s.go: claim.JSON()":      "the run's output claim: the same fixed fields",
	"internal/sweeprunner/k8s.go: cacheClaim.JSON()": "the rig's cache claim: the same fixed fields",
	"internal/sweeprunner/k8s.go: job.JSON()":        "the sweep Job: its containers, mounts and command come from the Spec, which holds no file list",
	"internal/sweeprunner/k8s.go: reader.JSON()":     "the result reader pod: one sleep container and one mounted claim",
}

// TestCacheTrimNoOtherLargeClientSideApply pins every kubectl apply in the
// daemon and the sweep runner: each is server-side, or one of the reviewed
// small objects, which hold no file list and so cannot grow past the
// 262144-byte last-applied annotation limit. A new apply fails this test until
// its object is reviewed and named in cacheTrimSmallApplies (gt-gg9hv.40). The
// walk covers every directory under each package, so a .go file in a nested
// directory is audited like a top-level one.
func TestCacheTrimNoOtherLargeClientSideApply(t *testing.T) {
	t.Parallel()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..")

	seen := map[string]bool{}
	serverSide := 0
	var problems []string
	for _, pkg := range []string{"internal/daemon", "internal/sweeprunner"} {
		dir := filepath.Join(root, filepath.FromSlash(pkg))
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			sites, err := cacheTrimApplySites(path)
			if err != nil {
				t.Fatalf("parsing %s: %v", rel, err)
			}
			for _, site := range sites {
				if site.serverSide {
					serverSide++
					continue
				}
				key := pkg + "/" + rel + ": " + site.object
				if _, reviewed := cacheTrimSmallApplies[key]; reviewed {
					seen[key] = true
					continue
				}
				problems = append(problems, key+" is a client-side apply that is not a reviewed small object")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
	}
	for key := range cacheTrimSmallApplies {
		if !seen[key] {
			problems = append(problems, key+" is listed but no apply sends that document any more")
		}
	}
	if serverSide == 0 {
		problems = append(problems, "no apply in the daemon or the sweep runner is server-side")
	}
	if len(problems) > 0 {
		t.Fatalf("kubectl applies: %s", strings.Join(problems, "; "))
	}
}

// cacheTrimApplySite is one kubectl `apply` argv built in a file.
type cacheTrimApplySite struct {
	// object is the expression whose document the call applies, or
	// "<no document>" when the argv builder names none.
	object string
	// serverSide is whether the argv names --server-side.
	serverSide bool
}

// cacheTrimApplySites returns every kubectl apply argv in path, with the
// document it sends.
func cacheTrimApplySites(path string) ([]cacheTrimApplySite, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		return nil, err
	}
	text := func(n ast.Node) string {
		return string(src[fset.Position(n.Pos()).Offset:fset.Position(n.End()).Offset])
	}
	var sites []cacheTrimApplySite
	w := &cacheTrimWalker{visit: func(n ast.Node, ancestors []ast.Node) {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return
		}
		if value, err := strconv.Unquote(lit.Value); err != nil || value != "apply" {
			return
		}
		sites = append(sites, cacheTrimApplySiteAt(ancestors, text))
	}}
	ast.Walk(w, file)
	return sites, nil
}

// cacheTrimApplySiteAt describes the apply whose verb literal sits under
// ancestors: the argv is the innermost call or composite literal holding it,
// and a step beside that argv names the document it applies.
func cacheTrimApplySiteAt(ancestors []ast.Node, text func(ast.Node) string) cacheTrimApplySite {
	site := cacheTrimApplySite{object: "<no document>"}
	for i := len(ancestors) - 1; i >= 0; i-- {
		switch ancestors[i].(type) {
		case *ast.CallExpr, *ast.CompositeLit:
			site.serverSide = cacheTrimNamesServerSide(ancestors[i])
		default:
			continue
		}
		break
	}
	for i := len(ancestors) - 1; i >= 0; i-- {
		kv, ok := ancestors[i].(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Argv" {
			continue
		}
		if i == 0 {
			break
		}
		step, ok := ancestors[i-1].(*ast.CompositeLit)
		if !ok {
			break
		}
		for _, elt := range step.Elts {
			pair, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := pair.Key.(*ast.Ident); ok && k.Name == "Stdin" {
				site.object = text(pair.Value)
			}
		}
		break
	}
	return site
}

// cacheTrimNamesServerSide reports whether expr builds an argv holding the
// --server-side flag.
func cacheTrimNamesServerSide(expr ast.Node) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if value, err := strconv.Unquote(lit.Value); err == nil && value == "--server-side" {
			found = true
		}
		return true
	})
	return found
}

// cacheTrimWalker visits every node with the ancestors above it, so a string
// literal can be read in the argv it builds.
type cacheTrimWalker struct {
	path  []ast.Node
	visit func(n ast.Node, ancestors []ast.Node)
}

func (w *cacheTrimWalker) Visit(n ast.Node) ast.Visitor {
	if n == nil {
		w.path = w.path[:len(w.path)-1]
		return w
	}
	w.visit(n, w.path)
	w.path = append(w.path, n)
	return w
}

// cacheTrimWriterFiles is the claim as the list writer sees it: it takes every
// write and answers the room it was given.
type cacheTrimWriterFiles struct{ free int64 }

func (cacheTrimWriterFiles) MkdirAll(string, os.FileMode) error          { return nil }
func (cacheTrimWriterFiles) Remove(string) error                         { return nil }
func (cacheTrimWriterFiles) WriteFile(string, []byte, os.FileMode) error { return nil }
func (cacheTrimWriterFiles) Rename(string, string) error                 { return nil }
func (f cacheTrimWriterFiles) Free() (int64, error)                      { return f.free, nil }

// TestCacheTrimFitCheckAgreesWithTheWriter: the early fit check refuses exactly
// the rooms the list writer refuses. For the same removal list, every room from
// one KiB under the writer's requirement to one KiB over is judged by both, and
// they give the same answer: the equal case is one the WRITER accepts, and the
// case below it is one the writer refuses (gt-gg9hv.41).
func TestCacheTrimFitCheckAgreesWithTheWriter(t *testing.T) {
	t.Parallel()
	listing, room := cacheTrimListingAcrossTheCeilingOnAKiBGrid(t)
	entries, err := cacheTrimParseListing([]byte(listing))
	if err != nil {
		t.Fatalf("parsing the listing: %v", err)
	}
	plan := cachevol.PlanTrim(entries, 0, false)
	paths := make([]string, 0, len(plan.Removals))
	for _, removal := range plan.Removals {
		paths = append(paths, removal.Path)
	}
	for _, free := range []int64{room - 1024, room, room + 1024} {
		_, writeErr := polecatimage.WriteTrimList(paths, cacheTrimWriterFiles{free: free})
		fitErr := cacheTrimClaimFits(entries, 0, 0, free)
		if (writeErr == nil) != (fitErr == nil) {
			t.Errorf("room %d: the writer says %v, the fit check says %v; they must agree", free, writeErr, fitErr)
		}
		if free >= room && fitErr != nil {
			t.Errorf("room %d is what the writer needs, and the fit check refused it: %v", free, fitErr)
		}
		if free < room && fitErr == nil {
			t.Errorf("room %d is under what the writer needs, and the fit check accepted it", free)
		}
	}
}

// TestCacheTrimRefusalAndUnknownLinesHaveTheirOwnGate: the fit refusal and the
// free-space-unknown line are each held to one an hour under a key of their own,
// so a repeat of one is silent while the other is still said.
func TestCacheTrimRefusalAndUnknownLinesHaveTheirOwnGate(t *testing.T) {
	t.Parallel()
	listing, room := cacheTrimListingAcrossTheCeilingOnAKiBGrid(t)
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		cacheTrimListVerb: {stdout: listing},
		cacheTrimFreeVerb: {stdout: cacheTrimDfReply(t, room-1024)},
	})
	f.d.runDiskMaintenance() // the refusal is said
	if n := strings.Count(f.log.String(), "nothing trimmed"); n != 1 {
		t.Fatalf("refusal lines after the first pass = %d, want 1:\n%s", n, f.log.String())
	}
	f.d.cacheTrimLogKeyed("cache_trim_free_unknown:"+cacheTrimNamespace+"/"+cacheTrimClaimName, "unknown line said")
	if !strings.Contains(f.log.String(), "unknown line said") {
		t.Errorf("the unknown line was held back by the refusal's gate:\n%s", f.log.String())
	}
	f.d.cacheTrimLogKeyed("cache_trim_no_fit:"+cacheTrimNamespace+"/"+cacheTrimClaimName, "refusal said again")
	if strings.Contains(f.log.String(), "refusal said again") {
		t.Errorf("a second refusal inside the hour was logged:\n%s", f.log.String())
	}
}

// cacheTrimNoOpCap is the cap the no-op tests trim under: the plugin's default,
// so a claim each test fills reads as a figure the reader knows.
const cacheTrimNoOpCap = 10 << 30

// cacheTrimDfUsedReply is df -Pk's reply for a claim holding used bytes and
// leaving free: the header df -Pk prints and one data row, Used and Available
// each in whole KiB.
func cacheTrimDfUsedReply(t *testing.T, used, free int64) string {
	t.Helper()
	if used%1024 != 0 || free%1024 != 0 {
		t.Fatalf("cacheTrimDfUsedReply(%d, %d): df -Pk reports whole KiB", used, free)
	}
	return "Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
		fmt.Sprintf("overlay 1000000 %d %d 1%% /", used/1024, free/1024)
}

// cacheTrimNoOpListingJSON is a listing the 10GB cap has something to trim: the
// fixture's three build-cache files are tiny, and a cap they fit under leaves
// the Job nothing to remove, so no trim is applied to judge.
const cacheTrimNoOpListingJSON = `[` +
	`{"path":"gocache/aa/a","cache":"build","size":5368709120,"atime":"2026-10-29T01:00:00Z","mtime":"2026-10-29T01:00:00Z"},` +
	`{"path":"gocache/bb/b","cache":"build","size":5368709120,"atime":"2026-10-29T02:00:00Z","mtime":"2026-10-29T02:00:00Z"},` +
	`{"path":"gocache/cc/c","cache":"build","size":5368709120,"atime":"2026-10-29T03:00:00Z","mtime":"2026-10-29T03:00:00Z"}]`

// cacheTrimDuReply is du -sk's reply for a claim holding used bytes: the size
// in whole KiB and the path du measured.
func cacheTrimDuReply(t *testing.T, used int64) string {
	t.Helper()
	if used%1024 != 0 {
		t.Fatalf("cacheTrimDuReply(%d): du -sk reports whole KiB", used)
	}
	return fmt.Sprintf("%d\t%s\n", used/1024, cacheTrimMountPath)
}

// newCacheTrimNoOpFixture builds the fixture in mode under the plugin's 10GB
// cap, with a listing the cap has something to trim, answering the pass's du
// read -- the claim's size after the trim -- with reply.
func newCacheTrimNoOpFixture(t *testing.T, mode string, reply cliReply) *cacheTrimFixture {
	t.Helper()
	f := newCacheTrimFixture(t, mode, map[string]cliReply{
		cacheTrimListVerb: {stdout: cacheTrimNoOpListingJSON},
	})
	f.d.diskMaint.getenv = func(key string) string {
		if key == cacheTrimCapEnv {
			return "10GB"
		}
		return ""
	}
	f.setDuReply(reply)
	return f
}

// setDfReply answers the pass's df read with reply, in place of the fixture's
// default room.
func (f *cacheTrimFixture) setDfReply(reply cliReply) {
	inner := f.cli.answer
	f.cli.answer = func(c cliCall) cliReply {
		if strings.Contains(cacheTrimVerb(c.args), " df -Pk ") {
			return reply
		}
		return inner(c)
	}
}

// setDuReply answers the pass's du read -- the claim's size after a trim --
// with reply, in place of the fixture's default.
func (f *cacheTrimFixture) setDuReply(reply cliReply) {
	inner := f.cli.answer
	f.cli.answer = func(c cliCall) cliReply {
		if cacheTrimVerb(c.args) == cacheTrimDuVerb {
			return reply
		}
		return inner(c)
	}
}

// answerFrom makes reply the answer for every later call whose verb starts with
// prefix, so a test changes one kind of call between passes.
func (f *cacheTrimFixture) answerFrom(prefix string, reply cliReply) {
	inner := f.cli.answer
	f.cli.answer = func(c cliCall) cliReply {
		if strings.HasPrefix(cacheTrimVerb(c.args), prefix) {
			return reply
		}
		return inner(c)
	}
}

// trimDue puts the cache trim back in the due state, so a test can run a second
// pass after the first one spent the interval.
func (f *cacheTrimFixture) trimDue() {
	f.d.diskMaintSchedule.record(diskMaintCacheTrimName, diskMaintNow.Add(-3*time.Hour))
}

// noOpItems reads the trim's over-the-cap items through the attention source
// the daemon builds, so the store the trim writes is exercised through the
// collector that serves it to the queue.
func (f *cacheTrimFixture) noOpItems(t *testing.T) []attention.Item {
	t.Helper()
	src := f.d.attentionSources(diskMaintNow)
	items, err := src.collectCacheTrimNoOp(context.Background())
	if err != nil {
		t.Fatalf("collectCacheTrimNoOp: %v", err)
	}
	return items
}

// TestCacheTrimNoOpRaisesOneItemWhenTheClaimStaysOverTwiceTheCap is the bead's
// first box: a completed trim that leaves the claim above twice the cap raises
// one low item keyed by namespace and claim and naming both figures, a second
// pass raises no second item, and a claim under twice the cap raises none.
func TestCacheTrimNoOpRaisesOneItemWhenTheClaimStaysOverTwiceTheCap(t *testing.T) {
	t.Parallel()
	f := newCacheTrimNoOpFixture(t, agentconfig.DiskMaintenanceModeApply, cliReply{stdout: cacheTrimDuReply(t, 63<<30)})
	f.d.runDiskMaintenance()

	items := f.noOpItems(t)
	if len(items) != 1 {
		t.Fatalf("the collector yielded %d items, want one for a claim over the cap: %+v\nlog: %s", len(items), items, f.log.String())
	}
	it := items[0]
	if want := cacheTrimNoOpKey(cacheTrimNamespace, cacheTrimClaimName); it.Key != want {
		t.Errorf("item key = %q, want %q", it.Key, want)
	}
	if it.Kind != attention.KindCacheTrimNoOp {
		t.Errorf("item kind = %q, want %q", it.Kind, attention.KindCacheTrimNoOp)
	}
	if it.Severity != attention.SeverityLow {
		t.Errorf("item severity = %q, want %q", it.Severity, attention.SeverityLow)
	}
	for _, want := range []string{
		polecatimage.Human(63 << 30),
		polecatimage.Human(cacheTrimNoOpCap),
		cacheTrimClaimName,
	} {
		if !strings.Contains(it.Summary, want) {
			t.Errorf("item summary %q does not name %q", it.Summary, want)
		}
	}
	// The queue's own collector set raises the kind: a store no collector reads
	// would never put the item in front of anyone.
	if !slices.ContainsFunc(f.d.attentionSources(diskMaintNow).collectors(), func(c attentionCollector) bool {
		return c.kind == attention.KindCacheTrimNoOp
	}) {
		t.Errorf("no attention collector raises the %q kind", attention.KindCacheTrimNoOp)
	}

	// The next trim measures the same claim again and replaces the item rather
	// than adding a second: one claim is one item.
	f.trimDue()
	f.d.runDiskMaintenance()
	if got := f.noOpItems(t); len(got) != 1 {
		t.Errorf("the second pass yielded %d items, want the one: %+v", len(got), got)
	}
	if got := f.listReads(); got != 2 {
		t.Fatalf("the pass read the listing %d times, want 2", got)
	}

	// 19GB against a 10GB cap is under twice the cap, and no item is raised.
	under := newCacheTrimNoOpFixture(t, agentconfig.DiskMaintenanceModeApply, cliReply{stdout: cacheTrimDuReply(t, 19<<30)})
	under.d.runDiskMaintenance()
	if got := under.noOpItems(t); len(got) != 0 {
		t.Errorf("a claim at 19GB against a 10GB cap yielded %+v, want no item", got)
	}

	// Exactly twice the cap is at the boundary, not over it.
	at := newCacheTrimNoOpFixture(t, agentconfig.DiskMaintenanceModeApply, cliReply{stdout: cacheTrimDuReply(t, 2*cacheTrimNoOpCap)})
	at.d.runDiskMaintenance()
	if got := at.noOpItems(t); len(got) != 0 {
		t.Errorf("a claim at exactly twice the cap yielded %+v, want no item", got)
	}
}

// TestCacheTrimNoOpClearsWhenTheClaimComesDown is the bead's second box: after
// the item exists, a later completed trim that reads the claim at or under
// twice the cap drops it.
func TestCacheTrimNoOpClearsWhenTheClaimComesDown(t *testing.T) {
	t.Parallel()
	f := newCacheTrimNoOpFixture(t, agentconfig.DiskMaintenanceModeApply, cliReply{stdout: cacheTrimDuReply(t, 63<<30)})
	f.d.runDiskMaintenance()
	if got := f.noOpItems(t); len(got) != 1 {
		t.Fatalf("the first pass yielded %d items, want the one it raised: %+v", len(got), got)
	}

	// The next trim finds 12GB: under twice the 10GB cap, so the item goes.
	f.setDuReply(cliReply{stdout: cacheTrimDuReply(t, 12<<30)})
	f.trimDue()
	f.d.runDiskMaintenance()
	if got := f.noOpItems(t); len(got) != 0 {
		t.Errorf("a claim at 12GB against a 10GB cap yielded %+v, want the item dropped", got)
	}
}

// TestCacheTrimNoOpReadsTheClaimWithDu is the bead's first box: a completed trim
// is judged by du's size of the claim, not df's Used, which on a local-path
// claim is the whole node filesystem. df at 58GB used with du at 11GB against a
// 10GB cap raises nothing; du at 25GB raises the item naming 25GB.
func TestCacheTrimNoOpReadsTheClaimWithDu(t *testing.T) {
	t.Parallel()
	df := cliReply{stdout: cacheTrimDfUsedReply(t, 58<<30, 1<<30)}

	t.Run("du is under twice the cap", func(t *testing.T) {
		t.Parallel()
		f := newCacheTrimNoOpFixture(t, agentconfig.DiskMaintenanceModeApply, cliReply{stdout: cacheTrimDuReply(t, 11<<30)})
		f.setDfReply(df)
		f.d.runDiskMaintenance()
		if got := f.noOpItems(t); len(got) != 0 {
			t.Errorf("df at %s used and du at %s against a %s cap yielded %+v, want no item",
				polecatimage.Human(58<<30), polecatimage.Human(11<<30), polecatimage.Human(cacheTrimNoOpCap), got)
		}
	})

	t.Run("du is over twice the cap", func(t *testing.T) {
		t.Parallel()
		f := newCacheTrimNoOpFixture(t, agentconfig.DiskMaintenanceModeApply, cliReply{stdout: cacheTrimDuReply(t, 25<<30)})
		f.setDfReply(df)
		f.d.runDiskMaintenance()
		items := f.noOpItems(t)
		if len(items) != 1 {
			t.Fatalf("du at 25GB against a 10GB cap yielded %+v, want one item", items)
		}
		if want := polecatimage.Human(25 << 30); !strings.Contains(items[0].Summary, want) {
			t.Errorf("item summary %q does not name du's %q", items[0].Summary, want)
		}
		if stale := polecatimage.Human(58 << 30); strings.Contains(items[0].Summary, stale) {
			t.Errorf("item summary %q names df's %q, want du's figure", items[0].Summary, stale)
		}
	})
}

// TestCacheTrimNoOpNeverFailsATrim is the bead's third box and its second:
// a du that fails or prints anything but one whole-KiB line after the apply
// leaves the reading unknown, raises no item, and says why once an hour under a
// key of its own; a dry run and a refused trim raise nothing.
func TestCacheTrimNoOpNeverFailsATrim(t *testing.T) {
	t.Parallel()
	const measured = "the claim could not be measured after the trim"
	tests := []struct {
		name  string
		reply cliReply
		want  string
	}{
		{"du is not in the pod", cliReply{stderr: "du: executable file not found in $PATH", code: 127}, "exit status 127"},
		{"du prints nothing", cliReply{}, "du printed 0 non-empty line(s), want one"},
		{"du prints two lines", cliReply{stdout: "1\t/a\n2\t/b\n"}, "du printed 2 non-empty line(s), want one"},
		{"du's line names no path", cliReply{stdout: "1024\n"}, "du's line names no path"},
		{"du's size is not a number", cliReply{stdout: "lots\t" + cacheTrimMountPath + "\n"}, "du's size column is not a whole number of KiB"},
		{"du's size is negative", cliReply{stdout: "-1\t" + cacheTrimMountPath + "\n"}, "du names -1 KiB size"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newCacheTrimNoOpFixture(t, agentconfig.DiskMaintenanceModeApply, tc.reply)
			now := diskMaintNow
			f.d.diskMaint.now = func() time.Time { return now }
			f.d.runDiskMaintenance() // 04:45

			if got := f.noOpItems(t); len(got) != 0 {
				t.Errorf("an unreadable claim yielded %+v, want no item", got)
			}
			logged := f.log.String()
			if got := strings.Count(logged, measured); got != 1 {
				t.Errorf("the unreadable reading is logged %d times, want 1: %s", got, logged)
			}
			if !strings.Contains(logged, tc.want) {
				t.Errorf("the line does not name the reason %q: %s", tc.want, logged)
			}
			// The gate is the measurement's own: the fit check's key being
			// held back never hides this line, and this line never hides it.
			f.d.cacheTrimLogKeyed("cache_trim_free_unknown:"+cacheTrimNamespace+"/"+cacheTrimClaimName, "the fit's unknown line")
			if !strings.Contains(f.log.String(), "the fit's unknown line") {
				t.Errorf("the measurement's gate held back the fit check's line: %s", f.log.String())
			}
			if strings.Contains(logged, "the apply failed") {
				t.Errorf("an unreadable claim failed the trim: %s", logged)
			}

			// A verdict spends the full interval: fifteen minutes later nothing
			// runs, which is where a failure would have retried.
			now = now.Add(15 * time.Minute)
			f.d.runDiskMaintenance()
			if got := f.listReads(); got != 1 {
				t.Errorf("the pass trimmed %d times fifteen minutes after a verdict, want 1", got)
			}

			// A second trim inside the hour says the same line once: it has a
			// gate of its own.
			f.trimDue()
			f.d.runDiskMaintenance() // 05:00
			if got := f.listReads(); got != 2 {
				t.Fatalf("the forced second trim read the listing %d times, want 2", got)
			}
			if got := strings.Count(f.log.String(), measured); got != 1 {
				t.Errorf("the line is logged %d times inside the hour, want once: %s", got, f.log.String())
			}

			// Past the hour it is said again: the hold is a rate, not silence.
			now = now.Add(time.Hour)
			f.trimDue()
			f.d.runDiskMaintenance() // 06:00
			if got := strings.Count(f.log.String(), measured); got != 2 {
				t.Errorf("the line is logged %d times after an hour, want twice: %s", got, f.log.String())
			}
		})
	}

	// A dry run applies nothing, so there is nothing to judge and no item.
	dry := newCacheTrimNoOpFixture(t, "", cliReply{stdout: cacheTrimDuReply(t, 63<<30)})
	dry.d.patrolConfig = diskMaintEnabled("", agentconfig.DiskMaintenanceModeDryRun)
	dry.d.runDiskMaintenance()
	if got := dry.applyCalls(); len(got) != 0 {
		t.Fatalf("the dry run applied %d documents", len(got))
	}
	if got := dry.noOpItems(t); len(got) != 0 {
		t.Errorf("a dry run yielded %+v, want no item", got)
	}

	// A trim refused before its apply is not measured either: no running pod
	// mounts the claim, and the lister Job's log does not parse, so the listing
	// is refused, no trim is applied, and there is no reading to judge.
	refused := newCacheTrimNoOpFixture(t, agentconfig.DiskMaintenanceModeApply, cliReply{stdout: cacheTrimDuReply(t, 63<<30)})
	inner := refused.cli.answer
	refused.cli.answer = func(c cliCall) cliReply {
		switch verb := cacheTrimVerb(c.args); {
		case strings.HasPrefix(verb, "get pods "):
			return cliReply{stdout: `{"items":[]}`}
		case strings.HasPrefix(verb, "logs job/"+cachevol.DefaultListJobName+"-"):
			return cliReply{stdout: "trim-list: no listing"}
		}
		return inner(c)
	}
	refused.d.runDiskMaintenance()
	for _, doc := range refused.applied(t) {
		if doc != "Job "+cacheTrimListJob {
			t.Fatalf("the refused trim applied %q; applied %v", doc, refused.applied(t))
		}
	}
	if got := refused.noOpItems(t); len(got) != 0 {
		t.Errorf("a refused trim yielded %+v, want no item", got)
	}

	// A failed apply raises nothing: the trim never ran, so the claim was not
	// left over the cap by it.
	failed := newCacheTrimNoOpFixture(t, agentconfig.DiskMaintenanceModeApply, cliReply{stdout: cacheTrimDuReply(t, 63<<30)})
	failedInner := failed.cli.answer
	failed.cli.answer = func(c cliCall) cliReply {
		if strings.HasPrefix(cacheTrimVerb(c.args), "apply ") {
			return cliReply{stderr: "Error from server: the server does not allow this", code: 1}
		}
		return failedInner(c)
	}
	failed.d.runDiskMaintenance()
	if got := failed.noOpItems(t); len(got) != 0 {
		t.Errorf("a failed apply yielded %+v, want no item", got)
	}

	// A Job still running when the wait ends is not measured: the claim holds
	// what the Job is removing, so a reading then would raise a false item.
	unfinished := newCacheTrimNoOpFixture(t, agentconfig.DiskMaintenanceModeApply, cliReply{stdout: cacheTrimDuReply(t, 63<<30)})
	unfinishedInner := unfinished.cli.answer
	unfinished.cli.answer = func(c cliCall) cliReply {
		if strings.HasPrefix(cacheTrimVerb(c.args), "wait ") {
			return cliReply{stderr: "error: timed out waiting for the condition", code: 1}
		}
		return unfinishedInner(c)
	}
	unfinished.d.runDiskMaintenance()
	if got := unfinished.noOpItems(t); len(got) != 0 {
		t.Errorf("a Job that did not finish yielded %+v, want no item", got)
	}
}

// TestCacheTrimNoOpClearsWhenTheClaimVanishes is the bead's first box: an item a
// completed trim raised goes when a later pass finds the claim gone -- the claim
// listing names no claim of that name, or no pod holds it -- while a pass whose
// cluster call cannot tell leaves it.
func TestCacheTrimNoOpClearsWhenTheClaimVanishes(t *testing.T) {
	t.Parallel()
	const overTheCap = 63 << 30
	failWith := cliReply{stderr: "Error from server (Timeout): the server did not respond", code: 1}
	// raised runs a trim that finishes with the claim over twice the cap, the
	// reading the item is raised from.
	raised := func(t *testing.T) *cacheTrimFixture {
		t.Helper()
		f := newCacheTrimNoOpFixture(t, agentconfig.DiskMaintenanceModeApply, cliReply{stdout: cacheTrimDuReply(t, overTheCap)})
		f.d.runDiskMaintenance()
		if got := f.noOpItems(t); len(got) != 1 {
			t.Fatalf("the first pass yielded %d items, want the one it raised: %+v\nlog: %s", len(got), got, f.log.String())
		}
		return f
	}

	t.Run("the claim listing names no claim", func(t *testing.T) {
		t.Parallel()
		f := raised(t)
		f.answerFrom(cacheTrimClaimsVerb, cliReply{stdout: `{"items":[]}`})
		f.trimDue()
		f.d.runDiskMaintenance()
		if got := f.noOpItems(t); len(got) != 0 {
			t.Errorf("a pass that found the claim gone yielded %+v, want no item", got)
		}
	})

	// With no pod the claim is read through the lister Job and trimmed, but a
	// measure needs a running pod, so nothing reads the claim's size afterwards
	// and the item the earlier measure raised is left as it was.
	t.Run("no pod holds the claim", func(t *testing.T) {
		t.Parallel()
		f := raised(t)
		f.answerFrom("get pods ", cliReply{stdout: `{"items":[]}`})
		f.trimDue()
		f.d.runDiskMaintenance()
		if !slices.Contains(f.applied(t), "Job "+cacheTrimListJob) {
			t.Errorf("a pass that found no pod holding the claim did not read it through the lister Job; applied %v", f.applied(t))
		}
		if got := f.noOpItems(t); len(got) != 1 {
			t.Errorf("a pass that found no pod holding the claim yielded %+v, want the item left as it was", got)
		}
	})

	// A call that fails cannot tell whether the claim is there, so the record is
	// left as it was: the item survives both the claim listing's failure and the
	// pod listing's.
	for _, tc := range []struct {
		name    string
		prefix  string
		survive string
	}{
		{"the claim listing fails", cacheTrimClaimsVerb, "the cluster's claims could not be read"},
		{"the pod listing fails", "get pods ", "the pods that mount it could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := raised(t)
			f.answerFrom(tc.prefix, failWith)
			f.trimDue()
			f.d.runDiskMaintenance()
			if got := f.noOpItems(t); len(got) != 1 {
				t.Errorf("a pass whose cluster call failed yielded %+v, want the item left as it was", got)
			}
			if logged := f.log.String(); !strings.Contains(logged, tc.survive) {
				t.Errorf("the failed pass does not say %q: %s", tc.survive, logged)
			}
		})
	}
}

// TestCacheTrimNoOpClearsWhenNoProfileMountsTheClaim pins the claim a profile
// drops: no pass reaches a claim no profile mounts, so its item is cleared from
// the profiles the pass read. A profile read that fails clears nothing.
func TestCacheTrimNoOpClearsWhenNoProfileMountsTheClaim(t *testing.T) {
	t.Parallel()
	raised := func(t *testing.T) *cacheTrimFixture {
		t.Helper()
		f := newCacheTrimNoOpFixture(t, agentconfig.DiskMaintenanceModeApply, cliReply{stdout: cacheTrimDuReply(t, 63<<30)})
		f.d.runDiskMaintenance()
		if got := f.noOpItems(t); len(got) != 1 {
			t.Fatalf("the first pass yielded %d items, want the one it raised: %+v\nlog: %s", len(got), got, f.log.String())
		}
		return f
	}

	t.Run("the profiles no longer mount the claim", func(t *testing.T) {
		t.Parallel()
		f := raised(t)
		f.d.diskMaint.sandboxes = func() (map[string]agentconfig.SandboxProfile, error) {
			return map[string]agentconfig.SandboxProfile{}, nil
		}
		f.trimDue()
		f.d.runDiskMaintenance()
		if got := f.noOpItems(t); len(got) != 0 {
			t.Errorf("a pass whose profiles drop the claim yielded %+v, want no item", got)
		}
	})

	t.Run("the profiles cannot be read", func(t *testing.T) {
		t.Parallel()
		f := raised(t)
		f.d.diskMaint.sandboxes = func() (map[string]agentconfig.SandboxProfile, error) {
			return nil, errors.New("profiles unavailable")
		}
		f.trimDue()
		f.d.runDiskMaintenance()
		if got := f.noOpItems(t); len(got) != 1 {
			t.Errorf("a pass that could not read the profiles yielded %+v, want the item left as it was", got)
		}
	})
}

// TestCacheTrimNoOpKeepsItemWhileAPodHoldsTheClaimWithoutRunning pins the branch
// that keeps an item: a pod holds the claim but none is running, so the claim is
// still there and a later trim can measure it.
func TestCacheTrimNoOpKeepsItemWhileAPodHoldsTheClaimWithoutRunning(t *testing.T) {
	t.Parallel()
	f := newCacheTrimNoOpFixture(t, agentconfig.DiskMaintenanceModeApply, cliReply{stdout: cacheTrimDuReply(t, 63<<30)})
	f.d.runDiskMaintenance()
	if got := f.noOpItems(t); len(got) != 1 {
		t.Fatalf("the first pass yielded %d items, want the one it raised: %+v\nlog: %s", len(got), got, f.log.String())
	}

	f.answerFrom("get pods ", cliReply{stdout: `{"items":[` + cacheTrimPodJSON("sbx-b", "Pending", cacheTrimClaimName) + `]}`})
	f.trimDue()
	f.d.runDiskMaintenance()
	if got := f.noOpItems(t); len(got) != 1 {
		t.Errorf("a pass whose only holder is pending yielded %+v, want the item kept", got)
	}
	if !slices.Contains(f.applied(t), "Job "+cacheTrimListJob) {
		t.Errorf("the pass did not read the pending holder's claim through the lister Job; applied %v", f.applied(t))
	}
}

// TestCacheTrimNoOpSummaryAllowsForOpenSandboxPaths is the bead's second box:
// the item's summary keeps the used and cap figures and says open sandbox paths
// can explain a claim still over the threshold, since the reading is the whole
// claim rather than only what the trim left.
func TestCacheTrimNoOpSummaryAllowsForOpenSandboxPaths(t *testing.T) {
	t.Parallel()
	f := newCacheTrimNoOpFixture(t, agentconfig.DiskMaintenanceModeApply, cliReply{stdout: cacheTrimDuReply(t, 63<<30)})
	f.d.runDiskMaintenance()

	items := f.noOpItems(t)
	if len(items) != 1 {
		t.Fatalf("the collector yielded %d items, want one for a claim over the cap: %+v\nlog: %s", len(items), items, f.log.String())
	}
	for _, want := range []string{
		polecatimage.Human(63 << 30),
		polecatimage.Human(cacheTrimNoOpCap),
		"open sandbox paths can explain this; the claim is read whole, not just what the trim left",
	} {
		if !strings.Contains(items[0].Summary, want) {
			t.Errorf("item summary %q does not name %q", items[0].Summary, want)
		}
	}
}
