package gtwrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// podTownRoot is the town root inside the daemon pod: the path the Deployment
// sets as GT_TOWN_ROOT and mounts the town-state claim at. It is
// deploy/k8s/daemon's TownRootPath, and a test pins the two.
const podTownRoot = "/Users/sloan/gt"

// podNameRE matches a pod name as kubectl prints it. A name read from the cluster
// goes into argv, so it is checked before any command uses it.
var podNameRE = regexp.MustCompile(`^[a-z0-9]([-.a-z0-9]*[a-z0-9])?$`)

// Kubectl reaches the daemon pod through the kubectl CLI: the kubernetes target.
// It execs into the one Running pod of a Deployment, looked up on every call, and
// has no throwaway rung: the pod is the only place the tool runs.
type Kubectl struct {
	// Town is the town root on the Mac: the caller passes $HOME/gt, which needs no
	// town marker, since the Mac is no longer a town. A working directory under it
	// maps onto podTownRoot; any other directory maps to podTownRoot itself.
	Town string
	// Context names the kubectl context every command runs in.
	Context string
	// Namespace is the namespace of the Deployment.
	Namespace string
	// Deployment names the Deployment whose one Running pod a call execs into.
	Deployment string
	// Container names the container in that pod the tool runs in.
	Container string
	// Tool is the tool the doctor checks for in the container: the name the
	// wrapper was invoked as. Launch does not check it; the exec runs it.
	Tool string
	// Shell runs the kubectl command lines.
	Shell Shell
	// LookPath finds kubectl on PATH. Nil refuses: an unwired check is not a pass.
	LookPath func(file string) (string, error)
	// Note receives the stderr note for a working directory the pod cannot see.
	// Nil refuses the call, so a mapped directory is never silent.
	Note io.Writer
	// ProbeTimeout bounds each probe. Zero means probeTimeout.
	ProbeTimeout time.Duration
}

// kubeLadder is what the probes found: the rung, why, and the pod a call execs
// into when the rung is exec.
type kubeLadder struct {
	rung   Rung
	reason string
	pod    string
}

// Launch runs call by exec into the pod the ladder finds, and refuses with the
// doctor's reason when no rung holds. A working directory the pod cannot see is
// mapped onto podTownRoot with a note on Note. A mapped directory the pod lacks
// runs in the nearest existing ancestor under podTownRoot, and the tool is told
// the caller's directory in GT_CALLER_CWD. A credential in the environment is
// dropped with a note, since the environment travels in the kubectl argv.
func (k Kubectl) Launch(call Call) (int, error) {
	if err := k.validate(); err != nil {
		return exitDown, err
	}
	if err := checkCall(call); err != nil {
		return exitDown, err
	}
	l := k.ladder()
	if l.rung != RungExec {
		return exitDown, fmt.Errorf("gtwrap: %s", l.reason)
	}
	env, dropped, err := screenEnv(call.Env)
	if err != nil {
		return exitDown, err
	}
	dir, under := podDir(k.Town, call.Dir)
	if !under {
		fmt.Fprintf(k.Note, "gt-wrap: %s is outside the town %s, which the pod cannot see, so the tool runs in %s\n", call.Dir, k.Town, podTownRoot)
	}
	if len(dropped) > 0 {
		fmt.Fprintf(k.Note, "gt-wrap: not forwarding %s into the pod: a credential stays in the pod's own Secrets and never goes in the kubectl argv\n", strings.Join(dropped, ", "))
	}
	call.Env = env
	return k.Shell.Attach(k.execArgv(l.pod, call, dir), nil)
}

// screenEnv drops the entries of env that carry a credential, and returns the names
// it dropped, never their values. kubectl exec forwards no environment, so each
// entry that crosses is an argument of the Mac's kubectl process and of the pod's
// env process. An entry with no "=" names no value, and env would run its name as
// a command, so the call is refused rather than passed on.
func screenEnv(env []string) (kept, dropped []string, err error) {
	kept = make([]string, 0, len(env))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, nil, errors.New("gtwrap: an environment entry has no value, so it cannot cross into the pod; the call is refused")
		}
		if isCredential(name) {
			dropped = append(dropped, name)
			continue
		}
		kept = append(kept, entry)
	}
	return kept, dropped, nil
}

// isCredential reports whether name carries a credential. The docker target screens
// only the CLAUDE_ family, because its values stay off the docker argv. Here every
// forwarded value is in argv, so the credential words match under every prefix:
// GT_DOLT_PASSWORD is dropped although the GT_ family forwards it for docker.
func isCredential(name string) bool {
	return slices.Contains(config.UnforwardedCredentialEnvVars(), name) || containsCredentialWord(strings.ToUpper(name))
}

