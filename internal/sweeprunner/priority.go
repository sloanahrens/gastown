package sweeprunner

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// controlPlaneClass is the class a protected control-plane workload must run
// at: above every class the sweep may run at, so the scheduler has the sweep to
// shed before it needs one of these pods (deploy/k8s/priority-classes.yaml).
const controlPlaneClass = "gt-control-plane"

// runnerGateClass is the class the Forgejo runner Deployment runs at: above the
// sweep, so a sweep filling the node cannot displace the runner of the Forgejo
// that schedules the landing gates.
const runnerGateClass = "gt-ci-gate"

// ControlPriority is one row of the expected-priorities table: the class a
// control-plane workload's pods must run at.
type ControlPriority struct {
	// Namespace is the namespace the Deployment's pods run in.
	Namespace string
	// Deployment is the Deployment the pods belong to; a pod belongs to it
	// when its owner's name is the Deployment's name or starts with it.
	Deployment string
	// Class is the PriorityClass the Deployment's pods must name.
	Class string
}

// ControlPriorities is the table of the control plane's live workloads and the
// priority class each runs at, verified 2026-11-05 after the overseer patched
// both Forgejo Deployments. The repo's record of it is
// deploy/k8s/control-priorities.yaml, which TestPriorityTableMatchesTheLiveDeployments
// holds equal to this table: the sweep's preflight compares the live pods
// against this one, and the file is what a reader and a reviewer check.
//
// The Forgejo Deployments are not in any repo, so the table is the only place
// the expected classes are recorded.
var ControlPriorities = []ControlPriority{
	{Namespace: "forgejo", Deployment: "forgejo", Class: controlPlaneClass},
	{Namespace: "forgejo-trial", Deployment: "forgejo", Class: controlPlaneClass},
	{Namespace: "forgejo", Deployment: "forgejo-runner", Class: runnerGateClass},
}

// PriorityGuard is the preflight that refuses a launch while a protected
// workload's pod runs at or below the sweep's priority, or while a live pod's
// priority differs from the table's row for it. A node with no room sheds the
// sweep first; a pod the sweep outranks is one the scheduler may displace for
// it, which is what took Forgejo down on 2026-11-05 (gt-gg9hv.39).
//
// One kubectl call reads the pods across every namespace and the PriorityClass
// objects the sweep compares against: the sweep's own priority is the value of
// the class its Job names, read from the cluster rather than written here.
func PriorityGuard(spec Spec, protected []string) Guard {
	return Guard{
		Step:   Step{Argv: kubectlArgv(spec, "get", "pods,priorityclasses", "-A", "-o", "json")},
		Refuse: refuseControlPriorities(protected),
	}
}

// refuseControlPriorities reads the guard's listing of the pods and the
// PriorityClasses and reports the refusal: a protected pod at or below the
// sweep's priority, or a live pod at another priority than its table row's
// class. A listing it cannot read, a class it cannot find and a pod whose
// priority it cannot see all refuse too: a preflight that did not answer is not
// one that passed.
func refuseControlPriorities(protected []string) func(stdout []byte) error {
	return func(stdout []byte) error {
		// A guard whose namespaces were not wired checks no pod, so it refuses
		// rather than passing: the setting has a default, so reaching here with
		// none is a config the daemon did not build.
		if len(protected) == 0 {
			return newPriorityDecline("not launching: no protected namespace is configured (tier_sweep.runner.protected_namespaces)")
		}
		listing, err := readPriorityListing(stdout)
		if err != nil {
			return newPriorityDecline("not launching: reading the pods' priorities: %v", err)
		}
		// The class value is read from the class the Job names, so an operator
		// who moves the sweep to another class moves this check with it.
		sweep, ok := listing.classes[PriorityClass]
		if !ok {
			return newPriorityDecline("not launching: the cluster lists no PriorityClass %q, the class the sweep's Job names", PriorityClass)
		}
		if decline := refuseProtectedPods(listing.pods, protected, sweep); decline != nil {
			return decline
		}
		// A nil *PriorityDecline returned as an error is a non-nil error, so
		// the pass is returned as an untyped nil.
		if decline := refusePriorityTable(listing, sweep); decline != nil {
			return decline
		}
		return nil
	}
}

