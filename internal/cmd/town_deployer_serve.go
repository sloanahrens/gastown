package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/deployerinbox"
)

// serveSteps is the deployer's real plan, apply and rollback work. It stays nil
// until the real steps are wired (gt-1afrbd), and the serve command refuses to
// run without them, so no request is answered against no steps (gt-fqsb68).
var serveSteps deployerinbox.Steps

var townDeployerServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Answer one request in the deployer's inbox",
	Long: `Take the oldest request file in --inbox, run it through the deployer's
plan, apply and rollback steps, write its answer into --answers, and move the
request into <state>/done. The deployer's LaunchDaemon runs it on WatchPaths.

--once is required: the command answers one request and exits. The directory
<state>/done must exist before any step runs, so no request is answered with
no directory to move it into.

Examples:
  gt town deployer serve --once --as-user deployer --inbox ~deployer/.gt/deployer/inbox --answers ~deployer/.gt/deployer/answers --state ~deployer/.gt/deployer/state`,
	Args: cobra.NoArgs,
	RunE: runTownDeployerServe,
}

func init() {
	townDeployerCmd.AddCommand(townDeployerServeCmd)
	townDeployerServeCmd.Flags().Bool("once", false, "answer exactly one request, then exit (required)")
	townDeployerServeCmd.Flags().String("as-user", "", "the deployer user the command serves (required)")
	townDeployerServeCmd.Flags().String("inbox", "", "the deployer's inbox directory (required)")
	townDeployerServeCmd.Flags().String("answers", "", "the directory the answer is written into (required)")
	townDeployerServeCmd.Flags().String("state", "", "the state directory; the request moves into <state>/done (required)")
	for _, name := range []string{"as-user", "inbox", "answers", "state"} {
		_ = townDeployerServeCmd.MarkFlagRequired(name)
	}
}

// townDeployerServeEnv is what `gt town deployer serve` reads from its session:
// the flags and the seams, so a test drives every refusal with its own steps.
type townDeployerServeEnv struct {
	once    bool
	inbox   string
	answers string
	state   string
	steps   deployerinbox.Steps
	now     func() time.Time
	out     io.Writer
}

func runTownDeployerServe(cmd *cobra.Command, _ []string) error {
	once, err := cmd.Flags().GetBool("once")
	if err != nil {
		return fmt.Errorf("reading --once: %w", err)
	}
	inbox, err := cmd.Flags().GetString("inbox")
	if err != nil {
		return fmt.Errorf("reading --inbox: %w", err)
	}
	answers, err := cmd.Flags().GetString("answers")
	if err != nil {
		return fmt.Errorf("reading --answers: %w", err)
	}
	state, err := cmd.Flags().GetString("state")
	if err != nil {
		return fmt.Errorf("reading --state: %w", err)
	}
	return runTownDeployerServeIn(context.Background(), townDeployerServeEnv{
		once:    once,
		inbox:   inbox,
		answers: answers,
		state:   state,
		steps:   serveSteps,
		now:     time.Now,
		out:     cmd.OutOrStdout(),
	})
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

	// os.ReadDir returns the entries sorted by name, and a request's name is
	// its UTC stamp, so the first entry is the oldest.
	entries, err := os.ReadDir(env.inbox)
	if err != nil {
		return fmt.Errorf("reading the inbox %s: %w", env.inbox, err)
	}
	if len(entries) == 0 {
		return nil
	}
	name := entries[0].Name()
	path := filepath.Join(env.inbox, name)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading the request %s: %w", path, err)
	}
	req, err := deployerinbox.ParseRequest(name, data)
	if err != nil {
		return fmt.Errorf("the oldest request is not valid, so it is left in the inbox: %w", err)
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
