package k8spolicy

import (
	"strings"
	"testing"
)

func TestRenderServicesRefusalsNameTheirCause(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		env   string
		hosts string
		want  string
	}{
		{"unparsable file", "kind", "control_plane_host: [", "does not parse"},
		{"no host", "kind", "registry_host: r\n", "no control_plane_host"},
		{"host that would end the scalar", "kind", "control_plane_host: \"a}\\n---\"\n", "not a host name"},
		{"environment that is not a plain word", "../x", "control_plane_host: h\n", "environment name"},
		{"empty environment", "", "control_plane_host: h\n", "environment name"},
	}
	for _, tc := range cases {
		got, err := RenderServices(tc.env, []byte(tc.hosts))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want one containing %q", tc.name, err, tc.want)
		}
		if got != "" {
			t.Errorf("%s: rendered text despite the refusal: %q", tc.name, got)
		}
	}
}

func TestRenderServicesNamesTheFourServicesWithTheHost(t *testing.T) {
	t.Parallel()
	got, err := RenderServices("lab", []byte("control_plane_host: cp.example\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range ControlPlaneServiceNames {
		if !strings.Contains(got, "{name: "+name+", namespace: openshell}") {
			t.Errorf("no Service named %s in:\n%s", name, got)
		}
	}
	if n := strings.Count(got, "externalName: cp.example}"); n != 4 {
		t.Errorf("%d Services point at cp.example, want 4", n)
	}
}
