package cmd

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/formula"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/version"
	"github.com/steveyegge/gastown/internal/workspace"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// Formula command flags
var (
	formulaListJSON   bool
	formulaShowJSON   bool
	formulaShowRaw    bool
	formulaRunPR      int
	formulaRunRig     string
	formulaRunDryRun  bool
	formulaRunAgent   string
	formulaRunFiles   []string
	formulaRunSet     []string
	formulaCreateType string

	formulaSyncDryRun bool
	formulaSyncJSON   bool
)

var formulaCmd = &cobra.Command{
	Use:     "formula",
	Aliases: []string{"formulas"},
	GroupID: GroupWork,
	Short:   "Manage workflow formulas",
	RunE:    requireSubcommand,
	Long: `Manage workflow formulas - reusable molecule templates.

Formulas are TOML/JSON files that define workflows with steps, variables,
and composition rules. They can be "poured" to create molecules or "wisped"
for ephemeral patrol cycles.

Commands:
  list    List available formulas from all search paths
  show    Display formula details (steps, variables, composition)
  run     Execute a formula (pour and dispatch)
  create  Create a new formula template

Search paths (in order):
  1. .beads/formulas/ (project)
  2. ~/.beads/formulas/ (user)
  3. $GT_ROOT/.beads/formulas/ (orchestrator)

Examples:
  gt formula list                    # List all formulas
  gt formula show shiny              # Show formula details
  gt formula run shiny --pr=123      # Run formula on PR #123
  gt formula create my-workflow      # Create new formula template`,
}

var formulaListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available formulas",
	Long: `List available formulas from all search paths.

Searches for formula files (.formula.toml, .formula.json) in:
  1. .beads/formulas/ (project)
  2. ~/.beads/formulas/ (user)
  3. $GT_ROOT/.beads/formulas/ (orchestrator)

Examples:
  gt formula list            # List all formulas
  gt formula list --json     # JSON output`,
	RunE: runFormulaList,
}

var formulaShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Display formula details",
	Long: `Display detailed information about a formula.

Shows:
  - Formula metadata (name, type, description)
  - Variables with defaults and constraints
  - Steps with dependencies
  - Composition rules (extends, aspects)

The formula is shown as bd cooks it: every inherited step, with overrides,
expansions and the town overlay applied, as gt prime renders it. --raw shows
the file as written.

Examples:
  gt formula show shiny
  gt formula show mol-doc-audit --raw
  gt formula show rule-of-five --json`,
	Args: cobra.ExactArgs(1),
	RunE: runFormulaShow,
}

var formulaRunCmd = &cobra.Command{
	Use:   "run [name]",
	Short: "Execute a formula",
	Long: `Execute a formula by pouring it and dispatching work.

This command:
  1. Looks up the formula by name (or uses default from rig config)
  2. Pours it to create a molecule (or uses existing proto)
  3. Dispatches the molecule to available workers

For PR-based workflows, use --pr to specify the GitHub PR number.

If no formula name is provided, uses the default formula configured in
the rig's settings/config.json under workflow.default_formula.

Options:
  --pr=N        Run formula on GitHub PR #N
  --rig=NAME    Target specific rig (default: inferred from cwd, or sole registered rig)
  --agent=ALIAS Override agent for all legs (e.g., claude-haiku)
  --dry-run     Show what would happen without executing

bd cooks the formula with the --set pairs as its vars. In a convoy formula
the step with metadata.convoy = "synthesis" is the synthesis and every other
step is a leg (metadata.focus, .agent, .review_only); the base_prompt,
output_* and review_only vars set the leg prompt, output files and
analysis-only legs. A workflow step's metadata.target names where it is slung
and metadata.interactive keeps it in the current session.

Agent precedence (highest to lowest):
  1. Per-leg metadata.agent in the formula
  2. --agent CLI flag
  3. Rig/town default agent (fallback)

There is no formula-level agent: bd's strict decode rejects a top-level
agent key, so set metadata.agent per leg or pass --agent (it applies to
workflow steps too).

Examples:
  gt formula run shiny                    # Run formula in current rig
  gt formula run                          # Run default formula from rig config
  gt formula run shiny --pr=123           # Run on PR #123
  gt formula run security-audit --rig=beads  # Run in specific rig
  gt formula run release --dry-run        # Preview execution
  gt formula run code-review --agent=claude-haiku  # All legs use claude-haiku`,
	Args: cobra.MaximumNArgs(1),
	RunE: runFormulaRun,
}

var formulaSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync on-disk formulas with the formulas embedded in this gt binary",
	Long: `Sync updates the town's on-disk formulas ($GT_ROOT/.beads/formulas/) to
match the formulas embedded in this gt binary.

Delivery is two-staged and this is only the second stage: a formula fix merged
to origin/main reaches the town only after gt is rebuilt (the fix ships inside
the binary) and this command runs. Sync therefore delivers the formulas of the
binary that runs it, and reports the commit they came from plus any formula
files that changed on the build branch since, so a "synced" line is never read
as "current".

The binary is canonical. Each town copy is checked by content hash against
the embedded formula and the hash gt recorded when it last wrote the file
(.beads/formulas/.installed.json). A copy whose hash gt never wrote was edited
or copied by hand: sync replaces it and names it. Customize through an overlay
(gt formula overlay --help) or change the formula in gastown source.

Files the binary does not embed are never touched, only reported: copies of
formulas deleted from source, hand-written formulas, and *.bak copies. Each
is promoted into gastown source or deleted by an operator.

Examples:
  gt formula sync                 # Sync $GT_ROOT/.beads/formulas from this binary
  gt formula sync --dry-run       # Report what a sync would do, write nothing
  gt formula sync --json          # Machine-readable report`,
	Args: cobra.NoArgs,
	RunE: runFormulaSync,
}

var formulaCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new formula template",
	Long: `Create a new formula template file.

Creates a starter formula file in .beads/formulas/ with the given name.
The template includes common sections that you can customize.

Formula types:
  task      Single-step task formula (default)
  workflow  Multi-step workflow with dependencies
  patrol    Repeating patrol cycle (for wisps)

Examples:
  gt formula create my-task                  # Create task formula
  gt formula create my-workflow --type=workflow
  gt formula create nightly-check --type=patrol`,
	Args: cobra.ExactArgs(1),
	RunE: runFormulaCreate,
}

func init() {
	// List flags
	formulaListCmd.Flags().BoolVar(&formulaListJSON, "json", false, "Output as JSON")

	// Show flags
	formulaShowCmd.Flags().BoolVar(&formulaShowJSON, "json", false, "Output as JSON")
	formulaShowCmd.Flags().BoolVar(&formulaShowRaw, "raw", false, "Show the formula as written, without resolving extends and compose")

	// Run flags
	formulaRunCmd.Flags().IntVar(&formulaRunPR, "pr", 0, "GitHub PR number to run formula on")
	formulaRunCmd.Flags().StringVar(&formulaRunRig, "rig", "", "Target rig (default: inferred from cwd, or sole registered rig)")
	formulaRunCmd.Flags().BoolVar(&formulaRunDryRun, "dry-run", false, "Preview execution without running")
	formulaRunCmd.Flags().StringVar(&formulaRunAgent, "agent", "", "Override agent for all legs (e.g., claude-haiku)")
	formulaRunCmd.Flags().StringSliceVar(&formulaRunFiles, "files", nil, "Files to pass to formula legs (available as {{.files}} in templates)")
	formulaRunCmd.Flags().StringSliceVar(&formulaRunSet, "set", nil, "Set input variables as key=value pairs (available as {{.key}} in templates)")

	// Sync flags
	formulaSyncCmd.Flags().BoolVar(&formulaSyncDryRun, "dry-run", false, "Report what a sync would do without writing anything")
	formulaSyncCmd.Flags().BoolVar(&formulaSyncJSON, "json", false, "Output as JSON")

	// Create flags
	formulaCreateCmd.Flags().StringVar(&formulaCreateType, "type", "task", "Formula type: task, workflow, or patrol")

	// Add subcommands
	formulaCmd.AddCommand(formulaListCmd)
	formulaCmd.AddCommand(formulaShowCmd)
	formulaCmd.AddCommand(formulaRunCmd)
	formulaCmd.AddCommand(formulaSyncCmd)
	formulaCmd.AddCommand(formulaCreateCmd)

	rootCmd.AddCommand(formulaCmd)
}

