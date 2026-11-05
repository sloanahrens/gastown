package k8spolicy

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// forwardDoc is the part of a rendered document the forwarder test reads.
type forwardDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				HostNetwork       bool   `yaml:"hostNetwork"`
				PriorityClassName string `yaml:"priorityClassName"`
				Containers        []struct {
					Name      string   `yaml:"name"`
					Image     string   `yaml:"image"`
					Args      []string `yaml:"args"`
					Resources struct {
						Requests map[string]string `yaml:"requests"`
						Limits   map[string]string `yaml:"limits"`
					} `yaml:"resources"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// decodeForward parses every document of a rendered text and fails the test
// when the text is not YAML, which a comparison of strings cannot tell.
func decodeForward(t *testing.T, text string) []forwardDoc {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(text))
	var docs []forwardDoc
	for {
		var d forwardDoc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			return docs
		}
		if err != nil {
			t.Fatalf("the rendered text does not parse: %v\n%s", err, text)
		}
		docs = append(docs, d)
	}
}

// forwardDaemonSet returns the DaemonSet of a rendered forwarder and fails the
// test when there is none or more than one.
func forwardDaemonSet(t *testing.T, text string) forwardDoc {
	t.Helper()
	var found []forwardDoc
	for _, d := range decodeForward(t, text) {
		if d.Kind == "DaemonSet" {
			found = append(found, d)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d DaemonSets in the render, want 1:\n%s", len(found), text)
	}
	return found[0]
}

// TestRenderRegistryForwardOnTheKindHostsFile reads the kind hosts file and
// checks the DaemonSet the render makes of it: the host network, a listener on
// 127.0.0.1 only, a relay to the file's control_plane_host, a pinned image,
// the resource requests and limit, and the priority class.
func TestRenderRegistryForwardOnTheKindHostsFile(t *testing.T) {
	t.Parallel()
	hostsText := readRepoFile(t, "../../deploy/k8s/values/kind/hosts.yaml")
	var hosts struct {
		ControlPlaneHost string `yaml:"control_plane_host"`
	}
	if err := yaml.Unmarshal([]byte(hostsText), &hosts); err != nil {
		t.Fatal(err)
	}
	got, err := RenderRegistryForward("kind", []byte(hostsText))
	if err != nil {
		t.Fatal(err)
	}
	ds := forwardDaemonSet(t, got)
	if ds.Metadata.Namespace != "sweep-registry-forward" {
		t.Errorf("namespace = %q, want sweep-registry-forward", ds.Metadata.Namespace)
	}
	if !ds.Spec.Template.Spec.HostNetwork {
		t.Error("hostNetwork is not true")
	}
	if pc := ds.Spec.Template.Spec.PriorityClassName; pc != "gt-control-plane" {
		t.Errorf("priorityClassName = %q, want gt-control-plane", pc)
	}
	cs := ds.Spec.Template.Spec.Containers
	if len(cs) != 1 || cs[0].Name != "socat" {
		t.Fatalf("containers = %+v, want one container named socat", cs)
	}
	c := cs[0]
	if want := "alpine/socat:1.8.0.3"; c.Image != want {
		t.Errorf("image = %q, want %q", c.Image, want)
	}
	wantArgs := []string{"TCP-LISTEN:5001,fork,reuseaddr,bind=127.0.0.1", "TCP:" + hosts.ControlPlaneHost + ":5001"}
	if !reflect.DeepEqual(c.Args, wantArgs) {
		t.Errorf("args = %q, want %q", c.Args, wantArgs)
	}
	if !reflect.DeepEqual(c.Resources.Requests, map[string]string{"cpu": "10m", "memory": "16Mi"}) {
		t.Errorf("requests = %v, want cpu 10m and memory 16Mi", c.Resources.Requests)
	}
	if !reflect.DeepEqual(c.Resources.Limits, map[string]string{"memory": "64Mi"}) {
		t.Errorf("limits = %v, want memory 64Mi", c.Resources.Limits)
	}
	if strings.Contains(got, "0.0.0.0") {
		t.Errorf("the render names 0.0.0.0; the registry has no authentication and the listener must bind 127.0.0.1 only:\n%s", got)
	}
}

// TestRenderRegistryForwardFollowsTheRegistryPort proves the port comes from
// registry_host: a hosts file with another port gets that port on both the
// listener and the relay, and the old 5001 appears nowhere.
func TestRenderRegistryForwardFollowsTheRegistryPort(t *testing.T) {
	t.Parallel()
	got, err := RenderRegistryForward("lab", []byte("control_plane_host: cp.example\nregistry_host: localhost:6000\n"))
	if err != nil {
		t.Fatal(err)
	}
	ds := forwardDaemonSet(t, got)
	want := []string{"TCP-LISTEN:6000,fork,reuseaddr,bind=127.0.0.1", "TCP:cp.example:6000"}
	if args := ds.Spec.Template.Spec.Containers[0].Args; !reflect.DeepEqual(args, want) {
		t.Errorf("args = %q, want %q", args, want)
	}
	if strings.Contains(got, "5001") {
		t.Errorf("the render names port 5001 although registry_host is localhost:6000:\n%s", got)
	}
}

// TestRenderRegistryForwardRefusalsNameTheirKey drives each refusal: a missing
// key is named, a registry_host that is not host:port with a port from 1 to
// 65535 is refused, and nothing is rendered on a refusal.
func TestRenderRegistryForwardRefusalsNameTheirKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		env   string
		hosts string
		want  string
	}{
		{"no control_plane_host", "kind", "registry_host: localhost:5001\n", "no control_plane_host"},
		{"no registry_host", "kind", "control_plane_host: cp.example\n", "no registry_host"},
		{"unparsable file", "kind", "control_plane_host: [", "does not parse"},
		{"control_plane_host that would end the scalar", "kind", "control_plane_host: \"a}\\n---\"\nregistry_host: localhost:5001\n", "not a host name"},
		{"registry_host without a port", "kind", "control_plane_host: cp.example\nregistry_host: localhost\n", "host:port"},
		{"registry_host with port 0", "kind", "control_plane_host: cp.example\nregistry_host: localhost:0\n", "host:port"},
		{"registry_host with port 65536", "kind", "control_plane_host: cp.example\nregistry_host: localhost:65536\n", "host:port"},
		{"registry_host with a leading zero", "kind", "control_plane_host: cp.example\nregistry_host: localhost:05001\n", "host:port"},
		{"registry_host with a sign", "kind", "control_plane_host: cp.example\nregistry_host: localhost:+5001\n", "host:port"},
		{"registry_host with a bad name", "kind", "control_plane_host: cp.example\nregistry_host: -bad:5001\n", "host:port"},
		{"environment that is not a plain word", "../x", "control_plane_host: cp.example\nregistry_host: localhost:5001\n", "environment name"},
		{"empty environment", "", "control_plane_host: cp.example\nregistry_host: localhost:5001\n", "environment name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := RenderRegistryForward(tc.env, []byte(tc.hosts))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want one containing %q", err, tc.want)
			}
			if got != "" {
				t.Errorf("rendered text despite the refusal: %q", got)
			}
		})
	}
}
