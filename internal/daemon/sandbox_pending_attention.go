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

	// sandboxPodNamespace is the namespace the OpenShell kubernetes driver
	// places a sandbox pod in. The gateway holds that setting, not the town's
	// sandbox profiles, so gt names the one value its cluster runs here; the
	// pods of the 2026-11-12 evidence (default--gt-nux, default--gt-splendid)
	// ran in default. A town whose gateway names another namespace reads none
	// of its sandbox pods, and this is the line to change (gt-1gtq5g).
	sandboxPodNamespace = "default"

	// sandboxPendingKeyPrefix is the item key's prefix; the pod's namespace and
	// name follow it, so one pod has one item however many ticks it stays
	// unschedulable.
	sandboxPendingKeyPrefix = "sandbox-pending:"
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
// Unschedulable refusal counts here.
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
	var out []attention.Item
	for _, namespace := range sandboxPendingNamespaces(profiles) {
		items, err := d.pendingSandboxPods(ctx, kubeconfig, namespace, s.now)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	return out, nil
}

// sandboxPendingNamespaces is the namespaces the profiles' sandbox pods run in.
// A profile that does not use the kubernetes driver has no pod in the cluster
// and names none, so a town with no kubernetes profile raises nothing and no
// pod is read.
func sandboxPendingNamespaces(profiles map[string]agentconfig.SandboxProfile) []string {
	for _, profile := range profiles {
		if profile.Driver == agentconfig.SandboxDriverKubernetes {
			return []string{sandboxPodNamespace}
		}
	}
	return nil
}

// pendingSandboxPod is the part of a pod the unschedulable read decides on.
type pendingSandboxPod struct {
	Metadata struct {
		Name              string    `json:"name"`
		Namespace         string    `json:"namespace"`
		CreationTimestamp time.Time `json:"creationTimestamp"`
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

// pendingSandboxPods reads namespace's pod list and returns one item per pod
// that has stayed Pending and unschedulable past sandboxPendingAfter.
//
// A reply that cannot be decoded, names no item list, or names a pod of another
// namespace refuses: an unreadable list would read as no pod waiting, which is
// a false clear. A pod with no creation time is not judged, since its age is
// unknown, and it raises nothing.
func (d *Daemon) pendingSandboxPods(ctx context.Context, kubeconfig, namespace string, now time.Time) ([]attention.Item, error) {
	out, err := d.cacheTrimKubectl(ctx, kubeconfig, "get", "pods", "-n", namespace, "-o", "json")
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
		if !unplaced || pod.Metadata.CreationTimestamp.IsZero() {
			continue
		}
		age := now.Sub(pod.Metadata.CreationTimestamp)
		if age <= sandboxPendingAfter {
			continue
		}
		summary := fmt.Sprintf("sandbox pod %s/%s pending %s, unschedulable: %s", namespace, pod.Metadata.Name, parkedAge(age), message)
		if message == "" {
			summary = fmt.Sprintf("sandbox pod %s/%s pending %s and unschedulable; the scheduler gave no message", namespace, pod.Metadata.Name, parkedAge(age))
		}
		items = append(items, attention.Item{
			Key:      sandboxPendingKeyPrefix + namespace + "/" + pod.Metadata.Name,
			Kind:     attention.KindSandboxPending,
			Severity: attention.SeverityHigh,
			Summary:  summary,
		})
	}
	slices.SortFunc(items, func(a, b attention.Item) int { return strings.Compare(a.Key, b.Key) })
	return items, nil
}

// pendingSandboxItem picks the unschedulable-pod items out of the kind's
// previous items, so a failed collector keeps only its own (collectAttention).
func pendingSandboxItem(it attention.Item) bool {
	return it.Kind == attention.KindSandboxPending && strings.HasPrefix(it.Key, sandboxPendingKeyPrefix)
}
