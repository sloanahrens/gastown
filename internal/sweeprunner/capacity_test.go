package sweeprunner

import (
	"strings"
	"testing"
)

// capacityListing is a `kubectl get nodes,pods -A -o json` answer with two
// nodes and the pods below, written the way kubectl prints it: one List whose
// items name their own kind.
const capacityListing = `{
  "apiVersion": "v1",
  "kind": "List",
  "items": [
    {"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"allocatable": {"cpu": "12", "memory": "24Gi"}}},
    {"kind": "Node", "metadata": {"name": "gt-worker"}, "status": {"allocatable": {"cpu": "8", "memory": "16Gi"}}},
    {"kind": "Pod", "metadata": {"name": "forgejo"}, "spec": {"nodeName": "gt-control", "containers": [
      {"name": "forgejo", "resources": {"requests": {"cpu": "500m", "memory": "512Mi"}}},
      {"name": "sidecar"}
    ]}, "status": {"phase": "Running"}},
    {"kind": "Pod", "metadata": {"name": "forwarder"}, "spec": {"nodeName": "gt-control", "containers": [
      {"name": "forwarder", "resources": {"requests": {"cpu": "1", "memory": "1Gi"}}}
    ]}, "status": {"phase": "Running"}},
    {"kind": "Pod", "metadata": {"name": "old-sweep"}, "spec": {"nodeName": "gt-control", "containers": [
      {"name": "runner", "resources": {"requests": {"cpu": "4", "memory": "5Gi"}}}
    ]}, "status": {"phase": "Succeeded"}},
    {"kind": "Pod", "metadata": {"name": "crashed-sweep"}, "spec": {"nodeName": "gt-control", "containers": [
      {"name": "runner", "resources": {"requests": {"cpu": "2", "memory": "2Gi"}}}
    ]}, "status": {"phase": "Failed"}},
    {"kind": "Pod", "metadata": {"name": "pending-placed"}, "spec": {"nodeName": "gt-control", "containers": [
      {"name": "compact", "resources": {"requests": {"cpu": "250m", "memory": "256Mi"}}}
    ]}, "status": {"phase": "Pending"}},
    {"kind": "Pod", "metadata": {"name": "unscheduled"}, "spec": {"containers": [
      {"name": "waiting", "resources": {"requests": {"cpu": "3", "memory": "3Gi"}}}
    ]}, "status": {"phase": "Pending"}},
    {"kind": "Pod", "metadata": {"name": "worker-job"}, "spec": {"nodeName": "gt-worker", "containers": [
      {"name": "worker", "resources": {"requests": {"cpu": "2", "memory": "4Gi"}}}
    ]}, "status": {"phase": "Running"}}
  ]
}`

// TestCapacityFreeIsAllocatableMinusRequests reads a canned listing: each
// node's requested amount is the sum over the containers of the pods placed on
// it that have not terminated, a container with no request asks for nothing,
// and a pod the scheduler has not placed is counted on no node.
func TestCapacityFreeIsAllocatableMinusRequests(t *testing.T) {
	t.Parallel()
	nodes, err := ParseCapacity([]byte(capacityListing))
	if err != nil {
		t.Fatalf("ParseCapacity: %v", err)
	}
	want := map[string]NodeCapacity{
		// forgejo 500m/512Mi + sidecar 0 + forwarder 1/1Gi + 250m/256Mi; the
		// Succeeded and Failed pods are ended and the unscheduled pod is on no
		// node.
		"gt-control": {
			Node:        "gt-control",
			Allocatable: Amount{CPU: 12_000, Memory: 24 << 30},
			Requested:   Amount{CPU: 1_750, Memory: 1_792 << 20},
		},
		"gt-worker": {
			Node:        "gt-worker",
			Allocatable: Amount{CPU: 8_000, Memory: 16 << 30},
			Requested:   Amount{CPU: 2_000, Memory: 4 << 30},
		},
	}
	if len(nodes) != len(want) {
		t.Fatalf("ParseCapacity named %d nodes, want %d: %+v", len(nodes), len(want), nodes)
	}
	for _, got := range nodes {
		w, ok := want[got.Node]
		if !ok {
			t.Errorf("ParseCapacity named an unexpected node %q", got.Node)
			continue
		}
		if got != w {
			t.Errorf("node %q = %+v, want %+v", got.Node, got, w)
		}
	}
	// The free amounts are the numbers the daemon logs and decides on.
	control := want["gt-control"]
	if got, wantFree := control.Free(), (Amount{CPU: 10_250, Memory: 22_784 << 20}); got != wantFree {
		t.Errorf("gt-control free = %s, want %s", got, wantFree)
	}
}