// Doctor reports the rungs of the ladder for the tool the wrapper was invoked as:
// kubectl on PATH, the context reachable, one Running pod, and the tool present
// in the pod's container. The first rung that fails is the diagnosis. Launch
// stops at the pod, so the tool rung is a doctor-only check.
func (k Kubectl) Doctor() (Diagnosis, error) {
	if err := k.validate(); err != nil {
		return Diagnosis{}, err
	}
	if _, err := toolFor(k.Tool); err != nil {
		return Diagnosis{}, err
	}
	l := k.ladder()
	diag := Diagnosis{Rung: l.rung, Container: k.Container, Image: k.deploymentRef(), Reason: l.reason}
	if l.rung != RungExec {
		return diag, nil
	}
	diag.Container = l.pod + "/" + k.Container
	if err := k.checkTool(l.pod); err != nil {
		diag.Rung = RungDown
		diag.Reason = err.Error()
		return diag, nil
	}
	diag.Reason = fmt.Sprintf("the pod %s is running and %s answers in container %s", l.pod, k.Tool, k.Container)
	return diag, nil
}

// ladder runs the probes of rungs 1 to 3 in order and stops at the first that
// fails. Launch and Doctor both come through here, so they name the same pod.
func (k Kubectl) ladder() kubeLadder {
	if _, err := k.LookPath("kubectl"); err != nil {
		return kubeLadder{rung: RungDown, reason: "kubectl is not on PATH, so the kubernetes target cannot run; install the kubectl CLI and run the command again"}
	}
	if _, exit, err := k.probe("get", "--raw", "/readyz"); err != nil || exit != 0 {
		return kubeLadder{rung: RungDown, reason: fmt.Sprintf("the kubectl context %s is not reachable: get /readyz did not answer, so %s cannot be reached; check that the cluster is up and the context is in the kubeconfig, then run the command again", k.Context, k.deploymentRef())}
	}
	selector, err := k.selector()
	if err != nil {
		return kubeLadder{rung: RungDown, reason: fmt.Sprintf("the selector of %s cannot be read, so its pod cannot be found: %v", k.deploymentRef(), err)}
	}
	out, exit, err := k.probe("-n", k.Namespace, "get", "pods", "-l", selector, "--field-selector=status.phase=Running", "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil || exit != 0 {
		return kubeLadder{rung: RungDown, reason: fmt.Sprintf("the Running pods of %s cannot be listed, so the call did not run", k.deploymentRef())}
	}
	pods := strings.Fields(out)
	if len(pods) != 1 {
		return kubeLadder{rung: RungDown, reason: fmt.Sprintf("%s has %d Running pods, and the call needs exactly one; check the pods with `kubectl --context %s -n %s get pods -l %s`", k.deploymentRef(), len(pods), k.Context, k.Namespace, selector)}
	}
	if !podNameRE.MatchString(pods[0]) {
		return kubeLadder{rung: RungDown, reason: fmt.Sprintf("the pod %s of %s has a name the wrapper refuses to pass to kubectl", pods[0], k.deploymentRef())}
	}
	return kubeLadder{rung: RungExec, reason: fmt.Sprintf("the pod %s of %s is running", pods[0], k.deploymentRef()), pod: pods[0]}
}

// selector reads the Deployment's own pod selector, so the pod lookup follows the
// Deployment as it is rather than a label the wrapper guesses. An empty selector
// would match every pod in the namespace, so it is refused.
func (k Kubectl) selector() (string, error) {
	out, exit, err := k.probe("-n", k.Namespace, "get", "deployment", k.Deployment, "-o", "jsonpath={.spec.selector.matchLabels}")
	if err != nil {
		return "", fmt.Errorf("the kubectl read did not run: %w", err)
	}
	if exit != 0 {
		return "", fmt.Errorf("the kubectl read exited %d", exit)
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(out), &labels); err != nil {
		return "", errors.New("the selector is not a label map")
	}
	if len(labels) == 0 {
		return "", errors.New("the selector names no label")
	}
	pairs := make([]string, 0, len(labels))
	for key, value := range labels {
		pairs = append(pairs, key+"="+value)
	}
	slices.Sort(pairs)
	return strings.Join(pairs, ","), nil
}

// checkTool runs one check that the tool answers in the pod's container, and
// refuses when it cannot answer or finds nothing on PATH.
func (k Kubectl) checkTool(pod string) error {
	args := []string{"-n", k.Namespace, "exec", pod, "-c", k.Container, "--", "sh", "-c", toolCheck, "sh", k.Tool}
	out, exit, err := k.probe(args...)
	if err != nil {
		return fmt.Errorf("gtwrap: check that %s answers in container %s of pod %s: %w", k.Tool, k.Container, pod, err)
	}
	if exit != 0 || strings.TrimSpace(out) == "" {
		return fmt.Errorf("gtwrap: %s does not answer in container %s of pod %s: the check exited %d", k.Tool, k.Container, pod, exit)
	}
	return nil
}

