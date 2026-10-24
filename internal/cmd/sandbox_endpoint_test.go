package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// dockerForgejoPolicy is the Forgejo rule of the Docker policy shape: the host
// alias with no allowed_ips, and Dolt's endpoint beside it on another port.
const dockerForgejoPolicy = `version: 1
network_policies:
  forgejo_git:
    endpoints:
      - host: host.docker.internal
        port: 3000
        protocol: rest
        enforcement: enforce
        rules:
          - allow: { method: GET, path: "/*/*.git/info/refs" }
          - allow: { method: POST, path: "/*/*.git/git-upload-pack" }
  dolt:
    endpoints:
      - host: host.docker.internal
        port: 3307
        protocol: tcp
`

// k8sForgejoPolicy is the Forgejo rule of the Kubernetes policy shape: the
// in-cluster Service name with its allowed_ips, and Dolt on the same host.
const k8sForgejoPolicy = `version: 1
network_policies:
  forgejo_git:
    endpoints:
      - host: control-plane.openshell.svc.cluster.local
        allowed_ips: ["192.168.5.2/32"]
        port: 3000
        protocol: rest
        enforcement: enforce
        rules:
          - allow: { method: GET, path: "/*/*.git/info/refs" }
  dolt:
    endpoints:
      - host: control-plane.openshell.svc.cluster.local
        allowed_ips: ["192.168.5.2/32"]
        port: 3307
        protocol: tcp
        tls: skip
`

// writePolicyRoot writes each named policy under a fresh temp root and returns
// the root, so a profile's relative policy path resolves inside it.
func writePolicyRoot(t *testing.T, policies map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range policies {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing fixture policy %s: %v", name, err)
		}
	}
	return root
}

