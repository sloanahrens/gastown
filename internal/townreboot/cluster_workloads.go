package townreboot

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
)

// clusterWorkloads are the Deployments a drained stop may leave scaled to
// zero, in the order the check reports them (gt-fvna6b).
var clusterWorkloads = []string{k8sdaemon.Name, k8sdaemon.DashboardName}

// replicasDeployment is the part of a Deployment the replicas check reads: the
// desired count and the count the status reports ready.
type replicasDeployment struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Replicas int `json:"replicas"`
	} `json:"spec"`
	Status struct {
		ReadyReplicas int `json:"readyReplicas"`
	} `json:"status"`
}

// replicasList is the items array of kubectl get deploy's JSON.
type replicasList struct {
	Items []replicasDeployment `json:"items"`
}

// replicasScaleFix is the repair for a Deployment a drained stop scaled to
// zero: scale it back to one.
func replicasScaleFix(name string) string {
	return fmt.Sprintf("kubectl --context colima-gt-control -n %s scale deploy/%s --replicas=1",
		k8sdaemon.DefaultNamespace, name)
}

// ReplicasCheck reads the daemon and dashboard Deployments and passes when each
// runs its desired replicas with every one ready; a Deployment a drained stop
// scaled to zero is the operations doc's step 6 (gt-fvna6b).
func ReplicasCheck(run Runner) Check {
	return Check{
		Name: "6 replicas",
		Run: func(ctx context.Context) Result {
			if run == nil {
				// A check built without a runner is a wiring bug; a panic here
				// would hide every check after it (gt-k3bf8x).
				return Result{Detail: "no runner configured"}
			}
			out, err := run(ctx, "kubectl", "--context", "colima-gt-control",
				"-n", k8sdaemon.DefaultNamespace, "get", "deploy",
				k8sdaemon.Name, k8sdaemon.DashboardName, "-o", "json")
			if err != nil {
				// The Deployment list is unknown, so the row says what failed
				// rather than reading an unreadable list as every replica up.
				return Result{Detail: fmt.Sprintf("read deployments: %v", err)}
			}
			var list replicasList
			if err := json.Unmarshal(out, &list); err != nil {
				// An unparseable list says nothing about the replicas, so it is
				// a failed row rather than an empty one that would pass.
				return Result{Detail: fmt.Sprintf("parse deployments: %v", err)}
			}
			byName := make(map[string]replicasDeployment, len(list.Items))
			for _, d := range list.Items {
				byName[d.Metadata.Name] = d
			}
			var problems, fixes, healthy []string
			for _, name := range clusterWorkloads {
				d, found := byName[name]
				if !found {
					// A Deployment the read omitted is not evidence that it is
					// up, so the row says it was not listed.
					problems = append(problems, name+" is not in the deployment list")
					continue
				}
				if d.Spec.Replicas < 1 {
					problems = append(problems, fmt.Sprintf(
						"%s has %d replicas: expected after a drained stop, and nothing is running",
						name, d.Spec.Replicas))
					fixes = append(fixes, replicasScaleFix(name))
					continue
				}
				if d.Status.ReadyReplicas < d.Spec.Replicas {
					problems = append(problems, fmt.Sprintf("%s %d/%d ready",
						name, d.Status.ReadyReplicas, d.Spec.Replicas))
					continue
				}
				healthy = append(healthy, fmt.Sprintf("%s %d/%d ready",
					name, d.Status.ReadyReplicas, d.Spec.Replicas))
			}
			if len(problems) > 0 {
				return Result{Detail: strings.Join(problems, "; "), Fix: strings.Join(fixes, " && ")}
			}
			return Result{OK: true, Detail: strings.Join(healthy, ", ")}
		},
	}
}
