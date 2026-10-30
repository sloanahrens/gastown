package k8spolicy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// kindDrift renders the Docker policy at dockerPath with the defaults and
// compares it with the Kubernetes policy at kindPath. It returns "" when they
// are the same text, and otherwise the failure message naming the first line that
// differs. The test below passes the checked-in files; another test passes a copy
// with one line changed, so the failing branch runs.
func kindDrift(t *testing.T, dockerPath, kindPath string) string {
	t.Helper()
	docker := readRepoFile(t, dockerPath)
	kind := readRepoFile(t, kindPath)
	d := firstDifference(mustRender(t, docker, defaultParams()), kind)
	if d == "" {
		return ""
	}
	return fmt.Sprintf("%s is not the default render of %s (%s); re-render it with render-policy", kindPath, dockerPath, d)
}

// servicesProblems parses the multi-document Services file and returns what is
// wrong with it: it must define exactly the four Services control-plane, dolt,
// forgejo and tracker, each of type ExternalName to host.lima.internal, none with
// a selector. An empty result means the file is as the cluster expects.
func servicesProblems(text string) []string {
	type service struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Spec struct {
			Type         string         `yaml:"type"`
			ExternalName string         `yaml:"externalName"`
			Selector     map[string]any `yaml:"selector"`
		} `yaml:"spec"`
	}
	var problems []string
	seen := map[string]bool{}
	dec := yaml.NewDecoder(strings.NewReader(text))
	for {
		var svc service
		err := dec.Decode(&svc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return append(problems, "services file does not parse: "+err.Error())
		}
		if svc.Kind != "Service" {
			problems = append(problems, fmt.Sprintf("a document is a %q, not a Service", svc.Kind))
			continue
		}
		name := svc.Metadata.Name
		seen[name] = true
		if svc.Spec.Type != "ExternalName" || svc.Spec.ExternalName != "host.lima.internal" {
			problems = append(problems, fmt.Sprintf("Service %s is %s to %q, want ExternalName to host.lima.internal", name, svc.Spec.Type, svc.Spec.ExternalName))
		}
		if svc.Spec.Selector != nil {
			problems = append(problems, fmt.Sprintf("Service %s has a selector", name))
		}
	}
	for _, name := range []string{"control-plane", "dolt", "forgejo", "tracker"} {
		if !seen[name] {
			problems = append(problems, "no Service named "+name)
		}
	}
	if len(seen) != 4 {
		problems = append(problems, fmt.Sprintf("%d distinct Services, want 4", len(seen)))
	}
	return problems
}

// TestCheckedInKindPolicyIsTheRender is the one test of this work item that
// reads the checked-in files, by repo-relative path and without writing: the
// checked-in Kubernetes policy must be exactly the default render of the Docker
// policy, so the two cannot drift, and the Services file must be what the
// cluster expects. When the Docker policy changes without re-rendering
// deploy/k8s/policy/kind.yaml, it fails naming the first line that differs. It
// does not edit kind.yaml: re-rendering is the overseer's.
func TestCheckedInKindPolicyIsTheRender(t *testing.T) {
	t.Parallel()
	if msg := kindDrift(t, "../../deploy/polecat/policy.yaml", "../../deploy/k8s/policy/kind.yaml"); msg != "" {
		t.Error(msg)
	}
	for _, p := range servicesProblems(readRepoFile(t, "../../deploy/k8s/services/control-plane.yaml")) {
		t.Error(p)
	}
}

// TestKindDriftOnACopyOfKindYaml drives the failing branch of the drift check
// the way a stale kind.yaml would: a temp copy of the checked-in file with one
// line changed fails and names that line number and the changed text, and an
// identical copy passes. The checked-in files are only read.
func TestKindDriftOnACopyOfKindYaml(t *testing.T) {
	t.Parallel()
	const docker = "../../deploy/polecat/policy.yaml"
	original := readRepoFile(t, "../../deploy/k8s/policy/kind.yaml")
	dir := t.TempDir()
	write := func(name, text string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	if msg := kindDrift(t, docker, write("same.yaml", original)); msg != "" {
		t.Errorf("an identical copy failed: %s", msg)
	}

	lines := strings.Split(original, "\n")
	const changed = 2 // the third line, index 2
	lines[changed] += " DRIFT"
	msg := kindDrift(t, docker, write("drifted.yaml", strings.Join(lines, "\n")))
	if msg == "" {
		t.Fatal("a copy with one line changed passed")
	}
	for _, want := range []string{fmt.Sprintf("line %d:", changed+1), fmt.Sprintf("%q", lines[changed])} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}

// TestCheckedInServices checks the Services rules on a stored copy of the
// Services file (testdata/services.yaml) and on versions of it with one thing
// wrong: a missing Service, a selector, another type, another target, a fifth
// Service and a document that is not a Service.
func TestCheckedInServices(t *testing.T) {
	t.Parallel()
	good := readRepoFile(t, "testdata/services.yaml")
	if got := servicesProblems(good); len(got) != 0 {
		t.Fatalf("the stored services file has problems: %v", got)
	}
	dolt := "metadata: {name: dolt, namespace: openshell}\nspec: {type: ExternalName, externalName: host.lima.internal}"
	if !strings.Contains(good, dolt) {
		t.Fatal("the stored services file no longer holds the dolt Service this test edits")
	}
	cases := []struct{ name, text, want string }{
		{"a Service is missing", strings.Replace(good, "name: dolt,", "name: dolt2,", 1), "no Service named dolt"},
		{"a selector", strings.Replace(good, dolt, "metadata: {name: dolt, namespace: openshell}\nspec: {type: ExternalName, externalName: host.lima.internal, selector: {app: dolt}}", 1), "Service dolt has a selector"},
		{"another type", strings.Replace(good, dolt, strings.Replace(dolt, "type: ExternalName", "type: ClusterIP", 1), 1), "Service dolt is ClusterIP"},
		{"another target", strings.Replace(good, dolt, strings.Replace(dolt, "host.lima.internal", "elsewhere.example", 1), 1), `to "elsewhere.example"`},
		{"a fifth Service", good + "---\napiVersion: v1\nkind: Service\nmetadata: {name: extra, namespace: openshell}\nspec: {type: ExternalName, externalName: host.lima.internal}\n", "5 distinct Services, want 4"},
		{"not a Service", good + "---\nkind: ConfigMap\n", `a document is a "ConfigMap"`},
		{"unparseable", "kind: [", "does not parse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := strings.Join(servicesProblems(tc.text), "\n")
			if !strings.Contains(got, tc.want) {
				t.Errorf("problems %q lack %q", got, tc.want)
			}
		})
	}
}
