package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
)

var beadCmd = &cobra.Command{
	Use:     "bead",
	Aliases: []string{"bd"},
	GroupID: GroupWork,
	Short:   "Bead management utilities",
	Long: `Utilities for managing beads across repositories.

Provides operations that span multiple beads repositories, such as
moving beads between repos and viewing beads by ID with automatic
prefix-based routing.

Subcommands:
  move    Move a bead from one repository to another
  show    Show details of a bead (routes by prefix)
  read    Alias for show
  probe   Create an ephemeral bead for a one-off test/routing question`,
}

var beadMoveCmd = &cobra.Command{
	Use:   "move <bead-id> <target-prefix>",
	Short: "Move a bead to a different repository",
	Long: `Move a bead from one repository to another.

This creates a copy of the bead in the target repository (with the new prefix)
and closes the source bead with a reference to the new location.

The target prefix determines which repository receives the bead.
Common prefixes: gt- (gastown), bd- (beads), hq- (headquarters)

Examples:
  gt bead move gt-abc123 bd-     # Move gt-abc123 to beads repo as bd-*
  gt bead move hq-xyz bd-        # Move hq-xyz to beads repo
  gt bead move bd-123 gt-        # Move bd-123 to gastown repo`,
	Args: cobra.ExactArgs(2),
	RunE: runBeadMove,
}

var beadMoveDryRun bool

var beadShowCmd = &cobra.Command{
	Use:   "show <bead-id> [flags]",
	Short: "Show details of a bead",
	Long: `Displays the full details of a bead by ID.

This is an alias for 'gt show'. All bd show flags are supported.

Examples:
  gt bead show gt-abc123          # Show a gastown issue
  gt bead show hq-xyz789          # Show a town-level bead
  gt bead show bd-def456          # Show a beads issue
  gt bead show gt-abc123 --json   # Output as JSON`,
	DisableFlagParsing: true, // Pass all flags through to bd show
	RunE: func(cmd *cobra.Command, args []string) error {
		return runShow(cmd, args)
	},
}

var beadReadCmd = &cobra.Command{
	Use:   "read <bead-id> [flags]",
	Short: "Show details of a bead (alias for 'show')",
	Long: `Displays the full details of a bead by ID.

This is an alias for 'gt bead show'. All bd show flags are supported.

Examples:
  gt bead read gt-abc123          # Show a gastown issue
  gt bead read hq-xyz789          # Show a town-level bead
  gt bead read bd-def456          # Show a beads issue
  gt bead read gt-abc123 --json   # Output as JSON`,
	DisableFlagParsing: true, // Pass all flags through to bd show
	RunE: func(cmd *cobra.Command, args []string) error {
		return runShow(cmd, args)
	},
}

var beadProbeDescription string

var beadProbeCmd = &cobra.Command{
	Use:   "probe <title>",
	Short: "Create an ephemeral bead for a one-off test/routing question",
	Long: `Creates an ephemeral bead for investigating a one-off question (e.g.
"does --repo route to the rig I expect?") without leaving behind a bead that
outlives its purpose.

This is the fix for gt-eje7: two routing-probe beads (om-hoc, be-h2d) were
left open as type=bug at P2 after the question they existed to answer had
already been settled. Every field a dispatcher sorts on said "real work";
only the title said otherwise, and a title is the field most likely to be
skimmed.

A probe bead created here is structurally non-dispatchable on two
independent axes, because either alone reproduces the bug:
  - ephemeral (wisp-type=probe): invisible to 'bd list'/'bd ready' while
    open, and reaped on a 1h TTL by 'gt compact' if forgotten.
  - type=chore, priority=4: even if it survives past that TTL and gets
    promoted to a permanent bead, it reads as low-priority housekeeping,
    never a dispatchable bug.

Close it (bd close <id>) as soon as the question is answered — in the same
investigation, not as a follow-up.

Examples:
  gt bead probe "TEST-ROUTING-PROBE: does --repo route to gastown?"
  gt bead probe Does bd query find beads by label? --description "checking gt-abc"`,
	Args: cobra.MinimumNArgs(1),
	RunE: runBeadProbe,
}

func runBeadProbe(cmd *cobra.Command, args []string) error {
	title := strings.Join(args, " ")

	workDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working dir: %w", err)
	}

	bd := beads.New(workDir)
	issue, err := bd.CreateProbeBead(title, beadProbeDescription)
	if err != nil {
		return fmt.Errorf("creating probe bead: %w", err)
	}

	fmt.Printf("%s Created probe %s\n", style.Bold.Render("✓"), issue.ID)
	fmt.Printf("  Close it as soon as you have your answer: bd close %s\n", issue.ID)

	return nil
}

