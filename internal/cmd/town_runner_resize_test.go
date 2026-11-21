package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	forgejorunner "github.com/steveyegge/gastown/deploy/k8s/forgejo-runner"
)

const (
	resizePodName  = "forgejo-runner-7d9f"
	resizeNodeName = "gt-control"
)

// mustJSON is v as the JSON a kubectl read answers with.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling the fixture: %v", err)
	}
	return string(out)
}

// liveObjectJSON is a rendered object as a `kubectl get -o json` answer, so the
// drift compare reads the live object as the render names it and finds no
// difference.
func liveObjectJSON(t *testing.T, object any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := forgejorunner.YAML(&buf, object); err != nil {
		t.Fatalf("rendering the fixture: %v", err)
	}
	var decoded map[string]any
	if err := yaml.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decoding the rendered fixture: %v", err)
	}
	return mustJSON(t, decoded)
}

// resizeManifests is the control cluster's runner render.
func resizeManifests(t *testing.T) forgejorunner.Manifests {
	t.Helper()
	v, err := forgejorunner.ControlValues()
	if err != nil {
		t.Fatalf("reading the control values: %v", err)
	}
	m, err := forgejorunner.Build(v)
	if err != nil {
		t.Fatalf("rendering the runner: %v", err)
	}
	return m
}

// dindContainerJSON is a container entry with cpu as its limit, or none when cpu
// is empty.
func dindContainerJSON(cpu string) map[string]any {
	container := map[string]any{"name": forgejorunner.DindContainerName}
	if cpu != "" {
		container["resources"] = map[string]any{"limits": map[string]any{"cpu": cpu}}
	}
	return container
}

// runnerPodJSON is one runner pod's answer: its name, the node it runs on, the
// spec list its dind container is in with that CPU, and its conditions.
func runnerPodJSON(name, node, list, cpu string, conditions ...map[string]any) string {
	spec := map[string]any{"nodeName": node}
	runner := map[string]any{"name": "runner"}
	if list == "containers" {
		spec["containers"] = []any{dindContainerJSON(cpu), runner}
		spec["initContainers"] = []any{}
	} else {
		spec["containers"] = []any{runner}
		spec["initContainers"] = []any{dindContainerJSON(cpu)}
	}
	if conditions == nil {
		conditions = []map[string]any{}
	}
	return string(mustMarshalForTest(map[string]any{
		"metadata": map[string]any{"name": name},
		"spec":     spec,
		"status":   map[string]any{"conditions": conditions},
	}))
}

// mustMarshalForTest is mustJSON without a *testing.T, for the small fixtures
// the helpers above build in expressions.
func mustMarshalForTest(v any) []byte {
	out, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return out
}

// resizePodListJSON is a `kubectl get pods -o json` answer holding pods.
func resizePodListJSON(pods ...string) string {
	items := make([]json.RawMessage, 0, len(pods))
	for _, pod := range pods {
		items = append(items, json.RawMessage(pod))
	}
	return string(mustMarshalForTest(map[string]any{"items": items}))
}

// nodeJSON is a node's answer with the given allocatable CPU.
func nodeJSON(cpu string) string {
	return string(mustMarshalForTest(map[string]any{
		"status": map[string]any{"allocatable": map[string]any{"cpu": cpu}},
	}))
}

// resizeCluster is the recorded cluster's answers: the pre-patch pods, the
// read-back pod and the node. answer, when set, answers a call before the
// defaults and says whether it did.
type resizeCluster struct {
	pods, pod, node string
	answer          func(recordedKubectlCall) (out string, exit int, err error, handled bool)
}

