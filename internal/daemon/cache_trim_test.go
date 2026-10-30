package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"slices"
	"strings"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/disks"
	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

// cacheTrimClaimName is the claim the fixtures mount.
const cacheTrimClaimName = "gt-shared-cache"

// cacheTrimJobName is the Job the pass names after that claim.
const cacheTrimJobName = "gt-cache-trim-" + cacheTrimClaimName

// cacheTrimPodsJSON is one running sandbox pod that mounts the claim, the pod
// a listing is read through and PlanTrim's sandbox count.
const cacheTrimPodsJSON = `{"items":[{"metadata":{"name":"sbx-a","namespace":"openshell"},` +
	`"spec":{"containers":[{"name":"agent","volumeMounts":[{"name":"gt-cache","mountPath":"/sandbox/.cache"}]}],` +
	`"volumes":[{"name":"gt-cache","persistentVolumeClaim":{"claimName":"gt-shared-cache"}}]},` +
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
	`{"kind":"Job","metadata":{"name":"gt-cache-trim-gt-shared-cache","namespace":"openshell",` +
	`"labels":{"app.kubernetes.io/name":"polecat-cache-trim","app.kubernetes.io/managed-by":"gt"}}},` +
	`{"kind":"ConfigMap","metadata":{"name":"gt-cache-trim-gt-shared-cache-7","namespace":"openshell",` +
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

// TestDiskMaintenanceAppliesTheShardsBeforeTheJob is the bead's gate: the
// patrol's own pass reaches cachevol.BuildTrimManifest and applies the
// manifest's documents in order, the shard ConfigMaps before the Job that
// mounts them, and the shards a finished Job read are deleted after it.
func TestDiskMaintenanceAppliesTheShardsBeforeTheJob(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, nil)
	f.d.runDiskMaintenance()

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
	want := []string{
		"ConfigMap " + cacheTrimShardName(0),
		"Job " + cacheTrimJobName,
	}
	if !slices.Equal(applied, want) {
		t.Fatalf("the pass applied %v, want the shards before the Job %v", applied, want)
	}

	// The stale Job is deleted before anything is applied: a completed Job's
	// pod never runs again, and its template cannot be applied over.
	deleteJob, ok := f.indexOf("delete job " + cacheTrimJobName + " -n openshell --ignore-not-found")
	if !ok {
		t.Fatalf("the pass did not delete the earlier run's Job; calls: %v", f.verbs())
	}
	firstApply, _ := f.indexOf("apply -f -")
	if deleteJob > firstApply {
		t.Errorf("the stale Job was deleted after the apply; calls: %v", f.verbs())
	}
	// The shards are deleted once the Job has finished, so a pod never mounts
	// one that is gone.
	wait, ok := f.indexOf("wait --for=condition=complete job/" + cacheTrimJobName + " -n openshell --timeout=120s")
	if !ok {
		t.Fatalf("the pass did not wait for the Job; calls: %v", f.verbs())
	}
	shards, ok := f.indexOf("delete configmap -n openshell -l app.kubernetes.io/managed-by=gt,app.kubernetes.io/name=polecat-cache-trim")
	if !ok {
		t.Fatalf("the pass left the Job's shards behind; calls: %v", f.verbs())
	}
	if shards < wait {
		t.Errorf("the shards were deleted before the Job finished; calls: %v", f.verbs())
	}
}

// cacheTrimShardName is the shard ConfigMap a manifest of the fixture's list
// names at index.
func cacheTrimShardName(index int) string {
	return cachevol.TrimShardName(cacheTrimJobName, index)
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
	if !strings.Contains(logged, "cache trim "+cacheTrimClaimName+" dry_run") {
		t.Errorf("the log does not name the dry run: %s", logged)
	}
	if !strings.Contains(logged, cacheTrimJobName) || !strings.Contains(logged, cacheTrimShardName(0)) {
		t.Errorf("the log does not name the manifest's objects: %s", logged)
	}
}

