package daemon

// A sandbox pod the scheduler cannot place never starts its agent, and nothing
// else in the queue says so: the seat is alive, its bead is claimed, and the
// only trace is the cluster's own event (gt-1gtq5g). The collector here reads
// the sandbox pods of the town's kubernetes profiles and raises one item per
// pod that stayed Pending and unschedulable past sandboxPendingAfter, carrying
// the scheduler's message verbatim.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	agentconfig "github.com/steveyegge/gastown/internal/config"
)

const (
	// sandboxPendingAfter is how long a pod may stay Pending and unschedulable
	// before it raises an item: a pod placed a moment late is not news, and a
	// minute is longer than a healthy schedule takes (gt-1gtq5g).
	sandboxPendingAfter = time.Minute

	// sandboxPendingKeyPrefix is the item key's prefix; the pod's namespace and
	// name follow it, so one pod has one item however many ticks it stays
	// unschedulable.
	sandboxPendingKeyPrefix = "sandbox-pending:"

	// sandboxPendingReadTimeout bounds one namespace's pod read. A sandbox
	// cluster that does not answer (a VM restart, gt-hlo2ac) costs this read
	// and not the collectors behind it in the attention tick.
	sandboxPendingReadTimeout = 15 * time.Second

	// sandboxPendingRequestTimeout is kubectl's own bound on the same read, so
	// the API server gives up before the daemon does. It is passed for this
	// read only.
	sandboxPendingRequestTimeout = 10 * time.Second
)

