package sweeprunner

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
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
    {"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "12", "memory": "24Gi"}}},
    {"kind": "Node", "metadata": {"name": "gt-worker"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "8", "memory": "16Gi"}}},
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
			Ready:       true,
		},
		"gt-worker": {
			Node:        "gt-worker",
			Allocatable: Amount{CPU: 8_000, Memory: 16 << 30},
			Requested:   Amount{CPU: 2_000, Memory: 4 << 30},
			Ready:       true,
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
		if !reflect.DeepEqual(got, w) {
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
	const node = `{"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "12", "memory": "24Gi"}}}`
	for _, tc := range []struct {
		name    string
		listing string
		want    string
	}{
		{name: "not JSON", listing: "Error from server (Forbidden)", want: "reading the node and pod listing"},
		{name: "an item that is neither", listing: `{"items": [` + node + `, {"kind": "Service", "metadata": {"name": "x"}}]}`, want: "neither a node nor a pod"},
		{name: "no node", listing: `{"items": []}`, want: "names no node"},
		{name: "no cpu allocatable", listing: `{"items": [{"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"memory": "24Gi"}}}]}`, want: "no cpu allocatable"},
		{name: "no memory allocatable", listing: `{"items": [{"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "12"}}}]}`, want: "no memory allocatable"},
		{name: "an unreadable quantity", listing: `{"items": [` + node + `, {"kind": "Pod", "metadata": {"name": "p"}, "spec": {"nodeName": "gt-control", "containers": [{"resources": {"requests": {"cpu": "4x"}}}]}}]}`, want: "cpu"},
		{name: "a node with an unreadable quantity", listing: `{"items": [{"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "4x", "memory": "24Gi"}}}]}`, want: `node "gt-control": cpu`},
		{name: "a negative quantity", listing: `{"items": [` + node + `, {"kind": "Pod", "metadata": {"name": "p"}, "spec": {"nodeName": "gt-control", "containers": [{"resources": {"requests": {"memory": "-1Gi"}}}]}}]}`, want: "has a sign"},
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
		Ready:       true,
	}
	report, err := DecideCapacity([]NodeCapacity{exact}, job, nil)
	if err != nil {
		t.Fatalf("a node whose free amount is the job plus the headroom declined: %v", err)
	}
	if report.Node != "gt-control" || report.Job != job {
		t.Errorf("report = %+v, want the node and the job's request", report)
	}

	short := NodeCapacity{Node: "gt-control", Allocatable: Amount{CPU: job.CPU + headroom.CPU - 1, Memory: job.Memory + headroom.Memory}, Ready: true}
	report, err = DecideCapacity([]NodeCapacity{short}, job, nil)
	if err == nil {
		t.Fatalf("a node one milli-CPU short of the headroom launched: %+v", report)
	}
	if want := "node gt-control: free 7999m/11264Mi, job needs 6000m/9216Mi, headroom 2000m/2048Mi"; err.Error() != want {
		t.Errorf("decline = %q, want %q", err, want)
	}
	if _, err := DecideCapacity(nil, job, nil); err == nil {
		t.Error("a listing with no node decided; free capacity cannot be read from nothing")
	}

	// The roomiest node by CPU does not hold the job; another node does.
	roomy := NodeCapacity{Node: "gt-a", Allocatable: Amount{CPU: 10_000, Memory: 1 << 30}, Ready: true}
	fits := NodeCapacity{Node: "gt-b", Allocatable: Amount{CPU: 8_000, Memory: 16 << 30}, Ready: true}
	report, err = DecideCapacity([]NodeCapacity{roomy, fits}, job, nil)
	if err != nil {
		t.Fatalf("the node that fits was not chosen: %v", err)
	}
	if report.Node != "gt-b" {
		t.Errorf("chose node %q, want gt-b, the node the run fits beside", report.Node)
	}
}

// TestPodRequestTotalForTheDefaultBlock pins the amount the incident's runner
// block asks a node for: cpus and memory are the pod's total, and the dind
// sidecar's own share comes out of that total, so cpus 4 and memory 5 with dind
// ask for 4 CPU and 5 GiB.
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
	if want := (Amount{CPU: 4_000, Memory: 5 << 30}); got != want {
		t.Errorf("the pod asks for %d milli-CPU and %d bytes, want %d and %d (the runner's remaining 2000m/1Gi plus the dind sidecar's 2000m/4Gi)",
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
		t.Errorf("without dind the pod asks for %d milli-CPU and %d bytes, want the block's own %d and %d", got.CPU, got.Memory, want.CPU, want.Memory)
	}
}

// TestRenderJobRefusesAPodTotalTheSidecarExhausts holds the split to refusing a
// total that cannot hold the dind sidecar and leave the runner a share: the
// rendered pod can never ask for more than the operator set, and the runner is
// never rendered with nothing.
func TestRenderJobRefusesAPodTotalTheSidecarExhausts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		cpus     int
		memoryGB int
		want     string
	}{
		{name: "the sidecar takes every CPU", cpus: 2, memoryGB: 8, want: "spec.cpus"},
		{name: "the sidecar takes every GiB", cpus: 4, memoryGB: 4, want: "spec.memoryGB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := goldenSpec()
			spec.CPUs, spec.MemoryGB, spec.Dind = tc.cpus, tc.memoryGB, true
			if plan, err := (Kubernetes{}).Render(spec); err == nil {
				t.Fatalf("rendered cpus %d, memory %d with dind as %+v, want the pod total refused", tc.cpus, tc.memoryGB, plan.Job.Spec.Template.Spec)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
			// The Docker backend asks the runner container for the whole share,
			// so the same total is a run it can start.
			if _, err := (Docker{}).Render(spec); err != nil {
				t.Errorf("the Docker backend refused cpus %d, memory %d with dind: %v", tc.cpus, tc.memoryGB, err)
			}
		})
	}
}

