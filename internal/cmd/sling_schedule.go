package cmd

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/scheduler/capacity"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// shouldDeferDispatch checks the town config to decide dispatch mode.
// Returns (true, nil) when max_polecats > 0 (deferred dispatch).
// Returns (false, nil) when max_polecats <= 0 (direct dispatch).
func shouldDeferDispatch() (bool, error) {
	return shouldDeferDispatchWith(workspace.FindFromCwd, config.LoadOrCreateTownSettings)
}

// shouldDeferDispatchWith is shouldDeferDispatch with the town found by
// findTown and its settings read by loadSettings.
func shouldDeferDispatchWith(findTown func() (string, error), loadSettings func(path string) (*config.TownSettings, error)) (bool, error) {
	townRoot, err := findTown()
	if err != nil {
		// FindFromCwd errs only when the cwd itself is unreadable — "no
		// town" is an empty root below. Reading that as "direct dispatch"
		// would bypass a configured scheduler cap (gt-udrrw, gt-bfale).
		return false, fmt.Errorf("finding town root: %w (dispatch blocked — run from a readable directory)", err)
	}
	if townRoot == "" {
		return false, nil // No town — direct dispatch
	}

	settingsPath := config.TownSettingsPath(townRoot)
	settings, err := loadSettings(settingsPath)
	if err != nil {
		return false, fmt.Errorf("loading town settings: %w (dispatch blocked — fix config or use gt config set scheduler.max_polecats -1)", err)
	}

	schedulerCfg := settings.Scheduler
	if schedulerCfg == nil {
		return false, nil // No scheduler config — direct dispatch (default)
	}

	maxPol := schedulerCfg.GetMaxPolecats()
	if maxPol > 0 {
		return true, nil
	}
	return false, nil // -1 or 0 = direct dispatch
}

// ScheduleOptions holds options for scheduling a bead.
type ScheduleOptions struct {
	Formula      string   // Formula to apply at dispatch time (e.g., "mol-polecat-work")
	Args         string   // Natural language args for executor
	Vars         []string // Formula variables (key=value)
	BaseBranch   string   // Override base branch for polecat worktree
	ResumeBranch string   // Resume an existing branch (gh#3602); mutually exclusive with BaseBranch
	DryRun       bool     // Show what would be done without acting
	Force        bool     // Force schedule even if bead is hooked/in_progress
	NoMerge      bool     // Skip merge queue on completion
	ReviewOnly   bool     // Review-only mode: assignee evaluates and reports back, no merge/commit/push
	Account      string   // Claude Code account handle
	Agent        string   // Agent override (e.g., "claude-haiku")
	HookRawBead  bool     // Hook raw bead without default formula
	Ralph        bool     // Ralph Wiggum loop mode
}

// slingContextStore is the slice of a rig's beads a scheduled bead's sling
// context lives in: the Client primitives internal/beads' sling-context
// helpers call.
type slingContextStore = beads.SlingContextStore

// scheduleBead schedules a bead with the running gt's collaborators.
func scheduleBead(beadID, rigName string, opts ScheduleOptions) error {
	return realSlingDeps().scheduleSlingBead(beadID, rigName, opts)
}