// TestCapacityRefusesWhatItCannotRead refuses every listing a decision cannot be
// taken from: a plan must decline on all of them, never launch on a guess.
func TestCapacityRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	const node = `{"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"allocatable": {"cpu": "12", "memory": "24Gi"}}}`
	for _, tc := range []struct {
		name    string
		listing string
		want    string
	}{
		{name: "not JSON", listing: "Error from server (Forbidden)", want: "reading the node and pod listing"},
		{name: "an item that is neither", listing: `{"items": [` + node + `, {"kind": "Service", "metadata": {"name": "x"}}]}`, want: "neither a node nor a pod"},
		{name: "no node", listing: `{"items": []}`, want: "names no node"},
		{name: "no cpu allocatable", listing: `{"items": [{"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"allocatable": {"memory": "24Gi"}}}]}`, want: "no cpu allocatable"},
		{name: "no memory allocatable", listing: `{"items": [{"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"allocatable": {"cpu": "12"}}}]}`, want: "no memory allocatable"},
		{name: "an unreadable quantity", listing: `{"items": [` + node + `, {"kind": "Pod", "metadata": {"name": "p"}, "spec": {"nodeName": "gt-control", "containers": [{"resources": {"requests": {"cpu": "4x"}}}]}}]}`, want: "cpu"},
		{name: "a negative quantity", listing: `{"items": [` + node + `, {"kind": "Pod", "metadata": {"name": "p"}, "spec": {"nodeName": "gt-control", "containers": [{"resources": {"requests": {"memory": "-1Gi"}}}]}}]}`, want: "negative"},
		{name: "a pod on a node the listing omits", listing: `{"items": [` + node + `, {"kind": "Pod", "metadata": {"name": "p"}, "spec": {"nodeName": "gt-gone", "containers": []}}]}`, want: "gt-gone"},
		{name: "a node named twice", listing: `{"items": [` + node + `, ` + node + `]}`, want: "twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			nodes, err := ParseCapacity([]byte(tc.listing))
			if err == nil {
				t.Fatalf("ParseCapacity(%s) = %+v, want it refused", tc.listing, nodes)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ParseCapacity(%s) = %v, want the error to name %q", tc.listing, err, tc.want)
			}
		})
	}
}

