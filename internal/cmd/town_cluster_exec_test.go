package cmd

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// clusterExecCall is one process the read runner asked for.
type clusterExecCall struct {
	name string
	args []string
}

// clusterExecRecorder answers the read runner's injected exec in process and
// records every call, so a test proves which commands a run would start.
type clusterExecRecorder struct {
	calls []clusterExecCall
}

func (r *clusterExecRecorder) run(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
	r.calls = append(r.calls, clusterExecCall{name: name, args: append([]string(nil), args...)})
	return "answer", 0, nil
}

// clusterExecRead builds the read runner for execVia and kubeconfig over a
// recorder, drives one read with args, and returns what it answered and the
// calls it made.
func clusterExecRead(t *testing.T, execVia, kubeconfig string, args ...string) (string, int, error, []clusterExecCall) {
	t.Helper()
	recorder := &clusterExecRecorder{}
	runner, err := clusterKubectl(execVia, kubeconfig, recorder.run)
	if err != nil {
		return "", 0, err, recorder.calls
	}
	out, exit, err := runner(context.Background(), "", nil, "kubectl", args...)
	return out, exit, err, recorder.calls
}

// clusterExecArgv is the accepted read every test drives unless it names
// another: a namespace, the get verb, a target and an output pair.
func clusterExecArgv() []string {
	return []string{"-n", "openshell", "get", "deployment/daemon", "-o", "json"}
}

// TestClusterExecViaBuildsTheReadCommand holds the two accepted forms to the
// argv the Goal describes: an empty value a plain kubectl with the given
// kubeconfig, and docker-exec:<context>:<container> the node's kubectl reached
// through docker exec.
func TestClusterExecViaBuildsTheReadCommand(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		execVia    string
		kubeconfig string
		wantName   string
		wantArgs   []string
	}{
		{
			name:       "an empty value runs a plain kubectl with the given kubeconfig",
			kubeconfig: "/town/.runtime/sbx-k8s/kubeconfig",
			wantName:   "kubectl",
			wantArgs:   []string{"--kubeconfig", "/town/.runtime/sbx-k8s/kubeconfig", "-n", "openshell", "get", "deployment/daemon", "-o", "json"},
		},
		{
			name:     "an empty value with no kubeconfig runs a plain kubectl",
			wantName: "kubectl",
			wantArgs: []string{"-n", "openshell", "get", "deployment/daemon", "-o", "json"},
		},
		{
			name:       "the two-colon form runs the node's kubectl through docker exec",
			execVia:    "docker-exec:colima-gt-sbx:gt-sbx-worker",
			kubeconfig: "/a/kubeconfig/that/the/docker/path/ignores",
			wantName:   "docker",
			wantArgs:   []string{"--context", "colima-gt-sbx", "exec", "gt-sbx-worker", "kubectl", "--kubeconfig", "/etc/kubernetes/admin.conf", "-n", "openshell", "get", "deployment/daemon", "-o", "json"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, exit, err, calls := clusterExecRead(t, tt.execVia, tt.kubeconfig, clusterExecArgv()...)
			if err != nil {
				t.Fatalf("building and running the read: %v", err)
			}
			if out != "answer" || exit != 0 {
				t.Errorf("the read answered %q, exit %d; want the injected answer", out, exit)
			}
			if len(calls) != 1 {
				t.Fatalf("the read made %d exec calls, want 1: %v", len(calls), calls)
			}
			if calls[0].name != tt.wantName || !slices.Equal(calls[0].args, tt.wantArgs) {
				t.Errorf("the read ran %s %q, want %s %q", calls[0].name, calls[0].args, tt.wantName, tt.wantArgs)
			}
		})
	}
}

// TestClusterExecViaRefusesAnUnknownForm holds that a value that is neither
// accepted form is refused, naming both, before any exec.
func TestClusterExecViaRefusesAnUnknownForm(t *testing.T) {
	t.Parallel()
	for _, execVia := range []string{
		"kubectl",
		"plain",
		"notdocker:colima-gt-sbx:gt-sbx-worker",
		"kubectl:colima-gt-sbx:gt-sbx-worker",
		"docker-exec",
		"docker-exec:",
		"docker-exec:colima-gt-sbx",
		"docker-exec:colima-gt-sbx:gt-sbx-worker:extra",
		"docker-exec::gt-sbx-worker",
		"docker-exec:colima-gt-sbx:",
		"docker-exec:-rf:gt-sbx-worker",
		"docker-exec:colima-gt-sbx:-rf",
		"docker-exec:colima/gt-sbx:gt-sbx-worker",
	} {
		t.Run(execVia, func(t *testing.T) {
			t.Parallel()
			_, _, err, calls := clusterExecRead(t, execVia, "", clusterExecArgv()...)
			if err == nil {
				t.Fatalf("%q was accepted, want a refusal", execVia)
			}
			for _, form := range []string{"plain kubectl", "docker-exec:<context>:<container>"} {
				if !strings.Contains(err.Error(), form) {
					t.Errorf("the refusal %q does not name the accepted form %q", err, form)
				}
			}
			if len(calls) != 0 {
				t.Errorf("%q made exec calls %v, want none", execVia, calls)
			}
		})
	}
}

