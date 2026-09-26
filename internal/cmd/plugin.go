package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/plugin"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Plugin command flags
var (
	pluginListJSON     bool
	pluginShowJSON     bool
	pluginRunForce     bool
	pluginRunDryRun    bool
	pluginHistoryJSON  bool
	pluginHistoryLimit int
	pluginSyncSource   string
	pluginSyncClean    bool
	pluginSyncDryRun   bool
	pluginSyncForce    bool
	pluginRecordPlugin string
	pluginRecordResult string
	pluginRecordTitle  string
	pluginRecordBody   string
	pluginRecordRig    string
	pluginRecordLabels []string
)

var pluginCmd = &cobra.Command{
	Use:     "plugin",
	GroupID: GroupConfig,
	Short:   "Plugin management",
	Long: `Manage plugins, which the daemon heartbeat dispatches on a gate.

Plugins are periodic automation tasks defined by plugin.md files with TOML frontmatter.

PLUGIN LOCATIONS:
  ~/gt/plugins/           Town-level plugins (universal, apply everywhere)
  <rig>/plugins/          Rig-level plugins (project-specific)

GATE TYPES:
  cooldown    Run if enough time has passed (e.g., 1h)  [dispatched by the daemon heartbeat]
  cron        Run on a schedule (e.g., "0 9 * * *")    [parsed; nothing dispatches it, gt-qehkn]
  condition   Run if a check command returns exit 0    [parsed; nothing dispatches it, gt-qehkn]
  event       Run on events (e.g., startup)            [parsed; nothing dispatches it, gt-qehkn]
  manual      Never auto-run, trigger explicitly       [the daemon never dispatches it]

Examples:
  gt plugin list                    # List all discovered plugins
  gt plugin show <name>             # Show plugin details
  gt plugin list --json             # JSON output`,
	RunE: requireSubcommand,
}

var pluginListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all discovered plugins",
	Long: `List all plugins from town and rig plugin directories.

Plugins are discovered from:
  - ~/gt/plugins/ (town-level)
  - <rig>/plugins/ for each registered rig

When a plugin exists at both levels, the rig-level version takes precedence.

Examples:
  gt plugin list              # Human-readable output
  gt plugin list --json       # JSON output for scripting`,
	RunE: runPluginList,
}

var pluginShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show plugin details",
	Long: `Show detailed information about a plugin.

Displays the plugin's configuration, gate settings, and instructions.

Examples:
  gt plugin show rebuild-gt
  gt plugin show rebuild-gt --json`,
	Args: cobra.ExactArgs(1),
	RunE: runPluginShow,
}

var pluginRunCmd = &cobra.Command{
	Use:   "run <name>",
	Short: "Manually trigger a parked (manual-gate) plugin",
	Long: `Manually trigger a plugin that is parked on a manual gate.

This command does no work: it prints the plugin's instructions and records a
"printed" receipt, distinct from success, that does not satisfy the plugin's
own cooldown gate. Execute the instructions yourself, then record the real
result with ` + "`gt plugin record-run`" + ` (--result success, failure, skipped, or warning).
A receipt is never written as success on the command's say-so alone (gt-o1z7).

Gate behavior:
- closed cooldown: the run is refused; nothing is recorded (a refusal is not
  a run, and a receipt here would count toward the daemon's own cooldown)
- --force: bypasses the cooldown check only; the receipt is still a "printed,
  not executed" record
- script-type plugin (has a run.sh): refused; the daemon heartbeat is its
  scheduler and this command has no script interpreter (gt-o1z7)

Examples:
  gt plugin run github-sheriff          # parked plugin: prints instructions
  gt plugin run <name> --dry-run        # Show what would happen`,
	Args: cobra.ExactArgs(1),
	RunE: runPluginRun,
}

var pluginSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync plugins from source repo to runtime directories",
	Long: `Copy plugins from the gastown source repository to runtime plugin directories.

By default the source is the town's own gastown checkout:
<town>/gastown/mayor/rig/plugins, then the legacy crew/den and gastown/plugins
layouts. The working directory is never consulted — a checkout you happen to
be standing in is not the town's source of truth (gt-nc7q). Pass --source to
read a directory of your choosing.

Syncs to town-level plugins (~/gt/plugins/) so all rigs see the latest plugins.

Examples:
  gt plugin sync                           # Auto-detect source, sync to town
  gt plugin sync --source ./plugins        # Explicit source directory
  gt plugin sync --clean                   # Remove plugins not in source
  gt plugin sync --dry-run                 # Show what would happen
  gt plugin sync --force                   # Also overwrite runtime edits

A runtime plugin holding content the source repo never had (a hand edit, or
a file that exists only in the runtime copy) is left untouched and listed,
and the command exits non-zero: land the edit through the merge queue, or
pass --force to discard it. Content the repo once held is an older copy and
is replaced as usual (gt-o848l).`,
	RunE: runPluginSync,
}

var pluginHistoryCmd = &cobra.Command{
	Use:   "history <name>",
	Short: "Show plugin execution history",
	Long: `Show recent execution history for a plugin.

Queries ephemeral beads (wisps) that record plugin runs.

Examples:
  gt plugin history rebuild-gt
  gt plugin history rebuild-gt --json
  gt plugin history rebuild-gt --limit 20`,
	Args: cobra.ExactArgs(1),
	RunE: runPluginHistory,
}

var pluginRecordRunCmd = &cobra.Command{
	Use:   "record-run",
	Short: "Record a plugin run receipt",
	Long: `Record a plugin run receipt through the canonical plugin recorder.

The recorder creates an ephemeral type:plugin-run bead, closes it immediately,
and leaves the receipt available to plugin history/cooldown queries that use
closed beads. This keeps plugin scripts from leaking open run-log beads.

Two callers use this: a plugin's own "Record Result" section, and whoever
executed the instructions ` + "`gt plugin run`" + ` printed (script-type
plugins have no manual trigger; the daemon records its own receipt when it
runs their run.sh). A receipt never claims success for work that was not
done (gt-o1z7).`,
	RunE: runPluginRecordRun,
}

func init() {
	// List subcommand flags
	pluginListCmd.Flags().BoolVar(&pluginListJSON, "json", false, "Output as JSON")

	// Show subcommand flags
	pluginShowCmd.Flags().BoolVar(&pluginShowJSON, "json", false, "Output as JSON")

	// Run subcommand flags
	pluginRunCmd.Flags().BoolVar(&pluginRunForce, "force", false, "Bypass the cooldown check (the run is still recorded as printed, not executed)")
	pluginRunCmd.Flags().BoolVar(&pluginRunDryRun, "dry-run", false, "Show what would happen without executing")

	// History subcommand flags
	pluginHistoryCmd.Flags().BoolVar(&pluginHistoryJSON, "json", false, "Output as JSON")
	pluginHistoryCmd.Flags().IntVar(&pluginHistoryLimit, "limit", 10, "Maximum number of runs to show")

	// Record-run subcommand flags
	pluginRecordRunCmd.Flags().StringVar(&pluginRecordPlugin, "plugin", "", "Plugin name")
	pluginRecordRunCmd.Flags().StringVar(&pluginRecordResult, "result", "", "Run result label value")
	pluginRecordRunCmd.Flags().StringVar(&pluginRecordTitle, "title", "", "Receipt title")
	pluginRecordRunCmd.Flags().StringVar(&pluginRecordBody, "description", "", "Receipt description")
	pluginRecordRunCmd.Flags().StringVar(&pluginRecordRig, "rig", "", "Rig label value")
	pluginRecordRunCmd.Flags().StringArrayVarP(&pluginRecordLabels, "label", "l", nil, "Additional label for the receipt")

	// Sync subcommand flags
	pluginSyncCmd.Flags().StringVar(&pluginSyncSource, "source", "", "Source plugins directory (auto-detected if omitted)")
	pluginSyncCmd.Flags().BoolVar(&pluginSyncClean, "clean", false, "Remove plugins from target that don't exist in source")
	pluginSyncCmd.Flags().BoolVar(&pluginSyncDryRun, "dry-run", false, "Show what would happen without syncing")
	pluginSyncCmd.Flags().BoolVar(&pluginSyncForce, "force", false, "Overwrite runtime plugin edits the source repo does not have")

	// Add subcommands
	pluginCmd.AddCommand(pluginListCmd)
	pluginCmd.AddCommand(pluginShowCmd)
	pluginCmd.AddCommand(pluginRunCmd)
	pluginCmd.AddCommand(pluginHistoryCmd)
	pluginCmd.AddCommand(pluginRecordRunCmd)
	pluginCmd.AddCommand(pluginSyncCmd)

	rootCmd.AddCommand(pluginCmd)
}