// TestCapacityDecidesWithHeadroom holds the decision to the headroom: a run fits
// when its request plus the headroom fits in the free amount, is declined just
// under it, and a node the run does not fit beside does not hide one it does.
func TestCapacityDecidesWithHeadroom(t *testing.T) {
	t.Parallel()
	job := Amount{CPU: 6_000, Memory: 9 << 30}
	headroom := Amount{CPU: sweepHeadroomCPU, Memory: sweepHeadroomMemory}
	exact := NodeCapacity{
		Node:        "gt-control",
		Allocatable: job.Add(headroom),
	}
	report, err := decideCapacity([]NodeCapacity{exact}, job)
	if err != nil {
		t.Fatalf("a node whose free amount is the job plus the headroom declined: %v", err)
	}
	if report.Node != "gt-control" || report.Job != job {
		t.Errorf("report = %+v, want the node and the job's request", report)
	}

	short := NodeCapacity{Node: "gt-control", Allocatable: Amount{CPU: job.CPU + headroom.CPU - 1, Memory: job.Memory + headroom.Memory}}
	report, err = decideCapacity([]NodeCapacity{short}, job)
	if err == nil {
		t.Fatalf("a node one milli-CPU short of the headroom launched: %+v", report)
	}
	if want := "free 7999m/11264Mi, job needs 6000m/9216Mi, headroom 2000m/2048Mi"; err.Error() != want {
		t.Errorf("decline = %q, want %q", err, want)
	}

	// The roomiest node by CPU does not hold the job; another node does.
	roomy := NodeCapacity{Node: "gt-a", Allocatable: Amount{CPU: 10_000, Memory: 1 << 30}}
	fits := NodeCapacity{Node: "gt-b", Allocatable: Amount{CPU: 8_000, Memory: 16 << 30}}
	report, err = decideCapacity([]NodeCapacity{roomy, fits}, job)
	if err != nil {
		t.Fatalf("the node that fits was not chosen: %v", err)
	}
	if report.Node != "gt-b" {
		t.Errorf("chose node %q, want gt-b, the node the run fits beside", report.Node)
	}
}

// TestPodRequestTotalForTheDefaultBlock pins the amount the incident's runner
// block asks a node for: cpus and memory are the runner container's share, and
// the dind sidecar's own share is added beside it, so cpus 4 and memory 5 with
// dind ask for 6 CPU and 9 GiB.
func TestPodRequestTotalForTheDefaultBlock(t *testing.T) {
	t.Parallel()
	spec := goldenSpec()
	spec.CPUs, spec.MemoryGB, spec.Dind = 4, 5, true
	plan, err := Kubernetes{}.Render(spec)
	if err != nil {
		t.Fatalf("rendering the block: %v", err)
	}
	got, err := PodRequestTotal(plan.Job.Spec.Template.Spec)
	if err != nil {
		t.Fatalf("PodRequestTotal: %v", err)
	}
	if want := (Amount{CPU: 6_000, Memory: 9 << 30}); got != want {
		t.Errorf("the pod asks for %d milli-CPU and %d bytes, want %d and %d (runner 4000m/5Gi plus the dind sidecar 2000m/4Gi)",
			got.CPU, got.Memory, want.CPU, want.Memory)
	}

	spec.Dind = false
	plain, err := Kubernetes{}.Render(spec)
	if err != nil {
		t.Fatalf("rendering the block without dind: %v", err)
	}
	got, err = PodRequestTotal(plain.Job.Spec.Template.Spec)
	if err != nil {
		t.Fatalf("PodRequestTotal: %v", err)
	}
	if want := (Amount{CPU: 4_000, Memory: 5 << 30}); got != want {
		t.Errorf("without dind the pod asks for %d milli-CPU and %d bytes, want the runner's own %d and %d", got.CPU, got.Memory, want.CPU, want.Memory)
	}
}

