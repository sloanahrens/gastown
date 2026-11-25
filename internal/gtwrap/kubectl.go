package gtwrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/manifestdata"
)

// podTownRoot is the compiled-in town root inside the daemon pod, the one
// deploy/k8s/daemon renders as TownRootPath. A Kubectl whose PodRoot is empty maps
// onto it; the config key pod_town_root replaces it.
const podTownRoot = manifestdata.PodTownRoot

// podNameRE matches a pod name as kubectl prints it. A name read from the cluster
// goes into argv, so it is checked before any command uses it.
var podNameRE = regexp.MustCompile(`^[a-z0-9]([-.a-z0-9]*[a-z0-9])?$`)

// Kubectl reaches the daemon pod through the kubectl CLI: the kubernetes target.
// It execs into the one Running pod of a Deployment, looked up on every call, and
// has no throwaway rung: the pod is the only place the tool runs.
type Kubectl struct {
	// Town is the town root on the Mac: the caller passes $HOME/gt, which needs no
	// town marker, since the Mac is no longer a town. A working directory under it
	// maps onto the pod root; any other directory maps to the pod root itself.
	Town string
	// PodRoot is the town root inside the pod. Empty means podTownRoot. It must be
	// an absolute, clean path other than "/", since the pod walk-up script joins
	// under it and would otherwise search the whole pod.
	PodRoot string
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
	// Stat checks for a .beads directory when a bd call sets no BEADS_DIR. Nil
	// uses os.Stat. A .beads entry that cannot be read refuses the call, so an
	// unreadable candidate is never read as absent and routed to another rig.
	Stat func(string) (fs.FileInfo, error)
	// ReadFile reads the redirect file a walked .beads directory may hold. Nil
	// uses os.ReadFile. A redirect that exists but cannot be read refuses the
	// call, since read as absent it would name the worktree's own .beads.
	ReadFile func(string) ([]byte, error)
	// Stdout and Stderr are the wrapper's own streams. They receive the tool's
	// output when the pod town root and the Mac town differ: the tool prints pod
	// paths, and the wrapper rewrites them to the Mac's. When the roots are equal,
	// the output goes through the Shell unchanged, and a bd call's stderr is read
	// through them for its note. Nil refuses a rewriting call, so its output is never lost.
	Stdout io.Writer
	Stderr io.Writer
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
// mapped onto the pod root with a note on Note. A mapped directory the pod lacks
// runs in the nearest existing ancestor under the pod root, and the tool is told
// the caller's directory in GT_CALLER_CWD. A bd call that sets no BEADS_DIR, or
// an empty one, is given the .beads directory nearest its working directory,
// inside the town, so bd finds the same rig it did on the Mac. A credential in the
// environment is dropped with a note, since the environment travels in the
// kubectl argv.
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
	call.Env = env
	if err := k.settleBeadsDir(&call); err != nil {
		return exitDown, err
	}
	dir, under := podDir(k.Town, k.podRoot(), call.Dir)
	if !under {
		fmt.Fprintf(k.Note, "gt-wrap: %s is outside the town %s, which the pod cannot see, so the tool runs in %s\n", call.Dir, k.Town, k.podRoot())
	}
	if len(dropped) > 0 {
		fmt.Fprintf(k.Note, "gt-wrap: not forwarding %s into the pod: a credential stays in the pod's own Secrets and never goes in the kubectl argv\n", strings.Join(dropped, ", "))
	}
	// Keep a bd call's stderr tail so a broken exec stream is noted (gt-n3gkzs).
	tail := &tailBuffer{max: lostReplyTailBytes}
	exit, err := k.attach(l.pod, call, dir, tail)
	if note := lostReplyNote(call.Argv, exit, string(tail.buf)); note != "" {
		fmt.Fprintf(k.Note, "gt-wrap: %s\n", note)
	}
	k.noteReplacedPod(l.pod, exit)
	return exit, err
}

// lostReplyTailBytes bounds the stderr tail: the exec break is on the last line.
const lostReplyTailBytes = 2 << 10

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = b.buf[len(b.buf)-b.max:]
	}
	return len(p), nil
}

