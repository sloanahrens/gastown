package forgejoserver

// metaKeys are the metadata fields the cluster fills in by itself;
// clusterAnnotations are the annotations it writes.
var metaKeys = []string{"uid", "resourceVersion", "generation", "creationTimestamp", "managedFields", "finalizers"}
var clusterAnnotations = []string{
	"kubectl.kubernetes.io/last-applied-configuration", "deployment.kubernetes.io/revision",
	"pv.kubernetes.io/bind-completed", "pv.kubernetes.io/bound-by-controller",
	"volume.beta.kubernetes.io/storage-provisioner", "volume.kubernetes.io/selected-node",
	"volume.kubernetes.io/storage-provisioner",
}

// Strip returns a copy of obj (the map encoding/json decodes into) without the
// fields the cluster sets itself, for comparing a manifest with a live read.
// The input is not changed; a map the strip empties is removed, not an empty one.
func Strip(obj map[string]any) map[string]any {
	out := copyValue(obj).(map[string]any)
	delete(out, "status")
	drop(out, []string{"metadata"}, metaKeys)
	drop(out, []string{"metadata", "annotations"}, clusterAnnotations)
	drop(out, []string{"spec", "template", "metadata"}, []string{"creationTimestamp"})
	drop(out, []string{"spec", "template", "metadata", "annotations"}, []string{"kubectl.kubernetes.io/restartedAt"})
	switch out["kind"] {
	case "Service":
		drop(out, []string{"spec"}, []string{"clusterIP", "clusterIPs"})
		drop(out, []string{"spec", "ports"}, []string{"nodePort"})
	case "PersistentVolumeClaim":
		drop(out, []string{"spec"}, []string{"volumeName"})
	case "Namespace":
		drop(out, []string{"metadata", "labels"}, []string{"kubernetes.io/metadata.name"})
		drop(out, []string{"spec"}, []string{"finalizers"})
	}
	return out
}

// drop deletes keys from the map at path in parent, or from each element of a
// slice there; a map it empties is removed from its parent, an empty one stays.
func drop(parent map[string]any, path, keys []string) {
	if len(path) == 0 {
		return
	}
	key := path[0]
	m, ok := parent[key].(map[string]any)
	if !ok {
		if list, ok := parent[key].([]any); ok {
			for _, e := range list {
				if m, ok := e.(map[string]any); ok {
					dropKeys(m, keys)
				}
			}
		}
		return
	}
	had := len(m)
	if len(path) > 1 {
		drop(m, path[1:], keys)
	} else {
		dropKeys(m, keys)
	}
	if had > 0 && len(m) == 0 {
		delete(parent, key)
	}
}

func dropKeys(m map[string]any, keys []string) {
	for _, k := range keys {
		delete(m, k)
	}
}

// copyValue returns a deep copy of v: maps, slices and scalars, the JSON kinds.
func copyValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, v := range x {
			m[k] = copyValue(v)
		}
		return m
	case []any:
		s := make([]any, len(x))
		for i, v := range x {
			s[i] = copyValue(v)
		}
		return s
	}
	return v
}