// refuseProtectedPods names the first protected pod (in namespace and name
// order) that runs at or below the sweep's priority: the sweep would outrank
// it, so the scheduler could shed it to make room for the run.
func refuseProtectedPods(pods []listedPod, protected []string, sweep int) *PriorityDecline {
	var offenders []listedPod
	for _, pod := range pods {
		if !slices.Contains(protected, pod.namespace) {
			continue
		}
		// A pod whose priority the listing omits is not a pod known to be
		// above the sweep: the guard refuses rather than guessing.
		if pod.priority == nil {
			return newPriorityDecline("not launching: pod %s/%s names no priority, so it cannot be read as above the sweep's %d",
				pod.namespace, pod.name, sweep)
		}
		if *pod.priority <= sweep {
			offenders = append(offenders, pod)
		}
	}
	if len(offenders) == 0 {
		return nil
	}
	slices.SortFunc(offenders, func(a, b listedPod) int {
		return cmp.Or(cmp.Compare(a.namespace, b.namespace), cmp.Compare(a.name, b.name))
	})
	pod := offenders[0]
	return newPriorityDecline("not launching: pod %s/%s runs at priority %d, not above the sweep's %d; set priorityClassName %s",
		pod.namespace, pod.name, *pod.priority, sweep, controlPlaneClass)
}

// refusePriorityTable refuses while the table names a class the cluster does
// not list, a class at or below the sweep's, or a live pod whose priority is
// not the value of the class the table records for its Deployment: the cluster
// has drifted from the priorities the repo says are live, and the sweep cannot
// trust that a pod it outranks was ever meant to be safe from it.
func refusePriorityTable(listing priorityListing, sweep int) *PriorityDecline {
	if err := checkPriorityTable(ControlPriorities, listing.classes, sweep); err != nil {
		return newPriorityDecline("not launching: %v", err)
	}
	for _, pod := range listing.pods {
		row, ok := tableRow(pod)
		if !ok {
			continue
		}
		want := listing.classes[row.Class]
		if pod.priority == nil {
			return newPriorityDecline("not launching: pod %s/%s names no priority, so it cannot be checked against the table's %q",
				pod.namespace, pod.name, row.Class)
		}
		if *pod.priority != want {
			return newPriorityDecline("not launching: pod %s/%s runs at priority %d, not the %d the table records for deployment %s; set priorityClassName %s",
				pod.namespace, pod.name, *pod.priority, want, row.Deployment, row.Class)
		}
	}
	return nil
}

// checkPriorityTable reports the first row whose class the cluster does not
// list, or whose class runs at or below the sweep's, and nil when every row is
// a control-plane class above the sweep. The checked-in table is what
// deploy/k8s/control-priorities.yaml must hold to.
func checkPriorityTable(rows []ControlPriority, classes map[string]int, sweep int) error {
	for _, row := range rows {
		value, ok := classes[row.Class]
		if !ok {
			return fmt.Errorf("the cluster lists no PriorityClass %q, the class the table records for %s/%s",
				row.Class, row.Namespace, row.Deployment)
		}
		if value <= sweep {
			return fmt.Errorf("the table's class %q for %s/%s runs at priority %d, not above the sweep's %d",
				row.Class, row.Namespace, row.Deployment, value, sweep)
		}
	}
	return nil
}

// tableRow is the table row a pod belongs to: the longest Deployment name in
// the pod's namespace that owns it, so the runner's pods are read against the
// runner's row and not the Forgejo Deployment whose name they also start with.
func tableRow(pod listedPod) (ControlPriority, bool) {
	var best ControlPriority
	found := false
	for _, row := range ControlPriorities {
		if row.Namespace != pod.namespace || !pod.ownedBy(row.Deployment) {
			continue
		}
		if !found || len(row.Deployment) > len(best.Deployment) {
			best, found = row, true
		}
	}
	return best, found
}

// ownedBy reports whether a Deployment owns the pod: the pod's owner (the
// Deployment's ReplicaSet) is named after the Deployment, with a generated
// suffix when the cluster adds one.
func (p listedPod) ownedBy(deployment string) bool {
	for _, owner := range p.owners {
		if owner.Name == deployment || strings.HasPrefix(owner.Name, deployment+"-") {
			return true
		}
	}
	return false
}

