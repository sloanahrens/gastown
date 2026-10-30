package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/disks"
	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

const (
	// cacheTrimClaimName is the claim the fixtures mount.
	cacheTrimClaimName = "gt-shared-cache"
	// cacheTrimNamespace is the namespace the fixtures put that claim in.
	cacheTrimNamespace = "openshell"
	// cacheTrimJob is the Job the pass names after that claim.
	cacheTrimJob = cachevol.DefaultTrimJobName + "-" + cacheTrimClaimName
)

// cacheTrimClaimsJSON is the cluster's claim listing: the fixture's claim, in
// its namespace.
const cacheTrimClaimsJSON = `{"items":[{"metadata":{"name":"` + cacheTrimClaimName + `","namespace":"` + cacheTrimNamespace + `"}}]}`

// cacheTrimPodsJSON is one running sandbox pod that mounts the claim, the pod
// a listing is read through and PlanTrim's sandbox count.
const cacheTrimPodsJSON = `{"items":[{"metadata":{"name":"sbx-a","namespace":"` + cacheTrimNamespace + `"},` +
	`"spec":{"containers":[{"name":"agent","volumeMounts":[{"name":"gt-cache","mountPath":"/sandbox/.cache"}]}],` +
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
		verb := cacheTrimVerb(c.args)
		if reply, ok := overrides[verb]; ok {
			return reply
		}
		switch {
		case verb == cacheTrimClaimsVerb:
			return cliReply{stdout: cacheTrimClaimsJSON}
		case strings.HasPrefix(verb, "get pods "):
			return cliReply{stdout: cacheTrimPodsJSON}
		case strings.HasPrefix(verb, "exec "):
			return cliReply{stdout: cacheTrimListingJSON}
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

// setProfiles replaces the sandbox profiles the pass reads.
func (f *cacheTrimFixture) setProfiles(profiles map[string]agentconfig.SandboxProfile) {
	f.d.diskMaint.sandboxes = func() (map[string]agentconfig.SandboxProfile, error) { return profiles, nil }
}

// cacheTrimClaimsVerb is the pass's one cluster-wide claim read, the call that
// names the namespace every trim after it runs in.
const cacheTrimClaimsVerb = "get persistentvolumeclaim -A -o json"

// cacheTrimVerb is a kubectl call's verb and arguments, without the
// kubeconfig the daemon puts first.
func cacheTrimVerb(args []string) string {
	if len(args) < 3 || args[0] != "--kubeconfig" {
		return ""
	}
	return strings.Join(args[2:], " ")
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

// applied returns the documents the pass applied, as "Kind name" in apply order.
func (f *cacheTrimFixture) applied(t *testing.T) []string {
	t.Helper()
	var applied []string
	for _, call := range f.cli.recorded() {
		if !strings.HasPrefix(cacheTrimVerb(call.args), "apply -f -") {
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
	firstApply, _ := f.indexOf("apply -f - -n " + cacheTrimNamespace)
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
	if len(calls) == 0 {
		t.Fatal("the pass ran no kubectl call")
	}
	if calls[0] != cacheTrimClaimsVerb {
		t.Fatalf("the first kubectl call is %q, want the claim listing %q", calls[0], cacheTrimClaimsVerb)
	}
	for _, verb := range calls[1:] {
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
	// the other claim has no pod of its own, so its listing cannot be read.
	for _, verb := range f.verbs() {
		if strings.Contains(verb, otherNamespace) && !strings.HasPrefix(verb, "get pods ") {
			t.Errorf("the pass ran %q in the podless namespace", verb)
		}
	}
	if logged := f.log.String(); !strings.Contains(logged, "cache trim "+cacheTrimClaimName+" in "+otherNamespace+": no running pod mounts it") {
		t.Errorf("the log does not say the other claim had no pod of its own: %s", logged)
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
		"exec -n " + cacheTrimNamespace + " sbx-a -c agent -- " + cachevol.HelperPath + " trim-list --root /sandbox/.cache": {
			stderr: "trim-list: the claim at /sandbox/.cache: no such file", code: 3,
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

// TestDiskMaintenanceCacheTrimRefusesWhenNoPodMountsTheClaim pins the other
// unreadable listing: with no running pod that mounts the claim there is no
// listing to read, so the trim refuses instead of guessing one.
func TestDiskMaintenanceCacheTrimRefusesWhenNoPodMountsTheClaim(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		"get pods -n " + cacheTrimNamespace + " -o json": {stdout: `{"items":[{"metadata":{"name":"sbx-b","namespace":"` + cacheTrimNamespace + `"},` +
			`"spec":{"containers":[{"name":"agent","volumeMounts":[{"name":"gt-cache","mountPath":"/sandbox/.cache"}]}],` +
			`"volumes":[{"name":"gt-cache","persistentVolumeClaim":{"claimName":"` + cacheTrimClaimName + `"}}]},` +
			`"status":{"phase":"Pending"}}]}`},
	})
	f.d.runDiskMaintenance()

	for _, verb := range f.verbs() {
		if strings.HasPrefix(verb, "exec ") || strings.HasPrefix(verb, "apply ") {
			t.Errorf("the pass ran %q with no running pod that mounts the claim", verb)
		}
	}
	if logged := f.log.String(); !strings.Contains(logged, "no running pod mounts it") {
		t.Errorf("the log does not say why nothing was trimmed: %s", logged)
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

// cacheTrimPodJSON is one pod of a fixture's pod list, mounting claim.
func cacheTrimPodJSON(name, phase, claim string) string {
	return `{"metadata":{"name":"` + name + `","namespace":"` + cacheTrimNamespace + `"},` +
		`"spec":{"containers":[{"name":"agent","volumeMounts":[{"name":"gt-cache","mountPath":"/sandbox/.cache"}]}],` +
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
		execList   = "exec -n " + cacheTrimNamespace + " sbx-a -c agent -- " + cachevol.HelperPath + " trim-list --root /sandbox/.cache"
		getObjects = "get job,configmap -n " + cacheTrimNamespace + " -l app.kubernetes.io/managed-by=gt,app.kubernetes.io/name=polecat-cache-trim -o json"
		deleteJob  = "delete job " + cacheTrimJob + " -n " + cacheTrimNamespace + " --ignore-not-found --wait=true --timeout=60s"
		apply      = "apply -f - -n " + cacheTrimNamespace
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
		{"the pod list fails", map[string]cliReply{getPods: {stderr: "the server is unavailable", code: 1}},
			"the pods that mount it could not be read: kubectl"},
		{"the pod list is not JSON", map[string]cliReply{getPods: {stdout: "not json"}},
			"the pods that mount it could not be read: decoding the pod list"},
		{"the pod list names no items", map[string]cliReply{getPods: {stdout: `{}`}},
			"the pod list: the reply names no item list"},
		{"the listing is not JSON", map[string]cliReply{execList: {stdout: "not json"}},
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
	if !slices.Equal(claims, want) {
		t.Errorf("claims = %+v, want %+v", claims, want)
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "gt-other-cache") {
		t.Errorf("errs = %v, want the claim two profiles name with different images", errs)
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