// scheduleSlingBead schedules a bead for deferred dispatch via the capacity scheduler.
// Creates a sling context bead to hold scheduling state. The work bead is never modified.
func (d *slingDeps) scheduleSlingBead(beadID, rigName string, opts ScheduleOptions) error {
	townRoot, err := d.townOrEnv()
	if err != nil {
		return err
	}

	if err := d.verifyBead(beadID); err != nil {
		return fmt.Errorf("bead '%s' not found", beadID)
	}

	if _, isRig := d.isRigName(rigName); !isRig {
		return fmt.Errorf("'%s' is not a known rig", rigName)
	}
	if err := d.verifyInTargetRig(beadID, rigName, townRoot); err != nil {
		return err
	}

	if !opts.Force {
		if err := d.crossRigGuard(beadID, rigName+"/polecats/_", townRoot); err != nil {
			return err
		}
	}

	info, err := d.beadInfo(beadID)
	if err != nil {
		return fmt.Errorf("checking bead status: %w", err)
	}

	// Idempotency: check for existing open sling context for this work bead.
	// Fail fast on errors to avoid creating duplicate contexts on transient DB failures.
	//
	// Create the sling context in the target rig's beads dir so that the target
	// rig's witness can discover it during patrol. Previously this used the HQ
	// beads dir, which meant non-HQ rig witnesses never saw the context. (GH#3468)
	rigBeadsDir, ok := d.rigBeadsDir(townRoot, rigName)
	if !ok {
		return fmt.Errorf("cannot resolve target rig %q beads database for bead %s", rigName, beadID)
	}
	rigBeads := d.slingContexts(rigBeadsDir)
	existingCtx, _, findErr := beads.FindOpenSlingContext(rigBeads, beadID)
	if findErr != nil {
		return fmt.Errorf("checking for existing sling context: %w", findErr)
	}
	if existingCtx != nil {
		fmt.Fprintf(d.out, "%s Bead %s is already scheduled (context: %s), no-op\n",
			style.Dim.Render("○"), beadID, existingCtx.ID)
		return nil
	}

	// Guard against scheduling closed/tombstone beads (defense-in-depth, hq-ki2).
	// Mirrors the closed-bead guards in runSling (sling.go) and executeSling
	// (sling_dispatch.go). Deferred dispatch can route a closed cross-prefix
	// bead through scheduleBead; without this check, a dispatch is scheduled for
	// already-completed work. Not bypassed by --force — if you need to
	// re-dispatch, reopen the bead first.
	if info.Status == "closed" || info.Status == "tombstone" {
		return fmt.Errorf("bead %s is %s (work already completed)", beadID, info.Status)
	}

	if (info.Status == "pinned" || info.Status == "hooked" || info.Status == "in_progress") && !opts.Force {
		return fmt.Errorf("bead %s is already %s to %s\nUse --force to override", beadID, info.Status, info.Assignee)
	}

	if opts.Formula != "" {
		if err := d.verifyFormula(opts.Formula, filepath.Dir(rigBeadsDir), townRoot); err != nil {
			return fmt.Errorf("formula %q not found: %w", opts.Formula, err)
		}
	}

	if opts.DryRun {
		fmt.Fprintf(d.out, "Would schedule %s → %s\n", beadID, rigName)
		fmt.Fprintf(d.out, "  Would create sling context bead\n")
		return nil
	}

	// Cook formula after dry-run check to avoid side effects
	if opts.Formula != "" {
		workDir := d.hookDir(townRoot, beadID, "")
		if err := d.cook(opts.Formula, workDir, townRoot); err != nil {
			return fmt.Errorf("formula %q failed to cook: %w", opts.Formula, err)
		}
	}

	// Build sling context fields
	fields := &capacity.SlingContextFields{
		Version:    1,
		WorkBeadID: beadID,
		TargetRig:  rigName,
		EnqueuedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if opts.Formula != "" {
		fields.Formula = opts.Formula
	}
	if opts.Args != "" {
		fields.Args = opts.Args
	}
	if len(opts.Vars) > 0 {
		fields.Vars = strings.Join(opts.Vars, "\n")
	}
	if opts.BaseBranch != "" {
		fields.BaseBranch = opts.BaseBranch
	}
	if opts.ResumeBranch != "" {
		fields.ResumeBranch = opts.ResumeBranch
	}
	fields.NoMerge = opts.NoMerge
	fields.ReviewOnly = opts.ReviewOnly
	if opts.Account != "" {
		fields.Account = opts.Account
	}
	if opts.Agent != "" {
		fields.Agent = opts.Agent
	}
	fields.HookRawBead = opts.HookRawBead
	if opts.Ralph {
		fields.Mode = "ralph"
	}

	// Create sling context bead in the target rig's beads dir so the rig's
	// witness discovers it during patrol. (GH#3468)
	ctxBead, err := beads.CreateSlingContext(rigBeads, info.Title, beadID, fields)
	if err != nil {
		return fmt.Errorf("creating sling context: %w", err)
	}

	actor := d.actor()
	_ = d.logFeed(events.TypeSchedulerEnqueue, actor, events.SchedulerEnqueuePayload(beadID, rigName))

	fmt.Fprintf(d.out, "%s Scheduled %s → %s (context: %s)\n", style.Bold.Render("✓"), beadID, rigName, ctxBead.ID)
	return nil
}

// runBatchSchedule schedules multiple beads for deferred dispatch.
// Returns error when all schedule attempts fail.
func runBatchSchedule(beadIDs []string, rigName, townRoot string) error {
	return runBatchScheduleWith(slingOptionsFromFlags(), beadIDs, rigName, townRoot)
}

// runBatchScheduleWith is runBatchSchedule with opts in place of the flags.
func runBatchScheduleWith(o slingOptions, beadIDs []string, rigName, townRoot string) error {
	if o.dryRun {
		fmt.Printf("%s Would schedule %d beads to rig '%s':\n", style.Bold.Render("📋"), len(beadIDs), rigName)
		for _, beadID := range beadIDs {
			fmt.Printf("  Would schedule: %s → %s\n", beadID, rigName)
		}
		return nil
	}

	fmt.Printf("%s Scheduling %d beads to rig '%s'...\n", style.Bold.Render("📋"), len(beadIDs), rigName)

	successCount := 0
	for _, beadID := range beadIDs {
		formula := resolveFormula(o.formula, o.hookRawBead, townRoot, rigName)
		err := scheduleBead(beadID, rigName, ScheduleOptions{
			Formula:      formula,
			Args:         o.argsText,
			Vars:         o.vars,
			BaseBranch:   o.baseBranch,
			ResumeBranch: o.resumeBranch,
			DryRun:       false,
			Force:        o.force,
			NoMerge:      o.noMerge,
			ReviewOnly:   o.reviewOnly,
			Account:      o.account,
			Agent:        o.agent,
			HookRawBead:  o.hookRawBead,
			Ralph:        o.ralph,
		})
		if err != nil {
			fmt.Printf("  %s %s: %v\n", style.Dim.Render("✗"), beadID, err)
			continue
		}
		successCount++
	}

	fmt.Printf("\n%s Scheduled %d/%d beads\n", style.Bold.Render("📊"), successCount, len(beadIDs))
	if successCount == 0 {
		return fmt.Errorf("all %d schedule attempts failed", len(beadIDs))
	}
	return nil
}

// resolveRigForBead determines the rig that owns a bead from its ID prefix.
func resolveRigForBead(townRoot, beadID string) string {
	prefix := beads.ExtractPrefix(beadID)
	if prefix == "" {
		return ""
	}
	return beads.GetRigNameForPrefix(townRoot, prefix)
}

// resolveFormula determines the formula name from user flags and rig settings.
// Resolution order:
//  1. Explicit --formula flag
//  2. Rig property layers (wisp → bead → system default "mol-polecat-work")
//  3. Rig settings file (workflow.default_formula in settings/config.json)
//  4. Hardcoded fallback "mol-polecat-work"
//
// The property layers are the primary mechanism, supporting:
//
//	gt rig config set <rig> default_formula mol-evolve         # wisp layer
//	gt rig config set <rig> default_formula mol-evolve --global # bead layer
func resolveFormula(explicit string, hookRawBead bool, townRoot, rigName string) string {
	if hookRawBead {
		return ""
	}
	if explicit != "" {
		return explicit
	}
	// Check rig property layers: wisp → bead → system default (issue gt-y18).
	if townRoot != "" && rigName != "" {
		r := townRigBD(townRoot, &rig.Rig{
			Name: rigName,
			Path: filepath.Join(townRoot, rigName),
		})
		if df := r.GetStringConfig("default_formula"); df != "" {
			return df
		}
	}
	// Fallback: check rig settings file (legacy path, issue gt-boc).
	if townRoot != "" && rigName != "" {
		rigPath := filepath.Join(townRoot, rigName)
		if df := config.GetDefaultFormula(rigPath); df != "" {
			return df
		}
	}
	return "mol-polecat-work"
}

// areScheduled returns a set of bead IDs that have open sling contexts.
// Scans all rig beads dirs since sling contexts are created in the target
// rig's beads dir (GH#3468). On error, fails closed: treats ALL requested
// beads as scheduled to prevent false stranded detection and duplicate
// scheduling attempts.
func areScheduled(beadIDs []string) map[string]bool {
	return areScheduledForTown("", beadIDs)
}

// areScheduledForTown is areScheduled pinned to townRoot instead of
// discovering the town ambiently from cwd. Callers that already hold an
// explicit town root must pass it through — otherwise this falls back to
// ambient/cwd discovery, which can silently diverge from the caller's town
// under test isolation or multi-town use (gt-80o).
func areScheduledForTown(townRoot string, beadIDs []string) map[string]bool {
	return areScheduledWith(townRoot, beadIDs, workspace.FindFromCwd, beads.AreScheduled)
}

// areScheduledWith is areScheduledForTown with the cwd's town found by
// findTown and the sling contexts read by lookup.
func areScheduledWith(townRoot string, beadIDs []string, findTown func() (string, error), lookup func(townRoot string, beadIDs []string) map[string]bool) map[string]bool {
	result := make(map[string]bool)
	if len(beadIDs) == 0 {
		return result
	}

	if townRoot == "" {
		found, err := findTown()
		if err != nil || found == "" {
			// Can't determine town root — fail closed (treat all as scheduled)
			for _, id := range beadIDs {
				result[id] = true
			}
			return result
		}
		townRoot = found
	}

	return lookup(townRoot, beadIDs)
}

// isScheduled checks if a single bead has an open sling context.
// For batch checks in loops, use areScheduled() instead.
func isScheduled(beadID string) bool {
	scheduled := areScheduled([]string{beadID})
	return scheduled[beadID]
}

// detectSchedulerIDType determines what kind of ID was passed for scheduling.
// Returns "epic" or "task". Convoys were retired (gt-gzhin.7): a convoy is no
// longer schedulable, so its IDs fall through to the task path.
func detectSchedulerIDType(id string) (string, error) {
	info, err := getBeadInfo(id)
	if err != nil {
		return "", fmt.Errorf("cannot resolve bead '%s': %w", id, err)
	}

	switch info.IssueType {
	case "epic":
		return "epic", nil
	}

	for _, label := range info.Labels {
		switch label {
		case "gt:epic":
			return "epic", nil
		}
	}

	return "task", nil
}

// schedulerTaskOnlyFlagNames lists flags that only apply to task bead scheduling,
// not epic mode.
var schedulerTaskOnlyFlagNames = []string{
	"account", "agent", "ralph", "args", "var",
	"base-branch", "no-merge", "review-only",
}

// validateNoTaskOnlySchedulerFlags checks that no task-only flags were set.
func validateNoTaskOnlySchedulerFlags(cmd *cobra.Command, mode string) error {
	if cmd == nil {
		return nil
	}
	var used []string
	for _, name := range schedulerTaskOnlyFlagNames {
		if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
			used = append(used, "--"+name)
		}
	}
	if len(used) > 0 {
		return fmt.Errorf("%s mode does not support: %s\nThese flags only apply to task bead scheduling",
			mode, strings.Join(used, ", "))
	}
	return nil
}
