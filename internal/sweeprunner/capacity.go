package sweeprunner

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// The room a launch must leave on its node beside itself: a Forgejo pod
// replacement and the registry forwarder (gt-gg9hv.38).
const (
	sweepHeadroomCPU    = int64(2 * 1000) // milli-CPU
	sweepHeadroomMemory = int64(2 << 30)  // bytes
)

// Amount is a quantity of CPU and memory, in milli-CPU and bytes so the
// quantities kubectl prints ("6210m", "14492Mi") compare exactly, without a
// float that could round a request down.
type Amount struct {
	CPU    int64 // milli-CPU
	Memory int64 // bytes
}

// Add returns the sum of a and b.
func (a Amount) Add(b Amount) Amount {
	return Amount{CPU: a.CPU + b.CPU, Memory: a.Memory + b.Memory}
}

// Sub returns a less b. A negative result is meaningful: a node whose pods
// request more than it allocates has less than nothing free.
func (a Amount) Sub(b Amount) Amount {
	return Amount{CPU: a.CPU - b.CPU, Memory: a.Memory - b.Memory}
}

// String is the amount as the daemon's capacity lines show it: milli-CPU and
// MiB.
func (a Amount) String() string {
	return fmt.Sprintf("%dm/%dMi", a.CPU, a.Memory>>20)
}

// Taint is a node taint with the NoSchedule effect: a pod the node does not
// tolerate is kept off it (gt-gg9hv.38).
type Taint struct {
	Key   string
	Value string
}

// NodeCapacity is one node of the listing: what it allocates, the sum of the
// requests of the pods placed on it, and whether the cluster would place a new
// pod on it at all.
type NodeCapacity struct {
	Node        string
	Allocatable Amount
	Requested   Amount
	// Ready is the node's Ready condition. A listing that states anything but
	// True, or names no Ready condition at all, leaves it false: a node whose
	// health the listing does not state is not a node to launch on.
	Ready bool
	// Unschedulable is a node the cluster has cordoned: no new pod is placed
	// on it.
	Unschedulable bool
	// NoSchedule are the node's NoSchedule taints. A pod may be placed on the
	// node only when it tolerates every one of them.
	NoSchedule []Taint
}

// Free is the node's allocatable amount less what its pods request.
func (n NodeCapacity) Free() Amount { return n.Allocatable.Sub(n.Requested) }

// CapacityReport is what a capacity preflight read for one launch: the node the
// launch was judged against or, on a decline, the best-fitting node; its
// allocatable and requested amounts; and the amount the run itself asks for.
type CapacityReport struct {
	Node        string
	Allocatable Amount
	Requested   Amount
	Job         Amount
}

// Free is the node's allocatable amount less what its pods request.
func (r CapacityReport) Free() Amount { return r.Allocatable.Sub(r.Requested) }

// String is the report as the daemon's launch line shows it: the node the
// launch was judged against, its allocatable and requested amounts, and the
// run's own request.
func (r CapacityReport) String() string {
	return fmt.Sprintf("node %s: allocatable %s, requested %s, job %s", r.Node, r.Allocatable, r.Requested, r.Job)
}

// Decline is the report as the daemon's decline line shows it: the
// best-fitting node, the free room there, what the run needs, and the headroom
// the launch must leave beside it.
func (r CapacityReport) Decline() string {
	headroom := Amount{CPU: sweepHeadroomCPU, Memory: sweepHeadroomMemory}
	return fmt.Sprintf("node %s: free %s, job needs %s, headroom %s", r.Node, r.Free(), r.Job, headroom)
}

// NoRoomError is a preflight's refusal because no node has room for the run. A
// caller can tell it from a listing it could not read: this one is the standing
// fact of a cluster that is short, and its figures may change from one check to
// the next while it still says the same thing.
type NoRoomError struct {
	// Reason is the node and its figures, or why no node was eligible.
	Reason string
}

// Error is the decline line's content.
func (e *NoRoomError) Error() string { return e.Reason }

// Preflight is the capacity check a caller runs before a Plan applies anything:
// the program that lists the engine's nodes and pods, and how its listing is
// read into a decision. A caller declines on any error Read returns: a check
// that did not answer is not a pass (gt-gg9hv.38).
type Preflight struct {
	// Step is the program that reads the engine's capacity, run directly like
	// any step. The Kubernetes backend's is one kubectl call for every node and
	// pod.
	Step Step
	// Read turns Step's stdout into the report a caller logs, or the error that
	// declines the launch.
	Read func(stdout []byte) (CapacityReport, error)
}