// runFormulaList delegates to bd formula list
func runFormulaList(cmd *cobra.Command, args []string) error {
	bdArgs := []string{"formula", "list"}
	if formulaListJSON {
		bdArgs = append(bdArgs, "--json")
	}

	return passBdFormulaOutput(bdArgs, formulaListJSON)
}

// runFormulaShow shows a formula as bd cooks it (the steps an agent runs,
// inherited and expanded ones included); bd formula show lists only the file.
// --raw delegates to bd formula show.
func runFormulaShow(cmd *cobra.Command, args []string) error {
	formulaName := args[0]
	if !formulaShowRaw {
		townRoot, rigName := formulaShowScope()
		f, err := realFormulaCooker().cookForRender(formulaName, townRoot, rigName, nil)
		if err != nil {
			return err
		}
		if formulaShowJSON {
			return writeCookedFormulaJSON(os.Stdout, f)
		}
		renderCookedFormula(os.Stdout, f)
		return nil
	}
	bdArgs := []string{"formula", "show", formulaName}
	if formulaShowJSON {
		bdArgs = append(bdArgs, "--json")
	}

	return passBdFormulaOutput(bdArgs, formulaShowJSON)
}

// passBdFormulaOutput prints bd's answer for the operator. With asJSON the
// caller gets the --json payload (the envelope's data); without it bd's own
// prose, which machine mode replaces with the envelope, so that run opts out.
func passBdFormulaOutput(bdArgs []string, asJSON bool) error {
	// Keep-raw (gt-7iwy0.4.1): bd formula list/show prints for the operator,
	// prose and --json payload alike, and the caller opts out of machine mode
	// itself; no typed Client method expresses that passthrough.
	bdCmd := beads.CommandWithEnv("", nil, bdArgs...)
	bdCmd.Stderr = os.Stderr
	if !asJSON {
		bdCmd.Env = beads.WithoutMachineEnv(bdCmd.Env)
		bdCmd.Stdout = os.Stdout
		return bdCmd.Run()
	}

	// A buffer, not os.Stdout, so beads.Cmd unwraps the envelope. A failure
	// leaves its envelope in the buffer; bd's message is already on stderr.
	var stdout bytes.Buffer
	bdCmd.Stdout = &stdout
	if err := bdCmd.Run(); err != nil {
		return err
	}
	_, err := os.Stdout.Write(stdout.Bytes())
	return err
}

// runFormulaRun executes a formula by spawning a convoy of polecats.
// For convoy-type formulas, it creates a convoy bead, creates leg beads,
// and slings each leg to a separate polecat with leg-specific prompts.
func runFormulaRun(cmd *cobra.Command, args []string) error {
	// Determine target rig first (needed for default formula lookup)
	targetRig := formulaRunRig
	var rigPath string
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		townRoot = ""
	}
	if targetRig == "" {
		// Try to detect from current directory
		if townRoot != "" {
			rigName, r, rigErr := findCurrentRig(townRoot)
			if rigErr == nil && rigName != "" {
				targetRig = rigName
				if r != nil {
					rigPath = r.Path
				}
			}
			// Still no rig — auto-select when there is exactly one registered rig,
			// otherwise surface a helpful error (e.g. Deacon at HQ level on
			// non-default installs where "gastown" rig does not exist).
			if targetRig == "" {
				name, path, inferErr := autoInferRig(townRoot)
				if inferErr != nil {
					return inferErr
				}
				targetRig = name
				rigPath = path
			}
		} else {
			// No town root found, cannot determine target rig
			return fmt.Errorf("cannot determine target rig: not in a Gas Town workspace; use --rig=NAME")
		}
	} else if townRoot != "" {
		// If rig specified, construct path
		rigPath = filepath.Join(townRoot, targetRig)
	}

	// Get formula name from args or default
	var formulaName string
	if len(args) > 0 {
		formulaName = args[0]
	} else {
		// Try to get default formula from rig config
		if rigPath != "" {
			formulaName = config.GetDefaultFormula(rigPath)
		}
		if formulaName == "" {
			return fmt.Errorf("no formula specified and no default formula configured\n\nTo set a default formula, add to your rig's settings/config.json:\n  \"workflow\": {\n    \"default_formula\": \"<formula-name>\"\n  }")
		}
		fmt.Printf("%s Using default formula: %s\n", style.Dim.Render("Note:"), formulaName)
	}

	// bd cooks the formula where pour would, with the --set vars substituted
	// (gt-fd2cu.1.1): gastown parses no formula itself.
	f, err := realFormulaCooker().cookForRender(formulaName, townRoot, targetRig, formulaRunSet)
	if err != nil {
		return err
	}

	// Handle dry-run mode
	if formulaRunDryRun {
		return dryRunFormula(f, formulaName, targetRig)
	}

	switch f.Type {
	case "convoy":
		return executeConvoyFormula(f, formulaName, targetRig)
	case "workflow":
		return executeWorkflowFormula(f, formulaName, targetRig)
	default:
		fmt.Printf("%s Formula type '%s' not yet supported for execution.\n",
			style.Dim.Render("Note:"), f.Type)
		fmt.Printf("Currently only 'convoy' and 'workflow' formulas can be run.\n")
		fmt.Printf("\nTo run '%s' manually:\n", formulaName)
		fmt.Printf("  1. View formula:   gt formula show %s\n", formulaName)
		fmt.Printf("  2. Sling to rig:   gt sling %s %s  (cooks it and pours a wisp)\n", formulaName, targetRig)
		return nil
	}
}

// runFormulaSync syncs the town's on-disk formulas with the embedded set.
func runFormulaSync(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	msg, err := formulaSyncMessage(townRoot)
	if err != nil {
		return fmt.Errorf("syncing formulas: %w", err)
	}
	// The summary carries its own trailing newline; --json must not get one added.
	fmt.Print(msg)
	return nil
}

// formulaSyncMessage runs the formula sync against townRoot and formats a
// human-readable summary. Split out from runFormulaSync so the summary logic is
// testable without a real workspace.
func formulaSyncMessage(townRoot string) (string, error) {
	report, err := buildFormulaSyncReport(townRoot, formula.SyncOptions{
		DryRun: formulaSyncDryRun,
	})
	if err != nil {
		return "", err
	}
	if formulaSyncJSON {
		out, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return "", err
		}
		return string(out), nil
	}
	return formatFormulaSyncReport(report), nil
}