// resizeAnswer answers each call as a cluster holding the fixture would.
func resizeAnswer(t *testing.T, m forgejorunner.Manifests, cluster resizeCluster) func(recordedKubectlCall) (string, int, error) {
	t.Helper()
	liveDeployment := liveObjectJSON(t, m.Deployment)
	liveConfigMap := liveObjectJSON(t, m.ConfigMap)
	return func(call recordedKubectlCall) (string, int, error) {
		if cluster.answer != nil {
			if out, exit, err, handled := cluster.answer(call); handled {
				return out, exit, err
			}
		}
		switch {
		case isGetCall(call, "pods"):
			return cluster.pods, 0, nil
		case isGetCall(call, "node"):
			return cluster.node, 0, nil
		case isGetCall(call, "pod"):
			return cluster.pod, 0, nil
		case isGetCall(call, "deployment/"+forgejorunner.Name):
			return liveDeployment, 0, nil
		case isGetCall(call, "configmap/"+forgejorunner.Name+"-config"):
			return liveConfigMap, 0, nil
		case len(call.args) > 0 && call.args[0] == "patch":
			return cluster.pod, 0, nil
		}
		return "", -1, fmt.Errorf("the resize made an unexpected call: %v", call.args)
	}
}

// isGetCall reports whether call reads resource.
func isGetCall(call recordedKubectlCall, resource string) bool {
	return len(call.args) >= 2 && call.args[0] == "get" && call.args[1] == resource
}

// resizeDeps is the resize over the recorded cluster and the control render.
func resizeDeps(out io.Writer, k *recordingKubectl, cpu string) runnerResizeDeps {
	return runnerResizeDeps{
		kubectl: k.run,
		values:  forgejorunner.ControlValues,
		build:   forgejorunner.Build,
		encode:  forgejorunner.YAML,
		dindCPU: cpu,
		out:     out,
	}
}

// TestTownRunnerResizePatchesOnlyTheDindCPU: the patch is the resize's only
// write, it names the given CPU on the dind container in the list the pod
// carries it in, and no delete or restart is ever called.
func TestTownRunnerResizePatchesOnlyTheDindCPU(t *testing.T) {
	t.Parallel()
	for _, list := range []string{"initContainers", "containers"} {
		t.Run(list, func(t *testing.T) {
			t.Parallel()
			m := resizeManifests(t)
			pod := runnerPodJSON(resizePodName, resizeNodeName, list, "4")
			k := &recordingKubectl{answer: resizeAnswer(t, m, resizeCluster{
				pods: resizePodListJSON(runnerPodJSON(resizePodName, resizeNodeName, list, "8")),
				pod:  pod,
				node: nodeJSON("18"),
			})}
			out := &bytes.Buffer{}

			if err := runTownRunnerResize(context.Background(), resizeDeps(out, k, "4")); err != nil {
				t.Fatalf("resize: %v", err)
			}

			var writes []recordedKubectlCall
			for _, call := range k.calls {
				for _, arg := range call.args {
					if arg == "delete" || arg == "restart" || arg == "replace" {
						t.Errorf("the resize called %q: %v", arg, call.args)
					}
				}
				if len(call.args) == 0 || call.args[0] != "get" {
					writes = append(writes, call)
				}
			}
			if len(writes) != 1 {
				t.Fatalf("made %d writes, want the resize patch alone: %v", len(writes), k.calls)
			}
			patch := writes[0]
			wantArgs := []string{"patch", "pod", resizePodName, "-n", forgejorunner.DefaultNamespace, "--subresource", "resize", "--patch"}
			if len(patch.args) != len(wantArgs)+1 || !slices.Equal(patch.args[:len(wantArgs)], wantArgs) {
				t.Errorf("patch args = %v, want %v plus the body", patch.args, wantArgs)
			}
			if body := parsePatchBody(t, patch.args[len(patch.args)-1]); body.cpu(list) != "4" {
				t.Errorf("patch body asks for %q on %s, want the given cpu 4; body:\n%s", body.cpu(list), list, patch.args[len(patch.args)-1])
			}
			if printed := out.String(); !strings.Contains(printed, "now asks for cpu 4") {
				t.Errorf("output does not print the new limit; got:\n%s", printed)
			}
		})
	}
}

// patchBody is the resize patch's body, read back as the spec lists the patch
// can name.
type patchBody struct {
	Spec struct {
		Containers     []patchContainer `json:"containers"`
		InitContainers []patchContainer `json:"initContainers"`
	} `json:"spec"`
}

