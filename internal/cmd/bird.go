package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/bird"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/workspace"
)

func init() {
	birdCmd.GroupID = GroupAgents
	rootCmd.AddCommand(birdCmd)
	birdCmd.AddCommand(birdStartCmd, birdBindCmd, birdListCmd)
}

var birdCmd = &cobra.Command{
	Use:   "bird",
	Short: "Run claude sessions under bird names",
	Long: `Run claude under a bird name and keep track of which session is which.

A bird is a claude session with a name, so a session started for a bead can be
found and resumed later. The names come from gt's own pool, and the sessions
and the beads they run are recorded in the town's .runtime/agents/state.json.`,
	RunE: requireSubcommand,
}

var birdStartCmd = &cobra.Command{
	Use:   "start [claude args...]",
	Short: "Start claude under a bird name",
	Long: `Replace this process with the real claude, under a bird name when the
invocation is a session.

The arguments are claude's: --name names the session, --resume resumes one, and
a prompt is what it is asked. gt reads --bead to reuse the name that bead is
bound to, or to take a free one, and labels the bead agent:<name>; --bead is gt's
own and is never passed on to claude. When the invocation is not a session (a
print run, a version, help, a resumed session that is not a bird, or input or
output that is not a terminal) claude runs with the caller's other arguments
unchanged.

Examples:
  gt bird start                   # a session under a free bird name
  gt bird start --bead gt-abc     # under the name gt-abc is bound to
  gt bird start --name robin      # under robin, if no session runs it

The real claude is the town's bird_claude setting, or ~/.local/bin/claude when
the town sets none. --help is claude's, so an alias of claude keeps working;
'gt help bird start' prints this help.`,
	DisableFlagParsing: true,
	SilenceUsage:       true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return birdLeaf(processBirdSetup(), func(r *bird.Runner) error { return runBirdStart(r, args) })
	},
}

var birdBindCmd = &cobra.Command{
	Use:   "bind <bead>",
	Short: "Bind this session's bird name to a bead",
	Long: `Record that this session's bird name runs the bead.

The name is CLAUDE_AGENT_NAME, which gt bird start sets. A bead that last ran
under another name is reported, since this session is the one that resumes it.`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return birdFailure(&bird.UsageError{Arg: "gt bird bind", Wants: "exactly one bead id"})
		}
		return birdLeaf(processBirdSetup(), func(r *bird.Runner) error { return runBirdBind(r, args[0]) })
	},
}

var birdListCmd = &cobra.Command{
	Use:   "list",
	Short: "List the live birds",
	Long: `Print every bird whose process is still running, with the bead it runs and
the directory it runs in, dropping the records of the sessions that have
exited.`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) != 0 {
			return birdFailure(&bird.UsageError{Arg: "gt bird list", Wants: "no arguments"})
		}
		return birdLeaf(processBirdSetup(), func(r *bird.Runner) error { return runBirdList(r, cmd.OutOrStdout()) })
	},
}

// runBirdStart is the start leaf over the runner, so a test drives the call
// site where a bird error becomes the command's exit status.
func runBirdStart(r *bird.Runner, args []string) error {
	return birdFailure(r.Start(args))
}

// runBirdBind is bind's body over the runner.
func runBirdBind(r *bird.Runner, bead string) error {
	return birdFailure(r.Bind(bead))
}

// runBirdList is list's body over the runner.
func runBirdList(r *bird.Runner, w io.Writer) error {
	return birdFailure(r.List(w))
}

// exitStatus is an error that carries the status the process exits with.
type exitStatus interface{ ExitStatus() int }

// birdFailure gives a bird error its own exit status, so gt bird start fails
// with 127 for a claude that is not there and 2 for an argument it refuses, as
// the shell command it replaces did.
func birdFailure(err error) error {
	var coded exitStatus
	if errors.As(err, &coded) {
		return &ExitCodeError{Code: coded.ExitStatus(), Err: err}
	}
	return err
}

// birdSetup is what a gt bird command reads from the process: the operator's
// home, the town root, the terminal check, the claude path and the bd label
// writer. Every read is a field, so a test drives the failures without a town
// on this machine.
type birdSetup struct {
	home    func() (string, error)
	find    func() (string, error)
	guard   func(what, dir string) error
	isTTY   func(*os.File) bool
	claude  func(home, townRoot string) (string, error)
	label   func(dir, bead, label string) error
	unlabel func(dir, bead, label string) error
}

// processBirdSetup is what the gt bird commands read from this process.
func processBirdSetup() birdSetup {
	return birdSetup{
		home:   os.UserHomeDir,
		find:   workspace.FindFromCwdOrError,
		guard:  workspace.RefuseForbiddenRoot,
		isTTY:  func(f *os.File) bool { return term.IsTerminal(int(f.Fd())) },
		claude: birdClaudePath,
		label: func(dir, bead, label string) error {
			return beads.New(dir).Update(bead, beads.UpdateOptions{AddLabels: []string{label}})
		},
		unlabel: func(dir, bead, label string) error {
			return beads.New(dir).Update(bead, beads.UpdateOptions{RemoveLabels: []string{label}})
		},
	}
}

// birdLeaf runs op with the runner the setup builds.
func birdLeaf(s birdSetup, op func(*bird.Runner) error) error {
	r, err := s.runner()
	if err != nil {
		return birdFailure(err)
	}
	return op(r)
}

// runner is the bird runner the commands drive: the town's registry, the
// operator's home and the real claude.
func (s birdSetup) runner() (*bird.Runner, error) {
	home, err := s.home()
	if err != nil {
		return nil, fmt.Errorf("gt bird: finding the home directory: %w", err)
	}
	townRoot, err := s.townRoot(home)
	if err != nil {
		return nil, err
	}
	claude, err := s.claude(home, townRoot)
	if err != nil {
		return nil, err
	}
	return &bird.Runner{
		Registry:    bird.New(townRoot),
		Claude:      claude,
		Home:        home,
		Terminal:    bird.Terminal{Stdin: s.isTTY(os.Stdin), Stdout: s.isTTY(os.Stdout)},
		LabelBead:   s.label,
		UnlabelBead: s.unlabel,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
	}, nil
}

// townRoot is the town whose .runtime/agents/state.json records the birds: the
// one this directory is in, else ~/gt, which is where a start outside any town
// still agrees with the sessions inside one (gt-ed525). The guard refuses a
// root the hermetic harness marked as the operator's live town (gt-dr664).
func (s birdSetup) townRoot(home string) (string, error) {
	root, err := s.find()
	if err != nil {
		root = filepath.Join(home, "gt")
	}
	if err := s.guard("gt bird", root); err != nil {
		return "", err
	}
	return root, nil
}

// birdClaudePath is the real claude a start execs: the town's bird_claude
// setting, with a leading "~/" read from home, else ~/.local/bin/claude
// (gt-qipl5, gt-08chr). A settings file that cannot be read is an error, not
// the default, so a start never execs a claude the town did not name.
func birdClaudePath(home, townRoot string) (string, error) {
	settings, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
	if err != nil {
		return "", fmt.Errorf("gt bird: reading bird_claude from the town settings: %w", err)
	}
	configured := strings.TrimSpace(settings.BirdClaude)
	switch {
	case configured == "":
		return filepath.Join(home, ".local", "bin", "claude"), nil
	case configured == "~" || strings.HasPrefix(configured, "~/"):
		return filepath.Join(home, strings.TrimPrefix(configured, "~")), nil
	case !filepath.IsAbs(configured):
		return "", fmt.Errorf("gt bird: bird_claude %q is neither an absolute path nor under ~/", configured)
	}
	return configured, nil
}
