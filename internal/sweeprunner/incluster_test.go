package sweeprunner

import (
	"io/fs"
	"slices"
	"testing"
)

const inClusterContext = "colima-gt-control"

// kubeconfigNaming is a kubeconfig file that names the given context.
func kubeconfigNaming(name string) []byte {
	return []byte("apiVersion: v1\nkind: Config\ncontexts:\n- name: " + name + "\n  context:\n    cluster: c\n    user: u\n")
}

// readFiles is a file reader over files; any other path is absent.
func readFiles(files map[string][]byte) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		if data, ok := files[p]; ok {
			return data, nil
		}
		return nil, fs.ErrNotExist
	}
}

// TestRunnerArgvInCluster: the kubectl argv drops --context only in a pod where
// no kubeconfig names the context. The environment and the files are injected, so
// no case reads the process's own (gt-2by0ke).
func TestRunnerArgvInCluster(t *testing.T) {
	t.Parallel()
	pod := []string{"KUBERNETES_SERVICE_HOST=10.0.0.1", "HOME=/fx/home"}
	host := []string{"HOME=/fx/home"}
	cases := []struct {
		name        string
		environ     []string
		files       map[string][]byte
		wantContext bool
	}{
		{"in a pod with no context in any config file", pod, nil, false},
		{"in a pod whose default kubeconfig names other contexts only", pod, map[string][]byte{"/fx/home/.kube/config": kubeconfigNaming("other")}, false},
		{"on the host with the context resolvable", host, map[string][]byte{"/fx/home/.kube/config": kubeconfigNaming(inClusterContext)}, true},
		{"in a pod with a config file that names the context", pod, map[string][]byte{"/fx/home/.kube/config": kubeconfigNaming(inClusterContext)}, true},
		{"in a pod with KUBECONFIG naming the context", append([]string{"KUBECONFIG=/fx/kc"}, pod...), map[string][]byte{"/fx/kc": kubeconfigNaming(inClusterContext)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			spec := goldenSpec()
			spec.Context = inClusterContext
			spec.InCluster = InClusterWithoutContext(tc.environ, inClusterContext, KubeconfigHasContext(tc.environ, readFiles(tc.files)))
			want := []string{"kubectl", "get", "jobs"}
			if tc.wantContext {
				want = []string{"kubectl", "--context", inClusterContext, "get", "jobs"}
			}
			if got := kubectlArgv(spec, "get", "jobs"); !slices.Equal(got, want) {
				t.Errorf("argv = %q, want %q", got, want)
			}
		})
	}
}

// TestRunnerArgvInClusterNoStepNamesAContext: a rendered plan for an in-cluster
// Spec carries no --context in any step, and a host Spec carries it in every
// kubectl step.
func TestRunnerArgvInClusterNoStepNamesAContext(t *testing.T) {
	t.Parallel()
	for _, inCluster := range []bool{true, false} {
		spec := goldenSpec()
		spec.Context = inClusterContext
		spec.InCluster = inCluster
		plan, err := Kubernetes{}.Render(spec)
		if err != nil {
			t.Fatalf("Render(InCluster=%v): %v", inCluster, err)
		}
		kubectlSteps := 0
		for _, step := range plan.Steps {
			if len(step.Argv) == 0 || step.Argv[0] != kubectl {
				continue
			}
			kubectlSteps++
			if has := slices.Contains(step.Argv, "--context"); has == inCluster {
				t.Errorf("InCluster=%v: step %q has --context = %v", inCluster, step.Argv, has)
			}
		}
		if kubectlSteps == 0 {
			t.Errorf("InCluster=%v: the plan has no kubectl step", inCluster)
		}
	}
}

// TestKubeconfigHasContextIgnoresUnreadableFiles: a file that is absent or not
// YAML names no context, and a later file in KUBECONFIG still counts.
func TestKubeconfigHasContextIgnoresUnreadableFiles(t *testing.T) {
	t.Parallel()
	environ := []string{"KUBECONFIG=/fx/missing:/fx/bad:/fx/good"}
	read := readFiles(map[string][]byte{"/fx/bad": []byte("contexts: [unclosed"), "/fx/good": kubeconfigNaming("x")})
	has := KubeconfigHasContext(environ, read)
	if !has("x") {
		t.Error("the later file's context was not found")
	}
	if has("y") {
		t.Error("a context no file names was found")
	}
}
