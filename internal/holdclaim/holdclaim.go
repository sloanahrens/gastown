// Package holdclaim writes the operator's hold and ESTOP sentinels into the
// daemon pod's claim as well as the Mac's town tree. The pod reads its dispatch
// hold and ESTOP from the town-state claim it mounts at podMount, so a
// sentinel that stays only on the Mac never stops the pod's dispatchers.
//
// The pod setting is read from settings/daemon.env: GT_DAEMON_POD_CONTEXT and
// GT_DAEMON_POD_NAMESPACE name the kubectl context and namespace of the daemon
// Deployment. They are given together or not at all, as gt update's
// --pod-context and --pod-namespace are. Neither set is the Mac-only town, and
// the writers do nothing beyond the local file.
package holdclaim

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// ContextEnv and NamespaceEnv are the daemon.env names of the pod setting.
const (
	ContextEnv   = "GT_DAEMON_POD_CONTEXT"
	NamespaceEnv = "GT_DAEMON_POD_NAMESPACE"
)

// deployName and podMount are the daemon Deployment's name and the path its
// town-state claim is mounted at, as deploy/k8s/daemon sets them. They are kept
// here, not imported, so the writer does not link the manifest package; the
// holdclaim tests fail if they drift from it.
const (
	deployName = "daemon"
	podMount   = "/Users/sloan/gt"
)

// kubectlTimeout bounds one kubectl call, so a pod that stops answering fails
// the write instead of holding the command open.
const kubectlTimeout = 60 * time.Second

// Exec runs argv with stdin and returns its stdout. The writers run every pod
// call through one, so a test can fake the cluster without kubectl.
type Exec func(ctx context.Context, argv []string, stdin []byte) ([]byte, error)

// Kubectl is the production Exec: it runs argv as a process, never through a
// shell. Its error names the command and the exit, never the captured output,
// which can hold a secret.
func Kubectl(ctx context.Context, argv []string, stdin []byte) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("holdclaim: no command to run")
	}
	ctx, cancel := context.WithTimeout(ctx, kubectlTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: argv is built here from validated settings, never a shell string
	cmd.Stdin = bytes.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", argv[0], err)
	}
	return out, nil
}

// Target names the daemon Deployment in the cluster.
type Target struct {
	Context   string
	Namespace string
}

// Writer mirrors the Mac's sentinels into the pod's claim. It is nil for the
// Mac-only town, so a caller checks for nil before every call.
type Writer struct {
	townRoot string
	podRoot  string
	run      Exec
	prefix   []string
}

// New returns a Writer for the town at townRoot that writes into the claim of
// the daemon Deployment named by target. It refuses a blank or malformed
// target: the context and namespace are the only things that choose which
// cluster the write lands in.
func New(townRoot string, target Target, run Exec) (*Writer, error) {
	if townRoot == "" {
		return nil, errors.New("holdclaim: no town root to map the sentinels from")
	}
	if run == nil {
		return nil, errors.New("holdclaim: no exec to reach the pod with")
	}
	if strings.TrimSpace(target.Context) == "" || strings.HasPrefix(target.Context, "-") || strings.ContainsAny(target.Context, " \t\r\n") {
		return nil, fmt.Errorf("%s %q: want a kubectl context name", ContextEnv, target.Context)
	}
	if !namespacePattern.MatchString(target.Namespace) {
		return nil, fmt.Errorf("%s %q: want a namespace name", NamespaceEnv, target.Namespace)
	}
	return &Writer{
		townRoot: townRoot,
		podRoot:  podMount,
		run:      run,
		prefix: []string{
			"kubectl", "--context", target.Context, "-n", target.Namespace,
			"exec", "-i", "deploy/" + deployName, "--",
		},
	}, nil
}

// namespacePattern is a DNS-1123 label, the form a namespace name takes.
var namespacePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// FromTown reads the pod setting for townRoot and returns the Writer it names,
// or nil with no error when the setting is absent. A setting that is only half
// given, blank, or unreadable is an error: the caller must refuse rather than
// set the Mac's sentinel and report the pod done.
func FromTown(townRoot string, run Exec) (*Writer, error) {
	if townRoot == "" {
		// An empty root would read settings/daemon.env from the working directory.
		return nil, errors.New("no town root to read the pod setting from")
	}
	env, err := config.LoadDaemonEnv(townRoot)
	if err != nil {
		return nil, fmt.Errorf("reading the pod setting: %w", err)
	}
	kubeContext, hasContext := env[ContextEnv]
	namespace, hasNamespace := env[NamespaceEnv]
	if !hasContext && !hasNamespace {
		return nil, nil
	}
	if !hasContext || !hasNamespace {
		return nil, fmt.Errorf("%s and %s must be given together", ContextEnv, NamespaceEnv)
	}
	return New(townRoot, Target{Context: kubeContext, Namespace: namespace}, run)
}

// Reach asks the pod one cheap question before any step is taken: whether its
// claim mount is there. A pod that does not answer is refused here, so a set or
// a lift never touches the Mac's copy while the pod cannot be reached.
func (w *Writer) Reach(ctx context.Context) error {
	if _, err := w.run(ctx, w.argv("test", "-d", w.podRoot), nil); err != nil {
		return fmt.Errorf("probing the pod: %w", err)
	}
	return nil
}

// Set writes content to the claim file that mirrors local, then reads it back
// through the pod and refuses the write when the bytes differ.
func (w *Writer) Set(ctx context.Context, local string, content []byte) error {
	podPath, err := w.podPath(local)
	if err != nil {
		return err
	}
	if _, err := w.run(ctx, w.argv("tee", podPath), content); err != nil {
		return fmt.Errorf("writing %s in the pod: %w", podPath, err)
	}
	got, err := w.run(ctx, w.argv("cat", podPath), nil)
	if err != nil {
		return fmt.Errorf("reading %s back from the pod: %w", podPath, err)
	}
	if !bytes.Equal(got, content) {
		return fmt.Errorf("read-back of %s in the pod differs from the bytes written", podPath)
	}
	return nil
}

