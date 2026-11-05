package k8spolicy

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// registryForwardNamespace is where the forwarder runs.
const registryForwardNamespace = "sweep-registry-forward"

// registryForwardImage is the socat image the forwarder runs. Its tag is
// pinned: latest would change under the node without a render.
const registryForwardImage = "alpine/socat:1.8.0.3"

// registryPortOK is a port written as a decimal number with no sign and no
// leading zero. The number is then checked against 65535.
var registryPortOK = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)

// RenderRegistryForward renders the node-local registry forwarder of one
// environment from its hosts.yaml (deploy/k8s/values/<env>/hosts.yaml). The
// forwarder listens on 127.0.0.1 of each node at the port of registry_host and
// relays to control_plane_host on that same port, so a Job on the node can name
// localhost:<port>/<image> with no node change. The host and the port come only
// from the hosts file (gt-gg9hv.25).
func RenderRegistryForward(env string, hostsYAML []byte) (string, error) {
	var hosts struct {
		ControlPlaneHost string `yaml:"control_plane_host"`
		RegistryHost     string `yaml:"registry_host"`
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
	if hosts.RegistryHost == "" {
		return "", fmt.Errorf("the hosts file has no registry_host")
	}
	registryName, portText, ok := strings.Cut(hosts.RegistryHost, ":")
	port, err := strconv.Atoi(portText)
	if !ok || err != nil || !ValidHost(registryName) || !registryPortOK.MatchString(portText) || port > 65535 {
		return "", fmt.Errorf("registry_host is not host:port with a port from 1 to 65535")
	}
	if env == "" || !externalNameOK.MatchString(env) {
		return "", fmt.Errorf("the environment name is empty or not a plain word")
	}
	listen := fmt.Sprintf("TCP-LISTEN:%s,fork,reuseaddr,bind=127.0.0.1", portText)
	relay := fmt.Sprintf("TCP:%s:%s", host, portText)
	var b strings.Builder
	fmt.Fprintf(&b, "# GENERATED from values/%s/hosts.yaml by `go run ./cmd/render-services --registry-forward`; do not edit.\n", env)
	b.WriteString("# A host-network forwarder on every node: the node's localhost port is the registry port\n")
	b.WriteString("# of registry_host, relayed to control_plane_host. The listener binds 127.0.0.1 only,\n")
	b.WriteString("# because the registry has no authentication.\n")
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Namespace\nmetadata: {name: %s}\n", registryForwardNamespace)
	b.WriteString("---\n")
	fmt.Fprintf(&b, "apiVersion: apps/v1\nkind: DaemonSet\nmetadata: {name: registry-forward, namespace: %s}\n", registryForwardNamespace)
	b.WriteString("spec:\n")
	b.WriteString("  selector: {matchLabels: {app: registry-forward}}\n")
	b.WriteString("  template:\n")
	b.WriteString("    metadata: {labels: {app: registry-forward}}\n")
	b.WriteString("    spec:\n")
	b.WriteString("      hostNetwork: true\n")
	b.WriteString("      priorityClassName: gt-control-plane\n")
	b.WriteString("      containers:\n")
	b.WriteString("        - name: socat\n")
	fmt.Fprintf(&b, "          image: %s\n", registryForwardImage)
	fmt.Fprintf(&b, "          args: [%q, %q]\n", listen, relay)
	b.WriteString("          resources:\n")
	b.WriteString("            requests: {cpu: 10m, memory: 16Mi}\n")
	b.WriteString("            limits: {memory: 64Mi}\n")
	return b.String(), nil
}