// TestPodRequestTotalRefusesAnUnreadableContainer holds the pod total to
// refusing a request it cannot read: counting it as zero would make the pod ask
// less than it does.
func TestPodRequestTotalRefusesAnUnreadableContainer(t *testing.T) {
	t.Parallel()
	pod := PodSpec{Containers: []Container{{
		Name:      runnerContainerName,
		Resources: &Resources{Requests: ResourceList{CPU: "four"}},
	}}}
	if got, err := PodRequestTotal(pod); err == nil {
		t.Fatalf("PodRequestTotal = %+v, want the unreadable request refused", got)
	} else if !strings.Contains(err.Error(), `container "runner"`) {
		t.Errorf("error = %v, want it to name the container", err)
	}
}

// TestCapacityReadDeclinesAListingItCannotRead holds the preflight's reader to
// the decline: a listing kubectl could not print is never read as free capacity.
func TestCapacityReadDeclinesAListingItCannotRead(t *testing.T) {
	t.Parallel()
	spec := goldenSpec()
	job, err := renderJob(spec)
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	report, err := capacityRead(job, renderSyncPod(spec), renderReader(spec), nil)([]byte("Error from server (Forbidden)"))
	if err == nil {
		t.Fatalf("capacityRead = %+v, want the unreadable listing refused", report)
	}
	if !strings.Contains(err.Error(), "cannot read the node's capacity") {
		t.Errorf("error = %v, want it to name the node's capacity", err)
	}
}

