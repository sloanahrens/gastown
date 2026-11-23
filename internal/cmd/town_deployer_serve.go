package cmd

import (
	"context"
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

// serveSteps is the deployer's plan, apply, rollback and record steps; nil
// until bead gt-1afrbd wires them, and the run refuses a nil one.
var serveSteps deployerinbox.Steps

var townDeployerServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Answer one request from the deployer's inbox and exit",
	Long: `Answer the oldest request in --inbox once: run its plan, apply or rollback
steps, write the answer into --answers and move the request into <state>/done.

Examples:
  gt town deployer serve --once --as-user deployer --inbox ~deployer/.gt/deployer/inbox --answers ~deployer/.gt/deployer/answers --state ~deployer/.gt/deployer/state`,
	Args: cobra.NoArgs,
	RunE: runTownDeployerServe,
}

func init() {
	townDeployerCmd.AddCommand(townDeployerServeCmd)
	townDeployerServeCmd.Flags().Bool("once", false, "answer one request and exit; serve refuses without it")
	townDeployerServeCmd.Flags().String("as-user", "", "the deployer user the run acts for (required)")
	townDeployerServeCmd.Flags().String("inbox", "", "the deployer's inbox directory (required)")
	townDeployerServeCmd.Flags().String("answers", "", "the directory answers are written into (required)")
	townDeployerServeCmd.Flags().String("state", "", "the deployer's state directory, holding done/ (required)")
	for _, name := range []string{"as-user", "inbox", "answers", "state"} {
		_ = townDeployerServeCmd.MarkFlagRequired(name)
	}
}

func runTownDeployerServe(cmd *cobra.Command, _ []string) error {
	once, err := cmd.Flags().GetBool("once")
	if err != nil {
		return fmt.Errorf("reading --once: %w", err)
	}
	values := make(map[string]string, 3)
	for _, name := range []string{"inbox", "answers", "state"} {
		v, err := cmd.Flags().GetString(name)
		if err != nil {
			return fmt.Errorf("reading --%s: %w", name, err)
		}
		values[name] = v
	}
	return runTownDeployerServeIn(cmd.Context(), townDeployerServeEnv{
		once:    once,
		inbox:   values["inbox"],
		answers: values["answers"],
		state:   values["state"],
		steps:   serveSteps,
		now:     time.Now,
		out:     cmd.OutOrStdout(),
	})
}

// townDeployerServeEnv is what `gt town deployer serve` runs on: the flags' values
// and the seams, so a test drives every refusal with its own steps.
type townDeployerServeEnv struct {
	once    bool
	inbox   string
	answers string
	state   string
	steps   deployerinbox.Steps
	now     func() time.Time
	out     io.Writer
	// readFile reads a request file; nil is os.ReadFile. A test fails it, since a regular file
	// the inbox lists and the daemon cannot read is a race or a permission no unit test may make.
	readFile func(string) ([]byte, error)
}

// runTownDeployerServeIn is what `gt town deployer serve` runs. Every refusal
// comes before a step runs, so a refused run changes nothing.
func runTownDeployerServeIn(ctx context.Context, env townDeployerServeEnv) error {
	if !env.once {
		return errors.New("gt town deployer serve needs --once: it answers one request and exits, and no watch loop is built")
	}
	if env.steps == nil {
		return errors.New("gt town deployer serve has no plan, apply or rollback steps wired, so it answers no request")
	}
	done := filepath.Join(env.state, "done")
	info, err := os.Stat(done)
	if err != nil {
		return fmt.Errorf("gt town deployer serve needs the directory %s to move a request into: %w", done, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("gt town deployer serve needs %s to be a directory to move a request into", done)
	}

	// os.ReadDir returns the entries sorted by name, and a request's name is its UTC stamp, so
	// the first request-named regular file is the oldest. A dotfile, a directory or any name that
	// is not a request's is not one, and stays where it is.
	entries, err := os.ReadDir(env.inbox)
	if err != nil {
		return fmt.Errorf("reading the inbox %s: %w", env.inbox, err)
	}
	name := ""
	for _, e := range entries {
		if e.Type().IsRegular() && deployerinbox.IsRequestName(e.Name()) {
			name = e.Name()
			break
		}
	}
	if name == "" {
		if _, err := fmt.Fprintln(env.out, "deployer: no request in the inbox"); err != nil {
			return fmt.Errorf("printing the no-request line: %w", err)
		}
		return nil
	}
	path := filepath.Join(env.inbox, name)
	read := env.readFile
	if read == nil {
		read = os.ReadFile
	}
	data, err := read(path)
	if err != nil {
		return fmt.Errorf("reading the request %s: %w", path, err)
	}
	req, err := deployerinbox.ParseRequest(name, data)
	if err != nil {
		return fmt.Errorf("the oldest request is not valid, so it is left in the inbox: %w", err)
	}

	// An answer already published for this request means an earlier run answered it and its move
	// into done failed: running the steps again would plan or apply a second time and WriteAnswer
	// would refuse to publish over the first answer, so the request would be answered on every
	// run. Finish the move and run no step.
	answerPath := filepath.Join(env.answers, req.ID+".json")
	if _, err := os.Stat(answerPath); err == nil {
		if err := os.Rename(path, filepath.Join(done, name)); err != nil {
			return fmt.Errorf("moving the already answered request %s into %s: %w", path, done, err)
		}
		if _, err := fmt.Fprintf(env.out, "deployer: request %s was already answered at %s; moved to done\n", req.ID, answerPath); err != nil {
			return fmt.Errorf("the request %s was already answered and is moved, but printing that failed: %w", req.ID, err)
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("checking whether the request %s already has an answer at %s: %w", req.ID, answerPath, err)
	}

	answer := deployerinbox.Handle(ctx, env.steps, req, env.now())
	if err := deployerinbox.WriteAnswer(deployerinbox.OSAnswerFS{}, env.answers, answer); err != nil {
		return fmt.Errorf("writing the answer to the request %s: %w", req.ID, err)
	}
	if err := os.Rename(path, filepath.Join(done, name)); err != nil {
		return fmt.Errorf("moving the request %s into %s: %w", path, done, err)
	}
	if _, err := fmt.Fprintf(env.out, "id: %s\nstatus: %s\nanswer: %s\n", req.ID, answer.Status, filepath.Join(env.answers, req.ID+".json")); err != nil {
		return fmt.Errorf("the request %s is answered, but printing its status failed: %w", req.ID, err)
	}
	return nil
}