// patchContainer is one container of the patch body.
type patchContainer struct {
	Name      string `json:"name"`
	Resources struct {
		Limits map[string]string `json:"limits"`
	} `json:"resources"`
}

// cpu is the CPU limit the body names for the dind container in list.
func (b patchBody) cpu(list string) string {
	containers := b.Spec.Containers
	if list == "initContainers" {
		containers = b.Spec.InitContainers
	}
	for _, container := range containers {
		if container.Name == forgejorunner.DindContainerName {
			return container.Resources.Limits["cpu"]
		}
	}
	return ""
}

// parsePatchBody reads a patch body and fails the test when it cannot.
func parsePatchBody(t *testing.T, body string) patchBody {
	t.Helper()
	var parsed patchBody
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("the patch body is not JSON: %v\n%s", err, body)
	}
	return parsed
}

// TestTownRunnerResizeRefusesAnInvalidCPU: a value the flag's grammar refuses
// stops the command before its first cluster call.
func TestTownRunnerResizeRefusesAnInvalidCPU(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "0", "0m", "-1", "1.5", "6000.5m", "abc", "08", " 4", "4m ", "99999999999999999999", "9223372036854775807"} {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			t.Parallel()
			k := &recordingKubectl{answer: func(call recordedKubectlCall) (string, int, error) {
				t.Errorf("made a cluster call %v although --dind-cpu is %q", call.args, value)
				return "", 0, nil
			}}

			err := runTownRunnerResize(context.Background(), resizeDeps(io.Discard, k, value))

			if err == nil || !strings.Contains(err.Error(), "--"+dindCPUFlag) {
				t.Fatalf("error = %v, want a refusal naming --%s", err, dindCPUFlag)
			}
			if len(k.calls) != 0 {
				t.Errorf("made %d cluster calls, want none: %v", len(k.calls), k.calls)
			}
		})
	}
}

// TestTownRunnerResizeRefusesCPUOverTheNodesAllocatable: a CPU larger than the
// node allocates is refused before the patch.
func TestTownRunnerResizeRefusesCPUOverTheNodesAllocatable(t *testing.T) {
	t.Parallel()
	m := resizeManifests(t)
	k := &recordingKubectl{answer: resizeAnswer(t, m, resizeCluster{
		pods: resizePodListJSON(runnerPodJSON(resizePodName, resizeNodeName, "initContainers", "8")),
		pod:  runnerPodJSON(resizePodName, resizeNodeName, "initContainers", "8"),
		node: nodeJSON("4000m"),
	})}

	err := runTownRunnerResize(context.Background(), resizeDeps(io.Discard, k, "8"))

	for _, want := range []string{"--" + dindCPUFlag + " 8", "node/" + resizeNodeName, "4000m"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %q", err, want)
		}
	}
	for _, call := range k.calls {
		if len(call.args) > 0 && call.args[0] == "patch" {
			t.Errorf("the resize patched the pod although --dind-cpu is over the allocatable: %v", call.args)
		}
	}
}

// TestTownRunnerResizeRefusesAPendingResize: a pod whose resize is pending,
// including the Infeasible and Deferred reasons, exits non-zero with the pod's
// resize status printed, and asks for no template comparison.
func TestTownRunnerResizeRefusesAPendingResize(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"Infeasible", "Deferred"} {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			const message = "Node didn't have enough resource: cpu"
			m := resizeManifests(t)
			readBack := runnerPodJSON(resizePodName, resizeNodeName, "initContainers", "8", map[string]any{
				"type": podResizePending, "status": "True", "reason": reason, "message": message,
			})
			k := &recordingKubectl{answer: resizeAnswer(t, m, resizeCluster{
				pods: resizePodListJSON(runnerPodJSON(resizePodName, resizeNodeName, "initContainers", "8")),
				pod:  readBack,
				node: nodeJSON("18"),
			})}
			out := &bytes.Buffer{}

			err := runTownRunnerResize(context.Background(), resizeDeps(out, k, "4"))

			if err == nil {
				t.Fatal("a pending resize succeeded")
			}
			printed := out.String()
			for _, want := range []string{"pending", reason, message} {
				if !strings.Contains(printed, want) {
					t.Errorf("output lacks %q; got:\n%s", want, printed)
				}
			}
		})
	}
}

