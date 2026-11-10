// Package holdclaim writes the operator's hold and ESTOP sentinels into the
// daemon pod's claim as well as the Mac's town tree. The pod reads its dispatch
// hold and ESTOP from the town-state claim it mounts at TownRootPath, so a
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

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/config"
)

// ContextEnv and NamespaceEnv are the daemon.env names of the pod setting.
const (
	ContextEnv   = "GT_DAEMON_POD_CONTEXT"
	NamespaceEnv = "GT_DAEMON_POD_NAMESPACE"
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
		podRoot:  k8sdaemon.TownRootPath,
		run:      run,
		prefix: []string{
			"kubectl", "--context", target.Context, "-n", target.Namespace,
			"exec", "-i", "deploy/" + k8sdaemon.Name, "--",
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

// Set writes content to the claim file that mirrors path, then reads it back
// through the pod and refuses the write when the bytes differ.
func (w *Writer) Set(local string, content []byte) error {
	podPath, err := w.podPath(local)
	if err != nil {
		return err
	}
	if _, err := w.run(context.Background(), w.argv("tee", podPath), content); err != nil {
		return fmt.Errorf("writing %s in the pod: %w", podPath, err)
	}
	got, err := w.run(context.Background(), w.argv("cat", podPath), nil)
	if err != nil {
		return fmt.Errorf("reading %s back from the pod: %w", podPath, err)
	}
	if !bytes.Equal(got, content) {
		return fmt.Errorf("read-back of %s in the pod differs from the bytes written", podPath)
	}
	return nil
}

// Mirror copies the Mac's sentinel at path into the claim: it reads the local
// file and writes the same bytes through Set.
func (w *Writer) Mirror(local string) error {
	content, err := os.ReadFile(local) //nolint:gosec // G304: local is a sentinel the caller built under the town root
	if err != nil {
		return fmt.Errorf("reading %s to mirror it into the pod: %w", local, err)
	}
	return w.Set(local, content)
}

// Remove deletes the claim file that mirrors path. A file that is already gone
// is not an error, so a repeated lift succeeds.
func (w *Writer) Remove(local string) error {
	podPath, err := w.podPath(local)
	if err != nil {
		return err
	}
	if _, err := w.run(context.Background(), w.argv("rm", "-f", podPath), nil); err != nil {
		return fmt.Errorf("removing %s from the pod: %w", podPath, err)
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
