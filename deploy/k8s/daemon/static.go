package daemon

import (
	"fmt"

	"github.com/steveyegge/gastown/deploy/k8s"
)

// staticObjects returns the static control-cluster set in the order Render
// writes it and refuses a namespaced object outside the namespaces this render
// may touch. Build passes k8s.Static; a source that fails is returned, never
// read as an empty set.
func staticObjects(source func() ([]k8s.StaticObject, error), renderNamespace string) ([]k8s.StaticObject, error) {
	objects, err := source()
	if err != nil {
		return nil, fmt.Errorf("reading the static control-cluster manifests: %w", err)
	}
	return orderedStatic(objects, renderNamespace)
}

// orderedStatic orders objects as Render writes them — the cluster-scoped ones
// first, then the namespaced ones, each in k8s.Static's order — so the admission
// policies exist before the Services and workloads they judge. A namespaced
// object whose namespace is not renderNamespace, the sweep's or Forgejo's is
// refused, naming the object.
func orderedStatic(objects []k8s.StaticObject, renderNamespace string) ([]k8s.StaticObject, error) {
	ordered := make([]k8s.StaticObject, 0, len(objects))
	for _, object := range objects {
		if object.Cluster {
			ordered = append(ordered, object)
		}
	}
	for _, object := range objects {
		if object.Cluster {
			continue
		}
		if !staticNamespaceAllowed(object.Namespace, renderNamespace) {
			return nil, fmt.Errorf("static %s %s is in namespace %q, outside the render's %q, %s or %s",
				object.Kind, object.Name, object.Namespace, renderNamespace, SweepNamespace, CacheTrimNamespace)
		}
		ordered = append(ordered, object)
	}
	return ordered, nil
}

// staticNamespaceAllowed reports whether a static object may land in namespace:
// the render's own, the sweep's, or Forgejo's, where the cache-trim Role lives
// (the operator's allowlist).
func staticNamespaceAllowed(namespace, renderNamespace string) bool {
	switch namespace {
	case renderNamespace, SweepNamespace, CacheTrimNamespace:
		return true
	}
	return false
}