// capacityRead is the Read for a launch that creates the given Job, sync pod
// and reader pod and tolerates the given taints: it sums what those objects ask
// for, reads kubectl's node and pod listing, and decides whether at least one
// eligible node fits the run beside what that node already carries. A request
// it cannot read is a decline, never a run sized from zero.
func capacityRead(job *Job, syncPod, reader *Pod, tolerations []Toleration) func(stdout []byte) (CapacityReport, error) {
	return func(stdout []byte) (CapacityReport, error) {
		request, err := jobRequest(job, syncPod, reader)
		if err != nil {
			return CapacityReport{}, fmt.Errorf("cannot read the run's resource request: %w", err)
		}
		nodes, err := ParseCapacity(stdout)
		if err != nil {
			return CapacityReport{}, fmt.Errorf("cannot read the node's capacity: %w", err)
		}
		return DecideCapacity(nodes, request, tolerations)
	}
}

// ParseCapacity reads the document `kubectl get nodes,pods -A -o json` prints:
// one List whose items are every node and every pod of the cluster. Each node
// reports its allocatable amounts, whether its Ready condition is True, whether
// it is cordoned, and its NoSchedule taints; it carries the sum of the requests
// of the pods placed on it whose phase is not Succeeded or Failed. A pod's
// request is what the scheduler adds to a node for it: the pod's overhead, plus,
// per resource, the larger of the sum over its containers and the largest
// request of a single init container. A pod the scheduler has not placed asks
// no node for anything and is not counted. Anything else that cannot be read is
// an error: a request missed is a node that reads freer than it is.
func ParseCapacity(stdout []byte) ([]NodeCapacity, error) {
	var listing clusterListing
	if err := json.Unmarshal(stdout, &listing); err != nil {
		return nil, fmt.Errorf("reading the node and pod listing: %w", err)
	}
	byName := make(map[string]*NodeCapacity, len(listing.Items))
	var order []string
	var pods []listingItem
	for _, item := range listing.Items {
		switch item.Kind {
		case "Node":
			allocatable, err := allocatableAmount(item)
			if err != nil {
				return nil, err
			}
			if _, seen := byName[item.Metadata.Name]; seen {
				return nil, fmt.Errorf("the listing names node %q twice", item.Metadata.Name)
			}
			node := &NodeCapacity{
				Node:          item.Metadata.Name,
				Allocatable:   allocatable,
				Ready:         nodeReady(item.Status.Conditions),
				Unschedulable: item.Spec.Unschedulable,
			}
			for _, taint := range item.Spec.Taints {
				if taint.Effect == "NoSchedule" {
					node.NoSchedule = append(node.NoSchedule, Taint{Key: taint.Key, Value: taint.Value})
				}
			}
			byName[item.Metadata.Name] = node
			order = append(order, item.Metadata.Name)
		case "Pod":
			pods = append(pods, item)
		default:
			return nil, fmt.Errorf("the listing holds a %q, which is neither a node nor a pod", item.Kind)
		}
	}
	if len(order) == 0 {
		return nil, errors.New("the listing names no node")
	}
	for _, pod := range pods {
		if podTerminated(pod.Status.Phase) || pod.Spec.NodeName == "" {
			continue
		}
		node, ok := byName[pod.Spec.NodeName]
		if !ok {
			return nil, fmt.Errorf("pod %q is placed on node %q, which the listing does not name", pod.Metadata.Name, pod.Spec.NodeName)
		}
		request, err := podsRequest(pod)
		if err != nil {
			return nil, fmt.Errorf("pod %q: %w", pod.Metadata.Name, err)
		}
		node.Requested = node.Requested.Add(request)
	}
	nodes := make([]NodeCapacity, len(order))
	for i, name := range order {
		nodes[i] = *byName[name]
	}
	return nodes, nil
}