// getPluginScanner creates a scanner with town root and all rig names.
func getPluginScanner() (*plugin.Scanner, string, error) {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return nil, "", fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Load rigs config to get rig names
	rigsConfigPath := constants.MayorRigsPath(townRoot)
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}

	// Extract rig names
	rigNames := make([]string, 0, len(rigsConfig.Rigs))
	for name := range rigsConfig.Rigs {
		rigNames = append(rigNames, name)
	}
	sort.Strings(rigNames)

	scanner := plugin.NewScanner(townRoot, rigNames)
	return scanner, townRoot, nil
}

func runPluginList(cmd *cobra.Command, args []string) error {
	scanner, townRoot, err := getPluginScanner()
	if err != nil {
		return err
	}

	plugins, err := scanner.DiscoverAll()
	if err != nil {
		return fmt.Errorf("discovering plugins: %w", err)
	}

	// Sort plugins by name
	sort.Slice(plugins, func(i, j int) bool {
		return plugins[i].Name < plugins[j].Name
	})

	if pluginListJSON {
		return outputPluginListJSON(plugins)
	}

	return outputPluginListText(plugins, townRoot)
}

func outputPluginListJSON(plugins []*plugin.Plugin) error {
	summaries := make([]plugin.PluginSummary, len(plugins))
	for i, p := range plugins {
		summaries[i] = p.Summary()
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(summaries)
}

func outputPluginListText(plugins []*plugin.Plugin, townRoot string) error {
	if len(plugins) == 0 {
		fmt.Printf("%s No plugins discovered\n", style.Dim.Render("○"))
		fmt.Printf("\n  Plugin directories:\n")
		fmt.Printf("    %s/plugins/\n", townRoot)
		fmt.Printf("\n  Create a plugin by adding a directory with plugin.md\n")
		return nil
	}

	fmt.Printf("%s Discovered %d plugin(s)\n\n", style.Success.Render("●"), len(plugins))

	// Group by location
	townPlugins := make([]*plugin.Plugin, 0)
	rigPlugins := make(map[string][]*plugin.Plugin)

	for _, p := range plugins {
		if p.Location == plugin.LocationTown {
			townPlugins = append(townPlugins, p)
		} else {
			rigPlugins[p.RigName] = append(rigPlugins[p.RigName], p)
		}
	}

	// Print town-level plugins
	if len(townPlugins) > 0 {
		fmt.Printf("  %s\n", style.Bold.Render("Town-level plugins:"))
		for _, p := range townPlugins {
			printPluginSummary(p)
		}
		fmt.Println()
	}

	// Print rig-level plugins by rig
	rigNames := make([]string, 0, len(rigPlugins))
	for name := range rigPlugins {
		rigNames = append(rigNames, name)
	}
	sort.Strings(rigNames)

	for _, rigName := range rigNames {
		fmt.Printf("  %s\n", style.Bold.Render(fmt.Sprintf("Rig %s:", rigName)))
		for _, p := range rigPlugins[rigName] {
			printPluginSummary(p)
		}
		fmt.Println()
	}

	return nil
}

func printPluginSummary(p *plugin.Plugin) {
	gateType := "manual"
	if p.Gate != nil && p.Gate.Type != "" {
		gateType = string(p.Gate.Type)
	}

	desc := p.Description
	if len(desc) > 50 {
		desc = desc[:47] + "..."
	}

	typeTag := gateType
	if p.IsExecWrapper() {
		typeTag = "exec-wrapper"
	}

	fmt.Printf("    %s %s\n", style.Bold.Render(p.Name), style.Dim.Render(fmt.Sprintf("[%s]", typeTag)))
	if desc != "" {
		fmt.Printf("      %s\n", style.Dim.Render(desc))
	}
}

func runPluginShow(cmd *cobra.Command, args []string) error {
	name := args[0]

	scanner, _, err := getPluginScanner()
	if err != nil {
		return err
	}

	p, err := scanner.GetPlugin(name)
	if err != nil {
		return err
	}

	if pluginShowJSON {
		return outputPluginShowJSON(p)
	}

	return outputPluginShowText(p)
}

func outputPluginShowJSON(p *plugin.Plugin) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(p)
}

