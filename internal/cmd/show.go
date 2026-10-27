package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
)

func init() {
	showCmd.GroupID = GroupWork
	rootCmd.AddCommand(showCmd)
}

var showCmd = &cobra.Command{
	Use:   "show <bead-id> [flags]",
	Short: "Show details of a bead",
	Long: `Displays the full details of a bead by ID.

Prints bd show's output - all bd show flags are supported.
Works with any bead prefix (gt-, bd-, hq-, etc.) and routes
to the correct beads database automatically.

Examples:
  gt show gt-abc123          # Show a gastown issue
  gt show hq-xyz789          # Show a town-level bead (mail, etc.)
  gt show bd-def456          # Show a beads issue
  gt show gt-abc123 --json   # Output as JSON
  gt show gt-abc123 -v       # Verbose output`,
	DisableFlagParsing: true, // Pass all flags through to bd show
	SilenceUsage:       true, // bd show prints no usage; a failure stays the message bd wrote
	RunE:               runShow,
}

func runShow(cmd *cobra.Command, args []string) error {
	// Handle --help since DisableFlagParsing bypasses Cobra's help handling
	if helped, err := checkHelpFlag(cmd, args); helped || err != nil {
		return err
	}

	if len(args) == 0 {
		return fmt.Errorf("bead ID required\n\nUsage: gt show <bead-id> [flags]")
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	return runShowWith(os.Stdout, args, beads.New(cwd).ShowText)
}

// runShowWith is runShow's body over the writer and the show read, so a test
// drives the call site where bd's failure becomes the command's exit code.
func runShowWith(out io.Writer, args []string, show func(string, []string) (string, error)) error {
	// Decision (gt-p85pw.10): bd now runs with its stdout piped to gt, not on
	// the operator's terminal, so it prints without a tty. Its stderr is in the
	// returned error, not shown directly.
	text, err := show(extractBeadIDFromArgs(args), args)
	return writeShowText(out, text, err)
}

// writeShowText prints bd show's text to out, then returns bd's error, so the
// failure reaches the operator as the command's error (gt-p85pw.10).
func writeShowText(out io.Writer, text string, showErr error) error {
	if text != "" {
		if _, writeErr := io.WriteString(out, text); writeErr != nil {
			return writeErr
		}
	}
	return showErr
}

// extractBeadIDFromArgs returns the first positional bead ID, falling back to
// bd show's --id form for IDs that look like flags. bd show processes positional
// IDs before --id values even when --id appears earlier in argv.
func extractBeadIDFromArgs(args []string) string {
	idFlag := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 < len(args) {
				return args[i+1]
			}
			break
		}
		if strings.HasPrefix(arg, "--id=") {
			if idFlag == "" {
				idFlag = strings.TrimPrefix(arg, "--id=")
			}
			continue
		}
		if arg == "--id" {
			if i+1 < len(args) {
				if idFlag == "" {
					idFlag = args[i+1]
				}
				i++
			}
			continue
		}
		if showFlagConsumesNextArg(arg) {
			i++
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			return arg
		}
	}
	return idFlag
}

func showFlagConsumesNextArg(arg string) bool {
	switch arg {
	case "--as-of", "--actor", "--db", "--directory", "--dolt-auto-commit", "--format", "-C":
		return true
	default:
		return false
	}
}