// TestClusterExecViaRefusesAKubeconfigThatIsNotAPath holds that a kubeconfig
// reading as a flag or holding a control character is refused before it
// reaches kubectl.
func TestClusterExecViaRefusesAKubeconfigThatIsNotAPath(t *testing.T) {
	t.Parallel()
	for _, kubeconfig := range []string{"-rf", "--namespace=openshell", "a\tb"} {
		t.Run(kubeconfig, func(t *testing.T) {
			t.Parallel()
			_, _, err, calls := clusterExecRead(t, "", kubeconfig, clusterExecArgv()...)
			if err == nil {
				t.Fatalf("kubeconfig %q was accepted, want a refusal", kubeconfig)
			}
			if len(calls) != 0 {
				t.Errorf("the refused kubeconfig %q made exec calls %v, want none", kubeconfig, calls)
			}
		})
	}
}

// TestClusterExecViaPassesOnlyGetReads holds that every accepted call carries
// the get verb, and that a write verb is refused before any exec.
func TestClusterExecViaPassesOnlyGetReads(t *testing.T) {
	t.Parallel()
	reads := [][]string{
		{"-n", "openshell", "get", "deployment/daemon", "-o", "json"},
		{"-n", "openshell", "get", "serviceaccount/daemon", "-o", "json"},
		{"get", "validatingadmissionpolicy/gt-guard", "-o", "json"},
	}
	writeVerbs := []string{"apply", "create", "delete", "patch", "replace", "edit", "scale", "annotate", "label", "set", "rollout", "exec", "port-forward", "cp", "run", "expose", "drain", "cordon", "taint"}

	for _, args := range reads {
		_, _, err, calls := clusterExecRead(t, "", "", args...)
		if err != nil {
			t.Fatalf("reading %q: %v", args, err)
		}
		if len(calls) != 1 {
			t.Fatalf("reading %q made %d exec calls, want 1", args, len(calls))
		}
		if !slices.Contains(calls[0].args, "get") {
			t.Errorf("reading %q ran %q, want the get verb", args, calls[0].args)
		}
		for _, call := range calls {
			for _, verb := range writeVerbs {
				if slices.Contains(call.args, verb) {
					t.Errorf("reading %q ran the write verb %q in %q", args, verb, call.args)
				}
			}
		}
	}
	for _, verb := range writeVerbs {
		_, _, err, calls := clusterExecRead(t, "", "", verb, "deployment/daemon", "-o", "json")
		if err == nil {
			t.Errorf("the %q verb was accepted, want a refusal", verb)
		}
		if len(calls) != 0 {
			t.Errorf("the %q verb made exec calls %v, want none", verb, calls)
		}
	}
}

// TestClusterExecViaRefusesSecretSpellings drives execViaGuard with every
// Secret spelling the bead lists and holds each to a refusal, and the one
// Secret read, `get secret <name>`, to acceptance with no output flag.
func TestClusterExecViaRefusesSecretSpellings(t *testing.T) {
	t.Parallel()
	refused := [][]string{
		{"get", "secret"},        // a list read of Secrets
		{"get", "secrets"},       // the plural
		{"get", "secret,pods"},   // a list holding a Secret
		{"get", "pods,secret"},   // the same list reordered
		{"get", "secrets.v1"},    // a version qualifier
		{"get", "secrets.v1."},   // a version qualifier with a dot
		{"get", "Secret"},        // another case
		{"get", "secret/creds"},  // the slash form
		{"get", "secrets/creds"}, // the plural slash form
		{"-n", "openshell", "get", "secret", "creds", "-o", "json"},
		{"-n", "openshell", "get", "secret", "creds", "-ojson"},
		{"-n", "openshell", "get", "secret", "creds", "-o=json"},
		{"-n", "openshell", "get", "secret", "creds", "--output", "json"},
		{"-n", "openshell", "get", "secret", "creds", "extra"},
	}
	for _, args := range refused {
		if _, err := execViaGuard(args); err == nil {
			t.Errorf("the read %q was accepted, want a refusal", args)
		}
	}

	// The one Secret read: the singular spelling, one name, no output flag.
	args := []string{"-n", "openshell", "get", "secret", "creds"}
	got, err := execViaGuard(args)
	if err != nil {
		t.Fatalf("the Secret read %q was refused: %v", args, err)
	}
	if !slices.Equal(got, args) {
		t.Errorf("execViaGuard returned %q, want the arguments unchanged %q", got, args)
	}
}