func outputPluginShowText(p *plugin.Plugin) error {
	fmt.Printf("%s %s\n", style.Bold.Render("Plugin:"), p.Name)
	fmt.Printf("%s %s\n", style.Bold.Render("Path:"), p.Path)

	if p.Description != "" {
		fmt.Printf("%s %s\n", style.Bold.Render("Description:"), p.Description)
	}

	// Location
	locStr := string(p.Location)
	if p.RigName != "" {
		locStr = fmt.Sprintf("%s (%s)", p.Location, p.RigName)
	}
	fmt.Printf("%s %s\n", style.Bold.Render("Location:"), locStr)

	fmt.Printf("%s %d\n", style.Bold.Render("Version:"), p.Version)

	// Agent routing: which preset this plugin's dog session runs.
	agentStr := p.Agent
	if agentStr == "" {
		agentStr = style.Dim.Render("(unset — role_agents.dog)")
	}
	fmt.Printf("%s %s\n", style.Bold.Render("Agent:"), agentStr)

	// Gate
	fmt.Println()
	fmt.Printf("%s\n", style.Bold.Render("Gate:"))
	if p.Gate != nil {
		fmt.Printf("  Type: %s\n", p.Gate.Type)
		if p.Gate.Duration != "" {
			fmt.Printf("  Duration: %s\n", p.Gate.Duration)
		}
		if p.Gate.Schedule != "" {
			fmt.Printf("  Schedule: %s\n", p.Gate.Schedule)
		}
		if p.Gate.Check != "" {
			fmt.Printf("  Check: %s\n", p.Gate.Check)
		}
		if p.Gate.On != "" {
			fmt.Printf("  On: %s\n", p.Gate.On)
		}
	} else {
		fmt.Printf("  Type: manual (no gate section)\n")
	}

	// Tracking
	if p.Tracking != nil {
		fmt.Println()
		fmt.Printf("%s\n", style.Bold.Render("Tracking:"))
		if len(p.Tracking.Labels) > 0 {
			fmt.Printf("  Labels: %s\n", strings.Join(p.Tracking.Labels, ", "))
		}
		fmt.Printf("  Digest: %v\n", p.Tracking.Digest)
	}

	// Execution
	if p.Execution != nil {
		fmt.Println()
		fmt.Printf("%s\n", style.Bold.Render("Execution:"))
		if p.Execution.Type != "" {
			fmt.Printf("  Type: %s\n", p.Execution.Type)
		}
		if len(p.Execution.Wrapper) > 0 {
			fmt.Printf("  Wrapper: %s\n", strings.Join(p.Execution.Wrapper, " "))
		}
		if p.Execution.Timeout != "" {
			fmt.Printf("  Timeout: %s\n", p.Execution.Timeout)
		}
		fmt.Printf("  Notify on failure: %v\n", p.Execution.NotifyOnFailure)
		if p.Execution.Severity != "" {
			fmt.Printf("  Severity: %s\n", p.Execution.Severity)
		}
	}

	// Instructions preview
	if p.Instructions != "" {
		fmt.Println()
		fmt.Printf("%s\n", style.Bold.Render("Instructions:"))
		lines := strings.Split(p.Instructions, "\n")
		preview := lines
		if len(lines) > 10 {
			preview = lines[:10]
		}
		for _, line := range preview {
			fmt.Printf("  %s\n", line)
		}
		if len(lines) > 10 {
			fmt.Printf("  %s\n", style.Dim.Render(fmt.Sprintf("... (%d more lines)", len(lines)-10)))
		}
	}

	return nil
}

