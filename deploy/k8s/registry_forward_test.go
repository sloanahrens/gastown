package k8s

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/k8spolicy"
)

// The committed registry forwarder is exactly the render of the kind hosts
// file, so its listener port and relay host cannot drift from the file
// (gt-gg9hv.25). The cloud environment has no Mac registry to forward and gets
// no such file.
func TestRegistryForwardIsFresh(t *testing.T) {
	t.Parallel()
	hosts, err := os.ReadFile(filepath.FromSlash("values/kind/hosts.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := k8spolicy.RenderRegistryForward("kind", hosts)
	if err != nil {
		t.Fatalf("rendering values/kind/hosts.yaml: %v", err)
	}
	got, err := os.ReadFile(filepath.FromSlash("values/kind/registry-forward.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Errorf("values/kind/registry-forward.yaml is not the render of hosts.yaml; run: go run ./cmd/render-services --registry-forward deploy/k8s/values/kind/hosts.yaml > deploy/k8s/values/kind/registry-forward.yaml")
	}
}