// formulaSyncReport is the whole of what a sync found and did. It exists as a
// struct so --json and the human summary can never disagree about the outcome.
type formulaSyncReport struct {
	DryRun      bool     `json:"dry_run"`
	Installed   []string `json:"installed"`
	Updated     []string `json:"updated"`
	Reinstalled []string `json:"reinstalled"`
	UpToDate    int      `json:"up_to_date"`

	// ReplacedDrift names town copies whose hash gt never wrote (hand-edited or
	// hand-copied); the binary is canonical, so sync replaced (or would replace)
	// each with the embedded formula.
	ReplacedDrift []string `json:"replaced_drift"`

	// Orphaned names town copies gt wrote that this binary no longer embeds;
	// sync leaves them on disk, where gt formula list still finds them.
	Orphaned []string `json:"orphaned_not_in_binary"`
	// Unowned names files in the formulas dir gt never wrote and the binary
	// does not embed: hand-written formulas, *.bak copies, backup directories.
	Unowned []string `json:"unowned_not_in_source"`

	BinaryVersion string   `json:"binary_version"`
	BinaryCommit  string   `json:"binary_commit"`
	BuiltAt       string   `json:"built_at,omitempty"`
	CompareRef    string   `json:"binary_compare_ref,omitempty"`
	CommitsBehind int      `json:"binary_commits_behind"`
	DriftFiles    []string `json:"formula_files_changed_on_build_branch,omitempty"`
	DriftChecked  bool     `json:"formula_drift_checked"`
	DriftReason   string   `json:"formula_drift_unchecked_reason,omitempty"`
}

// buildFormulaSyncReport runs the sync (or its dry run) and collects the
// embedded-content provenance a "synced" line cannot stand without.
func buildFormulaSyncReport(townRoot string, opts formula.SyncOptions) (*formulaSyncReport, error) {
	plan, err := formula.SyncFormulas(townRoot, opts)
	if err != nil {
		return nil, err
	}

	report := &formulaSyncReport{
		DryRun:        opts.DryRun,
		Installed:     emptyIfNil(plan.Installed()),
		Updated:       emptyIfNil(plan.Updated()),
		Reinstalled:   emptyIfNil(plan.Reinstalled()),
		UpToDate:      plan.UpToDate(),
		ReplacedDrift: emptyIfNil(plan.ReplacedDrift()),
		Orphaned:      emptyIfNil(plan.Orphaned()),
		Unowned:       emptyIfNil(plan.Unowned()),
		BinaryVersion: Version,
		BinaryCommit:  version.ShortCommit(Commit),
		BuiltAt:       BuildTime,
	}

	fillFormulaDrift(report, townRoot)
	return report, nil
}

// fillFormulaDrift records whether formula fixes merged on the build branch are
// missing from this binary. A sync cannot deliver those — only a rebuild can —
// so the report has to say it or "synced" reads as "current" (gt-dt7r).
//
// The checkout is resolved from townRoot rather than the ambient environment:
// drift is a property of the town being synced, and the alternative resolves
// (and, to report freshness, fetches) whichever source checkout the process
// happens to be near (gt-vxjz).
func fillFormulaDrift(report *formulaSyncReport, townRoot string) {
	repoDir, err := version.GetRepoRootForTown(townRoot)
	if err != nil {
		// No source checkout: drift is unknowable, not absent.
		report.DriftReason = err.Error()
		return
	}
	drift := version.CheckEmbeddedFormulaDrift(repoDir)
	report.DriftChecked = drift.Checked
	report.DriftFiles = drift.Files
	report.CompareRef = drift.CompareRef
	report.CommitsBehind = drift.CommitsBehind
	if !drift.Checked {
		report.DriftReason = drift.Reason
	}
}

// emptyIfNil keeps --json field types stable: a caller parsing this output gets
// [] for "none" rather than null.
func emptyIfNil(names []string) []string {
	if names == nil {
		return []string{}
	}
	return names
}

// formatFormulaSyncReport renders the human summary. Replaced and unowned files
// are the reason this is not a one-line command: each one is a formula the town
// ran that gastown source did not ship.
func formatFormulaSyncReport(r *formulaSyncReport) string {
	var b strings.Builder

	var parts []string
	if n := len(r.Installed); n > 0 {
		parts = append(parts, fmt.Sprintf("%d installed", n))
	}
	if n := len(r.Updated); n > 0 {
		parts = append(parts, fmt.Sprintf("%d updated", n))
	}
	if n := len(r.Reinstalled); n > 0 {
		parts = append(parts, fmt.Sprintf("%d reinstalled", n))
	}
	if n := len(r.ReplacedDrift); n > 0 {
		parts = append(parts, fmt.Sprintf("%d drifted copies replaced", n))
	}
	parts = append(parts, fmt.Sprintf("%d already current", r.UpToDate))

	switch {
	case r.DryRun:
		fmt.Fprintf(&b, "%s formulas sync would write: %s\n", style.Dim.Render("[dry-run]"), strings.Join(parts, ", "))
	case r.Changed() == 0:
		fmt.Fprintf(&b, "%s Formulas already up to date (%s).\n",
			style.Bold.Render("✓"), style.Dim.Render(fmt.Sprintf("%d formulas, from %s", r.UpToDate, r.binaryProvenance())))
	default:
		fmt.Fprintf(&b, "%s Synced formulas: %s\n", style.Bold.Render("✓"), strings.Join(parts, ", "))
	}

	if len(r.ReplacedDrift) > 0 {
		verb := "were"
		if r.DryRun {
			verb = "would be"
		}
		fmt.Fprintf(&b, "\n%s %d town copies %s replaced: their hash is not one gt wrote (hand-edited or hand-copied):\n",
			style.WarningPrefix, len(r.ReplacedDrift), verb)
		for _, name := range r.ReplacedDrift {
			fmt.Fprintf(&b, "      %s\n", name)
		}
		fmt.Fprint(&b, "  The binary is canonical. Put a change worth keeping in gastown source, or in an\n")
		fmt.Fprint(&b, "  overlay: gt formula overlay edit <name>   # ~/gt/formula-overlays/<name>.toml\n")
	}

	if len(r.Orphaned) > 0 {
		fmt.Fprintf(&b, "\n%s %d town formulas are no longer shipped in this binary; sync leaves them in place:\n",
			style.WarningPrefix, len(r.Orphaned))
		for _, name := range r.Orphaned {
			fmt.Fprintf(&b, "      %s\n", name)
		}
		fmt.Fprint(&b, "  Delete each from .beads/formulas/ once nothing still pours it.\n")
	}

	if len(r.Unowned) > 0 {
		fmt.Fprintf(&b, "\n%s %d files in .beads/formulas/ are not in gastown source and were never written by gt:\n",
			style.WarningPrefix, len(r.Unowned))
		for _, name := range r.Unowned {
			fmt.Fprintf(&b, "      %s\n", name)
		}
		fmt.Fprint(&b, "  Promote a formula the town still uses into gastown source; delete the rest.\n")
	}

	fmt.Fprintf(&b, "\n  Embedded formulas come from this binary: %s.\n", r.binaryProvenance())
	switch {
	case !r.DriftChecked:
		fmt.Fprintf(&b, "  Whether the build branch has newer formula content is UNKNOWN: %s\n",
			style.Dim.Render(r.DriftReason))
	case len(r.DriftFiles) > 0:
		fmt.Fprintf(&b, "%s %s, and %d formula file(s) changed there since this build:\n",
			style.WarningPrefix, r.behindPhrase(), len(r.DriftFiles))
		for _, f := range r.DriftFiles {
			fmt.Fprintf(&b, "      %s\n", f)
		}
		fmt.Fprint(&b, "  Those fixes are NOT in this binary, so this sync cannot deliver them.\n")
		fmt.Fprint(&b, "  Rebuild and install gt, then run 'gt formula sync' again.\n")
	default:
		fmt.Fprintf(&b, "  No formula file changed on %s since this build, so nothing is pending.\n",
			style.Dim.Render(r.CompareRef))
	}

	return b.String()
}