func TestForgejoEndpointForGateway(t *testing.T) {
	t.Parallel()
	dockerProfile := config.SandboxProfile{Image: "gastown-polecat:dev", Policy: "docker.yaml"}
	k8sProfile := config.SandboxProfile{
		Image:            "gastown-polecat:dev",
		Policy:           "k8s.yaml",
		Gateway:          "k8s",
		ControlPlaneHost: "control-plane.openshell.svc.cluster.local",
		Driver:           config.SandboxDriverKubernetes,
	}

	tests := []struct {
		name     string
		sandbox  map[string]config.SandboxProfile
		policies map[string]string
		gateway  string
		want     sandboxEndpoint
		wantErr  string
	}{
		{
			name:     "docker gateway binds the host alias with no allowed_ips",
			sandbox:  map[string]config.SandboxProfile{"polecat-default": dockerProfile},
			policies: map[string]string{"docker.yaml": dockerForgejoPolicy},
			gateway:  "openshell",
			want:     sandboxEndpoint{Host: "host.docker.internal", Port: 3000, AllowedIPs: []string{}},
		},
		{
			name:     "kubernetes gateway binds the Service name and its allowed_ips",
			sandbox:  map[string]config.SandboxProfile{"polecat-k8s": k8sProfile},
			policies: map[string]string{"k8s.yaml": k8sForgejoPolicy},
			gateway:  "k8s",
			want: sandboxEndpoint{
				Host:       "control-plane.openshell.svc.cluster.local",
				Port:       3000,
				AllowedIPs: []string{"192.168.5.2/32"},
			},
		},
		{
			name: "two profiles on one gateway that bind the same endpoint agree",
			sandbox: map[string]config.SandboxProfile{
				"polecat-a": {Image: "i", Policy: "k8s.yaml", Gateway: "k8s", ControlPlaneHost: "control-plane.openshell.svc.cluster.local"},
				"polecat-b": {Image: "i", Policy: "k8s.yaml", Gateway: "k8s", ControlPlaneHost: "control-plane.openshell.svc.cluster.local", CPU: "4"},
			},
			policies: map[string]string{"k8s.yaml": k8sForgejoPolicy},
			gateway:  "k8s",
			want: sandboxEndpoint{
				Host:       "control-plane.openshell.svc.cluster.local",
				Port:       3000,
				AllowedIPs: []string{"192.168.5.2/32"},
			},
		},
		{
			name: "two profiles on one gateway that bind different hosts are refused",
			sandbox: map[string]config.SandboxProfile{
				"polecat-a": {Image: "i", Policy: "docker.yaml", Gateway: "k8s"},
				"polecat-b": {Image: "i", Policy: "k8s.yaml", Gateway: "k8s", ControlPlaneHost: "control-plane.openshell.svc.cluster.local"},
			},
			policies: map[string]string{"docker.yaml": dockerForgejoPolicy, "k8s.yaml": k8sForgejoPolicy},
			gateway:  "k8s",
			wantErr:  "polecat-a and polecat-b both run on gateway \"k8s\" but bind different Forgejo endpoints",
		},
		{
			name:     "an unknown gateway names the gateways that exist",
			sandbox:  map[string]config.SandboxProfile{"polecat-default": dockerProfile, "polecat-k8s": k8sProfile},
			policies: map[string]string{"docker.yaml": dockerForgejoPolicy, "k8s.yaml": k8sForgejoPolicy},
			gateway:  "nope",
			wantErr:  `no sandbox profile names gateway "nope"; the gateways in settings are k8s, openshell`,
		},
		{
			name:     "no sandbox profiles at all says so",
			sandbox:  nil,
			policies: nil,
			gateway:  "openshell",
			wantErr:  "the gateways in settings are none",
		},
		{
			name:     "a policy with no Forgejo git rule on the host is refused",
			sandbox:  map[string]config.SandboxProfile{"polecat-default": dockerProfile},
			policies: map[string]string{"docker.yaml": "version: 1\nnetwork_policies: {}\n"},
			gateway:  "openshell",
			wantErr:  "no endpoint on host.docker.internal carries the Forgejo git rules",
		},
		{
			name:     "a malformed allowed_ips entry is refused",
			sandbox:  map[string]config.SandboxProfile{"polecat-k8s": k8sProfile},
			policies: map[string]string{"k8s.yaml": strings.Replace(k8sForgejoPolicy, "192.168.5.2/32", "192.168.5.2", 1)},
			gateway:  "k8s",
			wantErr:  `allowed_ips "192.168.5.2": want a CIDR`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := &config.TownSettings{Sandboxes: tt.sandbox}
			root := writePolicyRoot(t, tt.policies)
			got, err := forgejoEndpointForGateway(ts, tt.gateway, root)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("forgejoEndpointForGateway(%q) error = %v, want one containing %q", tt.gateway, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("forgejoEndpointForGateway(%q) unexpected error: %v", tt.gateway, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("forgejoEndpointForGateway(%q) = %+v, want %+v", tt.gateway, got, tt.want)
			}
		})
	}
}

// The shipped policies are the source of truth the endpoint is read from, so
// this reads them: a change to either file that moves the Forgejo endpoint
// shows up here, not as a 403 on the next sandbox clone.
func TestForgejoEndpointFromShippedPolicies(t *testing.T) {
	t.Parallel()
	ts := &config.TownSettings{Sandboxes: map[string]config.SandboxProfile{
		"polecat-default": {Image: "i", Policy: "deploy/polecat/policy.yaml"},
		"polecat-k8s": {
			Image:            "i",
			Policy:           "deploy/k8s/policy/kind.yaml",
			Gateway:          "k8s",
			ControlPlaneHost: "control-plane.openshell.svc.cluster.local",
		},
	}}
	const repoRoot = "../.."

	docker, err := forgejoEndpointForGateway(ts, "openshell", repoRoot)
	if err != nil {
		t.Fatalf("docker gateway: %v", err)
	}
	if want := (sandboxEndpoint{Host: "host.docker.internal", Port: 3000, AllowedIPs: []string{}}); !reflect.DeepEqual(docker, want) {
		t.Errorf("docker gateway endpoint = %+v, want %+v", docker, want)
	}

	k8s, err := forgejoEndpointForGateway(ts, "k8s", repoRoot)
	if err != nil {
		t.Fatalf("k8s gateway: %v", err)
	}
	want := sandboxEndpoint{
		Host:       "control-plane.openshell.svc.cluster.local",
		Port:       3000,
		AllowedIPs: []string{"192.168.5.2/32"},
	}
	if !reflect.DeepEqual(k8s, want) {
		t.Errorf("k8s gateway endpoint = %+v, want %+v", k8s, want)
	}
}