// Remove deletes the claim file that mirrors local and reads back that it is
// gone. A file already gone is not an error, so a repeated lift succeeds.
func (w *Writer) Remove(ctx context.Context, local string) error {
	podPath, err := w.podPath(local)
	if err != nil {
		return err
	}
	if _, err := w.run(ctx, w.argv("rm", "-f", podPath), nil); err != nil {
		return fmt.Errorf("removing %s from the pod: %w", podPath, err)
	}
	// The read-back is a find of the file's own name in its directory: it exits 0
	// whether the file is there or not, and prints the path only when it is. So
	// any non-zero exit is the call failing (kubectl itself exits 1 when it cannot
	// reach the pod), never a report that the copy is absent, and absence is read
	// from an empty listing after a successful call.
	out, err := w.run(ctx, w.argv("find", path.Dir(podPath), "-maxdepth", "1", "-name", path.Base(podPath)), nil)
	if err != nil {
		return fmt.Errorf("reading %s back from the pod: %w", podPath, err)
	}
	if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("%s is still present in the pod after the removal", podPath)
	}
	return nil
}

// Activate sets the sentinel at local, the pod's claim first and the Mac after,
// so a partial set never leaves the pod running. A pod that does not answer is
// refused before any step. A nil pod is the Mac-only town.
func Activate(ctx context.Context, pod *Writer, what, local string, content []byte) error {
	if pod != nil {
		if err := pod.Reach(ctx); err != nil {
			return fmt.Errorf("%s NOT set, the pod is unreachable and nothing changed: %w", what, err)
		}
		if err := pod.Set(ctx, local, content); err != nil {
			return fmt.Errorf("%s NOT in the pod, so it is not set on the Mac either: %w", what, err)
		}
	}
	if err := os.WriteFile(local, content, 0o644); err != nil {
		if pod != nil {
			return fmt.Errorf("%s set in the pod but NOT on the Mac, so the pod is STOPPED: writing %s: %w", what, local, err)
		}
		return fmt.Errorf("writing the %s %s: %w", what, local, err)
	}
	return nil
}

// Mirror sets the sentinel already up at local into the claim, through Activate.
func Mirror(ctx context.Context, pod *Writer, what, local string) error {
	if pod == nil {
		return nil
	}
	content, err := os.ReadFile(local) //nolint:gosec // G304: local is a sentinel the caller built under the town root
	if err != nil {
		return fmt.Errorf("reading the %s %s to set it in the pod: %w", what, local, err)
	}
	return Activate(ctx, pod, what, local, content)
}

// Lift clears the sentinel at local, the Mac's file first and the pod's claim
// last, so a partial lift leaves the pod STOPPED. A pod that does not answer is
// refused before any step. A nil pod is the Mac-only town.
func Lift(ctx context.Context, pod *Writer, what, local string) error {
	return liftWith(ctx, pod, what, local, os.Remove)
}

// liftWith is Lift with the Mac's removal given, so a test can make the removal
// fail or report success without the file going.
func liftWith(ctx context.Context, pod *Writer, what, local string, remove func(string) error) error {
	if pod != nil {
		if _, err := pod.podPath(local); err != nil {
			return fmt.Errorf("%s lift refused, nothing removed: %w", what, err)
		}
		if err := pod.Reach(ctx); err != nil {
			return fmt.Errorf("%s lift refused, the pod is unreachable and nothing changed, so the pod stays STOPPED: %w", what, err)
		}
	}
	// Remove without asking whether the sentinel is up: a path that cannot be
	// read must surface the removal error rather than read as lifted.
	// kept names what a refused Mac step leaves on the pod, when there is one.
	kept := ""
	if pod != nil {
		kept = ", pod copy untouched: the pod stays STOPPED"
	}
	if err := remove(local); err != nil && !os.IsNotExist(err) {
		if pod == nil {
			return fmt.Errorf("failed to remove the %s file %s: %w", what, local, err)
		}
		return fmt.Errorf("%s lift: Mac copy NOT removed%s: %w", what, kept, err)
	}
	if _, err := os.Lstat(local); err == nil {
		return fmt.Errorf("%s lift: Mac copy still present after removal%s", what, kept)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("%s lift: Mac copy NOT confirmed removed%s: %w", what, kept, err)
	}
	if pod == nil {
		return nil
	}
	if err := pod.Remove(ctx, local); err != nil {
		return fmt.Errorf("%s lift: Mac copy gone, pod copy still present or not confirmed gone: the pod stays STOPPED: %w", what, err)
	}
	return nil
}

// argv is the kubectl prefix followed by the command the pod runs.
func (w *Writer) argv(command string, args ...string) []string {
	out := make([]string, 0, len(w.prefix)+1+len(args))
	out = append(out, w.prefix...)
	out = append(out, command)
	return append(out, args...)
}

// podPath maps a sentinel path under the Mac's town root to the same file under
// the claim's mount in the pod. A path outside the town root is refused: the
// claim holds the town's files and nothing else.
func (w *Writer) podPath(local string) (string, error) {
	rel, err := filepath.Rel(w.townRoot, local)
	if err != nil {
		return "", fmt.Errorf("mapping %s into the pod: %w", local, err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%s is not under the town root, so it has no place in the pod's claim", local)
	}
	return path.Join(w.podRoot, filepath.ToSlash(rel)), nil
}
