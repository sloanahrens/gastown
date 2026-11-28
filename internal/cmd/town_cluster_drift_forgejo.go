package cmd

import (
	"errors"
	"fmt"
)

// forgejoServerNamespace is the namespace server.yaml puts every namespaced object in.
const forgejoServerNamespace = "forgejo"

// forgejoTargetKinds maps each kind a Forgejo manifest object may be to its scope.
var forgejoTargetKinds = map[string]bool{"Namespace": true, "PersistentVolumeClaim": false, "Service": false, "Deployment": false}

// forgejoDriftTarget is the drift target of one Forgejo manifest object. It
// refuses with no target an object with no kind, a kind outside
// forgejoTargetKinds, no metadata.name, or a namespaced object outside the
// forgejo namespace.
func forgejoDriftTarget(obj map[string]any) (driftTarget, error) {
	kind, _ := obj["kind"].(string)
	if kind == "" {
		return driftTarget{}, errors.New("a Forgejo manifest object names no kind, so it was not read")
	}
	cluster, known := forgejoTargetKinds[kind]
	if !known {
		return driftTarget{}, fmt.Errorf("the Forgejo manifest object is a kind the drift compare does not read: %q", kind)
	}
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if name == "" {
		return driftTarget{}, fmt.Errorf("the Forgejo %s names no metadata.name, so it was not read", kind)
	}
	if cluster {
		return clusterScopedDriftTarget(kind, name, obj), nil
	}
	namespace, _ := meta["namespace"].(string)
	if namespace != forgejoServerNamespace {
		return driftTarget{}, fmt.Errorf("the Forgejo %s %q is in namespace %q, not %s, so it was not read", kind, name, namespace, forgejoServerNamespace)
	}
	return namespacedDriftTarget(kind, name, namespace, obj), nil
}
