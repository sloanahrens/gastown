package k8spolicy

import (
	"fmt"
	"strings"
	"testing"
)

// firstDifference reports the first line at which got and want differ, as
// "line N: got ... want ...", or "" when they are the same text. A missing line
// reads as the end of the file, so a render that is longer or shorter than the
// checked-in file names the line where one of them stops.
func firstDifference(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(g) || i < len(w); i++ {
		var gl, wl string
		gok, wok := i < len(g), i < len(w)
		if gok {
			gl = g[i]
		}
		if wok {
			wl = w[i]
		}
		if gok && wok && gl == wl {
			continue
		}
		show := func(l string, ok bool) string {
			if !ok {
				return "<end of file>"
			}
			return fmt.Sprintf("%q", l)
		}
		return fmt.Sprintf("line %d: rendered %s, checked in %s", i+1, show(gl, gok), show(wl, wok))
	}
	return ""
}

// TestFirstDifferenceNamesTheLine checks the drift message: the first line that
// differs, by number, with both texts, and an end-of-file when one side stops.
func TestFirstDifferenceNamesTheLine(t *testing.T) {
	t.Parallel()
	base := "a\nb\nc\n"
	cases := []struct{ name, got, want, diff string }{
		{"the same", base, base, ""},
		{"one character changed", base, "a\nB\nc\n", `line 2: rendered "b", checked in "B"`},
		{"the checked-in file is shorter", base, "a\nb\n", `line 3: rendered "c", checked in ""`},
		{"the rendered text is shorter", "a\n", base, `line 2: rendered "", checked in "b"`},
		{"a line is added", "a\nx\nb\nc\n", base, `line 2: rendered "x", checked in "b"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := firstDifference(tc.got, tc.want); got != tc.diff {
				t.Errorf("firstDifference = %q, want %q", got, tc.diff)
			}
		})
	}
}

// TestCheckedInKindPolicyIsTheRender is the one test of this work item that
// reads the checked-in files, by repo-relative path and without writing: the
// checked-in Kubernetes policy must be exactly the default render of the Docker
// policy, so the two cannot drift, and the control-plane Services it names must
// be the four ExternalName Services the cluster defines. When the Docker policy
// changes without re-rendering deploy/k8s/policy/kind.yaml, it fails naming the
// first line that differs. It does not edit kind.yaml: re-rendering is the
// overseer's.
func TestCheckedInKindPolicyIsTheRender(t *testing.T) {
	t.Parallel()
	docker := readRepoFile(t, "../../deploy/polecat/policy.yaml")
	kind := readRepoFile(t, "../../deploy/k8s/policy/kind.yaml")

	rendered := mustRender(t, docker, defaultParams())
	if d := firstDifference(rendered, kind); d != "" {
		t.Errorf("deploy/k8s/policy/kind.yaml is not the default render of deploy/polecat/policy.yaml (%s); re-render it with render-policy", d)
	}

}

// TestCheckedInServices reads the Services file the policy's names resolve
// through: four ExternalName Services to the Mac, the four names, no selector.
func TestCheckedInServices(t *testing.T) {
	t.Parallel()
	services := readRepoFile(t, "../../deploy/k8s/services/control-plane.yaml")
	{
		if got := strings.Count(services, "\nkind: Service"); got != 4 {
			t.Errorf("services file defines %d Services, want 4", got)
		}
		if got := strings.Count(services, "externalName: host.lima.internal"); got != 4 {
			t.Errorf("%d Services are an ExternalName to host.lima.internal, want 4", got)
		}
		for _, name := range []string{"control-plane", "dolt", "forgejo", "tracker"} {
			if !strings.Contains(services, "metadata: {name: "+name+",") {
				t.Errorf("no Service named %s", name)
			}
		}
		if strings.Contains(services, "selector:") {
			t.Errorf("a Service has a selector key")
		}
	}
}