// TestDiskMaintenanceCacheTrimRefusesAnUnreadableListing pins the fail-closed
// read: a listing the pod cannot print trims nothing and says so, rather than
// building a manifest from a listing it never got.
func TestDiskMaintenanceCacheTrimRefusesAnUnreadableListing(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		"exec -n openshell sbx-a -c agent -- /usr/local/bin/polecat-image trim-list --root /sandbox/.cache": {
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
		"get pods -A -o json": {stdout: `{"items":[{"metadata":{"name":"sbx-b","namespace":"openshell"},` +
			`"spec":{"containers":[{"name":"agent","volumeMounts":[{"name":"gt-cache","mountPath":"/sandbox/.cache"}]}],` +
			`"volumes":[{"name":"gt-cache","persistentVolumeClaim":{"claimName":"gt-shared-cache"}}]},` +
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

// TestDiskMaintenanceCacheTrimLeavesShardsWhileTheJobRuns pins the cleanup's
// guard: a Job still running keeps its shards, since deleting a mount its pod
// is reading would fail the trim it is part of.
func TestDiskMaintenanceCacheTrimLeavesShardsWhileTheJobRuns(t *testing.T) {
	t.Parallel()
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		"wait --for=condition=complete job/" + cacheTrimJobName + " -n openshell --timeout=120s": {
			stderr: "error: timed out waiting for the condition", code: 1,
		},
	})
	f.d.runDiskMaintenance()

	if _, ok := f.indexOf("delete configmap -n openshell -l app.kubernetes.io/managed-by=gt,app.kubernetes.io/name=polecat-cache-trim"); ok {
		t.Errorf("the shards were deleted while the Job was still running; calls: %v", f.verbs())
	}
	if logged := f.log.String(); !strings.Contains(logged, "has not finished") {
		t.Errorf("the log does not say the Job has not finished: %s", logged)
	}
}

// TestDiskMaintenanceCacheTrimRefusesASecondClaimInOneNamespace pins the
// guard on the trim's shared labels: a town that mounts two claims has no
// labels to tell one trim's objects from the other's, so only the first claim's
// trim runs and the second says why it did not.
func TestDiskMaintenanceCacheTrimRefusesASecondClaimInOneNamespace(t *testing.T) {
	t.Parallel()
	const otherClaim = "gt-other-cache"
	f := newCacheTrimFixture(t, agentconfig.DiskMaintenanceModeApply, map[string]cliReply{
		"get pods -A -o json": {stdout: `{"items":[` +
			`{"metadata":{"name":"sbx-a","namespace":"openshell"},` +
			`"spec":{"containers":[{"name":"agent","volumeMounts":[{"name":"gt-cache","mountPath":"/sandbox/.cache"}]}],` +
			`"volumes":[{"name":"gt-cache","persistentVolumeClaim":{"claimName":"gt-shared-cache"}}]},` +
			`"status":{"phase":"Running"}},` +
			`{"metadata":{"name":"sbx-b","namespace":"openshell"},` +
			`"spec":{"containers":[{"name":"agent","volumeMounts":[{"name":"gt-cache","mountPath":"/sandbox/.cache"}]}],` +
			`"volumes":[{"name":"gt-cache","persistentVolumeClaim":{"claimName":"gt-other-cache"}}]},` +
			`"status":{"phase":"Running"}}]}`},
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

	applied := 0
	for _, verb := range f.verbs() {
		if strings.HasPrefix(verb, "apply -f -") {
			applied++
		}
	}
	// Only the first claim's trim applies: its shard and its Job.
	if applied != 2 {
		t.Errorf("the pass applied %d documents, want the first claim's two; calls: %v", applied, f.verbs())
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
		getPods    = "get pods -A -o json"
		execList   = "exec -n openshell sbx-a -c agent -- /usr/local/bin/polecat-image trim-list --root /sandbox/.cache"
		getObjects = "get job,configmap -n openshell -l app.kubernetes.io/managed-by=gt,app.kubernetes.io/name=polecat-cache-trim -o json"
		deleteJob  = "delete job " + cacheTrimJobName + " -n openshell --ignore-not-found"
		apply      = "apply -f -"
		deleteCM   = "delete configmap -n openshell -l app.kubernetes.io/managed-by=gt,app.kubernetes.io/name=polecat-cache-trim"
	)
	tests := []struct {
		name     string
		override map[string]cliReply
		want     string
	}{
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

// TestCacheTrimRefusesASettingItCannotActOn pins the two settings that stop a
// trim rather than narrowing it: a cap that is not a size would trim everything
// under a near-zero reading, and a claim name that is not a DNS-1123 label
// would reach kubectl as it was written.
func TestCacheTrimRefusesASettingItCannotActOn(t *testing.T) {
	t.Parallel()
	if _, err := cacheTrimCap(func(string) string { return "ten gigabytes" }); err == nil {
		t.Error("cacheTrimCap read a setting that is not a size")
	}
	if _, err := cacheTrimCap(func(string) string { return "" }); err != nil {
		t.Errorf("cacheTrimCap refused the default cap: %v", err)
	}

	_, errs := cacheTrimClaims(map[string]agentconfig.SandboxProfile{
		"bad-name": {
			Driver:               agentconfig.SandboxDriverKubernetes,
			Image:                "gastown-polecat:dev",
			KubernetesCacheClaim: "Gt_Cache",
		},
	})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "DNS-1123") {
		t.Errorf("errs = %v, want the claim name refused", errs)
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
