package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/deployerinbox"
)

// requestMode is the mode a request file carries. os.OpenFile's create mode is
// cut by the process umask, which a writer whose umask hides group and other
// read would leave the deployer unable to read, so the command chmods the file
// to this mode (gt-v5dblz).
const requestMode fs.FileMode = 0o644

// requestStampLayout is the UTC stamp that starts a request id.
const requestStampLayout = "20060102T150405Z"

var townDeployerRequestCmd = &cobra.Command{
	Use:   "request",
	Short: "Write one plan, apply or rollback request into the deployer's inbox",
	Long: `Write one request file into the deployer's inbox, and print its id and the
path the answer will appear at, <inbox>/../answers/<id>.json.

The file is <inbox>/<id>.json, where the id is a UTC stamp and eight random hex
digits. It is created exclusively, so a new request never replaces one already
in the inbox. Its three keys, id, kind and plan, are the request schema
internal/deployerinbox reads: a kind that is not plan, apply or rollback, or a
plan name that is not a plan directory name, is refused before anything is
written.

The command reads no kubeconfig and no credential; it needs only the inbox
directory. It refuses inside a pod, which cannot reach the deployer's host
inbox.

Examples:
  gt town deployer request --kind plan --plan 2026-11-19-first --inbox ~/gt/deployer/inbox
  gt town deployer request --kind apply --plan 2026-11-19-first --inbox ~/gt/deployer/inbox`,
	Args: cobra.NoArgs,
	RunE: runTownDeployerRequest,
}

func init() {
	townDeployerCmd.AddCommand(townDeployerRequestCmd)
	townDeployerRequestCmd.Flags().String("kind", "", "what to ask for: plan, apply or rollback (required)")
	townDeployerRequestCmd.Flags().String("plan", "", "the plan directory name to plan, apply or roll back (required)")
	townDeployerRequestCmd.Flags().String("inbox", "", "the deployer's inbox directory (required)")
	_ = townDeployerRequestCmd.MarkFlagRequired("kind")
	_ = townDeployerRequestCmd.MarkFlagRequired("plan")
	_ = townDeployerRequestCmd.MarkFlagRequired("inbox")
}

// townDeployerRequestEnv is what `gt town deployer request` reads from its
// session: the flags and the host seams, so a test drives every refusal.
type townDeployerRequestEnv struct {
	kind    string
	plan    string
	inbox   string
	getenv  func(string) string
	now     func() time.Time
	random  io.Reader
	marshal func(any) ([]byte, error)
	create  func(name string, perm fs.FileMode) (io.WriteCloser, error)
	chmod   func(name string, mode fs.FileMode) error
	remove  func(name string) error
	out     io.Writer
}

// townDeployerRequestFile is the request's wire form: the three keys
// deployerinbox.ParseRequest accepts.
type townDeployerRequestFile struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Plan string `json:"plan"`
}

func runTownDeployerRequest(cmd *cobra.Command, _ []string) error {
	kind, err := cmd.Flags().GetString("kind")
	if err != nil {
		return fmt.Errorf("reading --kind: %w", err)
	}
	plan, err := cmd.Flags().GetString("plan")
	if err != nil {
		return fmt.Errorf("reading --plan: %w", err)
	}
	inbox, err := cmd.Flags().GetString("inbox")
	if err != nil {
		return fmt.Errorf("reading --inbox: %w", err)
	}
	return runTownDeployerRequestIn(townDeployerRequestEnv{
		kind:    kind,
		plan:    plan,
		inbox:   inbox,
		getenv:  os.Getenv,
		now:     time.Now,
		random:  rand.Reader,
		marshal: json.Marshal,
		create:  createRequestFile,
		chmod:   os.Chmod,
		remove:  os.Remove,
		out:     cmd.OutOrStdout(),
	})
}

// runTownDeployerRequestIn is what `gt town deployer request` runs.
func runTownDeployerRequestIn(env townDeployerRequestEnv) error {
	if env.getenv(podEnv) != "" {
		return fmt.Errorf("gt town deployer request refuses inside a pod, where %s is set: the deployer's inbox is a host directory", podEnv)
	}
	if env.inbox == "" {
		return errors.New("gt town deployer request needs --inbox: the request file goes into the deployer's inbox directory")
	}

	id, err := newRequestID(env.now(), env.random)
	if err != nil {
		return err
	}
	data, err := env.marshal(townDeployerRequestFile{ID: id, Kind: env.kind, Plan: env.plan})
	if err != nil {
		return fmt.Errorf("encoding the %s request for the plan %s: %w", env.kind, env.plan, err)
	}
	name := id + ".json"

	// ParseRequest is the inbox's own reader: the command asks it to read the
	// bytes it is about to write, so a kind, plan or id the deployer would
	// refuse fails here instead of lying unread in the inbox.
	if _, err := deployerinbox.ParseRequest(name, data); err != nil {
		return fmt.Errorf("the request asks for --kind %q and --plan %q: %w", env.kind, env.plan, err)
	}

	path := filepath.Join(env.inbox, name)
	if err := writeRequestFile(env, path, data); err != nil {
		return err
	}

	answers := filepath.Join(env.inbox, "..", "answers", name)
	if _, err := fmt.Fprintf(env.out, "id: %s\nanswer: %s\n", id, answers); err != nil {
		return fmt.Errorf("the request %s is written to %s, but printing its id and answers path failed: %w", id, path, err)
	}
	return nil
}

// newRequestID returns <UTC stamp>-<8 hex random digits>.
func newRequestID(now time.Time, random io.Reader) (string, error) {
	var b [4]byte
	if _, err := io.ReadFull(random, b[:]); err != nil {
		return "", fmt.Errorf("reading the request id's random digits: %w", err)
	}
	return now.UTC().Format(requestStampLayout) + "-" + hex.EncodeToString(b[:]), nil
}

// createRequestFile creates name and fails when it exists, so a request
// already in the inbox is never replaced.
func createRequestFile(name string, perm fs.FileMode) (io.WriteCloser, error) {
	return os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
}

// writeRequestFile creates path exclusively, sets its mode and writes data; a
// failed step removes the partial request and returns its cause.
func writeRequestFile(env townDeployerRequestEnv, path string, data []byte) error {
	f, err := env.create(path, requestMode)
	if err != nil {
		return fmt.Errorf("writing the request %s: %w", path, err)
	}
	if err := env.chmod(path, requestMode); err != nil {
		cause := fmt.Errorf("setting the mode of the request %s: %w", path, err)
		if closeErr := f.Close(); closeErr != nil {
			cause = errors.Join(cause, fmt.Errorf("closing the request %s: %w", path, closeErr))
		}
		return discardRequestFile(env, path, cause)
	}
	if _, err := f.Write(data); err != nil {
		cause := fmt.Errorf("writing the request %s: %w", path, err)
		if closeErr := f.Close(); closeErr != nil {
			cause = errors.Join(cause, fmt.Errorf("closing the request %s: %w", path, closeErr))
		}
		return discardRequestFile(env, path, cause)
	}
	if err := f.Close(); err != nil {
		return discardRequestFile(env, path, fmt.Errorf("closing the request %s: %w", path, err))
	}
	return nil
}

// discardRequestFile removes a request file after a failed step and returns
// cause, joined with the removal failure when the file could not be removed.
// An absent file needs no removal.
func discardRequestFile(env townDeployerRequestEnv, path string, cause error) error {
	if err := env.remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(cause, fmt.Errorf("removing the incomplete request %s: %w", path, err))
	}
	return cause
}
