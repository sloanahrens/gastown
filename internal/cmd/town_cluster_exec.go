package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/diskmaint/registry"
	"github.com/steveyegge/gastown/internal/polecatimage"
	"github.com/steveyegge/gastown/internal/update"
)

// clusterExecViaDockerPrefix starts the form that reaches a sandbox node's
// kubectl through docker exec.
const clusterExecViaDockerPrefix = "docker-exec:"

// sandboxKubectlConfig is the kubeconfig a sandbox node's kubectl reads: the
// kind admin.conf, which lives only inside the node, so the interim docker-exec
// path uses it rather than the command's own kubeconfig.
const sandboxKubectlConfig = "/etc/kubernetes/admin.conf"

// ClusterExecViaInterimHeader names the interim docker-exec exception and its
// expiry, for the drift command to print when it reads through that path.
const ClusterExecViaInterimHeader = "interim read path: docker exec reaches the sandbox cluster's kubectl through /etc/kubernetes/admin.conf; this exception expires at the sandbox cluster's next rebuild, when the read certificate replaces it"

// clusterExecVia is the parsed --exec-via flag: how the drift compare reaches
// kubectl. The zero value runs a plain kubectl with the kubeconfig the command
// was given; docker runs the node's kubectl through docker exec.
type clusterExecVia struct {
	docker     bool
	context    string
	container  string
	kubeconfig string
}

// clusterExecViaForms names the two accepted --exec-via forms, so every refusal
// shows the reader what to type instead.
const clusterExecViaForms = `"" (a plain kubectl with the given kubeconfig) or docker-exec:<context>:<container> (docker exec kubectl on a sandbox node)`

// clusterKubectl returns the read runner for execVia and kubeconfig, over run.
// An unknown execVia is refused before run is called.
func clusterKubectl(execVia, kubeconfig string, run update.RunFunc) (update.RunFunc, error) {
	via, err := parseClusterExecVia(execVia, kubeconfig)
	if err != nil {
		return nil, err
	}
	return via.runner(run), nil
}

// parseClusterExecVia reads the --exec-via value. The empty value runs a plain
// kubectl with kubeconfig, docker-exec:<context>:<container> runs the node's
// kubectl through docker exec, and any other value is refused naming both
// accepted forms.
func parseClusterExecVia(execVia, kubeconfig string) (clusterExecVia, error) {
	if execVia == "" {
		if err := checkClusterKubeconfig(kubeconfig); err != nil {
			return clusterExecVia{}, err
		}
		return clusterExecVia{kubeconfig: kubeconfig}, nil
	}
	if !strings.HasPrefix(execVia, clusterExecViaDockerPrefix) {
		return clusterExecVia{}, fmt.Errorf("--exec-via %q is not accepted; the accepted forms are %s", execVia, clusterExecViaForms)
	}
	// Exactly the prefix, the context and the container: a value with another
	// colon names a form the parser would otherwise read a part of wrongly.
	parts := strings.Split(execVia, ":")
	if len(parts) != 3 {
		return clusterExecVia{}, fmt.Errorf("--exec-via %q is not accepted; the accepted forms are %s", execVia, clusterExecViaForms)
	}
	contextName, container := parts[1], parts[2]
	// The context and the container reach docker's argv, which reads a leading
	// "-" as a flag, so each must be a name docker accepts before it is passed.
	if !polecatimage.ContextName(contextName) {
		return clusterExecVia{}, fmt.Errorf("--exec-via %q names Docker context %q, which is not one Docker context name; the accepted forms are %s", execVia, contextName, clusterExecViaForms)
	}
	if !registry.ValidContainer(container) {
		return clusterExecVia{}, fmt.Errorf("--exec-via %q names container %q, which is not one container name; the accepted forms are %s", execVia, container, clusterExecViaForms)
	}
	return clusterExecVia{docker: true, context: contextName, container: container}, nil
}

