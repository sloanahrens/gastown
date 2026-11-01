package k8spolicy

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// ControlPlaneServiceNames are the four names a sandbox uses for the control
// plane. Each is an ExternalName Service to the environment's control-plane
// host; moving a service into a cluster later replaces its ExternalName with a
// selector Service of the same name.
var ControlPlaneServiceNames = []string{"control-plane", "dolt", "forgejo", "tracker"}

// externalNameOK is what an environment's control_plane_host may be: a DNS name
// or an upper-case placeholder for one. Nothing that could end the scalar or
// start another YAML document gets through.
var externalNameOK = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// RenderServices renders the control-plane Services of one environment from its
// hosts.yaml (deploy/k8s/values/<env>/hosts.yaml): the file's control_plane_host
// is the only place the host is written, so the checked-in Services cannot
// drift from it (gt-ht5he).
func RenderServices(env string, hostsYAML []byte) (string, error) {
	var hosts struct {
		ControlPlaneHost string `yaml:"control_plane_host"`
	}
	if err := yaml.Unmarshal(hostsYAML, &hosts); err != nil {
		return "", fmt.Errorf("the hosts file does not parse: %w", err)
	}
	host := hosts.ControlPlaneHost
	if host == "" {
		return "", fmt.Errorf("the hosts file has no control_plane_host")
	}
	if len(host) > 253 || !externalNameOK.MatchString(host) {
		return "", fmt.Errorf("control_plane_host is not a host name or placeholder")
	}
	if env == "" || !externalNameOK.MatchString(env) {
		return "", fmt.Errorf("the environment name is empty or not a plain word")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# GENERATED from values/%s/hosts.yaml by `go run ./cmd/render-services`; do not edit.\n", env)
	b.WriteString("# Names a sandbox uses for the control plane, all resolving to the Mac as seen from\n")
	b.WriteString("# the cluster's nodes. They are ExternalName Services, so the cluster DNS answers with\n")
	b.WriteString("# the host's address and the sandbox policy pins it (see `go run ./cmd/render-policy`).\n")
	for i, name := range ControlPlaneServiceNames {
		if i > 0 {
			b.WriteString("---\n")
		}
		fmt.Fprintf(&b, "apiVersion: v1\nkind: Service\nmetadata: {name: %s, namespace: openshell}\nspec: {type: ExternalName, externalName: %s}\n", name, host)
	}
	return b.String(), nil
}