// Changed reports the number of formula files this sync wrote (or would write).
func (r *formulaSyncReport) Changed() int {
	return len(r.Installed) + len(r.Updated) + len(r.Reinstalled) + len(r.ReplacedDrift)
}

// binaryProvenance names the gt build whose embedded formulas were delivered.
func (r *formulaSyncReport) binaryProvenance() string {
	commit := r.BinaryCommit
	if commit == "" {
		commit = "unknown commit"
	}
	s := fmt.Sprintf("gt %s (%s", r.BinaryVersion, commit)
	if r.BuiltAt != "" {
		s += ", built " + r.BuiltAt
	}
	return s + ")"
}

// behindPhrase describes the gap to the build branch, on the branch's own terms.
func (r *formulaSyncReport) behindPhrase() string {
	ref := r.CompareRef
	if ref == "" {
		ref = "the build branch"
	}
	if r.CommitsBehind > 0 {
		return fmt.Sprintf("this binary is %d commits behind %s", r.CommitsBehind, ref)
	}
	return fmt.Sprintf("this binary is behind %s", ref)
}

// dryRunFormula shows what would happen without executing
func dryRunFormula(f *cookedFormula, formulaName, targetRig string) error {
	fmt.Printf("%s Would execute formula:\n", style.Dim.Render("[dry-run]"))
	fmt.Printf("  Formula: %s\n", style.Bold.Render(formulaName))
	fmt.Printf("  Type:    %s\n", f.Type)
	fmt.Printf("  Rig:     %s\n", targetRig)
	if formulaRunPR > 0 {
		fmt.Printf("  PR:      #%d\n", formulaRunPR)
	}
	// Show effective agent override (GH#2118)
	if formulaRunAgent != "" {
		fmt.Printf("  Agent:   %s\n", formulaRunAgent)
	}

	// Show --set variables if provided
	if len(formulaRunSet) > 0 {
		fmt.Printf("  Set:")
		for _, s := range formulaRunSet {
			fmt.Printf(" %s", s)
		}
		fmt.Println()
	}

	if p := convoyPlanFrom(f); f.Type == "convoy" && len(p.Legs) > 0 {
		// Generate review ID for dry-run display
		reviewID := generateFormulaShortID()

		// The formula's vars and the --set pairs, for template rendering
		setVars := formulaRunVars(f, formulaRunSet)

		// Build target description
		var targetDescription string
		if formulaRunPR > 0 {
			targetDescription = fmt.Sprintf("PR #%d", formulaRunPR)
		} else {
			targetDescription = "local files"
		}

		// Fetch PR info if --pr flag is set
		var prTitle string
		var changedFiles []map[string]interface{}
		if formulaRunPR > 0 {
			prTitle, changedFiles = fetchPRInfo(formulaRunPR)
			if prTitle != "" {
				fmt.Printf("  PR Title: %s\n", prTitle)
			}
			if len(changedFiles) > 0 {
				fmt.Printf("  Changed files: %d\n", len(changedFiles))
			}
		}

		// Show output directory if configured
		var outputDir string
		if p.OutputDir != "" {
			dirCtx := formulaTemplateContext(formulaName, targetDescription, reviewID,
				formulaRunPR, prTitle, changedFiles, formulaRunFiles, setVars)
			outputDir = renderTemplateOrDefault(p.OutputDir, dirCtx, ".reviews/"+reviewID)
			fmt.Printf("\n  Output directory: %s\n", outputDir)
		}

		fmt.Printf("\n  Legs (%d parallel):\n", len(p.Legs))
		for _, leg := range p.Legs {
			// Show rendered output path for each leg
			if outputDir != "" {
				legCtx := formulaTemplateContext(formulaName, targetDescription, reviewID,
					formulaRunPR, prTitle, changedFiles, formulaRunFiles, setVars)
				legCtx["leg"] = map[string]interface{}{
					"id":          leg.ID,
					"title":       leg.Title,
					"focus":       leg.Focus,
					"description": leg.Description,
				}
				legPattern := renderTemplateOrDefault(p.LegPattern, legCtx, leg.ID+"-findings.md")
				outputPath := filepath.Join(outputDir, legPattern)
				agentSuffix := resolveFormulaLegAgent(leg.Agent, formulaRunAgent)
				if agentSuffix != "" {
					agentSuffix = fmt.Sprintf(" [agent: %s]", agentSuffix)
				}
				fmt.Printf("    • %s: %s%s\n      → %s\n", leg.ID, leg.Title, agentSuffix, outputPath)
			} else {
				agentSuffix := resolveFormulaLegAgent(leg.Agent, formulaRunAgent)
				if agentSuffix != "" {
					agentSuffix = fmt.Sprintf(" [agent: %s]", agentSuffix)
				}
				fmt.Printf("    • %s: %s%s\n", leg.ID, leg.Title, agentSuffix)
			}
		}
		if p.Synthesis != nil {
			fmt.Printf("\n  Synthesis:\n")
			if outputDir != "" {
				synthPath := filepath.Join(outputDir, p.SynthesisFile)
				fmt.Printf("    • %s\n      → %s\n", p.Synthesis.Title, synthPath)
			} else {
				fmt.Printf("    • %s\n", p.Synthesis.Title)
			}
		}
	}

	if f.Type == "workflow" && len(f.Steps) > 0 {
		fmt.Printf("\n  Steps (%d sequential):\n", len(f.Steps))
		for i, step := range f.Steps {
			needsStr := ""
			if len(step.Needs) > 0 {
				needsStr = fmt.Sprintf(" [needs: %s]", strings.Join(step.Needs, ", "))
			}
			readyStr := ""
			if len(step.Needs) == 0 {
				readyStr = " ← ready"
			}
			fmt.Printf("    %d. %s: %s%s%s\n", i+1, step.ID, step.Title, needsStr, readyStr)
		}
	}

	return nil
}

