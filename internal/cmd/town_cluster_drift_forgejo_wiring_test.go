package cmd

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/deploy/k8s"
	"github.com/steveyegge/gastown/internal/k8spolicy"
)

// TestForgejoDriftDeps holds the real deps to the server's four documents with
// the embedded hosts file's host filled into the database value.
func TestForgejoDriftDeps(t *testing.T) {
	t.Parallel()
	objects, err := realClusterDriftDeps(io.Discard).forgejoManifestObjects()
	if err != nil {
		t.Fatalf("the real deps refused the Forgejo objects: %v", err)
	}
	if len(objects) != 4 {
		t.Fatalf("objects = %d, want the server's four manifest objects", len(objects))
	}
	host, err := k8spolicy.ControlPlaneHost(k8s.ControlHostsYAML)
	if err != nil {
		t.Fatalf("reading the embedded hosts file: %v", err)
	}
	raw, err := json.Marshal(objects)
	if err != nil {
		t.Fatalf("encoding the objects: %v", err)
	}
	if want := `"name":"FORGEJO__database__HOST","value":"` + host + `:5442"`; !strings.Contains(string(raw), want) {
		t.Errorf("the objects do not hold %s", want)
	}
}

// TestForgejoDriftDepsUnwired: a deps value without the seam is an error naming it, never a panic.
func TestForgejoDriftDepsUnwired(t *testing.T) {
	t.Parallel()
	if objects, err := (clusterDriftDeps{}).forgejoManifestObjects(); err == nil {
		t.Fatalf("objects = %d with no error, want the unwired refusal", len(objects))
	} else if want := "no render of the Forgejo server's manifest objects is wired"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not hold %q", err, want)
	}
}

// TestForgejoDriftDepsRefusesAHostsFileItCannotUse: a hosts file naming no host is refused.
func TestForgejoDriftDepsRefusesAHostsFileItCannotUse(t *testing.T) {
	t.Parallel()
	if objects, err := forgejoObjectsFrom([]byte("registry_host: localhost:5001\n")); err == nil {
		t.Fatalf("objects = %d with no error, want the refusal", len(objects))
	}
}