// noteReplacedPod says when a call's 137 is the daemon pod replaced under it: one
// bounded read of the pod, made once and never retried. The read asks for the
// deletion timestamp as well as the name, since a terminating pod still names
// itself; --ignore-not-found makes a missing pod a successful empty answer, so a
// read that fails is reported as unknown and never as the pod being gone.
func (k Kubectl) noteReplacedPod(pod string, exit int) {
	if exit != 137 {
		return
	}
	out, readExit, err := k.probe("-n", k.Namespace, "get", "pod", pod, "--ignore-not-found", "-o", "jsonpath={.metadata.name}{\" \"}{.metadata.deletionTimestamp}")
	fields := strings.Fields(out)
	switch {
	case err != nil || readExit != 0:
		fmt.Fprintf(k.Note, "gt-wrap: exit 137 and the pod %s's state is unknown: the read of the pod after the call did not answer, so 137 may be the tool's end or the pod's, and the call's result is unknown\n", pod)
	case len(fields) != 1 || fields[0] != pod:
		fmt.Fprintf(k.Note, "gt-wrap: exit 137 and the pod %s is gone: the pod was replaced while the call ran (a daemon roll), so 137 is the pod's end, not the tool's, and the call's result is unknown\n", pod)
	}
}

// attach runs the call in the pod. When the pod's town root and the Mac's differ, a
// path the tool prints names the pod's root, which the Mac does not have, so the
// output is rewritten to the Mac's root line by line. A terminal can carry an
// interactive prompt the tool prints with no newline and waits on, so with one
// attached the rewriter writes an unterminated line as far as a later byte cannot
// change it; without one the line is held until it ends, since the stream may be
// binary. The exit status, the standard input and the walk-up script are the same
// either way. A bd call's stderr is also copied into tail, so a broken exec stream
// is noted after the call.
func (k Kubectl) attach(pod string, call Call, dir string, tail *tailBuffer) (int, error) {
	argv := k.execArgv(pod, call, dir)
	// A bd call's tail needs the stderr the wrapper writes, or the shell keeps its own.
	bd := call.Argv[0] == nativeBdTool
	if !k.rewrites() {
		if !bd || k.Stdout == nil || k.Stderr == nil {
			if call.Stdin != nil {
				return k.Shell.AttachStdin(argv, nil, call.Stdin)
			}
			return k.Shell.Attach(argv, nil)
		}
		return k.Shell.AttachStreams(argv, nil, call.Stdin, k.Stdout, io.MultiWriter(k.Stderr, tail))
	}
	// The same condition stdioFlags uses for a terminal: -t is passed only when the
	// tool's stdin is the caller's terminal.
	prompt := call.TTY && call.Stdin == nil
	out := newOutputRewriter(k.Stdout, k.podRoot(), k.Town, prompt)
	errOut := newOutputRewriter(k.Stderr, k.podRoot(), k.Town, prompt)
	exit, err := k.Shell.AttachStreams(argv, nil, call.Stdin, out, io.MultiWriter(errOut, tail))
	// The tool's last line need not end in a newline, so the two streams are
	// flushed before the call returns. A failure to write the output is reported
	// with the tool's own status, so the caller keeps it.
	return exit, errors.Join(err, out.Flush(), errOut.Flush())
}

// rewrites reports whether the pod's town root and the Mac's differ, and so
// whether a path the tool prints must be rewritten to work on the Mac.
func (k Kubectl) rewrites() bool {
	return k.podRoot() != k.Town
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
// the caller's mapped directory, and never above the pod root. A target outside the
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

// watchdogScript runs the tool as a child and kills it when the caller's stdin
// reaches end of file (gt-bkdnxv). The tool reads no stdin, so the end of the
// kubectl client's stdin is the only signal that nobody wants the tool any more.
// The tool gets /dev/null as stdin and fd 3 keeps the caller's stdin for the
// watchdog alone. The watchdog exits with the tool's status, 143 once it killed it.
const watchdogScript = `exec 3<&0
"$@" 0</dev/null 3<&- &
child=$!
( while read -r _ <&3; do :; done; kill -TERM "$child" 2>/dev/null ) &
watcher=$!
wait "$child"
status=$?
kill "$watcher" 2>/dev/null
exit "$status"`

// watchdogArgv returns the sh command line that runs tool under the stdin watchdog.
// The tool and its arguments follow the sh placeholder as positional words, so a
// word holding a space, a quote or a dollar sign is never read as shell syntax. An
// empty tool, or one with a blank command name, is refused: there is nothing to run.
func watchdogArgv(tool []string) ([]string, error) {
	if len(tool) == 0 || tool[0] == "" {
		return nil, errors.New("gtwrap: no command to run under the stdin watchdog")
	}
	return watchdogWrap(tool), nil
}

// watchdogWrap is the watchdogArgv line for a tool that already names a command.
func watchdogWrap(tool []string) []string {
	argv := []string{"sh", "-c", watchdogScript, "sh"}
	return append(argv, tool...)
}

// execArgv is the kubectl command line that runs call in pod, in dir. The env
// assignments travel as an env command's arguments, since kubectl exec forwards no
// environment; the Dolt host is set last so a forwarded value cannot override it.
// The shell then walks up from dir to the nearest directory the pod has, and runs
// the tool there. A call that follows a stream runs under the stdin watchdog, so
// the tool ends when kubectl's stdin closes; every other call keeps its argv.
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
	argv = append(argv, "sh", "-c", podWalkUpScript, "sh", dir, k.podRoot(), call.Dir)
	tool := call.Argv
	if isFollowCall(tool) {
		// isFollowCall passes only a gt call, which names a command, so the
		// watchdog's refusal for a tool with no name cannot hold here.
		tool = watchdogWrap(tool)
	}
	return append(argv, tool...)
}