// executeConvoyFormula spawns a convoy of polecats to execute a convoy formula
func executeConvoyFormula(f *cookedFormula, formulaName, targetRig string) error {
	fmt.Printf("%s Executing convoy formula: %s\n\n",
		style.Bold.Render("🚚"), formulaName)

	p := convoyPlanFrom(f)
	if len(p.Legs) == 0 {
		return fmt.Errorf("convoy formula '%s' has no legs", formulaName)
	}

	// Get town root and resolve rig-scoped bead prefix
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}
	townBeads := filepath.Join(townRoot, ".beads")

	// Resolve the target rig's beads prefix and directory so convoy legs
	// are created in the correct database. Legs need the rig prefix
	// (not hq-) so polecats can resolve them via prefix routing.
	rigPrefix := beads.GetPrefixForRig(townRoot, targetRig)
	rigBeadsDir := townBeads // default to town beads
	if rigPrefix != "hq" {
		// Look up the rig's beads path from routes
		routes, _ := beads.LoadRoutes(townBeads)
		for _, r := range routes {
			parts := strings.SplitN(r.Path, "/", 2)
			if len(parts) > 0 && parts[0] == targetRig {
				rigBeadsDir = filepath.Join(townRoot, r.Path, ".beads")
				break
			}
		}
	}

	// Step 1: Create convoy bead
	convoyID := fmt.Sprintf("%s-cv-%s", rigPrefix, generateFormulaShortID())
	convoyTitle := fmt.Sprintf("%s: %s", formulaName, f.Description)
	if len(convoyTitle) > 80 {
		convoyTitle = convoyTitle[:77] + "..."
	}

	// Build description with formula context
	description := fmt.Sprintf("Formula convoy: %s\n\nLegs: %d\nRig: %s",
		formulaName, len(p.Legs), targetRig)
	if formulaRunPR > 0 {
		description += fmt.Sprintf("\nPR: #%d", formulaRunPR)
	}

	// Guard against flag-like convoy titles (gt-e0kx5)
	if beads.IsFlagLikeTitle(convoyTitle) {
		return fmt.Errorf("refusing to create formula convoy: title %q looks like a CLI flag", convoyTitle)
	}

	townBd := beads.NewPinned(townBeads)
	rigBd := beads.NewPinned(rigBeadsDir)
	if _, err := townBd.CreateWithID(convoyID, beads.CreateOptions{
		Title:       convoyTitle,
		Description: description,
		Labels:      []string{"gt:convoy"},
		Priority:    -1,
	}); err != nil {
		return fmt.Errorf("creating convoy bead: %w", err)
	}

	fmt.Printf("%s Created convoy: %s\n", style.Bold.Render("✓"), convoyID)

	// Generate a unique review ID for this convoy run
	reviewID := generateFormulaShortID()

	// Build target description
	var targetDescription string
	if formulaRunPR > 0 {
		targetDescription = fmt.Sprintf("PR #%d", formulaRunPR)
	} else {
		targetDescription = "local files"
	}

	// Fetch PR info if --pr flag is set
	var prTitle string
	var changedFiles []map[string]interface{}
	if formulaRunPR > 0 {
		prTitle, changedFiles = fetchPRInfo(formulaRunPR)
	}

	// The formula's vars and the --set pairs, for template rendering.
	setVars := formulaRunVars(f, formulaRunSet)

	// Create output directory if configured
	var outputDir string
	if p.OutputDir != "" {
		dirCtx := formulaTemplateContext(formulaName, targetDescription, reviewID,
			formulaRunPR, prTitle, changedFiles, formulaRunFiles, setVars)
		outputDir = renderTemplateOrDefault(p.OutputDir, dirCtx, ".reviews/"+reviewID)

		// Create the directory
		if err := os.MkdirAll(outputDir, 0755); err != nil {
			fmt.Printf("%s Failed to create output directory %s: %v\n",
				style.Dim.Render("Warning:"), outputDir, err)
		} else {
			fmt.Printf("  %s Output directory: %s\n", style.Dim.Render("📁"), outputDir)
		}
	}

	// Step 2: Create leg beads and track them
	legBeads := make(map[string]string) // leg.ID -> bead ID
	for _, leg := range p.Legs {
		legBeadID := fmt.Sprintf("%s-leg-%s", rigPrefix, generateFormulaShortID())

		// Build leg description with prompt if available
		legDesc := leg.Description
		if p.BasePrompt != "" {
			// Build template context for this leg
			legCtx := formulaTemplateContext(formulaName, targetDescription, reviewID,
				formulaRunPR, prTitle, changedFiles, formulaRunFiles, setVars)
			legCtx["leg"] = map[string]interface{}{
				"id":          leg.ID,
				"title":       leg.Title,
				"focus":       leg.Focus,
				"description": leg.Description,
			}

			// Compute output path for this leg
			if p.OutputDir != "" {
				legPattern := renderTemplateOrDefault(p.LegPattern, legCtx, leg.ID+"-findings.md")
				outputPath := filepath.Join(outputDir, legPattern)
				legCtx["output_path"] = outputPath
				addOutputTemplateContext(legCtx, outputDir, p.SynthesisFile)
			}

			// Render the base prompt with template context
			renderedPrompt, err := renderTemplate(p.BasePrompt, legCtx)
			if err != nil {
				fmt.Printf("%s Failed to render template for %s: %v\n",
					style.Dim.Render("Warning:"), leg.ID, err)
				renderedPrompt = p.BasePrompt // Fall back to raw template
			}
			legDesc = fmt.Sprintf("%s\n\n---\nBase Prompt:\n%s", leg.Description, renderedPrompt)
		}

		if _, err := rigBd.CreateWithID(legBeadID, beads.CreateOptions{
			Title:       leg.Title,
			Description: legDesc,
			Priority:    -1,
		}); err != nil {
			fmt.Printf("%s Failed to create leg bead for %s: %v\n",
				style.Dim.Render("Warning:"), leg.ID, err)
			continue
		}

		// Track the leg with the convoy
		if err := addTrackingRelationFn(townBeads, convoyID, legBeadID); err != nil {
			fmt.Printf("%s Failed to track leg %s: %v\n",
				style.Dim.Render("Warning:"), leg.ID, err)
		}

		legBeads[leg.ID] = legBeadID
		fmt.Printf("  %s Created leg: %s (%s)\n", style.Dim.Render("○"), leg.ID, legBeadID)
	}

	// Step 3: Create synthesis bead if defined
	var synthesisBeadID string
	if p.Synthesis != nil {
		synthesisBeadID = fmt.Sprintf("%s-syn-%s", rigPrefix, generateFormulaShortID())

		synDesc := p.Synthesis.Description
		if synDesc == "" {
			synDesc = "Synthesize findings from all legs into unified output"
		}
		synCtx := formulaTemplateContext(formulaName, targetDescription, reviewID,
			formulaRunPR, prTitle, changedFiles, formulaRunFiles, setVars)
		if p.OutputDir != "" {
			addOutputTemplateContext(synCtx, outputDir, p.SynthesisFile)
		}
		if rendered, err := renderTemplate(synDesc, synCtx); err == nil {
			synDesc = rendered
		} else {
			fmt.Printf("%s Failed to render synthesis template: %v\n",
				style.Dim.Render("Warning:"), err)
		}

		if _, err := rigBd.CreateWithID(synthesisBeadID, beads.CreateOptions{
			Title:       p.Synthesis.Title,
			Description: synDesc,
			Priority:    -1,
		}); err != nil {
			fmt.Printf("%s Failed to create synthesis bead: %v\n",
				style.Dim.Render("Warning:"), err)
		} else {
			// Track synthesis with convoy
			_ = addTrackingRelationFn(townBeads, convoyID, synthesisBeadID)

			// Add dependencies: synthesis depends on all legs
			for _, legBeadID := range legBeads {
				_ = rigBd.AddDependency(synthesisBeadID, legBeadID)
			}

			fmt.Printf("  %s Created synthesis: %s\n", style.Dim.Render("★"), synthesisBeadID)
		}
	}

	// Step 4: Sling each leg to a polecat
	fmt.Printf("\n%s Dispatching legs to polecats...\n\n", style.Bold.Render("→"))

	slingCount := 0
	for _, leg := range p.Legs {
		legBeadID, ok := legBeads[leg.ID]
		if !ok {
			continue
		}

		// Build context message for the polecat
		contextMsg := fmt.Sprintf("Convoy leg: %s\nFocus: %s", leg.Title, leg.Focus)

		// Agent precedence (GH#2118): per-leg > CLI --agent
		legAgent := resolveFormulaLegAgent(leg.Agent, formulaRunAgent)

		slingArgs := buildConvoyLegSlingArgs(legBeadID, targetRig, leg.Description, leg.Title, legAgent, leg.ReviewOnly)

		slingCmd := exec.Command("gt", slingArgs...)
		slingCmd.Stdout = os.Stdout
		slingCmd.Stderr = os.Stderr

		if err := slingCmd.Run(); err != nil {
			fmt.Printf("%s Failed to sling leg %s: %v\n",
				style.Dim.Render("Warning:"), leg.ID, err)
			// Add comment to bead about failure
			_ = rigBd.AddComment(legBeadID, fmt.Sprintf("Failed to sling: %v", err))
			continue
		}

		slingCount++
		_ = contextMsg // Used in future for richer context
	}

	// Summary
	fmt.Printf("\n%s Convoy dispatched!\n", style.Bold.Render("✓"))
	fmt.Printf("  Convoy:  %s\n", convoyID)
	fmt.Printf("  Legs:    %d dispatched\n", slingCount)
	if synthesisBeadID != "" {
		fmt.Printf("  Synthesis: %s (blocked until legs complete)\n", synthesisBeadID)
	}
	fmt.Printf("\n  Track progress: gt convoy status %s\n", convoyID)

	return nil
}

