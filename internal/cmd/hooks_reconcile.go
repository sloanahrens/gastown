package cmd

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/hooks"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

// interimHookMarker identifies the mayor's interim host-hygiene PreToolUse
// hook (gt-nqcy) that 'gt hooks reconcile' removes.
const interimHookMarker = "no-root-scan.sh"

var hooksReconcileDryRun bool

var hooksReconcileCmd = &cobra.Command{
	Use:   "reconcile",
	Short: "One-time removal of the interim host-hygiene hook from settings.json files",
	Long: `Remove the mayor's interim PreToolUse hook (no-root-scan.sh) from every
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
// never carried the interim hook, reports unchanged rather than an error —
// LoadSettings already returns a usable zero-value config for a missing
// file, and stripInterimHook finds nothing to remove in it.
func reconcileTarget(target hooks.Target, dryRun bool) (changed bool, err error) {
	settings, err := hooks.LoadSettings(target.Path)
	if err != nil {
		return false, err
	}

	newEntries, removedAny := stripInterimHook(settings.Hooks.PreToolUse)
	if !removedAny {
		return false, nil
	}
	if dryRun {
		return true, nil
	}

	settings.Hooks.PreToolUse = newEntries
	data, err := marshalHooksOnly(settings)
	if err != nil {
		return false, fmt.Errorf("marshaling settings: %w", err)
	}

	if err := atomicfile.WriteFile(target.Path, data, 0o600); err != nil {
		return false, fmt.Errorf("writing settings: %w", err)
	}
	return true, nil
}

// marshalHooksOnly re-serializes settings with ONLY its "hooks" field
// replaced by the current settings.Hooks; every other top-level field is
// written back exactly as LoadSettings read it. This command's job is
// removing one hook entry, not the broader normalization
// hooks.MarshalSettings performs for 'gt hooks sync' (it unconditionally
// forces skipDangerousModePermissionPrompt, hasCompletedOnboarding, and
// permissions.defaultMode) — using that here would silently overwrite an
// operator's deliberately different permission settings on any file that
// happened to carry the interim hook.
func marshalHooksOnly(settings *hooks.SettingsJSON) ([]byte, error) {
	out := make(map[string]json.RawMessage, len(settings.Extra)+1)
	for k, v := range settings.Extra {
		out[k] = v
	}
	raw, err := json.Marshal(settings.Hooks)
	if err != nil {
		return nil, err
	}
	out["hooks"] = raw

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// stripInterimHook removes every Hook whose Command references
// interimHookMarker from entries, dropping an entry entirely if doing so
// leaves it with no hooks. Entries that never had any hooks (a bare
// matcher) pass through untouched — only an entry the removal actually
// emptied is dropped.
func stripInterimHook(entries []hooks.HookEntry) (result []hooks.HookEntry, removedAny bool) {
	for _, entry := range entries {
		var kept []hooks.Hook
		for _, h := range entry.Hooks {
			if strings.Contains(h.Command, interimHookMarker) {
				removedAny = true
				continue
			}
			kept = append(kept, h)
		}
		if len(kept) == 0 && len(entry.Hooks) > 0 {
			continue // every hook on this entry was the interim one — drop it
		}
		entry.Hooks = kept
		result = append(result, entry)
	}
	return result, removedAny
}