func init() {
	beadMoveCmd.Flags().BoolVarP(&beadMoveDryRun, "dry-run", "n", false, "Show what would be done")
	beadProbeCmd.Flags().StringVar(&beadProbeDescription, "description", "", "Optional description (what question this probe answers)")
	beadCmd.AddCommand(beadMoveCmd)
	beadCmd.AddCommand(beadShowCmd)
	beadCmd.AddCommand(beadReadCmd)
	beadCmd.AddCommand(beadProbeCmd)
	rootCmd.AddCommand(beadCmd)
}

// moveBeadInfo holds the essential fields we need to copy when moving beads
type moveBeadInfo struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Type        string   `json:"issue_type"`
	Priority    int      `json:"priority"`
	Description string   `json:"description"`
	Labels      []string `json:"labels"`
	Assignee    string   `json:"assignee"`
	Status      string   `json:"status"`
}

func runBeadMove(cmd *cobra.Command, args []string) error {
	sourceID := args[0]
	targetPrefix := args[1]

	// Normalize prefix (ensure it ends with -)
	if !strings.HasSuffix(targetPrefix, "-") {
		targetPrefix = targetPrefix + "-"
	}

	// Get source bead details — resolve rig directory from prefix so that
	// rig-prefixed beads are found in their rig database (GH#2126).
	output, err := BdCmd("show", sourceID, "--json").
		Dir(resolveBeadDir(sourceID)).
		StripBeadsDir().
		Output()
	if err != nil {
		return fmt.Errorf("getting bead %s: %w", sourceID, err)
	}

	// bd show --json returns an array
	var sources []moveBeadInfo
	if err := json.Unmarshal(output, &sources); err != nil {
		return fmt.Errorf("parsing bead data: %w", err)
	}
	if len(sources) == 0 {
		return fmt.Errorf("bead %s not found", sourceID)
	}
	source := sources[0]

	// Don't move closed beads
	if source.Status == "closed" {
		return fmt.Errorf("cannot move closed bead %s", sourceID)
	}

	fmt.Printf("%s Moving %s to %s...\n", style.Bold.Render("→"), sourceID, targetPrefix)
	fmt.Printf("  Title: %s\n", source.Title)
	fmt.Printf("  Type: %s\n", source.Type)

	// Guard against flag-like titles propagating during move (gt-e0kx5)
	if beads.IsFlagLikeTitle(source.Title) {
		return fmt.Errorf("refusing to move bead: title %q looks like a CLI flag", source.Title)
	}

	if beadMoveDryRun {
		fmt.Printf("\nDry run - would:\n")
		fmt.Printf("  1. Create new bead with prefix %s\n", targetPrefix)
		fmt.Printf("  2. Close %s with reference to new bead\n", sourceID)
		return nil
	}

	// Build create command for target.
	// Skip --prefix for empty or bare "-" (normalization above turns "" into "-").
	createArgs := []string{"create"}
	if targetPrefix != "" && targetPrefix != "-" {
		createArgs = append(createArgs, "--prefix", targetPrefix)
	}
	createArgs = append(createArgs,
		"--title="+source.Title,
		"--type", source.Type,
		"--priority", fmt.Sprintf("%d", source.Priority),
		"--silent", // Only output the ID
	)

	if source.Description != "" {
		createArgs = append(createArgs, "--description", source.Description)
	}
	if source.Assignee != "" {
		createArgs = append(createArgs, "--assignee", source.Assignee)
	}
	for _, label := range source.Labels {
		createArgs = append(createArgs, "--label", label)
	}

	// Create the new bead
	createCmd := beads.CommandWithEnv("", os.Environ(), createArgs...)
	createCmd.Stderr = os.Stderr
	newIDBytes, err := createCmd.Output()
	if err != nil {
		return fmt.Errorf("creating new bead: %w", err)
	}
	newID := strings.TrimSpace(string(newIDBytes))

	fmt.Printf("%s Created %s\n", style.Bold.Render("✓"), newID)

	// Close the source bead with reference
	closeReason := fmt.Sprintf("Moved to %s", newID)
	closeCmd := beads.CommandWithEnv("", os.Environ(), "close", sourceID, "--reason", closeReason)
	closeCmd.Stderr = os.Stderr
	if err := closeCmd.Run(); err != nil {
		// Clean up the new bead since we couldn't close the source
		fmt.Fprintf(os.Stderr, "Warning: failed to close source bead: %v\n", err)
		cleanupCmd := beads.CommandWithEnv("", os.Environ(), "close", newID, "--reason", "Cleanup: source bead close failed during move")
		if cleanupErr := cleanupCmd.Run(); cleanupErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: also failed to clean up new bead %s: %v\n", newID, cleanupErr)
			fmt.Fprintf(os.Stderr, "Both %s and %s remain open - manual cleanup needed\n", sourceID, newID)
		} else {
			fmt.Fprintf(os.Stderr, "Cleaned up new bead %s\n", newID)
		}
		return err
	}

	fmt.Printf("%s Closed %s (moved to %s)\n", style.Bold.Render("✓"), sourceID, newID)
	fmt.Printf("\nBead moved: %s → %s\n", sourceID, newID)

	return nil
}