// DecideCapacity picks the node the launch is judged against and reports
// whether the launch may go ahead: it may when at least one eligible node fits
// the run beside the headroom the control plane needs. A node is eligible when
// it is Ready, not cordoned, and carries no NoSchedule taint the Job pod does
// not tolerate. An eligible node that does not fit stays in the judgment but
// does not block a node that does: the launch goes ahead when the best of them
// fits, and a decline names the best-fitting eligible node and its figures.
//
// The check judges placement; it does not pin the Job to the node it names. The
// scheduler may still choose another eligible node, which had room when it
// placed the pod; this check promises only that one eligible node had room at
// the read (gt-gg9hv.38).
func DecideCapacity(nodes []NodeCapacity, job Amount, tolerations []Toleration) (CapacityReport, error) {
	if len(nodes) == 0 {
		return CapacityReport{}, errors.New("cannot read the node's capacity: the listing names no node")
	}
	eligible := make([]NodeCapacity, 0, len(nodes))
	for _, node := range nodes {
		if schedulable(node, tolerations) {
			eligible = append(eligible, node)
		}
	}
	if len(eligible) == 0 {
		return CapacityReport{}, &NoRoomError{Reason: "no node the Job may be placed on: every node is not Ready, cordoned, or carries a NoSchedule taint the Job does not tolerate"}
	}
	chosen := eligible[0]
	for _, node := range eligible[1:] {
		if roomier(node, chosen, job) {
			chosen = node
		}
	}
	report := CapacityReport{
		Node:        chosen.Node,
		Allocatable: chosen.Allocatable,
		Requested:   chosen.Requested,
		Job:         job,
	}
	if !fitsBesideHeadroom(chosen, job) {
		return report, &NoRoomError{Reason: report.Decline()}
	}
	return report, nil
}

// schedulable reports whether the cluster would place a new pod on the node:
// the node is Ready, not cordoned, and the pod tolerates every NoSchedule taint
// on it. A NotReady node is not eligible even when it lists room: the cluster is
// not placing new pods on it.
func schedulable(node NodeCapacity, tolerations []Toleration) bool {
	if !node.Ready || node.Unschedulable {
		return false
	}
	for _, taint := range node.NoSchedule {
		if !tolerated(taint, tolerations) {
			return false
		}
	}
	return true
}

// nodeReady reports whether the listing's Ready condition is True. Anything
// else — False, Unknown, or no Ready condition at all — is not ready: a node
// whose health the listing does not state is not a node to launch on.
func nodeReady(conditions []nodeCondition) bool {
	for _, condition := range conditions {
		if condition.Type == "Ready" {
			return condition.Status == "True"
		}
	}
	return false
}

// tolerated reports whether one of the pod's tolerations matches the taint. A
// toleration naming an effect tolerates only that effect; an "Exists" operator
// matches any value of its key and, with no key, any taint at all. A taint the
// pod does not tolerate is what keeps it off the node.
func tolerated(taint Taint, tolerations []Toleration) bool {
	for _, toleration := range tolerations {
		if toleration.Effect != "" && toleration.Effect != "NoSchedule" {
			continue
		}
		if toleration.Operator == "Exists" {
			if toleration.Key == "" || toleration.Key == taint.Key {
				return true
			}
			continue
		}
		if toleration.Key == taint.Key && toleration.Value == taint.Value {
			return true
		}
	}
	return false
}

// roomier reports whether a is the node the launch should be judged against
// rather than b: a node the run fits beside wins over one it does not, then the
// most free CPU, then the most free memory, then the name, so the choice does
// not depend on the order the listing printed.
func roomier(a, b NodeCapacity, job Amount) bool {
	if fitsA, fitsB := fitsBesideHeadroom(a, job), fitsBesideHeadroom(b, job); fitsA != fitsB {
		return fitsA
	}
	fa, fb := a.Free(), b.Free()
	if fa.CPU != fb.CPU {
		return fa.CPU > fb.CPU
	}
	if fa.Memory != fb.Memory {
		return fa.Memory > fb.Memory
	}
	return a.Node < b.Node
}

// fitsBesideHeadroom reports whether the run fits on the node and still leaves
// the headroom.
func fitsBesideHeadroom(node NodeCapacity, job Amount) bool {
	free := node.Free()
	return free.CPU-job.CPU >= sweepHeadroomCPU && free.Memory-job.Memory >= sweepHeadroomMemory
}