// priorityListing is what the priority guard's kubectl call answers: every
// pod's namespace and priority, and the PriorityClass values, from one listing.
type priorityListing struct {
	pods    []listedPod
	classes map[string]int
}

// listedPod is one pod the listing names: where it runs, its live priority (nil
// when the listing omits it) and its owners.
type listedPod struct {
	namespace string
	name      string
	priority  *int
	owners    []ownerRef
}

// ownerRef is an object's owner as the listing names it.
type ownerRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// readPriorityListing parses the guard's kubectl output: a List of pods and
// PriorityClasses, or a stream of single objects for a kubectl that prints a
// multi-type get that way. Both are read, because a listing the parser cannot
// follow would refuse every launch.
func readPriorityListing(stdout []byte) (priorityListing, error) {
	listing := priorityListing{classes: make(map[string]int)}
	dec := json.NewDecoder(bytes.NewReader(stdout))
	for {
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return priorityListing{}, fmt.Errorf("the listing is not JSON: %w", err)
		}
		var head struct {
			Kind  string            `json:"kind"`
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return priorityListing{}, fmt.Errorf("the listing is not JSON: %w", err)
		}
		if head.Kind != "List" {
			if err := listing.add(raw); err != nil {
				return priorityListing{}, err
			}
			continue
		}
		for _, item := range head.Items {
			if err := listing.add(item); err != nil {
				return priorityListing{}, err
			}
		}
	}
	return listing, nil
}

// add reads one listed object: a Pod joins the listing's pods, a PriorityClass
// its classes. Another kind is ignored, so a kubectl that answers more than was
// asked does not refuse the launch.
func (l *priorityListing) add(raw json.RawMessage) error {
	var item struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			Priority        *int       `json:"priority"`
			OwnerReferences []ownerRef `json:"ownerReferences"`
		} `json:"spec"`
		Value *int `json:"value"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return fmt.Errorf("the listing holds an item that is not an object: %w", err)
	}
	switch item.Kind {
	case "Pod":
		// A pod the listing does not place in a namespace could be in a
		// protected one: it is read as a listing the guard cannot follow, not
		// skipped, because a skipped pod is one the check silently does not
		// make.
		if item.Metadata.Namespace == "" || item.Metadata.Name == "" {
			return errors.New("the listing holds a pod with no namespace or name")
		}
		l.pods = append(l.pods, listedPod{
			namespace: item.Metadata.Namespace,
			name:      item.Metadata.Name,
			priority:  item.Spec.Priority,
			owners:    item.Spec.OwnerReferences,
		})
	case "PriorityClass":
		if item.Value != nil {
			l.classes[item.Metadata.Name] = *item.Value
		}
	}
	return nil
}

// PriorityDecline is the preflight's refusal: the launch was declined, not
// attempted and failed, so the cycle can defer instead of going red and the
// daemon can repeat the line at most once an hour while the reason stands.
type PriorityDecline struct {
	line string
}

// newPriorityDecline is a refusal whose sentence says which pod, which priority
// and what the operator sets to fix it.
func newPriorityDecline(format string, args ...any) *PriorityDecline {
	return &PriorityDecline{line: fmt.Sprintf(format, args...)}
}

// Error is the refusal as the launch reports it.
func (d *PriorityDecline) Error() string { return d.line }

// Line is the refusal as the daemon logs it: the sweep's log prefix and the
// sentence naming the pod and the fix.
func (d *PriorityDecline) Line() string { return "tier_sweep: " + d.line }

// declineRepeat is how often a standing decline repeats its line: often enough
// that an operator reading the daemon's log sees it, rare enough that a cycle
// every few minutes does not fill the log with one cause.
const declineRepeat = time.Hour

// DeclineLog reports a standing decline's line at most once in declineRepeat:
// the first report, a report after an hour, and a report of a changed line are
// all logged; the same line inside the hour is not.
type DeclineLog struct {
	last    time.Time
	message string
}

// Report returns whether the daemon should log message now, and records the
// report when it should.
func (l *DeclineLog) Report(now time.Time, message string) bool {
	if message == l.message && now.Sub(l.last) < declineRepeat {
		return false
	}
	l.last, l.message = now, message
	return true
}