// executeWorkflowFormula creates step beads with dependency wiring and dispatches
// ready steps (those with no unmet needs) to polecats on the target rig.
// Subsequent steps are auto-dispatched when their dependencies close. (gt-jh68)
func executeWorkflowFormula(f *cookedFormula, formulaName, targetRig string) error {
	fmt.Printf("%s Executing workflow formula: %s\n\n",
		style.Bold.Render("📋"), formulaName)

	if len(f.Steps) == 0 {
		return fmt.Errorf("workflow formula '%s' has no steps", formulaName)
	}

	// Get town beads directory
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return fmt.Errorf("finding town root: %w", err)
	}
	townBeads := filepath.Join(townRoot, ".beads")

	// Resolve the target rig's beads prefix and directory
	rigPrefix := beads.GetPrefixForRig(townRoot, targetRig)
	rigBeadsDir := townBeads
	if rigPrefix != "hq" {
		routes, _ := beads.LoadRoutes(townBeads)
		for _, r := range routes {
			parts := strings.SplitN(r.Path, "/", 2)
			if len(parts) > 0 && parts[0] == targetRig {
				rigBeadsDir = filepath.Join(townRoot, r.Path, ".beads")
				break
			}
		}
	}

	// Step 1: Create workflow root bead
	workflowID := fmt.Sprintf("hq-wf-%s", generateFormulaShortID())
	workflowTitle := fmt.Sprintf("%s: %s (%d steps)", formulaName,
		truncate(f.Description, 50), len(f.Steps))

	description := fmt.Sprintf("Workflow: %s\n\nSteps: %d\nRig: %s",
		formulaName, len(f.Steps), targetRig)

	if beads.IsFlagLikeTitle(workflowTitle) {
		return fmt.Errorf("refusing to create workflow: title %q looks like a CLI flag", workflowTitle)
	}

	rigBd := beads.NewPinned(rigBeadsDir)
	if _, err := beads.NewPinned(townBeads).CreateWithID(workflowID, beads.CreateOptions{
		Title:       workflowTitle,
		Description: description,
		Labels:      []string{"gt:convoy", "gt:workflow"},
		Priority:    -1,
	}); err != nil {
		return fmt.Errorf("creating workflow bead: %w", err)
	}

	fmt.Printf("%s Created workflow: %s\n", style.Bold.Render("✓"), workflowID)

	// Step 2: Create step beads and wire dependencies
	stepBeads := make(map[string]string) // step.ID -> bead ID

	for _, step := range f.Steps {
		stepBeadID := fmt.Sprintf("%s-wfs-%s", rigPrefix, generateFormulaShortID())
		stepDescription := workflowStepDescription(step)

		// The description goes in through Update, which sends it on stdin
		// (--body-file=-): large markdown would hit CLI arg length limits
		// and quoting issues as a create argument.
		_, err := rigBd.CreateWithID(stepBeadID, beads.CreateOptions{Title: step.Title, Priority: -1})
		if err == nil {
			err = rigBd.Update(stepBeadID, beads.UpdateOptions{Description: &stepDescription})
		}
		if err != nil {
			fmt.Printf("%s Failed to create step bead for %s: %v\n",
				style.Dim.Render("Warning:"), step.ID, err)
			continue
		}

		// Track the step with the workflow
		_ = addTrackingRelationFn(townBeads, workflowID, stepBeadID)

		// Wire dependencies: this step depends on its needs
		for _, needID := range step.Needs {
			depBeadID, ok := stepBeads[needID]
			if !ok {
				fmt.Printf("%s Step '%s' needs '%s' but it has no bead (ordering issue?)\n",
					style.Dim.Render("Warning:"), step.ID, needID)
				continue
			}
			_ = rigBd.AddDependency(stepBeadID, depBeadID)
		}

		stepBeads[step.ID] = stepBeadID

		needsStr := ""
		if len(step.Needs) > 0 {
			needsStr = fmt.Sprintf(" (needs: %s)", strings.Join(step.Needs, ", "))
		}
		fmt.Printf("  %s %s: %s%s\n", style.Dim.Render("○"), step.ID, stepBeadID, needsStr)
	}

	// Step 3: Identify and dispatch ready steps (those with no dependencies)
	// Interactive steps are hooked to the current session; others are slung to polecats.
	fmt.Printf("\n%s Dispatching ready steps...\n\n", style.Bold.Render("→"))

	// Check if any step in the workflow is interactive — if so, we'll need
	// to handle the molecule lifecycle in the current session.
	hasInteractive := false
	for _, step := range f.Steps {
		if step.metaBool("interactive") {
			hasInteractive = true
			break
		}
	}

	slingCount := 0
	interactiveCount := 0
	for _, step := range f.Steps {
		if len(step.Needs) > 0 {
			continue // has unmet dependencies — will be auto-dispatched
		}

		stepBeadID, ok := stepBeads[step.ID]
		if !ok {
			continue
		}

		if hasInteractive {
			// Interactive step: hook to current session instead of slinging to a polecat.
			// The user will execute this step in their current crew session.
			hooked := beads.StatusHooked
			_ = rigBd.Update(stepBeadID, beads.UpdateOptions{Status: &hooked})

			fmt.Printf("  %s %s: %s (interactive — hooked to current session)\n",
				style.Bold.Render("⇨"), step.ID, stepBeadID)
			fmt.Printf("    %s\n", step.Title)
			fmt.Printf("    When done: gt bead close %s\n\n", stepBeadID)
			interactiveCount++
			continue
		}

		// Non-interactive step: sling to the step's target, or to the rig's
		// polecat pool by default.
		stepTarget := workflowStepTarget(step, targetRig)
		slingArgs := buildWorkflowStepSlingArgs(stepBeadID, stepTarget, workflowStepDescription(step), step.Title, formulaRunAgent)

		slingCmd := exec.Command("gt", slingArgs...)
		slingCmd.Stdout = os.Stdout
		slingCmd.Stderr = os.Stderr

		if err := slingCmd.Run(); err != nil {
			fmt.Printf("%s Failed to sling step %s: %v\n",
				style.Dim.Render("Warning:"), step.ID, err)
			_ = rigBd.AddComment(stepBeadID, fmt.Sprintf("Failed to sling: %v", err))
			continue
		}

		slingCount++
	}

	// Summary
	blockedCount := len(f.Steps) - slingCount - interactiveCount
	fmt.Printf("\n%s Workflow dispatched!\n", style.Bold.Render("✓"))
	fmt.Printf("  Workflow: %s\n", workflowID)
	if interactiveCount > 0 {
		fmt.Printf("  Steps:    %d total, %d interactive (current session), %d dispatched, %d awaiting dependencies\n",
			len(f.Steps), interactiveCount, slingCount, blockedCount)
		fmt.Printf("\n  This workflow has interactive steps. Work through them sequentially:\n")
		fmt.Printf("    gt mol current                 — find current step\n")
		fmt.Printf("    gt bead close <step-id>        — advance to next step\n")
	} else {
		fmt.Printf("  Steps:    %d total, %d dispatched, %d awaiting dependencies\n",
			len(f.Steps), slingCount, blockedCount)
	}
	fmt.Printf("\n  Track progress: gt convoy status %s\n", workflowID)

	return nil
}