// TestClusterExecViaRefusesANamedTargetWithABadKindOrName holds the two-token
// form to the same kind table and name rule as the slash form.
func TestClusterExecViaRefusesANamedTargetWithABadKindOrName(t *testing.T) {
	t.Parallel()
	refused := [][]string{
		{"get", "Deployment", "daemon"},
		{"get", "pods", "daemon"},
		{"get", "deployment", "daemon/extra"},
		{"get", "secret", "creds/extra"},
		{"get", "deployment", ""},
	}
	for _, args := range refused {
		if _, err := execViaGuard(args); err == nil {
			t.Errorf("the named target %q was accepted, want a refusal", args)
		}
	}
	if _, err := execViaGuard([]string{"get", "deployment", "daemon"}); err != nil {
		t.Errorf("the named target deployment daemon was refused: %v", err)
	}
}

// TestClusterExecViaAcceptsCommaLists holds a comma list of table kinds to
// acceptance and a list holding one denied entry to a refusal.
func TestClusterExecViaAcceptsCommaLists(t *testing.T) {
	t.Parallel()
	accepted := [][]string{
		{"get", "deployment,service"},
		{"get", "deployments,configmaps"},
		{"get", "deployment/daemon,service/daemon"},
	}
	for _, args := range accepted {
		if _, err := execViaGuard(args); err != nil {
			t.Errorf("the list %q was refused: %v", args, err)
		}
	}
	refused := [][]string{
		{"get", "deployment,secrets"},
		{"get", "deployment,Secret"},
		{"get", "deployment,secrets.v1"},
		{"get", "deployment,pods"},
		{"get", "deployment,"},
		{"get", ",deployment"},
	}
	for _, args := range refused {
		if _, err := execViaGuard(args); err == nil {
			t.Errorf("the list %q was accepted, want a refusal", args)
		}
	}
}

// TestClusterExecViaRefusesUnknownKinds holds that a kind the table lacks, in
// any spelling the bead lists, is refused, and every table spelling accepted.
func TestClusterExecViaRefusesUnknownKinds(t *testing.T) {
	t.Parallel()
	refused := []string{
		"all",
		"pods",
		"deploy",
		"Deployment",
		"deployment.v1",
		"services.v1.core",
		"deployment:daemon",
		"node",
		"deployment daemon",
	}
	for _, kind := range refused {
		if _, err := execViaGuard([]string{"get", kind}); err == nil {
			t.Errorf("the kind %q was accepted, want a refusal", kind)
		}
	}
	accepted := []string{
		"deployment", "deployments", "statefulset", "statefulsets",
		"service", "services", "persistentvolumeclaim", "persistentvolumeclaims",
		"serviceaccount", "serviceaccounts", "configmap", "configmaps",
		"cronjob", "cronjobs", "namespace", "namespaces",
		"priorityclass", "priorityclasses", "customresourcedefinition", "customresourcedefinitions",
		"clusterrole", "clusterroles", "clusterrolebinding", "clusterrolebindings",
		"role", "roles", "rolebinding", "rolebindings",
		"validatingadmissionpolicy", "validatingadmissionpolicies",
		"validatingadmissionpolicybinding", "validatingadmissionpolicybindings",
		"networkpolicy", "networkpolicies",
	}
	for _, kind := range accepted {
		if _, err := execViaGuard([]string{"get", kind}); err != nil {
			t.Errorf("the table kind %q was refused: %v", kind, err)
		}
	}
}

