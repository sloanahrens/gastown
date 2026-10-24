package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/version"
	"github.com/steveyegge/gastown/internal/workspace"
	"gopkg.in/yaml.v3"
)

// `gt sandbox endpoint` (gt-ek39n) prints the Forgejo endpoint that a gateway's
// forgejo-polecat provider profile must bind. A sandbox reaches Forgejo at the
// host its profile's control_plane_host names, on the port and allowed_ips its
// policy file lists for that host, so all three come from those two places and
// nowhere else. scripts/openshell-providers.sh imports the profile from this
// output: the script keeps no copy of the values, and a gateway whose profile
// binds a different host gets a different profile instead of a 403 at clone.

var sandboxCmd = &cobra.Command{
	Use:     "sandbox",
	GroupID: GroupConfig,
	Short:   "Read the openshell sandbox profiles the town's seats run in",
	RunE:    requireSubcommand,
}

var sandboxEndpointCmd = &cobra.Command{
	Use:   "endpoint",
	Short: "Print the Forgejo endpoint a gateway's forgejo-polecat profile binds",
	Long: `Print the Forgejo endpoint a gateway's forgejo-polecat provider profile must
bind: the host, port and allowed_ips the sandboxes on that gateway reach
Forgejo at.

The gateway is the one a sandbox profile in settings/config.json names (a
profile with no gateway is on the default gateway, "openshell"). Its host is
the profile's control_plane_host. Its port and allowed_ips are the endpoint of
the policy file the profile names whose host is that host and whose rules are
the git smart-HTTP paths (.git/info/refs). Two profiles on one gateway must
bind the same endpoint.

Default output is three lines, shell-safe without quoting:

  host=control-plane.openshell.svc.cluster.local
  port=3000
  allowed_ips=192.168.5.2/32

allowed_ips is empty when the policy names none, as the Docker policy does.
--json prints {host, port, allowed_ips} instead.`,
	Args: cobra.NoArgs,
	RunE: runSandboxEndpoint,
}

var (
	sandboxEndpointGateway string
	sandboxEndpointRoot    string
	sandboxEndpointJSON    bool
	sandboxEndpointRender  string
)

func init() {
	sandboxEndpointCmd.Flags().StringVar(&sandboxEndpointGateway, "gateway", "", "openshell gateway name, e.g. k8s (required)")
	sandboxEndpointCmd.Flags().StringVar(&sandboxEndpointRoot, "root", "", "gt source checkout the policy paths are relative to (default: the town's gt source)")
	sandboxEndpointCmd.Flags().BoolVar(&sandboxEndpointJSON, "json", false, "Print {host, port, allowed_ips} as JSON")
	sandboxEndpointCmd.Flags().StringVar(&sandboxEndpointRender, "render", "", "Print the forgejo-polecat profile FILE with this endpoint in place of its Docker host alias, instead of the endpoint")
	_ = sandboxEndpointCmd.MarkFlagRequired("gateway")
	sandboxCmd.AddCommand(sandboxEndpointCmd)
	rootCmd.AddCommand(sandboxCmd)
}

// sandboxEndpoint is the Forgejo endpoint a forgejo-polecat profile binds.
type sandboxEndpoint struct {
	Host       string   `json:"host"`
	Port       int      `json:"port"`
	AllowedIPs []string `json:"allowed_ips"`
}

func runSandboxEndpoint(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("sandbox endpoint: %w", err)
	}
	ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return fmt.Errorf("sandbox endpoint: reading town settings: %w", err)
	}
	root := sandboxEndpointRoot
	if root == "" {
		if root, err = version.GetRepoRootForTown(townRoot); err != nil {
			return fmt.Errorf("sandbox endpoint: %w", err)
		}
	}
	ep, err := forgejoEndpointForGateway(ts, sandboxEndpointGateway, root)
	if err != nil {
		return fmt.Errorf("sandbox endpoint: %w", err)
	}
	if sandboxEndpointRender != "" {
		return renderSandboxProfile(cmd.OutOrStdout(), sandboxEndpointRender, ep)
	}
	return printSandboxEndpoint(cmd.OutOrStdout(), ep, sandboxEndpointJSON)
}

