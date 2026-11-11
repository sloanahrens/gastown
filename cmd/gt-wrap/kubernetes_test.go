package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/gtwrap"
)

// kubernetesConfig is a complete kubernetes target in the config file.
const kubernetesConfig = `{"target": "kubernetes", "context": "colima-gt-control", "namespace": "gt", "deployment": "daemon", "container": "daemon"}`

func TestRunBuildsTheKubernetesTargetFromItsFields(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, kubernetesConfig)
	if exit := h.run("gt", "status", "--line"); exit != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
	}
	if !h.kubeBuilt || h.targetSet {
		t.Fatalf("kubernetes built = %v, docker built = %v; want the kubernetes target only", h.kubeBuilt, h.targetSet)
	}
	want := kubeConfig{Context: "colima-gt-control", Namespace: "gt", Deployment: "daemon", Container: "daemon"}
	if h.kubeCfg != want {
		t.Fatalf("kubernetes target built from %+v, want %+v", h.kubeCfg, want)
	}
	if h.kubeTool != "gt" {
		t.Fatalf("kubernetes target told tool %q, want the invoked name gt", h.kubeTool)
	}
	if !h.fake.launched {
		t.Fatal("the wrapper launched nothing through the kubernetes target")
	}
}

// TestRunRefusesAKubernetesTargetMissingAField: each field the kubernetes target
// needs is refused by name, and no target is built for the refused config.
func TestRunRefusesAKubernetesTargetMissingAField(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"context", "namespace", "deployment", "container"} {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.writeConfig(t, withoutField(t, field))
			if exit := h.run("gt", "status"); exit != 1 {
				t.Fatalf("run = %d, want 1", exit)
			}
			if !strings.Contains(h.stderr.String(), `"`+field+`"`) {
				t.Fatalf("stderr %q does not name the missing field %q", h.stderr, field)
			}
			if h.kubeBuilt || h.fake.launched {
				t.Fatal("a target was built for a kubernetes config missing a field")
			}
		})
	}
}

// TestRunRefusesAKubernetesTargetWhoseFieldsDoNotParse: a kubernetes field of the
// wrong type refuses the config rather than being read as absent.
func TestRunRefusesAKubernetesTargetWhoseFieldsDoNotParse(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, `{"target": "kubernetes", "context": 5, "namespace": "gt", "deployment": "daemon", "container": "daemon"}`)
	if exit := h.run("gt", "status"); exit != 1 {
		t.Fatalf("run = %d, want 1", exit)
	}
	if !strings.Contains(h.stderr.String(), "kubernetes target fields") {
		t.Fatalf("stderr %q does not name the kubernetes fields", h.stderr)
	}
	if h.kubeBuilt || h.fake.launched {
		t.Fatal("a target was built for a kubernetes config whose fields do not parse")
	}
}

// TestRunKeepsTheComposeTargetParsing: the docker target reads no kubernetes fields,
// so a config that names docker parses as it did before, extra fields and all.
func TestRunKeepsTheComposeTargetParsing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, `{"target": "docker", "context": 5, "container": ["not", "a", "string"]}`)
	if exit := h.run("gt", "status"); exit != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
	}
	if !h.targetSet || h.kubeBuilt {
		t.Fatalf("docker built = %v, kubernetes built = %v; want the docker target only", h.targetSet, h.kubeBuilt)
	}
}

// TestDoctorExitsOneWhenTheKubernetesLadderIsDown: the doctor's refusal reaches the
// operator as exit 1 with the rung down and the reason, for the kubernetes target.
func TestDoctorExitsOneWhenTheKubernetesLadderIsDown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, kubernetesConfig)
	h.fake.diag.Rung = gtwrap.RungDown
	h.fake.diag.Container = "colima-gt-control/daemon"
	h.fake.diag.Image = "deployment/daemon in namespace gt"
	h.fake.diag.Reason = "kubectl is not on PATH, so the kubernetes target cannot run"
	if exit := h.run("gt", doctorFlag); exit != 1 {
		t.Fatalf("run(%s) = %d, want 1; stderr: %s", doctorFlag, exit, h.stderr)
	}
	if !h.fake.doctored || h.fake.launched {
		t.Fatalf("doctored = %v, launched = %v; want the doctor only", h.fake.doctored, h.fake.launched)
	}
	if !strings.Contains(h.stdout.String(), "rung: down") || !strings.Contains(h.stdout.String(), "kubectl is not on PATH") {
		t.Fatalf("report %q does not show the down rung and its reason", h.stdout)
	}
}

// withoutField is the kubernetes config with one field dropped, so the test names
// the field the config is missing.
func withoutField(t *testing.T, field string) string {
	t.Helper()
	fields := map[string]string{
		"target":     "kubernetes",
		"context":    "colima-gt-control",
		"namespace":  "gt",
		"deployment": "daemon",
		"container":  "daemon",
	}
	delete(fields, field)
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal the config without %q: %v", field, err)
	}
	return string(data)
}