// deploymentRef names the Deployment, its namespace and its context, for a reason.
func (k Kubectl) deploymentRef() string {
	return fmt.Sprintf("deployment/%s in namespace %s at context %s", k.Deployment, k.Namespace, k.Context)
}

// podRoot is the town root inside the pod: PodRoot when set, else podTownRoot.
func (k Kubectl) podRoot() string {
	if k.PodRoot == "" {
		return podTownRoot
	}
	return k.PodRoot
}

// podDir maps the caller's directory onto the pod's town root. A directory under
// the Mac's town keeps its path relative to the town; any other directory maps to
// the pod root, and the second result is false so the caller can note it.
func podDir(town, root, dir string) (string, bool) {
	rel, err := filepath.Rel(town, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return root, false
	}
	return path.Join(root, filepath.ToSlash(rel)), true
}

// settleBeadsDir gives a bd call that sets no BEADS_DIR the .beads directory
// nearest at or above its working directory, the same walk bd itself made before
// the pod's bd became a forwarder (gt-iy4cpy). The forwarder needs the variable
// to tell which rig the call runs in. A caller's BEADS_DIR that names a directory
// is never overridden, and a working directory outside the town is left alone, so
// the forwarder's own refusal stands. An empty BEADS_DIR names no directory, and
// the forwarder reads it as no rig, so the walk answers it too (gt-icl6ru); every
// entry naming the variable is dropped first, so the pod's env sees only the
// walked value.
func (k Kubectl) settleBeadsDir(call *Call) error {
	if call.Argv[0] != nativeBdTool {
		return nil
	}
	if dir, ok := envValue(call.Env, beadsDirVar); ok && dir != "" {
		return nil
	}
	podBeads, ok, err := k.nearestBeadsDir(call.Dir)
	if err != nil || !ok {
		return err
	}
	kept := make([]string, 0, len(call.Env)+1)
	for _, entry := range call.Env {
		if name, _, ok := strings.Cut(entry, "="); ok && name == beadsDirVar {
			continue
		}
		kept = append(kept, entry)
	}
	call.Env = append(kept, beadsDirVar+"="+podBeads)
	return nil
}

// nearestBeadsDir walks from dir up to the town root and returns the .beads
// directory nearest at or above it, named inside the pod, following the redirect
// a worktree's .beads holds to the rig's beads directory (gt-8y5mcn). A
// directory outside the town yields ok false. The town's own .beads answers a
// walk that finds none, since the town always has beads. A .beads entry the stat
// cannot read refuses: read as absent, it would send the call to another rig's
// database.
func (k Kubectl) nearestBeadsDir(dir string) (string, bool, error) {
	rel, inside, err := nearestBeadsRel(k.Town, dir, statDir(k.Stat), readFileOf(k.ReadFile))
	if err != nil || !inside {
		return "", inside, err
	}
	if rel == "" {
		rel = beadsDirName
	}
	return path.Join(k.podRoot(), filepath.ToSlash(rel)), true, nil
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
	if k.PodRoot != "" && (!path.IsAbs(k.PodRoot) || path.Clean(k.PodRoot) != k.PodRoot || k.PodRoot == "/") {
		return fmt.Errorf("gtwrap: the kubernetes target's pod root is %q; it must be an absolute, clean path other than \"/\"", k.PodRoot)
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
	case k.rewrites() && (k.Stdout == nil || k.Stderr == nil):
		return errors.New("gtwrap: the pod town root and the Mac town differ, so the tool's output is rewritten, but no output writer is wired for it; the call is refused")
	}
	return nil
}