const workflowTargetField = "workflow_target"

// workflowStepDescription is the step's description (bd substituted the --set
// vars), headed by its metadata.target sling target when it has one.
func workflowStepDescription(step cookedStep) string {
	target := strings.TrimSpace(step.metaString("target"))
	if target == "" {
		return step.Description
	}
	return fmt.Sprintf("%s: %s\n\n%s", workflowTargetField, target, step.Description)
}

// workflowStepTarget is where a step is slung: its metadata.target, else the
// formula's target rig ("rig" names it too).
func workflowStepTarget(step cookedStep, targetRig string) string {
	target := strings.TrimSpace(step.metaString("target"))
	if target == "" || target == "rig" {
		return targetRig
	}
	return target
}

// truncate shortens a string to maxLen, appending "..." if truncated.
// Truncates at the first newline if one appears before maxLen.
func truncate(s string, maxLen int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < maxLen {
		s = s[:i]
	}
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// buildConvoyLegSlingArgs constructs the gt-sling argument list for a convoy leg.
// Legs are tracked by the parent convoy; a sling creates no convoy of its own.
func buildConvoyLegSlingArgs(beadID, targetRig, description, title, agent string, reviewOnly bool) []string {
	args := []string{
		"sling", beadID, targetRig,
		"-a", description,
		"-s", title,
	}
	if agent != "" {
		args = append(args, "--agent", agent)
	}
	if reviewOnly {
		args = append(args, "--review-only")
	}
	return args
}

// buildWorkflowStepSlingArgs constructs the gt-sling argument list for a workflow step.
// Steps are tracked by the parent workflow bead; a sling creates no convoy of its own.
func buildWorkflowStepSlingArgs(beadID, targetRig, description, title, agent string) []string {
	args := []string{
		"sling", beadID, targetRig,
		"-a", description,
		"-s", title,
	}
	if agent != "" {
		args = append(args, "--agent", agent)
	}
	return args
}

// parseSetVars parses --set key=value pairs into a map for template rendering.
func parseSetVars(setArgs []string) map[string]interface{} {
	vars := make(map[string]interface{})
	for _, arg := range setArgs {
		if idx := strings.IndexByte(arg, '='); idx > 0 {
			vars[arg[:idx]] = arg[idx+1:]
		}
	}
	return vars
}

func formulaTemplateContext(formulaName, targetDescription, reviewID string, prNumber int, prTitle string, changedFiles []map[string]interface{}, files []string, setVars map[string]interface{}) map[string]interface{} {
	ctx := map[string]interface{}{
		"formula_name":       formulaName,
		"target_description": targetDescription,
		"review_id":          reviewID,
		"pr_number":          prNumber,
		"pr_title":           prTitle,
		"changed_files":      changedFiles,
		"files":              files,
	}
	for k, v := range setVars {
		ctx[k] = v
	}
	return ctx
}

func addOutputTemplateContext(ctx map[string]interface{}, outputDir, synthesisFile string) {
	ctx["output"] = map[string]interface{}{
		"directory": outputDir,
		"synthesis": synthesisFile,
	}
}

// renderTemplate renders a Go text/template with the given context map
func renderTemplate(tmplText string, ctx map[string]interface{}) (string, error) {
	tmpl, err := template.New("prompt").Parse(tmplText)
	if err != nil {
		return "", fmt.Errorf("parsing template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, ctx); err != nil {
		return "", fmt.Errorf("executing template: %w", err)
	}
	return buf.String(), nil
}

// renderTemplateOrDefault renders a template, returning defaultVal on error
func renderTemplateOrDefault(tmplText string, ctx map[string]interface{}, defaultVal string) string {
	if tmplText == "" {
		return defaultVal
	}
	result, err := renderTemplate(tmplText, ctx)
	if err != nil {
		return defaultVal
	}
	return result
}

// fetchPRInfo fetches PR title and changed files from GitHub using gh CLI
func fetchPRInfo(prNumber int) (string, []map[string]interface{}) {
	var prTitle string
	var changedFiles []map[string]interface{}

	// Get PR title
	titleCmd := exec.Command("gh", "pr", "view", fmt.Sprintf("%d", prNumber), "--json", "title", "--jq", ".title")
	titleOut, err := titleCmd.Output()
	if err == nil {
		prTitle = strings.TrimSpace(string(titleOut))
	}

	// Get changed files with stats
	filesCmd := exec.Command("gh", "pr", "view", fmt.Sprintf("%d", prNumber), "--json", "files", "--jq", ".files[] | \"\\(.path) \\(.additions) \\(.deletions)\"")
	filesOut, err := filesCmd.Output()
	if err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(filesOut)), "\n") {
			if line == "" {
				continue
			}
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				additions, err := strconv.Atoi(parts[1])
				if err != nil {
					continue
				}
				deletions, err := strconv.Atoi(parts[2])
				if err != nil {
					continue
				}
				changedFiles = append(changedFiles, map[string]interface{}{
					"path":      parts[0],
					"additions": additions,
					"deletions": deletions,
				})
			}
		}
	}

	return prTitle, changedFiles
}

// generateFormulaShortID generates a short random ID (5 lowercase chars)
func generateFormulaShortID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return strings.ToLower(base32.StdEncoding.EncodeToString(b)[:5])
}