// collectPendingSandboxPods raises one high item per sandbox pod that has been
// Pending and unschedulable for longer than sandboxPendingAfter, in every
// namespace the town's kubernetes sandbox profiles run pods in. The item names
// the pod, its age and the scheduler's message verbatim, so the overseer reads
// what the pod cannot get and never a guess at it. A pod that leaves Pending or
// disappears raises nothing, which clears its item.
//
// A pod Pending for another reason is not this item: an image pull has the pod
// scheduled already (PodScheduled is True), so only the scheduler's own
// Unschedulable refusal counts here. Every Unschedulable pod in the namespace
// counts, not only the seats' sandboxes: a pod that is not a sandbox but cannot
// schedule on the sandbox pool is still a capacity signal.
//
// A profile read or a cluster read that fails returns the error and no items,
// so the previous tick's items are kept rather than cleared by an unanswered
// read, and the next tick reads again.
func (s *attentionSources) collectPendingSandboxPods(ctx context.Context) ([]attention.Item, error) {
	d := s.d
	seams := d.diskMaintenanceSeams()
	profiles, err := seams.sandboxes()
	if err != nil {
		return nil, fmt.Errorf("reading the town's sandbox profiles: %w", err)
	}
	kubeconfig := diskMaintKubeconfig(d.config.TownRoot, seams.getenv)
	// The first-seen map is replaced only when every namespace read succeeds:
	// a failed read keeps the previous map, and a full read prunes it to the
	// pods it listed, so a pod that left or was recreated starts a new window.
	prev := d.sandboxPendingFirstSeen
	next := make(map[string]time.Time)
	var out []attention.Item
	for _, namespace := range sandboxPendingNamespaces(profiles) {
		items, err := d.pendingSandboxPods(ctx, kubeconfig, namespace, s.now, prev, next)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	d.sandboxPendingFirstSeen = next
	return out, nil
}

// sandboxPendingNamespaces is the distinct namespaces the kubernetes profiles'
// sandbox pods run in, sorted so each is read once in a fixed order. A profile
// that does not use the kubernetes driver has no pod in the cluster and names
// none, so a town with no kubernetes profile reads no pod and raises nothing.
func sandboxPendingNamespaces(profiles map[string]agentconfig.SandboxProfile) []string {
	seen := make(map[string]bool)
	var namespaces []string
	for _, profile := range profiles {
		if profile.Driver != agentconfig.SandboxDriverKubernetes {
			continue
		}
		if namespace := profile.PodNamespace(); !seen[namespace] {
			seen[namespace] = true
			namespaces = append(namespaces, namespace)
		}
	}
	slices.Sort(namespaces)
	return namespaces
}

// pendingSandboxPod is the part of a pod the unschedulable read decides on.
type pendingSandboxPod struct {
	Metadata struct {
		Name              string `json:"name"`
		Namespace         string `json:"namespace"`
		CreationTimestamp string `json:"creationTimestamp"`
	} `json:"metadata"`
	Status struct {
		Phase      string                    `json:"phase"`
		Conditions []pendingSandboxCondition `json:"conditions"`
	} `json:"status"`
}

// pendingSandboxCondition is one pod condition, as the pod reports it.
type pendingSandboxCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// pendingSandboxUnplaced returns the scheduler's message when the pod is
// Pending and its PodScheduled condition is False for reason Unschedulable,
// and whether it is. A Pending pod without that condition was not judged
// unschedulable by the scheduler, and is left alone.
func pendingSandboxUnplaced(pod pendingSandboxPod) (string, bool) {
	if pod.Status.Phase != "Pending" {
		return "", false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == "PodScheduled" && c.Status == "False" && c.Reason == "Unschedulable" {
			return c.Message, true
		}
	}
	return "", false
}

// errSandboxPendingNoAnswer is the cause a read carries when its deadline ends
// it before the cluster answers.
var errSandboxPendingNoAnswer = fmt.Errorf("no answer within %s", sandboxPendingReadTimeout)

// pendingSandboxRead reads namespace's pod list under sandboxPendingReadTimeout.
// The deadline runs on the daemon's clock and cancels the read's context, and
// the read is abandoned at that point whether or not the runner honors its
// context, so one hung cluster costs one read (gt-hlo2ac). A read that outlives
// its deadline returns an error, and the caller keeps the previous items.
//
// An abandoned runner goroutine stays alive until the runner returns; that is
// the one bound on the leak. Until then the namespace's read is outstanding, and
// a later tick does not start a second read of it: it returns an error and keeps
// the previous items, so a hung cluster holds at most one goroutine per namespace.
func (d *Daemon) pendingSandboxRead(ctx context.Context, kubeconfig, namespace string) ([]byte, error) {
	if outstanding, ok := d.sandboxPendingReads[namespace]; ok {
		select {
		case <-outstanding:
		default:
			return nil, fmt.Errorf("the pod list for namespace %s is still outstanding from an earlier tick", namespace)
		}
	}
	if d.sandboxPendingReads == nil {
		d.sandboxPendingReads = make(map[string]chan struct{})
	}
	returned := make(chan struct{})
	d.sandboxPendingReads[namespace] = returned

	readCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	deadline := d.clk().AfterFunc(sandboxPendingReadTimeout, func() { cancel(errSandboxPendingNoAnswer) })
	defer deadline.Stop()
	type reply struct {
		out []byte
		err error
	}
	done := make(chan reply, 1)
	go func() {
		defer close(returned)
		out, err := d.cacheTrimKubectl(readCtx, kubeconfig, "get", "pods", "-n", namespace, "-o", "json", fmt.Sprintf("--request-timeout=%s", sandboxPendingRequestTimeout))
		done <- reply{out, err}
	}()
	select {
	case r := <-done:
		return r.out, r.err
	case <-readCtx.Done():
		return nil, fmt.Errorf("reading the pod list for namespace %s: %w", namespace, context.Cause(readCtx))
	}
}

// pendingSandboxPods reads namespace's pod list and returns one item per pod
// that has stayed Pending and unschedulable past sandboxPendingAfter.
//
// A reply that cannot be decoded, names no item list, or names a pod of another
// namespace refuses: an unreadable list would read as no pod waiting, which is
// a false clear. A pod whose creation time is missing or does not parse has no
// age to judge, so its age runs from the first tick that saw it unschedulable
// (prevFirstSeen carries that across ticks; nextFirstSeen receives this read's
// untimed pods). Such a pod raises once that age passes sandboxPendingAfter, and
// its missing time is logged once per daemon run rather than skipped without a
// word.
func (d *Daemon) pendingSandboxPods(ctx context.Context, kubeconfig, namespace string, now time.Time, prevFirstSeen, nextFirstSeen map[string]time.Time) ([]attention.Item, error) {
	out, err := d.pendingSandboxRead(ctx, kubeconfig, namespace)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items *[]pendingSandboxPod `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decoding the pod list for namespace %s: %w", namespace, err)
	}
	if list.Items == nil {
		return nil, errors.New("decoding the pod list for namespace " + namespace + ": the reply names no item list")
	}
	var items []attention.Item
	for _, pod := range *list.Items {
		if pod.Metadata.Namespace != "" && pod.Metadata.Namespace != namespace {
			return nil, fmt.Errorf("the pod list for namespace %s names the pod %s in %s", namespace, pod.Metadata.Name, pod.Metadata.Namespace)
		}
		message, unplaced := pendingSandboxUnplaced(pod)
		if !unplaced {
			continue
		}
		ref := namespace + "/" + pod.Metadata.Name
		var summary string
		if created, err := time.Parse(time.RFC3339, pod.Metadata.CreationTimestamp); err == nil {
			age := now.Sub(created)
			if age <= sandboxPendingAfter {
				continue
			}
			summary = fmt.Sprintf("sandbox pod %s pending %s, unschedulable: %s", ref, parkedAge(age), message)
			if message == "" {
				summary = fmt.Sprintf("sandbox pod %s pending %s and unschedulable; the scheduler gave no message", ref, parkedAge(age))
			}
		} else {
			d.logSandboxPendingUntimed(namespace, pod.Metadata.Name)
			first, seen := prevFirstSeen[ref]
			if !seen {
				first = now
			}
			nextFirstSeen[ref] = first
			if now.Sub(first) <= sandboxPendingAfter {
				continue
			}
			summary = fmt.Sprintf("sandbox pod %s pending Unschedulable, age unknown, first seen %s: %s", ref, first.UTC().Format(time.RFC3339), message)
			if message == "" {
				summary = fmt.Sprintf("sandbox pod %s pending Unschedulable, age unknown, first seen %s; the scheduler gave no message", ref, first.UTC().Format(time.RFC3339))
			}
		}
		items = append(items, attention.Item{
			Key:      sandboxPendingKeyPrefix + ref,
			Kind:     attention.KindSandboxPending,
			Severity: attention.SeverityHigh,
			Summary:  summary,
		})
	}
	slices.SortFunc(items, func(a, b attention.Item) int { return strings.Compare(a.Key, b.Key) })
	return items, nil
}

// logSandboxPendingUntimed says once per daemon run that an unschedulable pod
// has no creation time, so the pod is treated as new and never alarms on an
// age it does not have (gt-1gtq5g).
func (d *Daemon) logSandboxPendingUntimed(namespace, name string) {
	key := namespace + "/" + name
	if _, seen := d.sandboxPendingUntimed.LoadOrStore(key, true); seen {
		return
	}
	d.logger.Printf("sandbox pod %s has no creation timestamp; treated as new", key)
}

// pendingSandboxItem picks the unschedulable-pod items out of the kind's
// previous items, so a failed collector keeps only its own (collectAttention).
func pendingSandboxItem(it attention.Item) bool {
	return it.Kind == attention.KindSandboxPending && strings.HasPrefix(it.Key, sandboxPendingKeyPrefix)
}