// TestCapacityReadDeclinesARequestItCannotRead holds the request side to the
// same decline: a rendered object whose request cannot be read declines the
// launch instead of sizing it from a zero.
func TestCapacityReadDeclinesARequestItCannotRead(t *testing.T) {
	t.Parallel()
	spec := goldenSpec()
	job, err := renderJob(spec)
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	reader := renderReader(spec)
	reader.Spec.Containers[0].Resources.Requests.CPU = "four"
	roomy := `{"items": [{"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "64", "memory": "256Gi"}}}]}`
	report, err := capacityRead(job, renderSyncPod(spec), reader, nil)([]byte(roomy))
	if err == nil {
		t.Fatalf("capacityRead = %+v, want the unreadable request refused", report)
	}
	if !strings.Contains(err.Error(), "resource request") {
		t.Errorf("error = %v, want it to name the run's resource request", err)
	}
	if !strings.Contains(err.Error(), `container "reader"`) {
		t.Errorf("error = %v, want it to name the container", err)
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
	  {"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "12", "memory": "24Gi"}}},
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
	if want := (Amount{CPU: 4_200, Memory: 5_376 << 20}); report.Job != want {
		t.Errorf("job = %s, want %s", report.Job, want)
	}
}

// TestK8sPreflightJobMatchesTheRenderedPods holds the figure the preflight
// decides on to the containers the renderer actually writes, dind or not: a
// share the renderer changes fails here rather than in a launch.
func TestK8sPreflightJobMatchesTheRenderedPods(t *testing.T) {
	t.Parallel()
	listing := `{"items": [
	  {"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "64", "memory": "256Gi"}}}
	]}`
	for _, dind := range []bool{true, false} {
		spec := goldenSpec()
		spec.Dind = dind
		plan, err := Kubernetes{}.Render(spec)
		if err != nil {
			t.Fatalf("rendering with dind %v: %v", dind, err)
		}
		want, err := PodRequestTotal(plan.Job.Spec.Template.Spec)
		if err != nil {
			t.Fatalf("the rendered Job's pod: %v", err)
		}
		for _, pod := range []*Pod{renderSyncPod(spec), renderReader(spec)} {
			request, err := PodRequestTotal(pod.Spec)
			if err != nil {
				t.Fatalf("the rendered pod %q: %v", pod.Metadata.Name, err)
			}
			want = want.Add(request)
		}
		report, err := plan.Preflight.Read([]byte(listing))
		if err != nil {
			t.Fatalf("the preflight declined a node with room: %v", err)
		}
		if report.Job != want {
			t.Errorf("dind %v: the preflight asks for %s, want the rendered pods' %s", dind, report.Job, want)
		}
	}
}

// TestParseQuantity holds the parser to the forms a quantity takes: a plain
// integer or decimal, milli-CPU, and MiB or GiB. Every other form (a fraction
// such as 1/2, Ki, an exponent, a sign, emptiness, trailing junk) is refused
// with the input in the error, so a request the preflight cannot read never
// counts as a node with room.
func TestParseQuantity(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value string
		scale int64
		want  int64
	}{
		{value: "4", scale: cpuMilli, want: 4_000},
		{value: "6210m", scale: cpuMilli, want: 6_210},
		{value: "0.5", scale: cpuMilli, want: 500},
		{value: "1.5", scale: cpuMilli, want: 1_500},
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
	for _, value := range []string{"", "abc", "4x", "1/2", "1/2Gi", "-1", "+1", "1e3", "1Ei2", "10Ei", "14492Ki", "4 Mi", "4m ", "999999999999999999999999999999", "0x10", "0b101", "0o7", "1_000", "1.2.3", ".", "0x10Mi"} {
		got, err := parseQuantity(value, cpuMilli)
		if err == nil {
			t.Errorf("parseQuantity(%q) = %d, want it refused", value, got)
			continue
		}
		if !strings.Contains(err.Error(), value) {
			t.Errorf("parseQuantity(%q) error = %v, want it to name the input", value, err)
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
		Job:         Amount{CPU: 4_200, Memory: 5_376 << 20},
	}
	if got, want := report.String(), "node gt-control: allocatable 12000m/24576Mi, requested 1750m/1792Mi, job 4200m/5376Mi"; got != want {
		t.Errorf("report = %q, want %q", got, want)
	}
	if got, want := report.Decline(), "node gt-control: free 10250m/22784Mi, job needs 4200m/5376Mi, headroom 2000m/2048Mi"; got != want {
		t.Errorf("decline = %q, want %q", got, want)
	}
}

// TestCapacityEligibleNodes holds the decision to the rule: the launch goes
// ahead when at least one eligible node fits, a node that fits is chosen even
// when another reads roomier, and a decline names the best-fitting eligible
// node. A node that is not Ready, is cordoned, or carries a NoSchedule taint the
// Job does not tolerate is never chosen, however much room it has, and an
// eligible node that does not fit does not block one that does.
func TestCapacityEligibleNodes(t *testing.T) {
	t.Parallel()
	job := Amount{CPU: 4_200, Memory: 5_376 << 20}
	headroom := Amount{CPU: sweepHeadroomCPU, Memory: sweepHeadroomMemory}
	fitting := job.Add(headroom)
	short := Amount{CPU: fitting.CPU - 1, Memory: fitting.Memory}

	for _, tc := range []struct {
		name        string
		listing     string
		tolerations []Toleration
		wantLaunch  bool
		// wantNode is the node a launch or decline names; wantErr, when set,
		// is what the decline's reason must hold instead.
		wantNode string
		wantErr  string
	}{
		{
			name: "only the second node has room",
			listing: `{"items": [
			  {"kind": "Node", "metadata": {"name": "gt-small"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "6", "memory": "24Gi"}}},
			  {"kind": "Node", "metadata": {"name": "gt-roomy"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "8", "memory": "24Gi"}}}
			]}`,
			wantLaunch: true,
			wantNode:   "gt-roomy",
		},
		{
			name: "neither fits names the best-fitting node",
			listing: `{"items": [
			  {"kind": "Node", "metadata": {"name": "gt-a"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "5", "memory": "24Gi"}}},
			  {"kind": "Node", "metadata": {"name": "gt-b"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "6", "memory": "24Gi"}}}
			]}`,
			wantNode: "gt-b",
			wantErr:  "node gt-b: free",
		},
		{
			name: "an ineligible node is never chosen",
			listing: `{"items": [
			  {"kind": "Node", "metadata": {"name": "gt-notready"}, "status": {"conditions": [{"type": "Ready", "status": "False"}], "allocatable": {"cpu": "32", "memory": "64Gi"}}},
			  {"kind": "Node", "metadata": {"name": "gt-cordon"}, "spec": {"unschedulable": true}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "32", "memory": "64Gi"}}},
			  {"kind": "Node", "metadata": {"name": "gt-tainted"}, "spec": {"taints": [{"key": "dedicated", "value": "ci", "effect": "NoSchedule"}]}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "32", "memory": "64Gi"}}},
			  {"kind": "Node", "metadata": {"name": "gt-tight"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "6", "memory": "24Gi"}}}
			]}`,
			wantNode: "gt-tight",
			wantErr:  "node gt-tight: free",
		},
		{
			name: "a node with no Ready condition is not eligible",
			listing: `{"items": [
			  {"kind": "Node", "metadata": {"name": "gt-unknown"}, "status": {"conditions": [{"type": "Ready", "status": "Unknown"}], "allocatable": {"cpu": "32", "memory": "64Gi"}}},
			  {"kind": "Node", "metadata": {"name": "gt-nocondition"}, "status": {"allocatable": {"cpu": "32", "memory": "64Gi"}}},
			  {"kind": "Node", "metadata": {"name": "gt-ready"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "6", "memory": "24Gi"}}}
			]}`,
			wantNode: "gt-ready",
			wantErr:  "node gt-ready: free",
		},
		{
			name: "a tolerated taint is a node the Job may use",
			listing: `{"items": [
			  {"kind": "Node", "metadata": {"name": "gt-cordon"}, "spec": {"unschedulable": true}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "32", "memory": "64Gi"}}},
			  {"kind": "Node", "metadata": {"name": "gt-tainted"}, "spec": {"taints": [{"key": "dedicated", "value": "ci", "effect": "NoSchedule"}]}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "32", "memory": "64Gi"}}}
			]}`,
			tolerations: []Toleration{{Key: "dedicated", Operator: "Equal", Value: "ci", Effect: "NoSchedule"}},
			wantLaunch:  true,
			wantNode:    "gt-tainted",
		},
		{
			name: "no eligible node declines",
			listing: `{"items": [
			  {"kind": "Node", "metadata": {"name": "gt-notready"}, "status": {"conditions": [{"type": "Ready", "status": "False"}], "allocatable": {"cpu": "32", "memory": "64Gi"}}},
			  {"kind": "Node", "metadata": {"name": "gt-cordon"}, "spec": {"unschedulable": true}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "32", "memory": "64Gi"}}},
			  {"kind": "Node", "metadata": {"name": "gt-tainted"}, "spec": {"taints": [{"key": "dedicated", "value": "ci", "effect": "NoSchedule"}]}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "32", "memory": "64Gi"}}}
			]}`,
			wantErr: "no node the Job may be placed on",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			nodes, err := ParseCapacity([]byte(tc.listing))
			if err != nil {
				t.Fatalf("ParseCapacity: %v", err)
			}
			report, err := DecideCapacity(nodes, job, tc.tolerations)
			if tc.wantLaunch {
				if err != nil {
					t.Fatalf("the node with room was not chosen: %v", err)
				}
			} else if err == nil {
				t.Fatalf("DecideCapacity = %+v, want it declined", report)
			} else if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("decline = %v, want it to hold %q", err, tc.wantErr)
			}
			if tc.wantNode != "" && report.Node != tc.wantNode {
				t.Errorf("report names node %q, want %q", report.Node, tc.wantNode)
			}
		})
	}

	// The headroom bound itself: a node with exactly the job plus the headroom
	// launches; one milli-CPU less declines against that same node.
	exact := `{"items": [{"kind": "Node", "metadata": {"name": "gt-exact"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "` +
		strconv.FormatInt(fitting.CPU, 10) + `m", "memory": "` +
		strconv.FormatInt(fitting.Memory>>20, 10) + `Mi"}}}]}`
	nodes, err := ParseCapacity([]byte(exact))
	if err != nil {
		t.Fatalf("ParseCapacity: %v", err)
	}
	if report, err := DecideCapacity(nodes, job, nil); err != nil {
		t.Fatalf("a node with exactly the job plus the headroom declined: %+v, %v", report, err)
	}
	nodes[0].Allocatable = short
	if report, err := DecideCapacity(nodes, job, nil); err == nil {
		t.Fatalf("a node one milli-CPU short of the headroom launched: %+v", report)
	}
}

// TestCapacityDocMatchesTheRule holds the preflight's doc comment to the rule
// the code applies: the launch goes ahead when at least one eligible node fits,
// an eligible node that does not fit does not block one that does, and a
// decline names the best-fitting eligible node. The doc is read from the source
// so a sentence about the wrong rule fails here, not in a review (gt-gg9hv.38).
func TestCapacityDocMatchesTheRule(t *testing.T) {
	t.Parallel()
	file, err := parser.ParseFile(token.NewFileSet(), "capacity.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing capacity.go: %v", err)
	}
	var doc string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "DecideCapacity" {
			doc = fn.Doc.Text()
			break
		}
	}
	if doc == "" {
		t.Fatal("capacity.go has no DecideCapacity doc comment to read")
	}
	if !strings.Contains(doc, "at least one eligible node") {
		t.Errorf("the preflight's doc does not state that at least one eligible node must fit:\n%s", doc)
	}
	if strings.Contains(doc, "every placeable node") {
		t.Errorf("the preflight's doc claims every placeable node fits:\n%s", doc)
	}
}