// checkClusterKubeconfig refuses a kubeconfig that cannot be a path, so a value
// that reads as a flag never reaches kubectl's argv.
func checkClusterKubeconfig(kubeconfig string) error {
	if kubeconfig == "" {
		return nil
	}
	if strings.HasPrefix(kubeconfig, "-") {
		return fmt.Errorf("--kubeconfig %q starts with '-', so it is not a path and nothing was run", kubeconfig)
	}
	if strings.ContainsFunc(kubeconfig, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("--kubeconfig %q holds a control character, so it is not a path and nothing was run", kubeconfig)
	}
	return nil
}

// clusterTargetKinds is the one table of the resource kinds a drift read may
// name, each with both kubectl spellings. A kind is compared exactly, so a
// version or group qualifier, another case and a spelling the table lacks are
// all refused; Secrets are absent because the only Secret read is
// `get secret <name>` (see execViaGuard).
var clusterTargetKinds = []struct{ singular, plural string }{
	{"deployment", "deployments"},
	{"statefulset", "statefulsets"},
	{"service", "services"},
	{"persistentvolumeclaim", "persistentvolumeclaims"},
	{"serviceaccount", "serviceaccounts"},
	{"configmap", "configmaps"},
	{"cronjob", "cronjobs"},
	{"namespace", "namespaces"},
	{"priorityclass", "priorityclasses"},
	{"customresourcedefinition", "customresourcedefinitions"},
	{"clusterrole", "clusterroles"},
	{"clusterrolebinding", "clusterrolebindings"},
	{"role", "roles"},
	{"rolebinding", "rolebindings"},
	{"validatingadmissionpolicy", "validatingadmissionpolicies"},
	{"validatingadmissionpolicybinding", "validatingadmissionpolicybindings"},
	{"networkpolicy", "networkpolicies"},
}

// execViaGuard admits only a read the drift compare may make and returns args
// unchanged: an optional -n <namespace>, the get verb, one target, and an
// optional output pair. Every other argument is refused before an exec, so the
// guard is an allowlist of these shapes and never a denylist of bad ones.
func execViaGuard(args []string) ([]string, error) {
	rest := args
	if len(rest) > 0 && rest[0] == "-n" {
		if len(rest) < 2 {
			return nil, errors.New(`a "-n" with no namespace is refused, so nothing was run`)
		}
		if err := checkClusterNamespace(rest[1]); err != nil {
			return nil, err
		}
		rest = rest[2:]
	}
	if len(rest) == 0 {
		return nil, errors.New("no kubectl verb was given, so nothing was run")
	}
	if rest[0] != "get" {
		return nil, fmt.Errorf("the %q verb is not a read, and the read runner passes only get, so nothing was run", rest[0])
	}
	target, output := splitClusterTarget(rest[1:])
	if err := checkClusterTarget(target); err != nil {
		return nil, err
	}
	if len(output) > 0 {
		if err := checkClusterOutput(output); err != nil {
			return nil, err
		}
		// A Secret's data is what any output format but the default table
		// carries, and the table is the only Secret read this comparison needs.
		if isClusterSecretRead(target) {
			return nil, fmt.Errorf("a Secret read carries no output flag, so nothing was run")
		}
	}
	return args, nil
}

// splitClusterTarget cuts the target tokens from the output pair: a target
// never starts with '-' and an output flag always does.
func splitClusterTarget(args []string) (target, output []string) {
	for i, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return args[:i], args[i:]
		}
	}
	return args, nil
}

// checkClusterTarget holds a read to one target: a kind, a kind and a name, or
// a comma list of kind or kind/name entries.
func checkClusterTarget(tokens []string) error {
	switch len(tokens) {
	case 0:
		return errors.New("the read names no resource, so nothing was run")
	case 1:
		for _, entry := range strings.Split(tokens[0], ",") {
			if err := checkClusterTargetEntry(entry); err != nil {
				return err
			}
		}
		return nil
	case 2:
		if tokens[0] == "secret" {
			return checkClusterName(tokens[1])
		}
		if !isClusterTargetKind(tokens[0]) {
			return fmt.Errorf("%q is not a resource kind the read runner reads, so nothing was run", tokens[0])
		}
		return checkClusterName(tokens[1])
	default:
		return fmt.Errorf("the read names %q, which is more than one target, so nothing was run", tokens)
	}
}