func TestPrintSandboxEndpoint(t *testing.T) {
	t.Parallel()
	ep := sandboxEndpoint{
		Host:       "control-plane.openshell.svc.cluster.local",
		Port:       3000,
		AllowedIPs: []string{"192.168.5.2/32", "10.0.0.1/32"},
	}

	var lines bytes.Buffer
	if err := printSandboxEndpoint(&lines, ep, false); err != nil {
		t.Fatalf("printSandboxEndpoint: %v", err)
	}
	wantLines := "host=control-plane.openshell.svc.cluster.local\nport=3000\nallowed_ips=192.168.5.2/32,10.0.0.1/32\n"
	if got := lines.String(); got != wantLines {
		t.Errorf("default output = %q, want %q", got, wantLines)
	}

	var empty bytes.Buffer
	if err := printSandboxEndpoint(&empty, sandboxEndpoint{Host: "host.docker.internal", Port: 3000}, false); err != nil {
		t.Fatalf("printSandboxEndpoint: %v", err)
	}
	if got, want := empty.String(), "host=host.docker.internal\nport=3000\nallowed_ips=\n"; got != want {
		t.Errorf("default output with no allowed_ips = %q, want %q", got, want)
	}

	var js bytes.Buffer
	if err := printSandboxEndpoint(&js, sandboxEndpoint{Host: "host.docker.internal", Port: 3000, AllowedIPs: []string{}}, true); err != nil {
		t.Fatalf("printSandboxEndpoint --json: %v", err)
	}
	if got, want := js.String(), `{"host":"host.docker.internal","port":3000,"allowed_ips":[]}`+"\n"; got != want {
		t.Errorf("--json output = %q, want %q", got, want)
	}
}

// shippedForgejoProfile is the forgejo-polecat profile the repo ships, which
// names the Docker host alias.
const shippedForgejoProfile = "../../deploy/polecat/profiles/forgejo-polecat.yaml"