// TestTownRunnerResizePrintsAnInProgressResizeAndTheLimit: a resize the kubelet
// is applying is not a failure; the status and the requested limit print.
func TestTownRunnerResizePrintsAnInProgressResizeAndTheLimit(t *testing.T) {
	t.Parallel()
	m := resizeManifests(t)
	readBack := runnerPodJSON(resizePodName, resizeNodeName, "initContainers", "4", map[string]any{
		"type": podResizeInProgress, "status": "True", "reason": "InProgress", "message": "resizing the container",
	})
	k := &recordingKubectl{answer: resizeAnswer(t, m, resizeCluster{
		pods: resizePodListJSON(runnerPodJSON(resizePodName, resizeNodeName, "initContainers", "8")),
		pod:  readBack,
		node: nodeJSON("18"),
	})}
	out := &bytes.Buffer{}

	if err := runTownRunnerResize(context.Background(), resizeDeps(out, k, "4")); err != nil {
		t.Fatalf("resize: %v", err)
	}

	printed := out.String()
	for _, want := range []string{"in progress", "now asks for cpu 4"} {
		if !strings.Contains(printed, want) {
			t.Errorf("output lacks %q; got:\n%s", want, printed)
		}
	}
}

// TestTownRunnerResizeSaysWhetherTheTemplateDiffers: the runtime limit and the
// rendered template compare as quantities, and a template asking for another
// value is named with the path it reverts the pod to.
func TestTownRunnerResizeSaysWhetherTheTemplateDiffers(t *testing.T) {
	t.Parallel()
	const cpuPath = "/spec/template/spec/initContainers/0/resources/limits/cpu"
	cases := []struct {
		name        string
		templateCPU string
		podCPU      string
		wantDiffers bool
	}{
		{name: "the template asks for another cpu", templateCPU: "2", podCPU: "4", wantDiffers: true},
		{name: "the template asks for the same cpu", templateCPU: "4", podCPU: "4"},
		{name: "the template and the pod differ only in form", templateCPU: "4", podCPU: "4000m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := resizeManifests(t)
			m.Deployment.Spec.Template.Spec.InitContainers[0].Resources.Limits.CPU = tc.templateCPU
			k := &recordingKubectl{answer: resizeAnswer(t, m, resizeCluster{
				pods: resizePodListJSON(runnerPodJSON(resizePodName, resizeNodeName, "initContainers", "8")),
				pod:  runnerPodJSON(resizePodName, resizeNodeName, "initContainers", tc.podCPU),
				node: nodeJSON("18"),
			})}
			out := &bytes.Buffer{}
			deps := resizeDeps(out, k, "4")
			deps.build = func(v forgejorunner.Values) (forgejorunner.Manifests, error) {
				built, err := forgejorunner.Build(v)
				if err != nil {
					return built, err
				}
				built.Deployment.Spec.Template.Spec.InitContainers[0].Resources.Limits.CPU = tc.templateCPU
				return built, nil
			}

			if err := runTownRunnerResize(context.Background(), deps); err != nil {
				t.Fatalf("resize: %v", err)
			}

			printed := out.String()
			if tc.wantDiffers {
				for _, want := range []string{"differs", cpuPath, "next restart"} {
					if !strings.Contains(printed, want) {
						t.Errorf("output lacks %q; got:\n%s", want, printed)
					}
				}
				if strings.Contains(printed, "is in step") {
					t.Errorf("output says the template is in step although it differs; got:\n%s", printed)
				}
				return
			}
			for _, want := range []string{"is in step", cpuPath} {
				if !strings.Contains(printed, want) {
					t.Errorf("output lacks %q; got:\n%s", want, printed)
				}
			}
			if strings.Contains(printed, "differs") {
				t.Errorf("output says the template differs although it matches; got:\n%s", printed)
			}
		})
	}
}