// TestPodRequestMatchesTheScheduler holds a pod's request to what the scheduler
// counts: overhead plus, per resource, the larger of the init containers taken
// one at a time and the sum of the containers. The rendered dind sidecar's
// amounts are the same constants its container writes.
func TestPodRequestMatchesTheScheduler(t *testing.T) {
	t.Parallel()
	const listing = `{"items": [
	  {"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "64", "memory": "256Gi"}}},
	  {"kind": "Pod", "metadata": {"name": "mixed"}, "spec": {
	    "nodeName": "gt-control",
	    "overhead": {"cpu": "100m", "memory": "64Mi"},
	    "initContainers": [
	      {"resources": {"requests": {"cpu": "1500m", "memory": "512Mi"}}},
	      {"resources": {"requests": {"cpu": "200m", "memory": "2Gi"}}}
	    ],
	    "containers": [
	      {"resources": {"requests": {"cpu": "600m", "memory": "1Gi"}}},
	      {"resources": {"requests": {"cpu": "400m", "memory": "512Mi"}}}
	    ]}, "status": {"phase": "Running"}},
	  {"kind": "Pod", "metadata": {"name": "init-heavy"}, "spec": {
	    "nodeName": "gt-control",
	    "initContainers": [{"resources": {"requests": {"cpu": "4", "memory": "8Gi"}}}],
	    "containers": [
	      {"resources": {"requests": {"cpu": "1", "memory": "1Gi"}}},
	      {"resources": {"requests": {"cpu": "1", "memory": "1Gi"}}}
	    ]}, "status": {"phase": "Running"}}
	]}`
	nodes, err := ParseCapacity([]byte(listing))
	if err != nil {
		t.Fatalf("ParseCapacity: %v", err)
	}
	// mixed: overhead 100m/64Mi + max(1500m/2Gi of one init container,
	// 1000m/1536Mi of the containers) = 1600m/2Gi+64Mi.
	// init-heavy: max(4000m/8Gi of the one init container, 2000m/2Gi of the
	// containers) = 4000m/8Gi.
	if want := (Amount{CPU: 5_600, Memory: 10<<30 + 64<<20}); nodes[0].Requested != want {
		t.Errorf("requested = %s, want %s", nodes[0].Requested, want)
	}

	orphan := `{"items": [
	  {"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "64", "memory": "256Gi"}}},
	  {"kind": "Pod", "metadata": {"name": "bad"}, "spec": {"nodeName": "gt-control", "overhead": {"cpu": "4x"}, "containers": []}, "status": {"phase": "Running"}}
	]}`
	if got, err := ParseCapacity([]byte(orphan)); err == nil {
		t.Errorf("ParseCapacity read an unreadable overhead as %+v, want it refused", got)
	}
	badInit := `{"items": [
	  {"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "64", "memory": "256Gi"}}},
	  {"kind": "Pod", "metadata": {"name": "bad-init"}, "spec": {"nodeName": "gt-control", "initContainers": [{"resources": {"requests": {"cpu": "4x"}}}], "containers": []}, "status": {"phase": "Running"}}
	]}`
	if got, err := ParseCapacity([]byte(badInit)); err == nil {
		t.Errorf("ParseCapacity read an unreadable init container's request as %+v, want it refused", got)
	}

	plan, err := Kubernetes{}.Render(goldenSpec())
	if err != nil {
		t.Fatalf("rendering the golden spec: %v", err)
	}
	var sidecar *Container
	for i := range plan.Job.Spec.Template.Spec.InitContainers {
		if plan.Job.Spec.Template.Spec.InitContainers[i].Name == dindContainerName {
			sidecar = &plan.Job.Spec.Template.Spec.InitContainers[i]
		}
	}
	if sidecar == nil {
		t.Fatalf("the Job renders no %q sidecar", dindContainerName)
	}
	got, err := amountOf(sidecar.Resources.Requests.CPU, sidecar.Resources.Requests.Memory)
	if err != nil {
		t.Fatalf("reading the sidecar's rendered requests: %v", err)
	}
	want, err := amountOf(dindCPU, dindMemory)
	if err != nil {
		t.Fatalf("reading the sidecar constants: %v", err)
	}
	if got != want {
		t.Errorf("the sidecar asks for %s, want the constants' %s", got, want)
	}
}