// TestK8sPreflightIsOneNodesPodsCall holds the plan's preflight to the one
// kubectl call that reads the node, and to a decision that counts the Job's own
// request with the sync and reader pods the run creates.
func TestK8sPreflightIsOneNodesPodsCall(t *testing.T) {
	t.Parallel()
	spec := goldenSpec()
	spec.CPUs, spec.MemoryGB, spec.Dind = 4, 5, true
	plan, err := Kubernetes{}.Render(spec)
	if err != nil {
		t.Fatalf("rendering the spec: %v", err)
	}
	if plan.Preflight == nil {
		t.Fatal("the plan renders no capacity preflight")
	}
	argv := strings.Join(plan.Preflight.Step.Argv, " ")
	for _, want := range []string{"kubectl", "--context " + spec.Context, "get nodes,pods", "-A -o json"} {
		if !strings.Contains(argv, want) {
			t.Errorf("the preflight runs %q, want it to hold %q", argv, want)
		}
	}

	listing := `{"items": [
	  {"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"allocatable": {"cpu": "12", "memory": "24Gi"}}},
	  {"kind": "Pod", "metadata": {"name": "forgejo"}, "spec": {"nodeName": "gt-control", "containers": [
	    {"name": "forgejo", "resources": {"requests": {"cpu": "500m", "memory": "512Mi"}}}]}, "status": {"phase": "Running"}}
	]}`
	report, err := plan.Preflight.Read([]byte(listing))
	if err != nil {
		t.Fatalf("the preflight declined a node with room: %v", err)
	}
	if report.Allocatable != (Amount{CPU: 12_000, Memory: 24 << 30}) {
		t.Errorf("allocatable = %s, want the node's 12000m/24576Mi", report.Allocatable)
	}
	if report.Requested != (Amount{CPU: 500, Memory: 512 << 20}) {
		t.Errorf("requested = %s, want the forgejo pod's 500m/512Mi", report.Requested)
	}
	// The Job's pod (runner plus dind), the sync pod and the reader pod.
	if want := (Amount{CPU: 6_200, Memory: 9_472 << 20}); report.Job != want {
		t.Errorf("job = %s, want %s", report.Job, want)
	}
}

// TestParseCapacityReadsTheAPIsQuantityForms holds the parser to the forms a
// quantity takes: a plain number, milli, binary and decimal suffixes, and an
// exponent. A suffix the API does not use, or a value that is not a number, is
// an error.
func TestParseCapacityReadsTheAPIsQuantityForms(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value string
		scale int64
		want  int64
	}{
		{value: "4", scale: cpuMilli, want: 4_000},
		{value: "6210m", scale: cpuMilli, want: 6_210},
		{value: "0.5", scale: cpuMilli, want: 500},
		{value: "1e3", scale: cpuMilli, want: 1_000_000},
		{value: "14492Mi", scale: memoryBytes, want: 14_492 << 20},
		{value: "1.5Gi", scale: memoryBytes, want: 3 << 29},
	} {
		got, err := parseQuantity(tc.value, tc.scale)
		if err != nil {
			t.Errorf("parseQuantity(%q, %d): %v", tc.value, tc.scale, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseQuantity(%q, %d) = %d, want %d", tc.value, tc.scale, got, tc.want)
		}
	}
	for _, value := range []string{"", "abc", "4x", "-1", "1Ei2"} {
		if got, err := parseQuantity(value, cpuMilli); err == nil {
			t.Errorf("parseQuantity(%q) = %d, want it refused", value, got)
		}
	}
	// A quantity that is not a whole unit rounds up, so a request never reads
	// lower than it is.
	if got, err := parseQuantity("0.0004", cpuMilli); err != nil || got != 1 {
		t.Errorf("parseQuantity(0.0004) = %d, %v; want 1 milli-unit", got, err)
	}
}

// TestCapacityReportShowsTheFiguresTheDaemonLogs holds the two lines to the
// shape the daemon writes: the launch line names allocatable, requested and the
// job, and the decline line names free, what the job needs and the headroom.
func TestCapacityReportShowsTheFiguresTheDaemonLogs(t *testing.T) {
	t.Parallel()
	report := CapacityReport{
		Node:        "gt-control",
		Allocatable: Amount{CPU: 12_000, Memory: 24 << 30},
		Requested:   Amount{CPU: 1_750, Memory: 1_792 << 20},
		Job:         Amount{CPU: 6_200, Memory: 9_472 << 20},
	}
	if got, want := report.String(), "allocatable 12000m/24576Mi, requested 1750m/1792Mi, job 6200m/9472Mi"; got != want {
		t.Errorf("report = %q, want %q", got, want)
	}
	if got, want := report.Decline(), "free 10250m/22784Mi, job needs 6200m/9472Mi, headroom 2000m/2048Mi"; got != want {
		t.Errorf("decline = %q, want %q", got, want)
	}
}