// TestTownRunnerResizeRefuses: every read, the patch and the compare can fail,
// and each failure is an error instead of a resize reported as done.
func TestTownRunnerResizeRefuses(t *testing.T) {
	t.Parallel()
	const noDindPod = `{"metadata":{"name":"forgejo-runner-7d9f"},"spec":{"nodeName":"gt-control","containers":[{"name":"runner"}]}}`
	const inBothPod = `{"metadata":{"name":"forgejo-runner-7d9f"},"spec":{"nodeName":"gt-control","containers":[{"name":"dind"}],"initContainers":[{"name":"dind"}]}}`
	noNodePod := runnerPodJSON(resizePodName, "", "initContainers", "8")
	const readBack = resizePodName

	cases := []struct {
		name   string
		mutate func(*runnerResizeDeps, *resizeCluster, *forgejorunner.Manifests)
		want   string
	}{
		{
			name: "values unreadable",
			mutate: func(d *runnerResizeDeps, _ *resizeCluster, _ *forgejorunner.Manifests) {
				d.values = func() (forgejorunner.Values, error) {
					return forgejorunner.Values{}, errors.New("file unreadable")
				}
			},
			want: "reading the control cluster's runner values",
		},
		{
			name: "render refuses",
			mutate: func(d *runnerResizeDeps, _ *resizeCluster, _ *forgejorunner.Manifests) {
				d.build = func(forgejorunner.Values) (forgejorunner.Manifests, error) {
					return forgejorunner.Manifests{}, errors.New("no labels")
				}
			},
			want: "rendering the runner",
		},
		{
			name: "the pods cannot be read",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.answer = failCall(func(call recordedKubectlCall) bool { return isGetCall(call, "pods") }, "", -1, errors.New("kubectl not found"))
			},
			want: "reading the runner pods",
		},
		{
			name: "the pods read exits nonzero",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.answer = failCall(func(call recordedKubectlCall) bool { return isGetCall(call, "pods") }, "", 1, nil)
			},
			want: "reading the runner pods: kubectl exited 1",
		},
		{
			name: "the pods answer is not a list",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.pods = `not json`
			},
			want: "not a pod list",
		},
		{
			name: "no runner pod is kept",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.pods = `{"items":[]}`
			},
			want: "no runner pod",
		},
		{
			name: "two runner pods are kept",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.pods = resizePodListJSON(
					runnerPodJSON(resizePodName, resizeNodeName, "initContainers", "8"),
					runnerPodJSON("forgejo-runner-other", resizeNodeName, "initContainers", "8"),
				)
			},
			want: "holds 2 runner pods",
		},
		{
			name: "the pod names no dind container",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.pods = resizePodListJSON(noDindPod)
			},
			want: "names no dind container",
		},
		{
			name: "the pod names dind in both lists",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.pods = resizePodListJSON(inBothPod)
			},
			want: "both containers and initContainers",
		},
		{
			name: "the pod names no node",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.pods = resizePodListJSON(noNodePod)
			},
			want: "names no node",
		},
		{
			name: "the node cannot be read",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.answer = failCall(func(call recordedKubectlCall) bool { return isGetCall(call, "node") }, "", -1, errors.New("kubectl not found"))
			},
			want: "reading node/" + resizeNodeName,
		},
		{
			name: "the node read exits nonzero",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.answer = failCall(func(call recordedKubectlCall) bool { return isGetCall(call, "node") }, "", 1, nil)
			},
			want: "reading node/" + resizeNodeName + ": kubectl exited 1",
		},
		{
			name: "the node answer is not an object",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.node = `not json`
			},
			want: "not a node object",
		},
		{
			name: "the node names no allocatable cpu",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.node = `{"status":{"allocatable":{}}}`
			},
			want: "names no allocatable CPU",
		},
		{
			name: "the allocatable cpu cannot be read",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.node = nodeJSON("lots")
			},
			want: "node/" + resizeNodeName + " allocates \"lots\", which is not a CPU quantity",
		},
		{
			name: "the patch cannot start",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.answer = failCall(func(call recordedKubectlCall) bool { return len(call.args) > 0 && call.args[0] == "patch" }, "", -1, errors.New("kubectl not found"))
			},
			want: "patching pod " + resizePodName,
		},
		{
			name: "the patch exits nonzero",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.answer = failCall(func(call recordedKubectlCall) bool { return len(call.args) > 0 && call.args[0] == "patch" }, "", 1, nil)
			},
			want: "patching pod " + resizePodName + ": kubectl exited 1",
		},
		{
			name: "the read-back cannot be read",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.answer = failCall(func(call recordedKubectlCall) bool { return isGetCall(call, "pod") }, "", -1, errors.New("kubectl not found"))
			},
			want: "reading pod/" + readBack,
		},
		{
			name: "the read-back is not a pod",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.pod = `not json`
			},
			want: "not a pod object",
		},
		{
			name: "the read-back loses the dind container",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.pod = `{"metadata":{"name":"forgejo-runner-7d9f"},"spec":{"containers":[{"name":"runner"}],"initContainers":[]}}`
			},
			want: "no longer names the dind container",
		},
		{
			name: "the read-back names no cpu limit",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.pod = runnerPodJSON(resizePodName, resizeNodeName, "initContainers", "")
			},
			want: "reports no cpu limit",
		},
		{
			name: "the live deployment cannot be read",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.answer = failCall(func(call recordedKubectlCall) bool {
					return isGetCall(call, "deployment/"+forgejorunner.Name)
				}, "", 1, nil)
			},
			want: "could not be read",
		},
		{
			name: "the template names no dind container",
			mutate: func(_ *runnerResizeDeps, _ *resizeCluster, m *forgejorunner.Manifests) {
				m.Deployment.Spec.Template.Spec.InitContainers = nil
			},
			want: "names no dind container in its pod template",
		},
		{
			name: "the template's cpu cannot be read",
			mutate: func(_ *runnerResizeDeps, _ *resizeCluster, m *forgejorunner.Manifests) {
				m.Deployment.Spec.Template.Spec.InitContainers[0].Resources.Limits.CPU = "lots"
			},
			want: "the rendered template's dind CPU",
		},
		{
			name: "the read-back cpu cannot be read",
			mutate: func(_ *runnerResizeDeps, c *resizeCluster, _ *forgejorunner.Manifests) {
				c.pod = runnerPodJSON(resizePodName, resizeNodeName, "initContainers", "lots")
			},
			want: "the pod's dind CPU",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := resizeManifests(t)
			cluster := resizeCluster{
				pods: resizePodListJSON(runnerPodJSON(resizePodName, resizeNodeName, "initContainers", "8")),
				pod:  runnerPodJSON(resizePodName, resizeNodeName, "initContainers", "4"),
				node: nodeJSON("18"),
			}
			deps := runnerResizeDeps{dindCPU: "4", out: io.Discard}
			tc.mutate(&deps, &cluster, &m)
			k := &recordingKubectl{answer: resizeAnswer(t, m, cluster)}
			if deps.kubectl == nil {
				deps.kubectl = k.run
			}
			if deps.values == nil {
				deps.values = forgejorunner.ControlValues
			}
			if deps.build == nil {
				// The fixture's render, so a row that changes it changes what the
				// resize compares against as well as what the live answer holds.
				deps.build = func(forgejorunner.Values) (forgejorunner.Manifests, error) {
					return m, nil
				}
			}
			if deps.encode == nil {
				deps.encode = forgejorunner.YAML
			}

			err := runTownRunnerResize(context.Background(), deps)

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// failCall answers the calls match selects with out, exit and err, and leaves
// every other call to the fixture.
func failCall(match func(recordedKubectlCall) bool, out string, exit int, err error) func(recordedKubectlCall) (string, int, error, bool) {
	return func(call recordedKubectlCall) (string, int, error, bool) {
		if match(call) {
			return out, exit, err, true
		}
		return "", 0, nil, false
	}
}

// TestTownRunnerResizeWiredRefusesAMissingSeam: every seam the resize reads
// refuses when it is missing, instead of panicking partway through.
func TestTownRunnerResizeWiredRefusesAMissingSeam(t *testing.T) {
	t.Parallel()
	if err := resizeDeps(io.Discard, &recordingKubectl{}, "4").Wired(); err != nil {
		t.Fatalf("the complete resize refuses: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*runnerResizeDeps)
		want   string
	}{
		{name: "no cluster client", mutate: func(d *runnerResizeDeps) { d.kubectl = nil }, want: "no cluster client"},
		{name: "no values", mutate: func(d *runnerResizeDeps) { d.values = nil }, want: "no runner values or renderer"},
		{name: "no renderer", mutate: func(d *runnerResizeDeps) { d.build = nil }, want: "no runner values or renderer"},
		{name: "no encoder", mutate: func(d *runnerResizeDeps) { d.encode = nil }, want: "no manifest encoder"},
		{name: "no output", mutate: func(d *runnerResizeDeps) { d.out = nil }, want: "no output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps := resizeDeps(io.Discard, &recordingKubectl{}, "4")
			tc.mutate(&deps)
			err := deps.Wired()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Wired() = %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestTownRunnerResizeRefusesAFlagItCannotRead: a command without the flag
// defined, or with it empty, refuses before any call.
func TestTownRunnerResizeRefusesAFlagItCannotRead(t *testing.T) {
	t.Parallel()
	t.Run("missing flag", func(t *testing.T) {
		t.Parallel()
		err := runTownRunnerResizeCmd(&cobra.Command{}, nil)
		if err == nil || !strings.Contains(err.Error(), "reading --"+dindCPUFlag) {
			t.Errorf("error = %v, want it to name reading --%s", err, dindCPUFlag)
		}
	})
	t.Run("empty flag", func(t *testing.T) {
		t.Parallel()
		cmd := &cobra.Command{}
		cmd.Flags().String(dindCPUFlag, "", "")
		out := &bytes.Buffer{}
		cmd.SetOut(out)
		err := runTownRunnerResizeCmd(cmd, nil)
		if err == nil || !strings.Contains(err.Error(), "--"+dindCPUFlag+" is required") {
			t.Errorf("error = %v, want the required-flag refusal", err)
		}
		if out.Len() != 0 {
			t.Errorf("the command printed %q before refusing", out.String())
		}
	})
}

// TestTownRunnerResizeFlagReachesThePatch: the command's flag read hands the
// value to the resize, and the resize patches with it.
func TestTownRunnerResizeFlagReachesThePatch(t *testing.T) {
	t.Parallel()
	m := resizeManifests(t)
	pod := runnerPodJSON(resizePodName, resizeNodeName, "initContainers", "6")
	k := &recordingKubectl{answer: resizeAnswer(t, m, resizeCluster{
		pods: resizePodListJSON(runnerPodJSON(resizePodName, resizeNodeName, "initContainers", "8")),
		pod:  pod,
		node: nodeJSON("18"),
	})}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	cmd.Flags().String(dindCPUFlag, "6", "")
	depsFor := func(out io.Writer, cpu string) runnerResizeDeps {
		return resizeDeps(out, k, cpu)
	}

	if err := runTownRunnerResizeIn(cmd, depsFor); err != nil {
		t.Fatalf("resize: %v", err)
	}

	var patch recordedKubectlCall
	for _, call := range k.calls {
		if len(call.args) > 0 && call.args[0] == "patch" {
			patch = call
		}
	}
	if patch.args == nil {
		t.Fatalf("no patch was made: %v", k.calls)
	}
	if body := parsePatchBody(t, patch.args[len(patch.args)-1]); body.cpu("initContainers") != "6" {
		t.Errorf("the patch asks for %q, want the flag's 6; body:\n%s", body.cpu("initContainers"), patch.args[len(patch.args)-1])
	}
}