func runPluginRun(cmd *cobra.Command, args []string) error {
	name := args[0]

	scanner, townRoot, err := getPluginScanner()
	if err != nil {
		return err
	}

	p, err := scanner.GetPlugin(name)
	if err != nil {
		return err
	}

	// Check gate status for cooldown gates
	gateOpen := true
	gateReason := ""
	if p.Gate != nil && p.Gate.Type == plugin.GateCooldown && !pluginRunForce {
		recorder := plugin.NewRecorder(townRoot)
		duration := p.Gate.Duration
		if duration == "" {
			duration = "1h" // default
		}
		count, err := recorder.CountRunsSince(p.Name, duration)
		if err != nil {
			// Log warning but continue
			fmt.Fprintf(os.Stderr, "Warning: checking gate status: %v\n", err)
		} else if count > 0 {
			gateOpen = false
			gateReason = fmt.Sprintf("ran %d time(s) within %s cooldown", count, duration)
		}
	}

	if pluginRunDryRun {
		fmt.Printf("%s Dry run for plugin: %s\n", style.Bold.Render("Plugin:"), p.Name)
		fmt.Printf("%s %s\n", style.Bold.Render("Location:"), p.Path)
		if p.Gate != nil {
			fmt.Printf("%s %s\n", style.Bold.Render("Gate type:"), p.Gate.Type)
		}
		if !gateOpen {
			fmt.Printf("%s %s (use --force to override)\n", style.Warning.Render("Gate closed:"), gateReason)
		} else {
			fmt.Printf("%s Would execute plugin instructions\n", style.Success.Render("Gate open:"))
		}
		return nil
	}

	if !gateOpen && !pluginRunForce {
		fmt.Printf("%s Gate closed: %s\n", style.Warning.Render("⚠"), gateReason)
		fmt.Printf("  Use --force to bypass gate check\n")

		// No receipt: the run never happened, so there is nothing to
		// record. The daemon's own manual-gate skip (handler.go) only
		// logs, for the same reason — a receipt here would count toward
		// CountRunsSince and push the daemon's own cooldown dispatch
		// further out for a run that was refused, not taken (gt-o1z7).
		return nil
	}

	// A script-type plugin the daemon can run in-process is not something
	// `gt plugin run` executes: there is no script interpreter here, so the
	// command cannot have run the plugin. Refusing outright is the honest
	// answer — a printed receipt would misstate what happened. Only a
	// cooldown gate has an automatic executor (the daemon heartbeat); a
	// script plugin on any other gate has no run path at all today
	// (gt-o1z7, d249eaeae0f3/fec069dde34c: executing run.sh from the CLI
	// both fails open on a bd error and can overlap the daemon's own run).
	if p.Execution != nil && p.Execution.Type == plugin.ExecTypeScript {
		fmt.Fprintf(os.Stderr, "Plugin %s has a run.sh; `gt plugin run` does not execute plugin scripts (there is no script interpreter here). If its gate is cooldown, the daemon heartbeat runs it; otherwise, run the script directly or edit the plugin's gate.\n", p.Name)
		return fmt.Errorf("plugin %s is script-type; gt plugin run does not run scripts", p.Name)
	}

	// Print the instructions for the agent/user to execute. This command
	// does the plugin's work for no one: the receipt says exactly that, and
	// the real result is recorded afterwards, by whoever did the work.
	fmt.Printf("%s Running plugin: %s\n", style.Success.Render("●"), p.Name)
	if pluginRunForce && !gateOpen {
		fmt.Printf("  %s\n", style.Dim.Render("(gate bypassed with --force)"))
	}
	fmt.Println()
	fmt.Printf("%s\n", style.Bold.Render("Instructions:"))
	fmt.Println(p.Instructions)

	recorder := plugin.NewRecorder(townRoot)
	beadID, err := recorder.RecordRun(plugin.PluginRunRecord{
		PluginName: p.Name,
		RigName:    p.RigName,
		Result:     plugin.ResultPrinted,
		Body:       "Manual run via gt plugin run: instructions printed, not executed. Record the real result with `gt plugin record-run --plugin " + p.Name + " --result <success|failure|skipped|warning>` once the work is done.",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to record run: %v\n", err)
	} else {
		fmt.Printf("\n%s Recorded run: %s\n", style.Dim.Render("●"), beadID)
		fmt.Println("This receipt says the instructions were printed, not executed. Record the real result after doing the work.")
	}

	return nil
}

func runPluginSync(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Determine source directory
	sourceDir := pluginSyncSource
	sourceRule := "explicit --source"
	if sourceDir == "" {
		src, err := plugin.FindGastownSource(townRoot)
		if err != nil {
			return err
		}
		sourceDir, sourceRule = src.Dir, src.Rule
	}

	// Resolve to absolute path
	if !filepath.IsAbs(sourceDir) {
		abs, err := filepath.Abs(sourceDir)
		if err != nil {
			return fmt.Errorf("resolving source path: %w", err)
		}
		sourceDir = abs
	}

	targetDir := filepath.Join(townRoot, "plugins")

	// Name the source and the rule that chose it on every run, including the
	// up-to-date one: which checkout supplies the plugins is what a sync from
	// the wrong directory gets silently wrong, and "already up to date" is
	// exactly where that hides (gt-nc7q).
	fmt.Printf("%s\n", style.Bold.Render("Plugin sync:"))
	fmt.Printf("  Source: %s (%s)\n", sourceDir, sourceRule)
	fmt.Printf("  Target: %s\n\n", targetDir)

	if pluginSyncDryRun {
		report, err := plugin.DetectDrift(sourceDir, targetDir)
		if err != nil {
			return fmt.Errorf("detecting drift: %w", err)
		}

		fmt.Printf("  %s\n\n", style.Dim.Render("dry run — nothing written"))

		if !report.HasDrift() && len(report.Extra) == 0 {
			fmt.Printf("  %s All plugins up to date\n", style.Success.Render("✓"))
			return nil
		}

		for _, d := range report.Drifted {
			fmt.Printf("  %s %s (content differs)\n", style.Warning.Render("~"), d.Name)
		}
		for _, name := range report.Missing {
			fmt.Printf("  %s %s (new, would be copied)\n", style.Success.Render("+"), name)
		}
		if pluginSyncClean {
			for _, name := range report.Extra {
				fmt.Printf("  %s %s (would be removed)\n", style.Error.Render("-"), name)
			}
		}
		return nil
	}

	result, err := plugin.SyncPluginsWithOptions(sourceDir, targetDir, plugin.SyncOptions{Clean: pluginSyncClean, Force: pluginSyncForce})
	if err != nil {
		return fmt.Errorf("syncing plugins: %w", err)
	}
	printPluginSyncResult(result)
	return reportProtectedPlugins(result.Protected)
}

// reportProtectedPlugins lists plugins the sync left untouched because their
// runtime copy holds edits the source repo lacks, and returns an error so
// callers (make install, rebuild-gt) see the drift instead of a success line.
func reportProtectedPlugins(protected map[string][]string) error {
	if len(protected) == 0 {
		return nil
	}
	names := make([]string, 0, len(protected))
	for name := range protected {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Fprintf(os.Stderr, "%s Left untouched: runtime edits the source repo does not have\n", style.Warning.Render("⚠"))
	for _, name := range names {
		for _, f := range protected[name] {
			fmt.Fprintf(os.Stderr, "  %s %s/%s\n", style.Warning.Render("!"), name, f)
		}
	}
	fmt.Fprintf(os.Stderr, "  Land these through the merge queue, or re-run with --force to discard them.\n")
	return fmt.Errorf("%d plugin(s) hold runtime edits and were not synced", len(protected))
}

func printPluginSyncResult(result *plugin.SyncResult) {
	if len(result.Copied) == 0 && len(result.Removed) == 0 && len(result.Protected) == 0 {
		fmt.Printf("%s Plugins already up to date (%d checked)\n",
			style.Success.Render("✓"), len(result.Skipped))
		return
	}

	fmt.Printf("%s Synced plugins\n", style.Success.Render("●"))
	for _, name := range result.Copied {
		fmt.Printf("  %s %s\n", style.Success.Render("↑"), name)
	}
	for _, name := range result.Removed {
		fmt.Printf("  %s %s\n", style.Error.Render("×"), name)
	}
	if len(result.Skipped) > 0 {
		fmt.Printf("  %s %d plugin(s) already current\n",
			style.Dim.Render("·"), len(result.Skipped))
	}
	for _, e := range result.Errors {
		fmt.Fprintf(os.Stderr, "  %s %s\n", style.Error.Render("!"), e)
	}
}

func runPluginHistory(cmd *cobra.Command, args []string) error {
	name := args[0]

	_, townRoot, err := getPluginScanner()
	if err != nil {
		return err
	}

	recorder := plugin.NewRecorder(townRoot)
	runs, err := recorder.GetRunsSince(name, "")
	if err != nil {
		return fmt.Errorf("querying history: %w", err)
	}

	if runs == nil {
		runs = []*plugin.PluginRunBead{}
	}

	// Apply limit
	if pluginHistoryLimit > 0 && len(runs) > pluginHistoryLimit {
		runs = runs[:pluginHistoryLimit]
	}

	if pluginHistoryJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(runs)
	}

	if len(runs) == 0 {
		fmt.Printf("%s No execution history for plugin: %s\n", style.Dim.Render("○"), name)
		return nil
	}

	fmt.Printf("%s Execution history for %s (%d runs)\n\n", style.Success.Render("●"), name, len(runs))

	for _, run := range runs {
		resultStyle, resultIcon := pluginHistoryGlyph(run.Result)

		fmt.Printf("  %s %s  %s\n",
			resultStyle.Render(resultIcon),
			run.CreatedAt.Format("2006-01-02 15:04"),
			style.Dim.Render(run.ID))
	}

	return nil
}

// pluginHistoryGlyph picks the icon and style `gt plugin history` renders for
// a run result. Warning gets its own glyph — it is what a run recorded when it
// found something and reported it, and folding it into the success checkmark
// would make it indistinguishable from a quiet, nothing-to-report run.
func pluginHistoryGlyph(result plugin.RunResult) (lipgloss.Style, string) {
	switch result {
	case plugin.ResultFailure:
		return style.Error, "✗"
	case plugin.ResultSkipped:
		return style.Dim, "○"
	case plugin.ResultWarning:
		return style.Warning, "!"
	default:
		return style.Success, "✓"
	}
}

func runPluginRecordRun(cmd *cobra.Command, args []string) error {
	if pluginRecordPlugin == "" {
		return fmt.Errorf("--plugin is required")
	}
	if pluginRecordResult == "" {
		return fmt.Errorf("--result is required")
	}

	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	recorder := plugin.NewRecorder(townRoot)
	beadID, err := recorder.RecordRun(plugin.PluginRunRecord{
		PluginName:  pluginRecordPlugin,
		RigName:     pluginRecordRig,
		Result:      plugin.RunResult(pluginRecordResult),
		Title:       pluginRecordTitle,
		Body:        pluginRecordBody,
		ExtraLabels: pluginRecordLabels,
	})
	if err != nil {
		return err
	}

	fmt.Fprintln(cmd.OutOrStdout(), beadID)
	return nil
}
