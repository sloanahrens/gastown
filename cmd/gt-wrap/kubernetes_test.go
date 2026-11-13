package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/gtwrap"
	"github.com/steveyegge/gastown/internal/manifestdata"
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

// TestRunKubernetesTargetRunsFromAnyDirectory: the kubernetes target maps onto
// $HOME/gt and needs no town marker, so a working directory outside any town is
// launched with its own path, and the target maps it with a note, not a refusal.
func TestRunKubernetesTargetRunsFromAnyDirectory(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, kubernetesConfig)
	outside := t.TempDir()
	h.surface.getwd = func() (string, error) { return outside, nil }
	if exit := h.run("gt", "status", "--line"); exit != 0 {
		t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
	}
	if want := filepath.Join(h.home, "gt"); h.builtTown != want {
		t.Fatalf("kubernetes target built for %q, want $HOME/gt %q", h.builtTown, want)
	}
	if !h.fake.launched || h.fake.call.Dir != outside {
		t.Fatalf("launched = %v with dir %q; want the call run from %q", h.fake.launched, h.fake.call.Dir, outside)
	}
}

// TestTownForRefusesAnUnsetHomeForTheKubernetesTarget: the kubernetes target maps
// onto $HOME/gt, so an unset HOME is refused rather than mapped from a relative gt.
func TestTownForRefusesAnUnsetHomeForTheKubernetesTarget(t *testing.T) {
	t.Parallel()
	s := surface{getenv: func(string) string { return "" }}
	if town, err := townFor(targetConfig{Target: kubernetesTarget}, s); err == nil {
		t.Fatalf("townFor = %q, want an error for an unset HOME", town)
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

// podRecorder is the pod's shell for these tests: the ladder's probes answer as a
// healthy cluster does, and each exec the wrapper attaches is recorded, so a test
// reads the root and directory arguments the walk-up script was given.
type podRecorder struct {
	attached [][]string
	// stdout and stderr are what a stream-carrying exec prints, and exit is what
	// it returns: the output and status of the pod's tool.
	stdout, stderr string
	exit           int
}

func (p *podRecorder) Attach(argv []string, _ []string) (int, error) {
	p.attached = append(p.attached, argv)
	return p.exit, nil
}

func (p *podRecorder) AttachStdin(argv []string, env []string, _ io.Reader) (int, error) {
	return p.Attach(argv, env)
}

// AttachStreams prints the canned output to the writers the wrapper wired and
// returns the tool's status, as a kubectl exec would.
func (p *podRecorder) AttachStreams(argv []string, env []string, _ io.Reader, stdout, stderr io.Writer) (int, error) {
	if _, err := io.WriteString(stdout, p.stdout); err != nil {
		return 0, err
	}
	if _, err := io.WriteString(stderr, p.stderr); err != nil {
		return 0, err
	}
	return p.Attach(argv, env)
}

func (p *podRecorder) Capture(_ context.Context, argv []string) (string, int, error) {
	line := strings.Join(argv, " ")
	switch {
	case strings.Contains(line, " --raw /readyz"):
		return "ok", 0, nil
	case strings.Contains(line, " deployment "):
		return `{"app":"daemon"}`, 0, nil
	case strings.Contains(line, " pods "):
		return "daemon-0", 0, nil
	}
	return "", 1, fmt.Errorf("unexpected probe: %s", line)
}

// newPodHarness is the harness with the real kubernetes target built over a
// podRecorder, so the config's pod_town_root reaches gtwrap.Kubectl as it does in
// the wrapper.
func newPodHarness(t *testing.T, config string) (*harness, *podRecorder) {
	t.Helper()
	h := newHarness(t)
	h.writeConfig(t, config)
	pod := &podRecorder{}
	h.surface.newKubectl = func(town string, kube kubeConfig, tool string) target {
		h.kubeBuilt, h.kubeCfg, h.kubeTool = true, kube, tool
		return gtwrap.Kubectl{
			Town:       town,
			PodRoot:    kube.PodRoot,
			Context:    kube.Context,
			Namespace:  kube.Namespace,
			Deployment: kube.Deployment,
			Container:  kube.Container,
			Tool:       tool,
			Shell:      pod,
			LookPath:   func(string) (string, error) { return "/usr/local/bin/kubectl", nil },
			Note:       h.stderr,
			Stdout:     h.stdout,
			Stderr:     h.stderr,
		}
	}
	return h, pod
}

// kubernetesConfigWith is the complete kubernetes config with extra top-level keys
// appended, the keys the wrapper reads beside the target's own fields.
func kubernetesConfigWith(extra string) string {
	return `{"target": "kubernetes", "context": "colima-gt-control", "namespace": "gt", "deployment": "daemon", "container": "daemon", ` + extra + `}`
}

// TestConfigPodTownRoot: pod_town_root replaces the compiled-in pod root for the
// mapping of the working directory and the walk-up's root and caller arguments; an
// absent key keeps the compiled-in root; a value that is not an absolute clean path
// refuses the file and names the key, under either target.
func TestConfigPodTownRoot(t *testing.T) {
	t.Parallel()
	t.Run("a set key maps the Mac town onto the configured root", func(t *testing.T) {
		t.Parallel()
		h, pod := newPodHarness(t, kubernetesConfigWith(`"pod_town_root": "/town"`))
		dir := filepath.Join(h.home, "gt", "rig", "x")
		h.surface.getwd = func() (string, error) { return dir, nil }
		if exit := h.run("gt", "status", "--line"); exit != 0 {
			t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
		}
		if len(pod.attached) != 1 {
			t.Fatalf("attached %d execs, want 1", len(pod.attached))
		}
		argv := pod.attached[0]
		if got := argv[len(argv)-6]; got != "/town/rig/x" {
			t.Fatalf("the mapped directory argument is %q, want %q", got, "/town/rig/x")
		}
		if got := argv[len(argv)-5]; got != "/town" {
			t.Fatalf("the root argument is %q, want the configured root %q", got, "/town")
		}
		if got := argv[len(argv)-4]; got != dir {
			t.Fatalf("the caller's directory argument is %q, want %q", got, dir)
		}
	})
	t.Run("an absent key keeps the compiled-in root", func(t *testing.T) {
		t.Parallel()
		h, pod := newPodHarness(t, kubernetesConfig)
		dir := filepath.Join(h.home, "gt", "rig", "x")
		h.surface.getwd = func() (string, error) { return dir, nil }
		if exit := h.run("gt", "status", "--line"); exit != 0 {
			t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
		}
		if h.kubeCfg.PodRoot != "" {
			t.Fatalf("the target was built with pod root %q, want none so the compiled-in root applies", h.kubeCfg.PodRoot)
		}
		argv := pod.attached[0]
		if got := argv[len(argv)-6]; got != manifestdata.PodTownRoot+"/rig/x" {
			t.Fatalf("the mapped directory argument is %q, want %q", got, manifestdata.PodTownRoot+"/rig/x")
		}
		if got := argv[len(argv)-5]; got != manifestdata.PodTownRoot {
			t.Fatalf("the root argument is %q, want the compiled-in root %q", got, manifestdata.PodTownRoot)
		}
	})
	refusals := []struct {
		name   string
		config string
	}{
		{name: "a relative value under kubernetes", config: kubernetesConfigWith(`"pod_town_root": "town"`)},
		{name: "an empty value under kubernetes", config: kubernetesConfigWith(`"pod_town_root": ""`)},
		{name: "a trailing slash under kubernetes", config: kubernetesConfigWith(`"pod_town_root": "/town/"`)},
		{name: "a relative value under docker", config: `{"target": "docker", "pod_town_root": "town"}`},
	}
	for _, tt := range refusals {
		t.Run("refuses "+tt.name, func(t *testing.T) {
			t.Parallel()
			h, pod := newPodHarness(t, tt.config)
			if exit := h.run("gt", "status", "--line"); exit != 1 {
				t.Fatalf("run = %d, want 1", exit)
			}
			if !strings.Contains(h.stderr.String(), `"pod_town_root"`) {
				t.Fatalf("stderr %q does not name the key pod_town_root", h.stderr)
			}
			if h.kubeBuilt || h.targetSet || len(pod.attached) != 0 {
				t.Fatal("a target was built or a tool ran for a config with a refused pod_town_root")
			}
		})
	}
}

// TestRunRewritesPodPathsInOutput: with pod_town_root set to a root the Mac does
// not have, a path the pod's tool prints is rewritten to the Mac's town path on
// stdout and on stderr, and the tool's exit status passes through.
func TestRunRewritesPodPathsInOutput(t *testing.T) {
	t.Parallel()
	h, pod := newPodHarness(t, kubernetesConfigWith(`"pod_town_root": "/town"`))
	pod.stdout = "/town/rig/polecats/coma/gastown/README.md\n"
	pod.stderr = "no such file: /town/rig/missing\n"
	pod.exit = 3
	if exit := h.run("gt", "status", "--line"); exit != 3 {
		t.Fatalf("run = %d, want the tool's exit status 3; stderr: %s", exit, h.stderr)
	}
	macTown := filepath.Join(h.home, "gt")
	if want := macTown + "/rig/polecats/coma/gastown/README.md\n"; h.stdout.String() != want {
		t.Fatalf("stdout = %q, want the Mac path %q", h.stdout, want)
	}
	if want := "no such file: " + macTown + "/rig/missing"; !strings.Contains(h.stderr.String(), want) {
		t.Fatalf("stderr %q does not hold the rewritten path %q", h.stderr, want)
	}
	if podPath := "no such file: /town/rig/missing"; strings.Contains(h.stderr.String(), podPath) {
		t.Fatalf("stderr %q still holds the pod's path %q", h.stderr, podPath)
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