// TestClusterExecViaRefusesUnknownFlags holds that a caller flag the allowlist
// does not name is refused before an exec in both exec-via modes, so a caller
// cannot override the kubeconfig, the context or the output.
func TestClusterExecViaRefusesUnknownFlags(t *testing.T) {
	t.Parallel()
	refused := [][]string{
		{"get", "deployment/daemon", "--kubeconfig", "/x"},
		{"get", "deployment/daemon", "--context", "other"},
		{"--context", "other", "get", "deployment/daemon"},
		{"get", "deployment/daemon", "--server", "https://x"},
		{"get", "deployment/daemon", "--token", "t"},
		{"get", "deployment/daemon", "--as", "u"},
		{"get", "deployment/daemon", "--user", "u"},
		{"get", "deployment/daemon", "--cluster", "c"},
		{"get", "deployment/daemon", "--insecure-skip-tls-verify"},
		{"get", "deployment/daemon", "-l", "app=x"},
		{"get", "deployment/daemon", "--field-selector", "x=y"},
		{"get", "deployment/daemon", "--template", "x"},
		{"get", "deployment/daemon", "-f", "file.yaml"},
		{"get", "deployment/daemon", "-A"},
		{"get", "deployment/daemon", "--all-namespaces"},
		{"get", "deployment/daemon", "-w"},
		{"get", "deployment/daemon", "--watch"},
		{"get", "deployment/daemon", "--raw", "/api/v1/pods"},
		{"get", "deployment/daemon", "--namespace", "openshell"},
		{"-n"},
	}
	for _, args := range refused {
		for _, execVia := range []string{"", "docker-exec:colima-gt-sbx:gt-sbx-worker"} {
			_, _, err, calls := clusterExecRead(t, execVia, "", args...)
			if err == nil {
				t.Errorf("execVia %q accepted the read %q, want a refusal", execVia, args)
			}
			if len(calls) != 0 {
				t.Errorf("the refused read %q made exec calls %v, want none", args, calls)
			}
		}
	}
}

// TestClusterExecViaRefusesANamespaceThatIsNotALabel holds a namespace that is
// not a DNS-1123 label to a refusal, and one that is to acceptance.
func TestClusterExecViaRefusesANamespaceThatIsNotALabel(t *testing.T) {
	t.Parallel()
	for _, namespace := range []string{"", "-rf", "Openshell", "a.b", "a_b", "a b"} {
		if _, err := execViaGuard([]string{"-n", namespace, "get", "deployment/daemon"}); err == nil {
			t.Errorf("the namespace %q was accepted, want a refusal", namespace)
		}
	}
	if _, err := execViaGuard([]string{"-n", "kube-system", "get", "deployment/daemon"}); err != nil {
		t.Errorf("the namespace kube-system was refused: %v", err)
	}
}

// TestClusterExecViaRefusesAReadWithNoVerbOrResource holds that an empty call,
// a call with no verb and a call with no target are each refused before any
// exec, and that more than one target token is refused.
func TestClusterExecViaRefusesAReadWithNoVerbOrResource(t *testing.T) {
	t.Parallel()
	refused := [][]string{
		nil,
		{"get"},
		{"get", "-o", "json"},
		{"-n", "openshell"},
		{"get", "deployment", "daemon", "extra"},
	}
	for _, args := range refused {
		_, _, err, calls := clusterExecRead(t, "", "", args...)
		if err == nil {
			t.Errorf("the read %q was accepted, want a refusal", args)
		}
		if len(calls) != 0 {
			t.Errorf("the refused read %q made exec calls %v, want none", args, calls)
		}
	}
}

// TestClusterExecViaRefusesAnotherProgram holds that a caller cannot name a
// program other than kubectl and so escape the read guard.
func TestClusterExecViaRefusesAnotherProgram(t *testing.T) {
	t.Parallel()
	recorder := &clusterExecRecorder{}
	runner, err := clusterKubectl("", "", recorder.run)
	if err != nil {
		t.Fatalf("building the read runner: %v", err)
	}
	if _, _, err := runner(context.Background(), "", nil, "bash", "get", "deployment/daemon", "-o", "json"); err == nil {
		t.Fatal("a caller naming bash was accepted, want a refusal")
	}
	if len(recorder.calls) != 0 {
		t.Errorf("the refused call made exec calls %v, want none", recorder.calls)
	}
}

// TestClusterExecViaHeaderNamesTheRebuild pins the exported interim header word
// for word, expiry statement included, because the drift command prints it.
func TestClusterExecViaHeaderNamesTheRebuild(t *testing.T) {
	t.Parallel()
	const want = "interim read path: docker exec reaches the sandbox cluster's kubectl through /etc/kubernetes/admin.conf; this exception expires at the sandbox cluster's next rebuild, when the read certificate replaces it"
	if ClusterExecViaInterimHeader != want {
		t.Errorf("the interim header is %q, want %q", ClusterExecViaInterimHeader, want)
	}
}

// TestClusterExecViaLeavesTheOutputFormToTheNextRule holds that a yaml read of
// a table kind is not this guard's business: only the command's own output pair
// rules refuse it.
func TestClusterExecViaLeavesTheOutputFormToTheNextRule(t *testing.T) {
	t.Parallel()
	_, _, err, calls := clusterExecRead(t, "", "", "-n", "openshell", "get", "deployment/daemon", "-o", "yaml")
	if err != nil {
		t.Errorf("a yaml read of a Deployment was refused: %v", err)
	}
	if len(calls) != 1 {
		t.Errorf("a yaml read of a Deployment made %d exec calls, want 1", len(calls))
	}
}