// renderSandboxProfile writes the profile file at path with ep in place of the
// Docker host alias it names.
func renderSandboxProfile(w io.Writer, path string, ep sandboxEndpoint) error {
	src, err := os.ReadFile(path) //nolint:gosec // G304: the operator names the profile file to render
	if err != nil {
		return fmt.Errorf("sandbox endpoint: %w", err)
	}
	out, err := renderForgejoProfile(src, ep)
	if err != nil {
		return fmt.Errorf("sandbox endpoint: %s: %w", path, err)
	}
	_, err = w.Write(out)
	return err
}

// renderForgejoProfile returns the forgejo-polecat provider profile src with ep
// in place of the Docker host alias it names: the host line, and the port line
// followed by an allowed_ips line when ep has any. A Docker endpoint (no
// allowed_ips) comes back byte for byte, so the Docker gateway imports the
// profile it always did. src must name host.docker.internal on port 3000, the
// one shape this rewrites.
func renderForgejoProfile(src []byte, ep sandboxEndpoint) ([]byte, error) {
	const (
		dockerHostLine = "  - host: host.docker.internal"
		dockerPortLine = "    port: 3000"
	)
	var out strings.Builder
	hosted, ported := false, false
	for _, line := range strings.SplitAfter(string(src), "\n") {
		text := strings.TrimSuffix(line, "\n")
		eol := line[len(text):]
		switch text {
		case dockerHostLine:
			hosted = true
			line = "  - host: " + ep.Host + eol
		case dockerPortLine:
			ported = true
			out.WriteString("    port: " + strconv.Itoa(ep.Port) + eol)
			if len(ep.AllowedIPs) > 0 {
				out.WriteString(`    allowed_ips: ["` + strings.Join(ep.AllowedIPs, `","`) + `"]` + "\n")
			}
			continue
		}
		out.WriteString(line)
	}
	if !hosted || !ported {
		return nil, fmt.Errorf("the profile names no host.docker.internal endpoint on port 3000 to rewrite")
	}
	return []byte(out.String()), nil
}

// printSandboxEndpoint writes ep as the three shell-safe lines, or as JSON.
func printSandboxEndpoint(w io.Writer, ep sandboxEndpoint, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(ep)
	}
	_, err := fmt.Fprintf(w, "host=%s\nport=%d\nallowed_ips=%s\n", ep.Host, ep.Port, strings.Join(ep.AllowedIPs, ","))
	return err
}

