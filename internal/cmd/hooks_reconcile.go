package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/hooks"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// interimHookMarker identifies the interim host-hygiene PreToolUse hook
// (gt-nqcy) that 'gt hooks reconcile' removes.
const interimHookMarker = "no-root-scan.sh"

var hooksReconcileDryRun bool

var hooksReconcileCmd = &cobra.Command{
	Use:   "reconcile",
	Short: "One-time removal of the interim host-hygiene hook from settings.json files",
	Long: `Remove the interim host-hygiene PreToolUse hook (no-root-scan.sh) from every
managed .claude/settings.json file that still carries it (gt-nqcy).

'gt hooks sync' does not remove it — the interim entry lives outside the
managed base/override config sync regenerates from, so it survives every
ordinary sync untouched.

Only hook entries whose command references no-root-scan.sh are removed;
every other hook (including a legitimate operator override) is left alone,
and every other settings.json field is written back unchanged.

Examples:
  gt hooks reconcile             # Remove the interim hook from every target
  gt hooks reconcile --dry-run   # Show which files would change`,
	RunE: runHooksReconcile,
}

func init() {
	hooksCmd.AddCommand(hooksReconcileCmd)
	hooksReconcileCmd.Flags().BoolVar(&hooksReconcileDryRun, "dry-run", false, "Show what would change without writing")
}

func runHooksReconcile(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	targets, err := hooks.DiscoverTargets(townRoot)
	if err != nil {
		return fmt.Errorf("discovering targets: %w", err)
	}

	if hooksReconcileDryRun {
		fmt.Println("Dry run - showing which settings.json files carry the interim hook...")
	} else {
		fmt.Println("Reconciling interim host-hygiene hook...")
	}
	fmt.Println()

	removed := 0
	unchanged := 0
	errCount := 0

	for _, target := range targets {
		changed, err := reconcileTarget(target, hooksReconcileDryRun)
		if err != nil {
			fmt.Printf("  %s %s: %v\n", style.Error.Render("✖"), target.DisplayKey(), err)
			errCount++
			continue
		}
		if !changed {
			unchanged++
			continue
		}

		relPath, pathErr := filepath.Rel(townRoot, target.Path)
		if pathErr != nil {
			relPath = target.Path
		}
		if hooksReconcileDryRun {
			fmt.Printf("  %s %s %s\n", style.Warning.Render("~"), relPath, style.Dim.Render("(would remove interim hook)"))
		} else {
			fmt.Printf("  %s %s %s\n", style.Success.Render("✓"), relPath, style.Dim.Render("(removed interim hook)"))
		}
		removed++
	}

	fmt.Println()
	verb := "Removed"
	if hooksReconcileDryRun {
		verb = "Would remove"
	}
	fmt.Printf("%s the interim hook from %d of %d target(s) (%d unchanged", verb, removed, len(targets), unchanged)
	if errCount > 0 {
		fmt.Printf(", %s", style.Error.Render(fmt.Sprintf("%d errors", errCount)))
	}
	fmt.Println(")")

	if errCount > 0 {
		return fmt.Errorf("hooks reconcile failed: %d target(s) had errors", errCount)
	}
	return nil
}

// reconcileTarget removes any PreToolUse hook referencing interimHookMarker
// from target's settings.json. Returns whether the file has (or, in a dry
// run, would have) changed. A target with no settings.json yet, or one that
// never carried the interim hook, reports unchanged rather than an error.
//
// The file is edited as raw JSON, not through hooks.HooksConfig: that typed
// struct has no field for events such as Notification or SubagentStop and
// hooks.Hook has none for keys such as timeout, so a typed round-trip would
// silently drop both. Only the PreToolUse array is replaced; every other
// top-level field, event, entry and hook key is written back as read. This
// is also why it does not use hooks.MarshalSettings, the normalization 'gt
// hooks sync' performs (it forces skipDangerousModePermissionPrompt,
// hasCompletedOnboarding and permissions.defaultMode, which would overwrite
// an operator's deliberately different settings).
func reconcileTarget(target hooks.Target, dryRun bool) (changed bool, err error) {
	data, err := os.ReadFile(target.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return false, &hooks.SettingsIntegrityError{Path: target.Path, Err: err}
	}
	rawHooks, ok := top["hooks"]
	if !ok {
		return false, nil
	}
	var events map[string]json.RawMessage
	if err := json.Unmarshal(rawHooks, &events); err != nil {
		return false, &hooks.SettingsIntegrityError{Path: target.Path, Err: fmt.Errorf("unmarshaling hooks: %w", err)}
	}
	rawPre, ok := events["PreToolUse"]
	if !ok {
		return false, nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(rawPre, &entries); err != nil {
		return false, &hooks.SettingsIntegrityError{Path: target.Path, Err: fmt.Errorf("unmarshaling hooks.PreToolUse: %w", err)}
	}

	newEntries, removedAny := stripInterimHook(entries)
	if !removedAny {
		return false, nil
	}
	if dryRun {
		return true, nil
	}

	if len(newEntries) == 0 {
		delete(events, "PreToolUse")
	} else if events["PreToolUse"], err = json.Marshal(newEntries); err != nil {
		return false, fmt.Errorf("marshaling settings: %w", err)
	}
	if top["hooks"], err = json.Marshal(events); err != nil {
		return false, fmt.Errorf("marshaling settings: %w", err)
	}
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return false, fmt.Errorf("marshaling settings: %w", err)
	}

	if err := atomicfile.WriteFile(target.Path, append(out, '\n'), 0o600); err != nil {
		return false, fmt.Errorf("writing settings: %w", err)
	}
	return true, nil
}

// stripInterimHook removes every hook whose command references
// interimHookMarker from the raw PreToolUse entries, dropping an entry
// entirely if doing so leaves it with no hooks. An entry the removal did not
// touch (one with no interim hook, no hooks at all, or one that does not
// parse) passes through byte-for-byte; a touched entry keeps every key it
// had, only its "hooks" array shrinking.
func stripInterimHook(entries []json.RawMessage) (result []json.RawMessage, removedAny bool) {
	for _, entry := range entries {
		kept, removed := stripInterimFromEntry(entry)
		if !removed {
			result = append(result, entry)
			continue
		}
		removedAny = true
		if kept != nil {
			result = append(result, kept)
		}
	}
	return result, removedAny
}

// stripInterimFromEntry returns entry without its interim hooks. removed
// reports whether any hook was dropped; kept is nil when that left the
// entry with no hooks, so the caller drops it.
func stripInterimFromEntry(entry json.RawMessage) (kept json.RawMessage, removed bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(entry, &fields) != nil {
		return entry, false
	}
	var hookList []json.RawMessage
	if json.Unmarshal(fields["hooks"], &hookList) != nil {
		return entry, false
	}

	var keptHooks []json.RawMessage
	for _, h := range hookList {
		var cmd struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(h, &cmd) == nil && strings.Contains(cmd.Command, interimHookMarker) {
			removed = true
			continue
		}
		keptHooks = append(keptHooks, h)
	}
	if !removed {
		return entry, false
	}
	if len(keptHooks) == 0 {
		return nil, true // every hook on this entry was the interim one
	}

	var err error
	if fields["hooks"], err = json.Marshal(keptHooks); err != nil {
		return entry, false
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return entry, false
	}
	return out, true
}