// runFormulaCreate creates a new formula template
func runFormulaCreate(cmd *cobra.Command, args []string) error {
	formulaName := args[0]

	// Find or create formulas directory
	formulasDir := ".beads/formulas"

	// Check if we're in a beads-enabled directory
	if _, err := os.Stat(".beads"); os.IsNotExist(err) {
		// Try user formulas directory
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("cannot find home directory: %w", err)
		}
		formulasDir = filepath.Join(home, ".beads", "formulas")
	}

	// Ensure directory exists
	if err := os.MkdirAll(formulasDir, 0755); err != nil {
		return fmt.Errorf("creating formulas directory: %w", err)
	}

	// Generate filename
	filename := filepath.Join(formulasDir, formulaName+".formula.toml")

	// Check if file already exists
	if _, err := os.Stat(filename); err == nil {
		return fmt.Errorf("formula already exists: %s", filename)
	}

	// Generate template based on type
	var template string
	switch formulaCreateType {
	case "task":
		template = generateTaskTemplate(formulaName)
	case "workflow":
		template = generateWorkflowTemplate(formulaName)
	case "patrol":
		template = generatePatrolTemplate(formulaName)
	default:
		return fmt.Errorf("unknown formula type: %s (use: task, workflow, or patrol)", formulaCreateType)
	}

	// Write the file
	if err := os.WriteFile(filename, []byte(template), 0644); err != nil {
		return fmt.Errorf("writing formula file: %w", err)
	}

	fmt.Printf("%s Created formula: %s\n", style.Bold.Render("✓"), filename)
	fmt.Printf("\nNext steps:\n")
	fmt.Printf("  1. Edit the formula: %s\n", filename)
	fmt.Printf("  2. View it:          gt formula show %s\n", formulaName)
	fmt.Printf("  3. Run it:           gt formula run %s\n", formulaName)

	return nil
}

func generateTaskTemplate(name string) string {
	// Sanitize name for use in template
	title := strings.ReplaceAll(name, "-", " ")
	title = cases.Title(language.English).String(title)

	return fmt.Sprintf(`# Formula: %s
# Type: task
# Created by: gt formula create

description = """%s task.

Add a detailed description here."""
formula = "%s"
version = 1

# Single step task
[[steps]]
id = "do-task"
title = "Execute task"
description = """
Perform the main task work.

**Steps:**
1. Understand the requirements
2. Implement the changes
3. Verify the work
"""

# Variables that can be passed when running the formula
# [vars]
# [vars.issue]
# description = "Issue ID to work on"
# required = true
#
# [vars.target]
# description = "Target branch"
# default = "main"
`, name, title, name)
}

func generateWorkflowTemplate(name string) string {
	title := strings.ReplaceAll(name, "-", " ")
	title = cases.Title(language.English).String(title)

	return fmt.Sprintf(`# Formula: %s
# Type: workflow
# Created by: gt formula create
#
# pour = true  — Steps materialized as sub-wisps (checkpoint recovery on crash)
# pour = false — Steps read inline (root-only, restart on failure) [DEFAULT]

description = """%s workflow.

A multi-step workflow with dependencies between steps."""
formula = "%s"
version = 1

# Step 1: Setup
[[steps]]
id = "setup"
title = "Setup environment"
description = """
Prepare the environment for the workflow.

**Steps:**
1. Check prerequisites
2. Set up working environment
"""

# Step 2: Implementation (depends on setup)
[[steps]]
id = "implement"
title = "Implement changes"
needs = ["setup"]
description = """
Make the necessary code changes.

**Steps:**
1. Understand requirements
2. Write code
3. Test locally
"""

# Step 3: Test (depends on implementation)
[[steps]]
id = "test"
title = "Run tests"
needs = ["implement"]
description = """
Verify the changes work correctly.

**Steps:**
1. Run unit tests
2. Run integration tests
3. Check for regressions
"""

# Step 4: Complete (depends on tests)
[[steps]]
id = "complete"
title = "Complete workflow"
needs = ["test"]
description = """
Finalize and clean up.

**Steps:**
1. Commit final changes
2. Clean up temporary files
"""

# Variables
[vars]
[vars.issue]
description = "Issue ID to work on"
required = true
`, name, title, name)
}

func generatePatrolTemplate(name string) string {
	title := strings.ReplaceAll(name, "-", " ")
	title = cases.Title(language.English).String(title)

	return fmt.Sprintf(`# Formula: %s
# Type: patrol
# Created by: gt formula create
#
# Patrol formulas are for repeating cycles (wisps).
# They run continuously and are NOT synced to git.

description = """%s patrol.

A patrol formula for periodic checks. Patrol formulas create wisps
(ephemeral molecules) that are NOT synced to git."""
formula = "%s"
version = 1

# The patrol step(s)
[[steps]]
id = "check"
title = "Run patrol check"
description = """
Perform the patrol inspection.

**Check for:**
1. Health indicators
2. Warning signs
3. Items needing attention

**On findings:**
- Log the issue
- Escalate if critical
"""

# Optional: remediation step
# [[steps]]
# id = "remediate"
# title = "Fix issues"
# needs = ["check"]
# description = """
# Fix any issues found during the check.
# """

# Variables (optional)
# [vars]
# [vars.verbose]
# description = "Enable verbose output"
# default = "false"
`, name, title, name)
}

// resolveFormulaLegAgent returns the effective agent for a convoy leg using
// the precedence: per-leg > CLI --agent. Returns "" if no agent override
// applies. See GH#2118.
func resolveFormulaLegAgent(legAgent, cliAgent string) string {
	if legAgent != "" {
		return legAgent
	}
	return cliAgent
}

// convoyLeg is one leg of a convoy formula: a cooked step, with its focus,
// agent and review_only in its metadata table.
type convoyLeg struct {
	ID, Title, Focus, Description, Agent string
	ReviewOnly                           bool
}

// convoyPlan is what gt formula run dispatches for a convoy formula, read from
// bd's cooked tree (gt-fd2cu.1.1): the synthesis step (metadata.convoy =
// "synthesis"), every other step as a leg, and the run settings the formula
// declares as vars, so --set overrides them like any input.
type convoyPlan struct {
	Legs       []convoyLeg
	Synthesis  *cookedStep
	BasePrompt string
	// OutputDir, LegPattern and SynthesisFile are Go templates; OutputDir
	// empty means the convoy writes no files.
	OutputDir, LegPattern, SynthesisFile string
}

func convoyPlanFrom(f *cookedFormula) convoyPlan {
	allReviewOnly := f.varValue("review_only") == "true"
	p := convoyPlan{
		BasePrompt:    f.varValue("base_prompt"),
		OutputDir:     f.varValue("output_directory"),
		LegPattern:    f.varValue("output_leg_pattern"),
		SynthesisFile: f.varValue("output_synthesis"),
	}
	for i := range f.Steps {
		s := &f.Steps[i]
		if s.metaString("convoy") == "synthesis" {
			p.Synthesis = s
			continue
		}
		p.Legs = append(p.Legs, convoyLeg{
			ID: s.ID, Title: s.Title, Description: s.Description,
			Focus: s.metaString("focus"), Agent: s.metaString("agent"),
			ReviewOnly: allReviewOnly || s.metaBool("review_only"),
		})
	}
	return p
}

// formulaRunVars is the template context's inputs: every var bd resolved (its
// --set value, else its default), then the --set pairs the formula does not
// declare.
func formulaRunVars(f *cookedFormula, setArgs []string) map[string]interface{} {
	vars := parseSetVars(setArgs)
	for _, v := range f.Vars {
		if v.Value != nil {
			vars[v.Name] = *v.Value
		}
	}
	return vars
}

// promptYesNo asks the user a yes/no question
func promptYesNo(question string) bool {
	fmt.Printf("%s [y/N]: ", question)
	reader := bufio.NewReader(os.Stdin)
	answer, _ := reader.ReadString('\n')
	answer = strings.TrimSpace(strings.ToLower(answer))
	return answer == "y" || answer == "yes"
}
