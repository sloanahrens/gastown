package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
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

	t.Run("a profile with no Docker endpoint is refused", func(t *testing.T) {
		_, err := renderForgejoProfile([]byte("id: other\nendpoints:\n  - host: example.test\n    port: 443\n"), sandboxEndpoint{Host: "host.docker.internal", Port: 3000})
		if err == nil || !strings.Contains(err.Error(), "host.docker.internal endpoint on port 3000") {
			t.Errorf("renderForgejoProfile error = %v, want one naming the missing host.docker.internal endpoint", err)
		}
	})
}
