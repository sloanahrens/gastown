package townreboot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// forgejoFix is the operations doc's repair for a Forgejo pod that the reboot
// left not Ready: clear the pod waiting out its backoff, and when its log names
// host.lima.internal, the VM's resolver is the cause and gt town vms start fixes
// it (gt-9deo22).
const forgejoFix = "delete a pod that is waiting out its backoff; " +
	"if the pod log says lookup host.lima.internal, run gt town vms start"

// forgejoPods is the part of the pods list the check reads; the rest of the
// object is ignored so a change kubectl makes to other fields cannot fail it.
type forgejoPods struct {
	Items []forgejoPod `json:"items"`
}

// forgejoPod carries the two fields the check reads: the name to report and the
// conditions that say whether the pod is Ready.
type forgejoPod struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Conditions []forgejoPodCondition `json:"conditions"`
	} `json:"status"`
}

// forgejoPodCondition is one entry of status.conditions.
type forgejoPodCondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
}

// ForgejoCheck reads the pods in the forgejo namespace over run and passes when
// every one of them is Ready; it is the operations doc's step 5 (gt-9deo22).
func ForgejoCheck(run Runner) Check {
	return Check{
		Name: "5 forgejo",
		Run: func(ctx context.Context) Result {
			if run == nil {
				// A check built without a runner is a wiring bug; a panic here
				// would hide every check after it (gt-k3bf8x).
				return Result{Detail: "no runner configured"}
			}
			out, err := run(ctx, "kubectl", "--context", "colima-gt-control",
				"-n", "forgejo", "get", "pods", "-o", "json")
			if err != nil {
				// The pod list is unknown, so the row says what failed rather
				// than reading an unreadable list as every pod Ready.
				return Result{Detail: fmt.Sprintf("list pods: %v", err)}
			}
			var list forgejoPods
			if err := json.Unmarshal(out, &list); err != nil {
				// An unparseable list says nothing about the pods, so it is a
				// failed row rather than an empty one that would pass.
				return Result{Detail: fmt.Sprintf("parse pods: %v", err)}
			}
			if len(list.Items) == 0 {
				// An empty list means Forgejo is not deployed, not that every
				// pod in it is Ready.
				return Result{Detail: "no pods in namespace forgejo"}
			}
			var notReady []string
			for _, pod := range list.Items {
				if !podReady(pod) {
					notReady = append(notReady, pod.Metadata.Name)
				}
			}
			if len(notReady) > 0 {
				return Result{Detail: "not ready: " + strings.Join(notReady, ", "), Fix: forgejoFix}
			}
			return Result{OK: true, Detail: fmt.Sprintf("%d pods ready", len(list.Items))}
		},
	}
}

// podReady reports whether the pod's Ready condition is True. A pod with no
// Ready condition yet, as during start-up, is not ready.
func podReady(pod forgejoPod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}
