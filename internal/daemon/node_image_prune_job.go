package daemon

import (
	"encoding/json"
	"strconv"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

// The control node's CRI socket and k3s binary, as measured on 2026-11-20 on the
// gt-control colima node. node-prune reads and removes the node's images through
// the node's own k3s crictl, so the Job mounts both.
const (
	nodeImagePruneNamespace   = "gt"
	nodeImagePruneSocket      = "/run/containerd/containerd.sock"
	nodeImagePruneEndpoint    = "unix://" + nodeImagePruneSocket
	nodeImagePruneK3sBinary   = "/usr/local/bin/k3s"
	nodeImagePruneK3sMount    = "/host/k3s"
	nodeImagePruneDeadlineSec = 900
)

// nodeImagePruneManifest returns the batch/v1 Job that runs node-prune on the
// control node. It keeps the newest keep images of each repository. Without apply
// the Job only plans; with apply it removes the planned images. Each spare is an
// image id or digest the plan must not remove, passed as its own --spare.
func nodeImagePruneManifest(name, image string, keep int, apply bool, spare []string) ([]byte, error) {
	command := []string{cachevol.HelperPath, "node-prune", "--keep", strconv.Itoa(keep),
		"--crictl-bin", nodeImagePruneK3sMount, "--crictl-arg", "crictl",
		"--endpoint", nodeImagePruneEndpoint}
	for _, s := range spare {
		command = append(command, "--spare", s)
	}
	if apply {
		command = append(command, "--apply")
	}
	return json.Marshal(map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata":   map[string]any{"name": name, "namespace": nodeImagePruneNamespace},
		"spec": map[string]any{
			"backoffLimit":            0,
			"activeDeadlineSeconds":   nodeImagePruneDeadlineSec,
			"ttlSecondsAfterFinished": nodeImagePruneDeadlineSec,
			"template": map[string]any{"spec": map[string]any{
				"restartPolicy":   "Never",
				"securityContext": map[string]any{"runAsUser": 0},
				"containers": []any{map[string]any{
					"name":    "prune",
					"image":   image,
					"command": command,
					"volumeMounts": []any{
						map[string]any{"name": "containerd-sock", "mountPath": nodeImagePruneSocket},
						map[string]any{"name": "k3s", "mountPath": nodeImagePruneK3sMount, "readOnly": true},
					},
				}},
				"volumes": []any{
					map[string]any{"name": "containerd-sock", "hostPath": map[string]any{"path": nodeImagePruneSocket, "type": "Socket"}},
					map[string]any{"name": "k3s", "hostPath": map[string]any{"path": nodeImagePruneK3sBinary, "type": "File"}},
				},
			}},
		},
	})
}