func TestRenderForgejoProfile(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(shippedForgejoProfile)
	if err != nil {
		t.Fatalf("reading the shipped profile: %v", err)
	}
	const dockerEndpointLines = "  - host: host.docker.internal\n    port: 3000\n"

	t.Run("the Docker endpoint renders the shipped file byte for byte", func(t *testing.T) {
		got, err := renderForgejoProfile(src, sandboxEndpoint{Host: "host.docker.internal", Port: 3000})
		if err != nil {
			t.Fatalf("renderForgejoProfile: %v", err)
		}
		if !bytes.Equal(got, src) {
			t.Errorf("the Docker render of %s differs from the file; the Docker gateway would import something new. got:\n%s", shippedForgejoProfile, got)
		}
	})

	t.Run("the Kubernetes endpoint replaces the host, the port and adds allowed_ips", func(t *testing.T) {
		ep := sandboxEndpoint{
			Host:       "control-plane.openshell.svc.cluster.local",
			Port:       3000,
			AllowedIPs: []string{"192.168.5.2/32"},
		}
		got, err := renderForgejoProfile(src, ep)
		if err != nil {
			t.Fatalf("renderForgejoProfile: %v", err)
		}
		want := strings.Replace(string(src), dockerEndpointLines,
			"  - host: control-plane.openshell.svc.cluster.local\n    port: 3000\n    allowed_ips: [\"192.168.5.2/32\"]\n", 1)
		if string(got) != want {
			t.Errorf("the Kubernetes render changed more than the endpoint. got:\n%s", got)
		}
	})

	t.Run("several allowed addresses render as one list and the port is the endpoint's", func(t *testing.T) {
		ep := sandboxEndpoint{
			Host:       "gt-chosen.example.test",
			Port:       4000,
			AllowedIPs: []string{"10.9.8.7/32", "10.9.8.8/32"},
		}
		got, err := renderForgejoProfile(src, ep)
		if err != nil {
			t.Fatalf("renderForgejoProfile: %v", err)
		}
		for _, line := range []string{
			"  - host: gt-chosen.example.test\n",
			"    port: 4000\n",
			"    allowed_ips: [\"10.9.8.7/32\",\"10.9.8.8/32\"]\n",
		} {
			if !strings.Contains(string(got), line) {
				t.Errorf("the render lacks %q; got:\n%s", line, got)
			}
		}
	})

	t.Run("the shape of the Docker lines does not matter, only the git rules", func(t *testing.T) {
		// A profile whose endpoint is not the Docker alias, with a comment and
		// another endpoint ahead of it: the rules pick the endpoint, and
		// everything else on the lines stays.
		profile := "id: x\nendpoints:\n  - host: other.test\n    port: 443\n    protocol: tcp\n" +
			"  - host: forgejo.test # the forge\n    port: 8080\n    protocol: rest\n" +
			"    rules:\n      - allow: { method: GET, path: \"/*/*.git/info/refs\" }\n"
		got, err := renderForgejoProfile([]byte(profile), sandboxEndpoint{Host: "gw.test", Port: 3000, AllowedIPs: []string{"10.0.0.1/32"}})
		if err != nil {
			t.Fatalf("renderForgejoProfile: %v", err)
		}
		want := "id: x\nendpoints:\n  - host: other.test\n    port: 443\n    protocol: tcp\n" +
			"  - host: gw.test # the forge\n    port: 3000\n    allowed_ips: [\"10.0.0.1/32\"]\n    protocol: rest\n" +
			"    rules:\n      - allow: { method: GET, path: \"/*/*.git/info/refs\" }\n"
		if string(got) != want {
			t.Errorf("render = %q, want %q", got, want)
		}
	})

	refused := []struct {
		name, profile, wantErr string
	}{
		{"no endpoint carries the git rules", "id: other\nendpoints:\n  - host: example.test\n    port: 443\n", "no endpoint with the Forgejo git rules"},
		{"two endpoints carry the git rules", twoGitEndpoints, "more than one endpoint with the Forgejo git rules"},
		{"the endpoint already names allowed_ips", strings.Replace(string(src), "    port: 3000\n", "    port: 3000\n    allowed_ips: [\"1.2.3.4/32\"]\n", 1), "already names allowed_ips"},
		{"host and port share a line", "id: x\nendpoints:\n  - {host: a.test, port: 1, rules: [{allow: {path: \"/x.git/info/refs\"}}]}\n", "host and port on one line"},
		{"a quoted host is not a plain value", strings.Replace(string(src), "- host: host.docker.internal", `- host: "host.docker.internal"`, 1), "not a plain value"},
		{"not yaml", "id: [unclosed", "parsing the profile"},
	}
	for _, tt := range refused {
		t.Run("refused: "+tt.name, func(t *testing.T) {
			_, err := renderForgejoProfile([]byte(tt.profile), sandboxEndpoint{Host: "gw.test", Port: 3000})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("renderForgejoProfile error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

const twoGitEndpoints = `id: x
endpoints:
  - host: a.test
    port: 1
    rules:
      - allow: { method: GET, path: "/*/*.git/info/refs" }
  - host: b.test
    port: 2
    rules:
      - allow: { method: GET, path: "/*/*.git/info/refs" }
`

func TestProfileEndpointDiff(t *testing.T) {
	t.Parallel()
	k8s := sandboxEndpoint{Host: "control-plane.openshell.svc.cluster.local", Port: 3000, AllowedIPs: []string{"192.168.5.2/32"}}
	docker := sandboxEndpoint{Host: "host.docker.internal", Port: 3000}
	profile := func(host string, port int, ips string) string {
		out := "resource_version: 7\nid: forgejo-polecat\nendpoints:\n  - host: " + host + "\n    port: " + strconv.Itoa(port) + "\n"
		if ips != "" {
			out += "    allowed_ips: [" + ips + "]\n"
		}
		return out
	}

	tests := []struct {
		name     string
		exported string
		ep       sandboxEndpoint
		wantDiff string // "" means bound
	}{
		{"the k8s profile binds the k8s endpoint", profile(k8s.Host, 3000, `"192.168.5.2/32"`), k8s, ""},
		{"the Docker profile binds the Docker endpoint", profile(docker.Host, 3000, ""), docker, ""},
		{"the Docker host on the k8s gateway is stale", profile(docker.Host, 3000, ""), k8s, "the profile binds host.docker.internal:3000 allowed_ips []"},
		// The cases a substring check passes: the wanted host is a part of
		// the profile's other host, or of its port.
		{"a host that merely contains the wanted host is stale", profile("x."+k8s.Host, 3000, `"192.168.5.2/32"`), k8s, "binds x.control-plane"},
		{"a longer host name is stale", profile("host.docker.internal.evil.test", 3000, ""), docker, "binds host.docker.internal.evil.test:3000"},
		{"a drifted port is stale", profile(k8s.Host, 3001, `"192.168.5.2/32"`), k8s, "binds control-plane.openshell.svc.cluster.local:3001"},
		{"a missing allowed_ips is stale", profile(k8s.Host, 3000, ""), k8s, "allowed_ips [], want"},
		{"a wrong allowed_ips is stale", profile(k8s.Host, 3000, `"192.168.5.20/32"`), k8s, "allowed_ips [192.168.5.20/32]"},
		{"an address where a prefix of it was wanted is stale", profile(k8s.Host, 3000, `"192.168.5.2/3"`), k8s, "allowed_ips [192.168.5.2/3]"},
		{"extra allowed_ips beyond the wanted are stale", profile(k8s.Host, 3000, `"192.168.5.2/32","10.0.0.1/32"`), k8s, "allowed_ips [192.168.5.2/32,10.0.0.1/32]"},
		{"the host named only in a comment is stale", "# " + k8s.Host + "\n" + profile(docker.Host, 3000, ""), k8s, "binds host.docker.internal"},
		{"a second endpoint on another host is stale", profile(k8s.Host, 3000, `"192.168.5.2/32"`) + "  - host: host.docker.internal\n    port: 3000\n", k8s, "binds host.docker.internal:3000"},
		{"a profile with no endpoint is stale", "id: forgejo-polecat\n", k8s, "names no endpoint"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := profileEndpointDiff([]byte(tt.exported), tt.ep)
			if err != nil {
				t.Fatalf("profileEndpointDiff: %v", err)
			}
			if tt.wantDiff == "" {
				if got != "" {
					t.Errorf("diff = %q, want the profile bound", got)
				}
				return
			}
			if !strings.Contains(got, tt.wantDiff) {
				t.Errorf("diff = %q, want one containing %q", got, tt.wantDiff)
			}
		})
	}

	t.Run("allowed_ips order does not matter", func(t *testing.T) {
		ep := sandboxEndpoint{Host: "h.test", Port: 3000, AllowedIPs: []string{"10.0.0.1/32", "10.0.0.2/32"}}
		got, err := profileEndpointDiff([]byte(profile("h.test", 3000, `"10.0.0.2/32","10.0.0.1/32"`)), ep)
		if err != nil || got != "" {
			t.Errorf("diff = %q, err = %v, want bound", got, err)
		}
	})

	t.Run("an unparsable export is an error", func(t *testing.T) {
		if _, err := profileEndpointDiff([]byte("endpoints: [unclosed"), k8s); err == nil {
			t.Error("profileEndpointDiff of malformed yaml = nil error, want one")
		}
	})
}

func TestStampResourceVersion(t *testing.T) {
	t.Parallel()
	got, err := stampResourceVersion([]byte("id: x\n"), []byte("resource_version: 7\nid: x\n"))
	if err != nil {
		t.Fatalf("stampResourceVersion: %v", err)
	}
	if want := "id: x\nresource_version: 7\n"; string(got) != want {
		t.Errorf("stamped = %q, want %q", got, want)
	}
	// A profile whose last line has no newline still gets the field on its own line.
	if got, err = stampResourceVersion([]byte("id: x"), []byte("resource_version: 3\n")); err != nil || string(got) != "id: x\nresource_version: 3\n" {
		t.Errorf("stamped = %q, err = %v, want the version on its own line", got, err)
	}
	for _, exported := range []string{"id: x\n", "resource_version: 0\n", "resource_version: [\n"} {
		if _, err := stampResourceVersion([]byte("id: x\n"), []byte(exported)); err == nil {
			t.Errorf("stampResourceVersion with export %q = nil error, want one", exported)
		}
	}
}

// The run function the command wires to: every flag reaches the behavior it
// names, and the flag combinations that ask two things at once are refused.
func TestExecuteSandboxEndpoint(t *testing.T) {
	t.Parallel()
	ts := &config.TownSettings{Sandboxes: map[string]config.SandboxProfile{
		"polecat-default": {Image: "i", Policy: "docker.yaml"},
		"polecat-k8s": {
			Image:            "i",
			Policy:           "k8s.yaml",
			Gateway:          "k8s",
			ControlPlaneHost: "control-plane.openshell.svc.cluster.local",
		},
	}}
	root := writePolicyRoot(t, map[string]string{"docker.yaml": dockerForgejoPolicy, "k8s.yaml": k8sForgejoPolicy})
	shipped, err := os.ReadFile(shippedForgejoProfile)
	if err != nil {
		t.Fatalf("reading the shipped profile: %v", err)
	}
	files := t.TempDir()
	profilePath := filepath.Join(files, "forgejo-polecat.yaml")
	exportPath := filepath.Join(files, "export.yaml")
	if err := os.WriteFile(profilePath, shipped, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exportPath, append([]byte("resource_version: 7\n"), shipped...), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(opts sandboxEndpointOptions) (string, error) {
		var out bytes.Buffer
		err := executeSandboxEndpoint(&out, opts, ts, root)
		return out.String(), err
	}

	t.Run("no flag prints the three lines", func(t *testing.T) {
		got, err := run(sandboxEndpointOptions{Gateway: "k8s"})
		want := "host=control-plane.openshell.svc.cluster.local\nport=3000\nallowed_ips=192.168.5.2/32\n"
		if err != nil || got != want {
			t.Errorf("output = %q, err = %v, want %q", got, err, want)
		}
	})

	t.Run("--json prints the JSON", func(t *testing.T) {
		got, err := run(sandboxEndpointOptions{Gateway: "openshell", JSON: true})
		if want := `{"host":"host.docker.internal","port":3000,"allowed_ips":[]}` + "\n"; err != nil || got != want {
			t.Errorf("output = %q, err = %v, want %q", got, err, want)
		}
	})

	t.Run("--render prints the profile bound to the gateway", func(t *testing.T) {
		got, err := run(sandboxEndpointOptions{Gateway: "k8s", Render: profilePath})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "  - host: control-plane.openshell.svc.cluster.local\n    port: 3000\n    allowed_ips: [\"192.168.5.2/32\"]\n") {
			t.Errorf("render lacks the k8s endpoint:\n%s", got)
		}
		docker, err := run(sandboxEndpointOptions{Gateway: "openshell", Render: profilePath})
		if err != nil || docker != string(shipped) {
			t.Errorf("the Docker render differs from the shipped profile (err = %v)", err)
		}
	})

	t.Run("--render with --from-export carries the gateway's resource_version", func(t *testing.T) {
		got, err := run(sandboxEndpointOptions{Gateway: "k8s", Render: profilePath, FromExport: exportPath})
		if err != nil || !strings.HasSuffix(got, "\nresource_version: 7\n") {
			t.Errorf("output ends %q, err = %v, want the version stamped last", got[max(0, len(got)-40):], err)
		}
	})

	t.Run("--check says bound or stale for each gateway", func(t *testing.T) {
		// exportPath holds the Docker profile.
		got, err := run(sandboxEndpointOptions{Gateway: "openshell", Check: exportPath})
		if err != nil || got != "bound\n" {
			t.Errorf("Docker gateway: output = %q, err = %v, want bound", got, err)
		}
		got, err = run(sandboxEndpointOptions{Gateway: "k8s", Check: exportPath})
		if err != nil || !strings.HasPrefix(got, "stale: the profile binds host.docker.internal:3000") {
			t.Errorf("k8s gateway: output = %q, err = %v, want stale on the Docker host", got, err)
		}
	})

	t.Run("a missing file is an error, not an answer", func(t *testing.T) {
		missing := filepath.Join(files, "missing.yaml")
		for _, opts := range []sandboxEndpointOptions{
			{Gateway: "k8s", Render: missing},
			{Gateway: "k8s", Check: missing},
			{Gateway: "k8s", Render: profilePath, FromExport: missing},
		} {
			if _, err := run(opts); err == nil {
				t.Errorf("run(%+v) = nil error, want one", opts)
			}
		}
	})

	t.Run("an unknown gateway is refused before any file is read", func(t *testing.T) {
		_, err := run(sandboxEndpointOptions{Gateway: "nope", Check: exportPath})
		if err == nil || !strings.Contains(err.Error(), `no sandbox profile names gateway "nope"`) {
			t.Errorf("error = %v, want the unknown gateway named", err)
		}
	})

	for name, opts := range map[string]sandboxEndpointOptions{
		"--render with --check":          {Gateway: "k8s", Render: profilePath, Check: exportPath},
		"--from-export without --render": {Gateway: "k8s", FromExport: exportPath},
		"--json with --render":           {Gateway: "k8s", JSON: true, Render: profilePath},
		"--json with --check":            {Gateway: "k8s", JSON: true, Check: exportPath},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			got, err := run(opts)
			if err == nil || got != "" {
				t.Errorf("output = %q, err = %v, want an error and no output", got, err)
			}
		})
	}
}

// The cobra wiring: the command is registered as `gt sandbox endpoint`, takes
// the flags the provider script passes, and refuses to run without --gateway.
func TestSandboxEndpointCommandWiring(t *testing.T) {
	t.Parallel()
	found, rest, err := rootCmd.Find([]string{"sandbox", "endpoint"})
	if err != nil || found != sandboxEndpointCmd || len(rest) != 0 {
		t.Fatalf("rootCmd.Find(sandbox endpoint) = %v, %v, %v; want the endpoint command", found, rest, err)
	}
	for _, flag := range []string{"gateway", "root", "json", "render", "from-export", "check"} {
		if sandboxEndpointCmd.Flags().Lookup(flag) == nil {
			t.Errorf("gt sandbox endpoint has no --%s flag", flag)
		}
	}
	if err := sandboxEndpointCmd.ValidateRequiredFlags(); err == nil || !strings.Contains(err.Error(), "gateway") {
		t.Errorf("ValidateRequiredFlags with no --gateway = %v, want an error naming it", err)
	}
}
