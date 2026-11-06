package sweeprunner

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The live-cluster golden listings are what the API printed on the control
// cluster, captured before the guards had a test that read them: the node's
// memory is 24556112Ki, the owners sit in metadata, and the protected
// namespaces hold two Succeeded restore pods at priority 0. Each fixture is named
// by one constant below, used both by the test that reads it and by
// goldenFixtures, so the two cannot drift apart (gt-gg9hv.49).
const goldenDir = "testdata/live-cluster"

const (
	capacityGolden = "capacity-listing.json"
	priorityGolden = "priority-listing.json"
)

// goldenFixtures is every listing a golden test reads. A .json file in
// testdata/live-cluster that is not named here is one no golden test reads, and
// TestGoldenFixturesAreAllRead fails it.
var goldenFixtures = []string{capacityGolden, priorityGolden}

// loadGoldenFixture reads a live-cluster listing a golden test consumes.
func loadGoldenFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(goldenDir, name))
	if err != nil {
		t.Fatalf("reading the live-cluster fixture %s: %v", name, err)
	}
	return data
}

// TestGoldenCapacityListing reads the node and pod listing the live cluster
// answers: the node is Ready with 12 CPU and 24556112Ki allocatable, its pods
// request 6210m and 14492Mi, and the two Succeeded restore pods count for
// nothing. The decision is the node's real free amount: a 4 CPU / 5 GiB Job
// needs 4000m beside the control plane's 2000m headroom and the node leaves
// 5790m, so it is declined by 210m; a 3 CPU / 5 GiB Job launches. Today's code
// refuses to read the Ki quantity at all, so the listing never reaches a
// decision (gt-gg9hv.49).
func TestGoldenCapacityListing(t *testing.T) {
	t.Parallel()
	nodes, err := ParseCapacity(loadGoldenFixture(t, capacityGolden))
	if err != nil {
		t.Fatalf("ParseCapacity on the live cluster's listing: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("the listing names %d nodes, want the one the live cluster printed: %+v", len(nodes), nodes)
	}
	want := NodeCapacity{
		Node:        "colima-gt-control",
		Allocatable: Amount{CPU: 12_000, Memory: 24_556_112 << 10},
		Requested:   Amount{CPU: 6_210, Memory: 14_492 << 20},
		Ready:       true,
	}
	if got := nodes[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("the live node = %+v, want %+v", got, want)
	}

	job := Amount{CPU: 4_000, Memory: 5 << 30}
	report, err := DecideCapacity(nodes, job, nil)
	var decline *NoRoomError
	if !errors.As(err, &decline) {
		t.Fatalf("DecideCapacity over the live node = %+v, %v; want the decline the node's free CPU makes", report, err)
	}
	const wantReason = "node colima-gt-control: free 5790m/9488Mi, job needs 4000m/5120Mi, headroom 2000m/2048Mi"
	if decline.Error() != wantReason {
		t.Errorf("the decline = %q, want %q", decline.Error(), wantReason)
	}

	// The same node launches a Job its free CPU holds beside the headroom: the
	// Ki quantity read, and nothing was declined for the listing's shape.
	smaller := Amount{CPU: 3_000, Memory: 5 << 30}
	if _, err := DecideCapacity(nodes, smaller, nil); err != nil {
		t.Errorf("DecideCapacity(%+v) = %v, want the live node to launch it", smaller, err)
	}
}

// TestGoldenPriorityGuard runs the priority guard over the pods the live
// cluster printed: the Forgejo pods at 1000000 are above the sweep's class, and
// the two Succeeded restore pods at priority 0 hold nothing the sweep could
// displace, so the listing passes. The same listing with one Forgejo pod moved
// to priority 0 declines naming that pod, so the pass is not the guard's
// default (gt-gg9hv.49).
func TestGoldenPriorityGuard(t *testing.T) {
	t.Parallel()
	listing := loadGoldenFixture(t, priorityGolden)
	guard := PriorityGuard(goldenSpec(), []string{"forgejo", "forgejo-trial"})
	if err := guard.Refuse(listing); err != nil {
		t.Fatalf("the live cluster's own pods refused the launch: %v", err)
	}

	altered := goldenListingWithPodPriority(t, listing, "forgejo", "forgejo-57658978cf-5nzvk", 0)
	decline := priorityDeclineOf(t, guard.Refuse(altered))
	const want = "pod forgejo/forgejo-57658978cf-5nzvk runs at priority 0"
	if !strings.Contains(decline.Line(), want) {
		t.Errorf("the decline = %q, want it to name %q", decline.Line(), want)
	}
}

// TestPriorityOwnersComeFromMetadata reads the live listing's Forgejo pod: its
// owners are the ReplicaSet in metadata.ownerReferences, and that ReplicaSet is
// the forgejo Deployment's rather than the forgejo-runner Deployment's whose
// name it also starts with. A pod with the old placement, owners under spec,
// is a shape the API never prints and parses no owner (gt-gg9hv.49).
func TestPriorityOwnersComeFromMetadata(t *testing.T) {
	t.Parallel()
	listing, err := readPriorityListing(loadGoldenFixture(t, priorityGolden))
	if err != nil {
		t.Fatalf("readPriorityListing on the live cluster's listing: %v", err)
	}
	var pod *listedPod
	for i := range listing.pods {
		if listing.pods[i].namespace == "forgejo" && listing.pods[i].name == "forgejo-57658978cf-5nzvk" {
			pod = &listing.pods[i]
		}
	}
	if pod == nil {
		t.Fatalf("the live listing names no forgejo/forgejo-57658978cf-5nzvk among %+v", listing.pods)
	}
	wantOwners := []ownerRef{{Kind: "ReplicaSet", Name: "forgejo-57658978cf"}}
	if !reflect.DeepEqual(pod.owners, wantOwners) {
		t.Errorf("owners = %+v, want %+v from metadata.ownerReferences", pod.owners, wantOwners)
	}
	if !pod.ownedBy("forgejo") {
		t.Error("the live Forgejo pod is not owned by deployment forgejo")
	}
	if pod.ownedBy("forgejo-runner") {
		t.Error("the live Forgejo pod is owned by the runner deployment whose name it only starts with")
	}

	const oldShape = `{"kind":"Pod","metadata":{"name":"forgejo-abc","namespace":"forgejo"},"spec":{"priority":1000000,"ownerReferences":[{"kind":"ReplicaSet","name":"forgejo-57658978cf"}]}}`
	old, err := readPriorityListing([]byte(oldShape))
	if err != nil {
		t.Fatalf("readPriorityListing on a pod with owners under spec: %v", err)
	}
	if len(old.pods) != 1 {
		t.Fatalf("the old-shape listing parsed %d pods, want 1", len(old.pods))
	}
	if len(old.pods[0].owners) != 0 || old.pods[0].ownedBy("forgejo") {
		t.Errorf("a pod with owners under spec parsed owners %+v, want none", old.pods[0].owners)
	}
}

// TestPodRequestHonoursPodLevelResources drives the scheduler's pod-level rule:
// for each resource a pod's own spec.resources.requests names, that figure plus
// the pod's overhead replaces the container arithmetic; a resource it does not
// name is counted as the containers ask. The live listing's pods name none, so
// they count as before (gt-gg9hv.49). The hand-built pod below is the shape the
// live listing has no example of.
func TestPodRequestHonoursPodLevelResources(t *testing.T) {
	t.Parallel()
	const listing = `{"items": [
	  {"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "16", "memory": "64Gi"}}},
	  {"kind": "Pod", "metadata": {"name": "pod-level-cpu"}, "spec": {
	    "nodeName": "gt-control",
	    "resources": {"requests": {"cpu": "6"}},
	    "overhead": {"cpu": "250m"},
	    "containers": [{"resources": {"requests": {"cpu": "2", "memory": "2Gi"}}}]
	  }, "status": {"phase": "Running"}},
	  {"kind": "Pod", "metadata": {"name": "pod-level-memory"}, "spec": {
	    "nodeName": "gt-control",
	    "resources": {"requests": {"memory": "3Gi"}},
	    "containers": [{"resources": {"requests": {"cpu": "1", "memory": "512Mi"}}}]
	  }, "status": {"phase": "Running"}}
	]}`
	nodes, err := ParseCapacity([]byte(listing))
	if err != nil {
		t.Fatalf("ParseCapacity: %v", err)
	}
	// pod-level-cpu: 6 CPU plus 250m overhead, not the container's 2; memory
	// from the container. pod-level-memory: 3Gi, not the container's 512Mi;
	// CPU from the container.
	want := Amount{CPU: 7_250, Memory: 5 << 30}
	if got := nodes[0].Requested; got != want {
		t.Errorf("requested = %s, want %s", got, want)
	}

	golden, err := ParseCapacity(loadGoldenFixture(t, capacityGolden))
	if err != nil {
		t.Fatalf("ParseCapacity on the live cluster's listing: %v", err)
	}
	if got, want := golden[0].Requested, (Amount{CPU: 6_210, Memory: 14_492 << 20}); got != want {
		t.Errorf("the live listing's pods request %s, want %s: they name no pod-level request", got, want)
	}
}

// TestPodRequestRefusesAnUnreadablePodLevelQuantity holds the pod-level read to
// the preflight's rule: a quantity it cannot read is an error, never a zero, so
// the node cannot read freer than it is.
func TestPodRequestRefusesAnUnreadablePodLevelQuantity(t *testing.T) {
	t.Parallel()
	const listing = `{"items": [
	  {"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "16", "memory": "64Gi"}}},
	  {"kind": "Pod", "metadata": {"name": "bad"}, "spec": {
	    "nodeName": "gt-control",
	    "resources": {"requests": {"cpu": "6x"}},
	    "containers": [{"resources": {"requests": {"cpu": "2"}}}]
	  }, "status": {"phase": "Running"}}
	]}`
	_, err := ParseCapacity([]byte(listing))
	if err == nil || !strings.Contains(err.Error(), "pod-level cpu") {
		t.Errorf("ParseCapacity with an unreadable pod-level cpu = %v, want an error naming the pod-level cpu", err)
	}
}

// TestGoldenFixturesAreAllRead fails a listing in testdata/live-cluster that no
// golden test reads: a fixture nothing consumes is one that can drift from the
// code it was captured for (gt-gg9hv.49).
func TestGoldenFixturesAreAllRead(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(goldenDir)
	if err != nil {
		t.Fatalf("reading %s: %v", goldenDir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		if !slices.Contains(goldenFixtures, name) {
			t.Errorf("%s/%s is not read by any golden test", goldenDir, name)
		}
	}
	for _, name := range goldenFixtures {
		if _, err := os.Stat(filepath.Join(goldenDir, name)); err != nil {
			t.Errorf("a golden test reads %s, which the directory does not hold: %v", name, err)
		}
	}
}

// goldenListingWithPodPriority returns a live-cluster listing with one pod's
// priority replaced, so a test drives the guard from the real pods rather than
// a hand-written shape. Numbers are decoded with UseNumber, so every other
// value is written back exactly as the listing printed it.
func goldenListingWithPodPriority(t *testing.T, listing []byte, namespace, name string, priority int) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(listing))
	dec.UseNumber()
	var document map[string]any
	if err := dec.Decode(&document); err != nil {
		t.Fatalf("decoding the live listing: %v", err)
	}
	items, _ := document["items"].([]any)
	for _, item := range items {
		object, _ := item.(map[string]any)
		metadata, _ := object["metadata"].(map[string]any)
		if metadata["namespace"] != namespace || metadata["name"] != name {
			continue
		}
		spec, _ := object["spec"].(map[string]any)
		if spec == nil {
			spec = map[string]any{}
			object["spec"] = spec
		}
		spec["priority"] = priority
		altered, err := json.Marshal(document)
		if err != nil {
			t.Fatalf("encoding the altered listing: %v", err)
		}
		return altered
	}
	t.Fatalf("the live listing holds no pod %s/%s", namespace, name)
	return nil
}