// forgejoEndpointForGateway returns the endpoint the profiles on gateway bind.
// policyRoot is the directory a profile's policy path is relative to. A gateway
// no profile names is an error that names the gateways that do exist; two
// profiles on one gateway that bind different endpoints are an error too,
// because the gateway's one forgejo-polecat profile could not satisfy both.
func forgejoEndpointForGateway(ts *config.TownSettings, gateway, policyRoot string) (sandboxEndpoint, error) {
	var names []string
	for name, p := range ts.Sandboxes {
		if p.WithDefaults().Gateway == gateway {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return sandboxEndpoint{}, fmt.Errorf("no sandbox profile names gateway %q; the gateways in settings are %s", gateway, sandboxGatewayList(ts))
	}
	sort.Strings(names)

	var first sandboxEndpoint
	for i, name := range names {
		ep, err := profileForgejoEndpoint(ts.Sandboxes[name], name, policyRoot)
		if err != nil {
			return sandboxEndpoint{}, err
		}
		if i == 0 {
			first = ep
			continue
		}
		if !sameSandboxEndpoint(first, ep) {
			return sandboxEndpoint{}, fmt.Errorf("sandbox profiles %s and %s both run on gateway %q but bind different Forgejo endpoints (%s:%d and %s:%d); give each gateway one endpoint",
				names[0], name, gateway, first.Host, first.Port, ep.Host, ep.Port)
		}
	}
	return first, nil
}

// sandboxGatewayList names the gateways the settings' sandbox profiles run on,
// for the error that says which gateways exist.
func sandboxGatewayList(ts *config.TownSettings) string {
	seen := map[string]bool{}
	var gateways []string
	for _, p := range ts.Sandboxes {
		gw := p.WithDefaults().Gateway
		if !seen[gw] {
			seen[gw] = true
			gateways = append(gateways, gw)
		}
	}
	if len(gateways) == 0 {
		return "none (settings/config.json has no sandboxes)"
	}
	sort.Strings(gateways)
	return strings.Join(gateways, ", ")
}

// profileForgejoEndpoint reads the endpoint one sandbox profile binds from the
// policy file it names.
func profileForgejoEndpoint(p config.SandboxProfile, name, policyRoot string) (sandboxEndpoint, error) {
	p = p.WithDefaults()
	if err := p.Validate(name); err != nil {
		return sandboxEndpoint{}, err
	}
	if !config.ValidSandboxForgejoHost(p.ControlPlaneHost) {
		return sandboxEndpoint{}, fmt.Errorf("sandboxes.%s.control_plane_host %q: want a bare host name", name, p.ControlPlaneHost)
	}
	// Validate has confined p.Policy to the policy root, so this reads a file
	// inside the gt source checkout.
	data, err := os.ReadFile(filepath.Join(policyRoot, p.Policy)) //nolint:gosec // G304: p.Policy is confined by Validate
	if err != nil {
		return sandboxEndpoint{}, fmt.Errorf("sandboxes.%s.policy: %w", name, err)
	}
	ep, err := policyForgejoEndpoint(data, p.ControlPlaneHost)
	if err != nil {
		return sandboxEndpoint{}, fmt.Errorf("sandboxes.%s.policy %s: %w", name, p.Policy, err)
	}
	return ep, nil
}

// The slice of a policy file this reads: each network policy's endpoints, with
// the rules that name the git smart-HTTP paths.
type policyDoc struct {
	NetworkPolicies map[string]struct {
		Endpoints []policyEndpoint `yaml:"endpoints"`
	} `yaml:"network_policies"`
}

type policyEndpoint struct {
	Host       string   `yaml:"host"`
	Port       int      `yaml:"port"`
	AllowedIPs []string `yaml:"allowed_ips"`
	Rules      []struct {
		Allow struct {
			Path string `yaml:"path"`
		} `yaml:"allow"`
	} `yaml:"rules"`
}

// policyForgejoEndpoint finds the one endpoint on host that carries the git
// smart-HTTP rules and returns its port and allowed_ips. The policy names the
// Forgejo rule by its rules, not by a port, so a policy that also reaches Dolt
// on the same host still resolves to Forgejo.
func policyForgejoEndpoint(data []byte, host string) (sandboxEndpoint, error) {
	var doc policyDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return sandboxEndpoint{}, fmt.Errorf("parsing the policy: %w", err)
	}
	var found []policyEndpoint
	for _, np := range doc.NetworkPolicies {
		for _, ep := range np.Endpoints {
			if ep.Host == host && hasGitSmartHTTPRule(ep) {
				found = append(found, ep)
			}
		}
	}
	switch len(found) {
	case 0:
		return sandboxEndpoint{}, fmt.Errorf("no endpoint on %s carries the Forgejo git rules (a path ending .git/info/refs)", host)
	case 1:
	default:
		return sandboxEndpoint{}, fmt.Errorf("%d endpoints on %s carry the Forgejo git rules; want one", len(found), host)
	}

	ep := found[0]
	if ep.Port < 1 || ep.Port > 65535 {
		return sandboxEndpoint{}, fmt.Errorf("the Forgejo endpoint on %s has port %d; want 1-65535", host, ep.Port)
	}
	ips := make([]string, 0, len(ep.AllowedIPs))
	for _, raw := range ep.AllowedIPs {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return sandboxEndpoint{}, fmt.Errorf("allowed_ips %q: want a CIDR such as 192.168.5.2/32", raw)
		}
		ips = append(ips, prefix.String())
	}
	return sandboxEndpoint{Host: host, Port: ep.Port, AllowedIPs: ips}, nil
}

func hasGitSmartHTTPRule(ep policyEndpoint) bool {
	for _, r := range ep.Rules {
		if strings.HasSuffix(r.Allow.Path, ".git/info/refs") {
			return true
		}
	}
	return false
}

func sameSandboxEndpoint(a, b sandboxEndpoint) bool {
	if a.Host != b.Host || a.Port != b.Port || len(a.AllowedIPs) != len(b.AllowedIPs) {
		return false
	}
	for i := range a.AllowedIPs {
		if a.AllowedIPs[i] != b.AllowedIPs[i] {
			return false
		}
	}
	return true
}