// TestAmountOfRefusesAQuantityItCannotRead holds the reader the sidecar share
// uses to the refusal on both resources: a quantity it cannot read is an error
// naming the resource, never a zero the share would silently lose.
func TestAmountOfRefusesAQuantityItCannotRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		cpu    string
		memory string
		want   string
	}{
		{name: "the CPU", cpu: "four", memory: "1Gi", want: "cpu"},
		{name: "the memory", cpu: "1", memory: "four", want: "memory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := amountOf(tc.cpu, tc.memory)
			if err == nil {
				t.Fatalf("amountOf(%q, %q) = %+v, want it refused", tc.cpu, tc.memory, got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestPodRequestCountsNativeSidecarsLikeTheScheduler: another pod's native
// sidecar (an init container with restartPolicy Always) keeps its request for the
// pod's whole life, so it ADDS to the sum of the containers, while an ordinary
// init container only competes as the largest of its kind, with every sidecar
// started before it running beside it (the scheduler's rule).
func TestPodRequestCountsNativeSidecarsLikeTheScheduler(t *testing.T) {
	t.Parallel()
	node := `{"kind": "Node", "metadata": {"name": "gt-control"}, "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "64", "memory": "256Gi"}}}`
	for _, tc := range []struct {
		name string
		pod  string
		want Amount
	}{
		{
			name: "a native sidecar adds to the containers",
			pod: `{"kind": "Pod", "metadata": {"name": "p"}, "spec": {"nodeName": "gt-control",
			  "initContainers": [{"restartPolicy": "Always", "resources": {"requests": {"cpu": "500m", "memory": "512Mi"}}}],
			  "containers": [{"resources": {"requests": {"cpu": "1", "memory": "1Gi"}}}]}, "status": {"phase": "Running"}}`,
			want: Amount{CPU: 1_500, Memory: 1<<30 + 512<<20},
		},
		{
			name: "an ordinary init container larger than the sum wins",
			pod: `{"kind": "Pod", "metadata": {"name": "p"}, "spec": {"nodeName": "gt-control",
			  "initContainers": [{"resources": {"requests": {"cpu": "4", "memory": "8Gi"}}}],
			  "containers": [{"resources": {"requests": {"cpu": "1", "memory": "1Gi"}}}, {"resources": {"requests": {"cpu": "1", "memory": "1Gi"}}}]}, "status": {"phase": "Running"}}`,
			want: Amount{CPU: 4_000, Memory: 8 << 30},
		},
		{
			name: "an ordinary init container after a sidecar runs beside it",
			pod: `{"kind": "Pod", "metadata": {"name": "p"}, "spec": {"nodeName": "gt-control",
			  "initContainers": [
			    {"restartPolicy": "Always", "resources": {"requests": {"cpu": "1", "memory": "1Gi"}}},
			    {"resources": {"requests": {"cpu": "3", "memory": "2Gi"}}}],
			  "containers": [{"resources": {"requests": {"cpu": "1", "memory": "1Gi"}}}]}, "status": {"phase": "Running"}}`,
			// containers 1/1Gi + sidecar 1/1Gi = 2/2Gi; the ordinary init peaks at
			// 3/2Gi + the running sidecar's 1/1Gi = 4/3Gi.
			want: Amount{CPU: 4_000, Memory: 3 << 30},
		},
		{
			name: "an ordinary init container before the sidecar does not carry it",
			pod: `{"kind": "Pod", "metadata": {"name": "p"}, "spec": {"nodeName": "gt-control",
			  "initContainers": [
			    {"resources": {"requests": {"cpu": "3", "memory": "2Gi"}}},
			    {"restartPolicy": "Always", "resources": {"requests": {"cpu": "1", "memory": "1Gi"}}}],
			  "containers": [{"resources": {"requests": {"cpu": "1", "memory": "1Gi"}}}]}, "status": {"phase": "Running"}}`,
			// the init ran alone at 3/2Gi; the steady sum is 2/2Gi.
			want: Amount{CPU: 3_000, Memory: 2 << 30},
		},
	} {
		nodes, err := ParseCapacity([]byte(`{"items": [` + node + `,` + tc.pod + `]}`))
		if err != nil {
			t.Fatalf("%s: ParseCapacity: %v", tc.name, err)
		}
		if got := nodes[0].Requested; got != tc.want {
			t.Errorf("%s: the pod requests %s, want %s", tc.name, got, tc.want)
		}
	}

	// The rendered Job pod follows the same rule: the dind sidecar is a native
	// sidecar, so an ordinary init container beside it counts as the largest of
	// its kind and the sidecar adds to the runner's share.
	pod := PodSpec{
		InitContainers: []Container{
			{Name: "dind", RestartPolicy: "Always", Resources: &Resources{Requests: ResourceList{CPU: "2", Memory: "4Gi"}}},
			{Name: "prep", Resources: &Resources{Requests: ResourceList{CPU: "5", Memory: "1Gi"}}},
		},
		Containers: []Container{{Name: "runner", Resources: &Resources{Requests: ResourceList{CPU: "2", Memory: "1Gi"}}}},
	}
	got, err := PodRequestTotal(pod)
	if err != nil {
		t.Fatalf("PodRequestTotal: %v", err)
	}
	// sum 4/5Gi; prep peaks at 5+2 = 7 CPUs and 1+4 = 5Gi.
	if want := (Amount{CPU: 7_000, Memory: 5 << 30}); got != want {
		t.Errorf("the rendered pod asks for %s, want %s", got, want)
	}
}

// TestCapacityNoExecuteTaintBlocksPlacement: a NoExecute taint the Job does not
// tolerate keeps a new pod off the node as a NoSchedule one does, and a
// toleration names the effect it covers.
func TestCapacityNoExecuteTaintBlocksPlacement(t *testing.T) {
	t.Parallel()
	nodes, err := ParseCapacity([]byte(`{"items": [{"kind": "Node", "metadata": {"name": "gt-control"},
	  "spec": {"taints": [{"key": "dedicated", "value": "ci", "effect": "NoExecute"}]},
	  "status": {"conditions": [{"type": "Ready", "status": "True"}], "allocatable": {"cpu": "16", "memory": "64Gi"}}}]}`))
	if err != nil {
		t.Fatalf("ParseCapacity: %v", err)
	}
	if schedulable(nodes[0], nil) {
		t.Error("a node with a NoExecute taint the Job does not tolerate is schedulable")
	}
	if schedulable(nodes[0], []Toleration{{Key: "dedicated", Operator: "Equal", Value: "ci", Effect: "NoSchedule"}}) {
		t.Error("a NoSchedule toleration tolerates a NoExecute taint")
	}
	if !schedulable(nodes[0], []Toleration{{Key: "dedicated", Operator: "Equal", Value: "ci", Effect: "NoExecute"}}) {
		t.Error("a node whose NoExecute taint the Job tolerates is not schedulable")
	}
	if !schedulable(nodes[0], []Toleration{{Key: "dedicated", Operator: "Exists"}}) {
		t.Error("an Exists toleration with no effect does not tolerate the NoExecute taint")
	}
}