// checkClusterTargetEntry holds one comma-list entry to a kind on the table,
// optionally with a name.
func checkClusterTargetEntry(entry string) error {
	kind, name, hasName := strings.Cut(entry, "/")
	if !isClusterTargetKind(kind) {
		return fmt.Errorf("%q is not a resource kind the read runner reads, so nothing was run", entry)
	}
	if hasName {
		return checkClusterName(name)
	}
	return nil
}

// isClusterTargetKind reports whether kind is one of the table's spellings.
func isClusterTargetKind(kind string) bool {
	for _, pair := range clusterTargetKinds {
		if kind == pair.singular || kind == pair.plural {
			return true
		}
	}
	return false
}

// checkClusterName refuses a name that cannot be a resource name, so a value
// that reads as a flag or another target never reaches kubectl's argv.
func checkClusterName(name string) error {
	invalid := name == "" || strings.HasPrefix(name, "-") ||
		strings.ContainsFunc(name, func(r rune) bool {
			return r < 0x20 || r == 0x7f || r == '/' || r == ',' || r == ':'
		})
	if invalid {
		return fmt.Errorf("%q is not a resource name, so nothing was run", name)
	}
	return nil
}

// checkClusterNamespace refuses a namespace that cannot be a DNS-1123 label, so
// a value that reads as a flag never reaches kubectl's argv.
func checkClusterNamespace(namespace string) error {
	invalid := namespace == "" || strings.HasPrefix(namespace, "-") ||
		strings.ContainsFunc(namespace, func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-'
		})
	if invalid {
		return fmt.Errorf("the namespace %q is not a Kubernetes namespace name, so nothing was run", namespace)
	}
	return nil
}

// checkClusterOutput holds the trailing arguments to one -o pair, leaving the
// format value to the rules that force it.
func checkClusterOutput(args []string) error {
	switch {
	case len(args) == 2 && (args[0] == "-o" || args[0] == "--output") && clusterOutputValue(args[1]):
		return nil
	case len(args) == 1 && strings.HasPrefix(args[0], "--output=") && clusterOutputValue(strings.TrimPrefix(args[0], "--output=")):
		return nil
	case len(args) == 1 && strings.HasPrefix(args[0], "-o") && !strings.HasPrefix(args[0], "--") &&
		clusterOutputValue(strings.TrimPrefix(strings.TrimPrefix(args[0], "-o"), "=")):
		return nil
	default:
		return fmt.Errorf("the output arguments %q are not one -o pair, so nothing was run", args)
	}
}

// clusterOutputValue reports whether value could be an output format, so a flag
// read as a value is refused.
func clusterOutputValue(value string) bool {
	return value != "" && !strings.HasPrefix(value, "-")
}

// isClusterSecretRead reports whether the accepted target is the one Secret
// read, `get secret <name>`: the singular spelling, one name.
func isClusterSecretRead(target []string) bool {
	return len(target) == 2 && target[0] == "secret"
}

// runner returns the read seam a drift run reads live objects through: it
// refuses anything but a kubectl get before run is called, then builds the
// kubectl call the exec-via form names.
func (v clusterExecVia) runner(run update.RunFunc) update.RunFunc {
	return func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
		// The runner decides which program reaches kubectl, so a caller cannot
		// name another one and escape the read-only guard.
		if name != "kubectl" {
			return "", -1, fmt.Errorf("the read runner runs kubectl only, and %q was asked for, so nothing was run", name)
		}
		checked, err := execViaGuard(args)
		if err != nil {
			return "", -1, err
		}
		name, argv := v.command(checked)
		return run(ctx, dir, env, name, argv...)
	}
}

// command builds the program and arguments that run args through docker exec or
// a plain kubectl. execViaGuard has already refused any caller flag that could
// override the fixed kubeconfig, so a caller cannot redirect the read.
func (v clusterExecVia) command(args []string) (string, []string) {
	if v.docker {
		argv := make([]string, 0, 7+len(args))
		argv = append(argv, "--context", v.context, "exec", v.container, "kubectl", "--kubeconfig", sandboxKubectlConfig)
		return "docker", append(argv, args...)
	}
	if v.kubeconfig == "" {
		return "kubectl", args
	}
	return "kubectl", append([]string{"--kubeconfig", v.kubeconfig}, args...)
}