// podWalkUpScript runs the tool in the nearest directory the pod has at or above
// the caller's mapped directory, and never above podTownRoot. A target outside the
// root, a target with a .. step, an absent root, a walk that finds no directory,
// and a directory the shell cannot enter each refuse in one line and exit 125, so
// the tool did not start; otherwise the tool's own exit status passes through. A
// walk that ends anywhere but the mapped directory names the caller's Mac directory
// in GT_CALLER_CWD, since the tool reads its identity there and the process
// directory is only an ancestor. Every directory crosses as a positional parameter,
// so a path holding a space or a quote stays one word.
const podWalkUpScript = `target=$1; root=$2; caller=$3; shift 3
refuse() { echo "gtwrap: $1, so the tool did not run" >&2; exit 125; }
case $target in
  "$root"|"$root"/*) ;;
  *) refuse "$target is not under the pod town root $root" ;;
esac
case $target in
  */..|*/../*) refuse "$target has a .. step" ;;
esac
if [ ! -d "$root" ]; then
  refuse "the pod town root $root is not a directory"
fi
d=$target
while [ ! -d "$d" ]; do
  p=$(dirname "$d")
  if [ -z "$p" ] || [ "$p" = "$d" ]; then
    refuse "no directory at or above $target is in the pod"
  fi
  d=$p
done
if [ "$d" != "$target" ]; then
  GT_CALLER_CWD=$caller
  export GT_CALLER_CWD
fi
cd "$d" 2>/dev/null || refuse "cannot enter $d"
exec "$@"`

// execArgv is the kubectl command line that runs call in pod, in dir. The env
// assignments travel as an env command's arguments, since kubectl exec forwards no
// environment; the Dolt host is set last so a forwarded value cannot override it.
// The shell then walks up from dir to the nearest directory the pod has, and runs
// the tool there.
func (k Kubectl) execArgv(pod string, call Call, dir string) []string {
	argv := k.kubectlArgv("-n", k.Namespace, "exec")
	argv = append(argv, stdioFlags(call)...)
	argv = append(argv, pod, "-c", k.Container, "--", "env")
	for _, entry := range call.Env {
		name, _, _ := strings.Cut(entry, "=")
		if name == doltHostVar {
			continue
		}
		argv = append(argv, entry)
	}
	argv = append(argv, doltHostVar+"="+doltHostGateway)
	argv = append(argv, "sh", "-c", podWalkUpScript, "sh", dir, podTownRoot, call.Dir)
	return append(argv, call.Argv...)
}

// deploymentRef names the Deployment, its namespace and its context, for a reason.
func (k Kubectl) deploymentRef() string {
	return fmt.Sprintf("deployment/%s in namespace %s at context %s", k.Deployment, k.Namespace, k.Context)
}

// podDir maps the caller's directory onto the pod's town root. A directory under
// the Mac's town keeps its path relative to the town; any other directory maps to
// the town root, and the second result is false so the caller can note it.
func podDir(town, dir string) (string, bool) {
	rel, err := filepath.Rel(town, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return podTownRoot, false
	}
	return path.Join(podTownRoot, filepath.ToSlash(rel)), true
}

// probe runs one kubectl command that only reads state, with the context flag set
// and bounded by ProbeTimeout.
func (k Kubectl) probe(args ...string) (string, int, error) {
	limit := k.ProbeTimeout
	if limit <= 0 {
		limit = probeTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	return k.Shell.Capture(ctx, append(k.kubectlArgv(), args...))
}

// kubectlArgv is the kubectl command line for args, in the configured context.
func (k Kubectl) kubectlArgv(args ...string) []string {
	return append([]string{"kubectl", "--context", k.Context}, args...)
}

// validate refuses a target that is not fully set up. A config field that is
// empty, or starts with a dash and would be read as a flag, is refused by name;
// so is a dependency left unwired, since an unwired check is not a pass.
func (k Kubectl) validate() error {
	fields := []struct{ name, value string }{
		{"context", k.Context},
		{"namespace", k.Namespace},
		{"deployment", k.Deployment},
		{"container", k.Container},
	}
	for _, f := range fields {
		if f.value == "" || strings.HasPrefix(f.value, "-") {
			return fmt.Errorf("gtwrap: the kubernetes target's %s is %q; it must name a value that does not start with a dash", f.name, f.value)
		}
	}
	switch {
	case k.Town == "":
		return errors.New("gtwrap: no town root, so the kubernetes target cannot map the working directory")
	case k.Shell == nil:
		return errors.New("gtwrap: no shell is wired for the kubernetes target, so the call is refused")
	case k.LookPath == nil:
		return errors.New("gtwrap: no kubectl check is wired for the kubernetes target, so the call is refused")
	case k.Note == nil:
		return errors.New("gtwrap: no note writer is wired for the kubernetes target, so a mapped directory would be silent; the call is refused")
	}
	return nil
}
