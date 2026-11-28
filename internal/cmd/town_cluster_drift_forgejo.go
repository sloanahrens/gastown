package cmd

import (
	"context"
	"errors"
	"fmt"

	forgejoserver "github.com/steveyegge/gastown/deploy/k8s/forgejo-server"
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

// compareForgejoDrift compares each Forgejo manifest object with the live object
// its kind names, and prints each difference in compareClusterDrift's form. A line
// an exception explains prints as explained and does not set differed. An object
// the cluster does not hold prints one line and sets differed, and the compare goes
// on. A refused target, a failed read and a Differences error each end the compare
// with an error naming the object.
func compareForgejoDrift(ctx context.Context, d clusterDriftDeps, objects []map[string]any, exceptions []driftException) (differed bool, err error) {
	if d.kubectl == nil {
		return false, errors.New("no kubectl read runner is wired, so no Forgejo object can be compared; nothing was read")
	}
	for _, obj := range objects {
		t, err := forgejoDriftTarget(obj)
		if err != nil {
			return differed, fmt.Errorf("cannot compare the Forgejo object %s: %w", forgejoObjectLabel(obj), err)
		}
		live, absent, err := readLiveObject(ctx, d, t.resource, t.namespace, t.cluster)
		if err != nil {
			return differed, fmt.Errorf("cannot compare the Forgejo object %s: %w", forgejoObjectLabel(obj), err)
		}
		if absent {
			fmt.Fprintf(d.out, "%s: the object is absent from the cluster\n", t.resource)
			differed = true
			continue
		}
		lines, err := forgejoserver.Differences(obj, live)
		if err != nil {
			return differed, fmt.Errorf("cannot compare the Forgejo object %s: %w", forgejoObjectLabel(obj), err)
		}
		unexplained, explained := filterExplained(lines, t.exceptionObject(), exceptions)
		for _, line := range unexplained {
			fmt.Fprintf(d.out, "%s: %s\n", t.resource, line)
		}
		for _, line := range explained {
			fmt.Fprintf(d.out, "%s: explained: %s\n", t.resource, line)
		}
		differed = differed || len(unexplained) > 0
	}
	return differed, nil
}

// forgejoObjectLabel names a Forgejo manifest object as Kind/name, read without a
// check so one that lacks either still gets a label. Errors use it, since the object
// may have no target; printed lines and live read errors use the lower-case resource.
func forgejoObjectLabel(obj map[string]any) string {
	kind, _ := obj["kind"].(string)
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	return kind + "/" + name
}