// allocatableAmount is a node's allocatable CPU and memory. A node that lists
// neither is an error: with no allocatable reading the free amount would be a
// guess.
func allocatableAmount(item listingItem) (Amount, error) {
	for _, resource := range []string{"cpu", "memory"} {
		if _, ok := item.Status.Allocatable[resource]; !ok {
			return Amount{}, fmt.Errorf("node %q lists no %s allocatable", item.Metadata.Name, resource)
		}
	}
	amount, err := resourceAmount(item.Status.Allocatable)
	if err != nil {
		return Amount{}, fmt.Errorf("node %q: %w", item.Metadata.Name, err)
	}
	return amount, nil
}

// podsRequest is the amount the scheduler adds to a node for one pod: the pod's
// overhead plus, per resource, the larger of the sum over its containers and
// the largest request of a single init container. Init containers run one at a
// time, so their requests do not add up, and what the scheduler reserves is the
// largest one beside the containers.
func podsRequest(pod listingItem) (Amount, error) {
	overhead, err := resourceAmount(pod.Spec.Overhead)
	if err != nil {
		return Amount{}, fmt.Errorf("overhead: %w", err)
	}
	largestInit := Amount{}
	for _, container := range pod.Spec.InitContainers {
		amount, err := resourceAmount(container.Resources.Requests)
		if err != nil {
			return Amount{}, err
		}
		if amount.CPU > largestInit.CPU {
			largestInit.CPU = amount.CPU
		}
		if amount.Memory > largestInit.Memory {
			largestInit.Memory = amount.Memory
		}
	}
	containers := Amount{}
	for _, container := range pod.Spec.Containers {
		amount, err := resourceAmount(container.Resources.Requests)
		if err != nil {
			return Amount{}, err
		}
		containers = containers.Add(amount)
	}
	return overhead.Add(Amount{
		CPU:    max(largestInit.CPU, containers.CPU),
		Memory: max(largestInit.Memory, containers.Memory),
	}), nil
}

// podTerminated reports whether a pod's phase says it has ended. A pod whose
// phase is empty or unknown is not terminated: its request may still be held.
func podTerminated(phase string) bool {
	return phase == "Succeeded" || phase == "Failed"
}

// resourceAmount is a resource list's CPU and memory requests. A resource the
// list does not name asks for nothing, which is what the API means by an absent
// request; a quantity that cannot be read is an error.
func resourceAmount(quantities map[string]string) (Amount, error) {
	var amount Amount
	if value, ok := quantities["cpu"]; ok {
		cpu, err := parseQuantity(value, cpuMilli)
		if err != nil {
			return Amount{}, fmt.Errorf("cpu: %w", err)
		}
		amount.CPU = cpu
	}
	if value, ok := quantities["memory"]; ok {
		memory, err := parseQuantity(value, memoryBytes)
		if err != nil {
			return Amount{}, fmt.Errorf("memory: %w", err)
		}
		amount.Memory = memory
	}
	return amount, nil
}

// The unit a parsed quantity is reported in: milli-CPU, because a container's
// CPU request is often a fraction of a core, and bytes, because a byte of
// memory is already the finest amount anyone requests.
const (
	cpuMilli    = int64(1_000)
	memoryBytes = int64(1)
)

// PodRequestTotal is the amount a rendered pod asks for: the sum over every one
// of its containers, its init containers included. The dind sidecar is a native
// sidecar, whose share the scheduler adds beside the containers' rather than
// takes the largest of, so every rendered container's request adds up. Each
// container's request must be a readable quantity; an unreadable one is an
// error rather than a zero.
func PodRequestTotal(pod PodSpec) (Amount, error) {
	total := Amount{}
	for _, containers := range [][]Container{pod.InitContainers, pod.Containers} {
		for _, container := range containers {
			if container.Resources == nil {
				continue
			}
			amount, err := resourceAmount(map[string]string{
				"cpu":    container.Resources.Requests.CPU,
				"memory": container.Resources.Requests.Memory,
			})
			if err != nil {
				return Amount{}, fmt.Errorf("container %q: %w", container.Name, err)
			}
			total = total.Add(amount)
		}
	}
	return total, nil
}

