package sweeprunner

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
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

// NodeCapacity is one node of the listing: what it allocates, and the sum of
// the requests of the pods placed on it.
type NodeCapacity struct {
	Node        string
	Allocatable Amount
	Requested   Amount
}

// Free is the node's allocatable amount less what its pods request.
func (n NodeCapacity) Free() Amount { return n.Allocatable.Sub(n.Requested) }

// CapacityReport is what a capacity preflight read for one launch: the node the
// launch was judged against, its allocatable and requested amounts, and the
// amount the run itself asks for.
type CapacityReport struct {
	Node        string
	Allocatable Amount
	Requested   Amount
	Job         Amount
}

// Free is the chosen node's allocatable amount less what its pods request.
func (r CapacityReport) Free() Amount { return r.Allocatable.Sub(r.Requested) }

// String is the report as the daemon's launch line shows it: the node's
// allocatable and requested amounts, and the run's own request.
func (r CapacityReport) String() string {
	return fmt.Sprintf("allocatable %s, requested %s, job %s", r.Allocatable, r.Requested, r.Job)
}

// Decline is the report as the daemon's decline line shows it: the free room,
// what the run needs, and the headroom the launch must leave beside it.
func (r CapacityReport) Decline() string {
	headroom := Amount{CPU: sweepHeadroomCPU, Memory: sweepHeadroomMemory}
	return fmt.Sprintf("free %s, job needs %s, headroom %s", r.Free(), r.Job, headroom)
}

// NoRoomError is a preflight's refusal because the node has no room for the
// run. A caller can tell it from a listing it could not read: this one is the
// standing fact of a node that is short, and its figures may change from one
// check to the next while it still says the same thing.
type NoRoomError struct {
	// Reason is the free amount, what the run needs and the headroom.
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

// capacityRead is the Read for a run that asks the node for job: it reads
// kubectl's node and pod listing, and decides whether the run fits beside what
// the node already carries.
func capacityRead(job Amount) func(stdout []byte) (CapacityReport, error) {
	return func(stdout []byte) (CapacityReport, error) {
		nodes, err := ParseCapacity(stdout)
		if err != nil {
			return CapacityReport{}, fmt.Errorf("cannot read the node's capacity: %w", err)
		}
		return decideCapacity(nodes, job)
	}
}

// ParseCapacity reads the document `kubectl get nodes,pods -A -o json` prints:
// one List whose items are every node and every pod of the cluster. Each node
// reports its allocatable amounts and the sum of the requests of the pods
// placed on it whose phase is not Succeeded or Failed. A pod's request is the
// sum over its containers, its init containers included, and a container that
// declares no request asks for nothing. A pod the scheduler has not placed asks
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
			byName[item.Metadata.Name] = &NodeCapacity{Node: item.Metadata.Name, Allocatable: allocatable}
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

// decideCapacity picks the node the launch is judged against and reports
// whether the run fits there with the headroom the control plane needs. The
// chosen node is the one that fits with the most free CPU, or, when none fits,
// the one with the most free CPU, so a decline names the roomiest node there is
// and a launch names the node the scheduler will pick.
func decideCapacity(nodes []NodeCapacity, job Amount) (CapacityReport, error) {
	if len(nodes) == 0 {
		return CapacityReport{}, errors.New("cannot read the node's capacity: the listing names no node")
	}
	chosen := nodes[0]
	for _, node := range nodes[1:] {
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

// podsRequest is a pod's request: the sum over its init containers and its
// containers.
func podsRequest(pod listingItem) (Amount, error) {
	total := Amount{}
	for _, containers := range [][]podContainer{pod.Spec.InitContainers, pod.Spec.Containers} {
		for _, container := range containers {
			amount, err := resourceAmount(container.Resources.Requests)
			if err != nil {
				return Amount{}, err
			}
			total = total.Add(amount)
		}
	}
	return total, nil
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
// of its containers, its init containers included, so a sidecar's share is part
// of the pod's. Each container's request must be a readable quantity; an
// unreadable one is an error rather than a zero.
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

// parseQuantity reads a Kubernetes resource quantity in units of scale: the
// base unit times scale, so CPU is read in milli-CPU and memory in bytes. The
// suffixes are the API's own: the decimal ones (n, u, m, k, M, G, T, P, E), the
// binary ones (Ki, Mi, Gi, Ti, Pi, Ei) and a plain or exponent number. A
// quantity that is empty, negative, or not one of those is an error; a quantity
// that is not a whole unit rounds up, so a request never reads lower than it is.
func parseQuantity(value string, scale int64) (int64, error) {
	number, suffix := splitQuantity(value)
	if number == "" {
		return 0, fmt.Errorf("%q is not a quantity", value)
	}
	mantissa, ok := new(big.Rat).SetString(number)
	if !ok {
		return 0, fmt.Errorf("%q is not a quantity", value)
	}
	if mantissa.Sign() < 0 {
		return 0, fmt.Errorf("%q is a negative quantity", value)
	}
	multiplier, ok := quantityMultiplier(suffix)
	if !ok {
		return 0, fmt.Errorf("%q has a suffix the API does not use", value)
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
// ends it. An exponent keeps its "e" in the number because a digit follows it,
// so "1e3" is a number and "1Ei" is a number with the "Ei" suffix.
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

// quantityMultiplier is the factor a quantity suffix multiplies its number by,
// as the API defines them.
func quantityMultiplier(suffix string) (*big.Rat, bool) {
	switch suffix {
	case "":
		return big.NewRat(1, 1), true
	case "n":
		return big.NewRat(1, 1_000_000_000), true
	case "u":
		return big.NewRat(1, 1_000_000), true
	case "m":
		return big.NewRat(1, 1_000), true
	case "k":
		return big.NewRat(1_000, 1), true
	case "M":
		return big.NewRat(1_000_000, 1), true
	case "G":
		return big.NewRat(1_000_000_000, 1), true
	case "T":
		return big.NewRat(1_000_000_000_000, 1), true
	case "P":
		return big.NewRat(1_000_000_000_000_000, 1), true
	case "E":
		return big.NewRat(1_000_000_000_000_000_000, 1), true
	case "Ki":
		return big.NewRat(1<<10, 1), true
	case "Mi":
		return big.NewRat(1<<20, 1), true
	case "Gi":
		return big.NewRat(1<<30, 1), true
	case "Ti":
		return big.NewRat(1<<40, 1), true
	case "Pi":
		return big.NewRat(1<<50, 1), true
	case "Ei":
		return big.NewRat(1<<60, 1), true
	}
	return nil, false
}

// clusterListing is the document `kubectl get nodes,pods -A -o json` prints: one
// List whose items are the cluster's nodes and pods, each naming its own kind.
type clusterListing struct {
	Items []listingItem `json:"items"`
}

// listingItem is one item of the listing: a node's allocatable amounts, or a
// pod's placement and the requests of its containers.
type listingItem struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		NodeName       string         `json:"nodeName"`
		InitContainers []podContainer `json:"initContainers"`
		Containers     []podContainer `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase       string            `json:"phase"`
		Allocatable map[string]string `json:"allocatable"`
	} `json:"status"`
}

// podContainer is a container's resources in the listing.
type podContainer struct {
	Resources struct {
		Requests map[string]string `json:"requests"`
	} `json:"resources"`
}