// TestPodRequestRefusesAnUnreadablePodLevelMemory is the memory twin of the
// cpu test above: the pod-level memory is read by its own branch, and a
// quantity it cannot read is an error that names the pod-level memory
// (gt-gg9hv.49).
func TestPodRequestRefusesAnUnreadablePodLevelMemory(t *testing.T) {
	t.Parallel()
	const listing = `{"items": [
	  {"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "16", "memory": "64Gi"}}},
	  {"kind": "Pod", "metadata": {"name": "bad"}, "spec": {
	    "nodeName": "gt-control",
	    "resources": {"requests": {"memory": "6x"}},
	    "containers": [{"resources": {"requests": {"memory": "2Gi"}}}]
	  }, "status": {"phase": "Running"}}
	]}`
	_, err := ParseCapacity([]byte(listing))
	if err == nil || !strings.Contains(err.Error(), "pod-level memory") {
		t.Errorf("ParseCapacity with an unreadable pod-level memory = %v, want an error naming the pod-level memory", err)
	}
}

// TestParseQuantityRefusesAMalformedOrOversizedExponent drives the exponent
// refusals one at a time: each names the value it refused and the reason, so a
// reader of the preflight's decline can tell a typo from an overflow
// (gt-gg9hv.49).
func TestParseQuantityRefusesAMalformedOrOversizedExponent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{name: "two exponents", value: "1e2e3", want: "more than one exponent"},
		{name: "an exponent that does not fit an int64", value: "1e99999999999999999999", want: "too large to read"},
		{name: "an exponent past the int64 bound", value: "1e100", want: "too large to read"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := parseQuantity(c.value, memoryBytes)
			if err == nil {
				t.Fatalf("parseQuantity(%q) read a value, want a refusal naming %q", c.value, c.want)
			}
			if !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), c.value) {
				t.Errorf("parseQuantity(%q) error = %v, want it to name the value and %q", c.value, err, c.want)
			}
		})
	}
}

// TestSplitExponentRefusesAnExponentWithNoDigits drives the empty-exponent
// refusal directly: parseQuantity reads a trailing e as the exa suffix before
// the exponent split sees it, so the split's own guard is reached only here
// (gt-gg9hv.49).
func TestSplitExponentRefusesAnExponentWithNoDigits(t *testing.T) {
	t.Parallel()
	_, _, err := splitExponent("1e", "1e")
	if err == nil || !strings.Contains(err.Error(), "no digits") || !strings.Contains(err.Error(), `"1e"`) {
		t.Errorf("splitExponent(1e) = %v, want a refusal naming the value and that the exponent has no digits", err)
	}
}