// amountOf reads a rendered container's two request quantities into an Amount.
// It is the same reader the preflight uses, so a figure the renderer writes and
// the figure the preflight counts cannot take different forms (gt-gg9hv.38).
func amountOf(cpu, memory string) (Amount, error) {
	cpuMilliAmount, err := parseQuantity(cpu, cpuMilli)
	if err != nil {
		return Amount{}, fmt.Errorf("cpu: %w", err)
	}
	memoryBytesAmount, err := parseQuantity(memory, memoryBytes)
	if err != nil {
		return Amount{}, fmt.Errorf("memory: %w", err)
	}
	return Amount{CPU: cpuMilliAmount, Memory: memoryBytesAmount}, nil
}

// parseQuantity reads a Kubernetes resource quantity as the preflight accepts
// it: a plain integer or decimal, or one of the suffixes m (milli), Mi and Gi.
// The forms are the ones a node and pod listing uses; any other suffix, a
// fraction, an exponent, a sign, or anything else is refused with the input in
// the error, because a request read wrong is a node read freer than it is. A
// quantity that is not a whole unit rounds up, so a request never reads lower
// than it is.
func parseQuantity(value string, scale int64) (int64, error) {
	number, suffix := splitQuantity(value)
	if number == "" {
		return 0, fmt.Errorf("%q is not a quantity", value)
	}
	if strings.Contains(number, "/") {
		return 0, fmt.Errorf("%q is a fraction, which the preflight does not read", value)
	}
	if strings.ContainsAny(number, "+-") {
		return 0, fmt.Errorf("%q has a sign, which no quantity the preflight reads uses", value)
	}
	if strings.ContainsAny(number, "eE") {
		return 0, fmt.Errorf("%q is an exponent, which the preflight does not read", value)
	}
	mantissa, ok := new(big.Rat).SetString(number)
	if !ok {
		return 0, fmt.Errorf("%q is not a quantity", value)
	}
	multiplier, ok := quantityMultiplier(suffix)
	if !ok {
		return 0, fmt.Errorf("%q has a suffix the preflight does not read", value)
	}
	scaled := new(big.Rat).Mul(mantissa, multiplier)
	scaled.Mul(scaled, new(big.Rat).SetInt64(scale))
	whole, remainder := new(big.Int).QuoRem(scaled.Num(), scaled.Denom(), new(big.Int))
	if remainder.Sign() != 0 {
		whole.Add(whole, big.NewInt(1))
	}
	if !whole.IsInt64() {
		return 0, fmt.Errorf("%q is too large to read", value)
	}
	return whole.Int64(), nil
}

// splitQuantity splits a quantity into its number and the run of letters that
// ends it.
func splitQuantity(value string) (number, suffix string) {
	end := len(value)
	for end > 0 {
		c := value[end-1]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			end--
			continue
		}
		break
	}
	return value[:end], value[end:]
}

// quantityMultiplier is the factor an accepted quantity suffix multiplies its
// number by.
func quantityMultiplier(suffix string) (*big.Rat, bool) {
	switch suffix {
	case "":
		return big.NewRat(1, 1), true
	case "m":
		return big.NewRat(1, 1_000), true
	case "Mi":
		return big.NewRat(1<<20, 1), true
	case "Gi":
		return big.NewRat(1<<30, 1), true
	}
	return nil, false
}

// clusterListing is the document `kubectl get nodes,pods -A -o json` prints: one
// List whose items are the cluster's nodes and pods, each naming its own kind.
type clusterListing struct {
	Items []listingItem `json:"items"`
}

// listingItem is one item of the listing: a node's allocatable amounts,
// cordon and taints, or a pod's placement and the requests of its containers.
type listingItem struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		NodeName       string            `json:"nodeName"`
		Unschedulable  bool              `json:"unschedulable"`
		Taints         []nodeTaint       `json:"taints"`
		Overhead       map[string]string `json:"overhead"`
		InitContainers []podContainer    `json:"initContainers"`
		Containers     []podContainer    `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase       string            `json:"phase"`
		Allocatable map[string]string `json:"allocatable"`
		Conditions  []nodeCondition   `json:"conditions"`
	} `json:"status"`
}

// nodeCondition is one condition of a node's status as the API prints it. The
// preflight reads only the Ready condition.
type nodeCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

// nodeTaint is one node taint as the API prints it.
type nodeTaint struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Effect string `json:"effect"`
}

// podContainer is a container's resources in the listing.
type podContainer struct {
	Resources struct {
		Requests map[string]string `json:"requests"`
	} `json:"resources"`
}
