package daemon

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"

	"github.com/steveyegge/gastown/internal/polecat/cachevol"
)

// nodeImagePruneSpareDigest is the only shape a spare may take. Each spare reaches
// crictl's argv, so a value that is not a sha256 digest, a dash-led one included,
// is refused before the Job is built.
var nodeImagePruneSpareDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

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
// the Job only plans; with apply it removes the planned images. Each spare is a
// sha256 digest the plan must not remove, passed as its own --spare. It refuses an
// empty name or image, a keep below 1, and a spare that is not sha256 plus 64
// lower-case hex digits.
func nodeImagePruneManifest(name, image string, keep int, apply bool, spare []string) ([]byte, error) {
	if name == "" {
		return nil, fmt.Errorf("node-prune name is empty")
	}
	if image == "" {
		return nil, fmt.Errorf("node-prune image is empty")
	}
	if keep < 1 {
		return nil, fmt.Errorf("node-prune keep is %d, want at least 1", keep)
	}
	for _, s := range spare {
		if !nodeImagePruneSpareDigest.MatchString(s) {
			return nil, fmt.Errorf("node-prune spare %q is not sha256 plus 64 lower-case hex digits", s)
		}
	}
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
