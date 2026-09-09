package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
)

var (
	polecatIdentityReconcileApply      bool
	polecatIdentityReconcileDeleteOnly bool
	polecatIdentityReconcileID         string
)

var polecatIdentityReconcileCmd = &cobra.Command{
	Use:   "reconcile [<rig>/<name>]",
	Short: "Merge a legacy town copy of an agent bead into its rig row, archive, and delete it",
	Long: `Completes the gt-8we migration for ONE agent bead (gt-a6g).

Dry-run (default) prints a field table: rig value, town value, winner.
--apply then (1) writes the merged fields to the rig row, (2) appends the
full town row to <town>/.beads/archive/agent-bead-legacy.jsonl, (3) deletes
the town row, (4) re-reads both stores and fails loudly on any mismatch.
Run it one ID at a time from an operator session; never from a patrol.

The positional argument accepts:
  <rig>/<name>        polecat worker (backward compat default)
  <rig>/witness       rig-level witness singleton
  <rig>/refinery      rig-level refinery singleton
  <rig>/crew/<name>   named crew worker

Use --id to pass a raw agent bead ID directly (e.g. gt-gastown-witness),
bypassing <rig>/<name> parsing entirely — needed for legacy IDs the builder
above cannot reconstruct.

--delete-only skips the merge step entirely: archive + delete + verify only,
no fields are written to the rig row. This is applied automatically (with a
notice) when the town row's agent_state is 'nuked' — a nuked row is a dead
incarnation and carries no information about a (possibly respawned) live rig
row, so its fields must never be merged in by severity or recency (gt-1361).`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var arg string
		if len(args) == 1 {
			arg = args[0]
		}
		townRoot, err := findTownRoot()
		if err != nil {
			return err
		}
		id, err := resolveReconcileID(townRoot, polecatIdentityReconcileID, arg)
		if err != nil {
			return err
		}
		return runReconcile(os.Stdout, townRoot, id, polecatIdentityReconcileApply, polecatIdentityReconcileDeleteOnly)
	},
}

func init() {
	polecatIdentityReconcileCmd.Flags().BoolVar(&polecatIdentityReconcileApply, "apply", false, "perform the merge, archive and delete (default is dry-run)")
	polecatIdentityReconcileCmd.Flags().BoolVar(&polecatIdentityReconcileDeleteOnly, "delete-only", false, "skip the merge step; archive + delete + verify only")
	polecatIdentityReconcileCmd.Flags().StringVar(&polecatIdentityReconcileID, "id", "", "raw agent bead ID to reconcile, bypassing <rig>/<name> parsing (e.g. gt-gastown-witness)")
	polecatIdentityCmd.AddCommand(polecatIdentityReconcileCmd)
}

// resolveReconcileID builds the agent bead ID to reconcile from either the
// --id override or the <rig>/<name> positional argument.
//
// <rig>/<name> defaults to a polecat worker name for backward compatibility;
// "witness" and "refinery" resolve to the rig-level singleton instead, and
// "crew/<name>" resolves to a named crew worker. Legacy IDs that don't fit
// this shape (e.g. a collapsed prefix==rig singleton) must use --id.
func resolveReconcileID(townRoot, rawID, arg string) (string, error) {
	if rawID != "" {
		if arg != "" {
			return "", fmt.Errorf("--id and <rig>/<name> are mutually exclusive")
		}
		return rawID, nil
	}
	if arg == "" {
		return "", fmt.Errorf("expected <rig>/<name> or --id")
	}
	parts := strings.Split(arg, "/")
	rig := parts[0]
	if rig == "" {
		return "", fmt.Errorf("expected <rig>/<name>, got %q", arg)
	}
	prefix := beads.GetPrefixForRig(townRoot, rig)
	switch len(parts) {
	case 2:
		switch parts[1] {
		case constants.RoleWitness:
			return beads.WitnessBeadIDWithPrefix(prefix, rig), nil
		case constants.RoleRefinery:
			return beads.RefineryBeadIDWithPrefix(prefix, rig), nil
		default:
			return beads.PolecatBeadIDWithPrefix(prefix, rig, parts[1]), nil
		}
	case 3:
		if parts[1] != constants.RoleCrew || parts[2] == "" {
			return "", fmt.Errorf("expected <rig>/<name>, <rig>/witness, <rig>/refinery, or <rig>/crew/<name>, got %q", arg)
		}
		return beads.CrewBeadIDWithPrefix(prefix, rig, parts[2]), nil
	default:
		return "", fmt.Errorf("expected <rig>/<name>, <rig>/witness, <rig>/refinery, or <rig>/crew/<name>, got %q", arg)
	}
}

// runReconcile is the testable core of `gt polecat identity reconcile`.
func runReconcile(out io.Writer, townRoot, id string, apply, deleteOnly bool) error {
	townBeadsDir := beads.GetTownBeadsPath(townRoot)
	rigBeadsDir := beads.ResolveBeadsDirForID(townBeadsDir, id)
	if beads.ResolveBeadsDir(rigBeadsDir) == beads.ResolveBeadsDir(townBeadsDir) {
		return fmt.Errorf("%s routes to the town database; nothing to reconcile", id)
	}
	rigBd := beads.NewRigLocal(filepath.Dir(rigBeadsDir))
	townBd := beads.NewRigLocal(townRoot)

	// GetAgentBeadInStoreOnly, never GetAgentBead/Show: bd's own per-ID
	// commands (show, delete) fall back to routes.jsonl prefix routing
	// once an ID isn't found in the local store, silently substituting the
	// OTHER database's row. That is how a live rig agent bead was deleted
	// for real (gt-1361) — a wrong-store read looked like the town row,
	// reported "no differing fields", and the delete that followed hit the
	// rig row instead. GetAgentBeadInStoreOnly uses `bd list --id`, which
	// bd never prefix-routes.
	rigIssue, _, err := rigBd.GetAgentBeadInStoreOnly(id)
	if err != nil {
		return fmt.Errorf("reading rig row %s: %w", id, err)
	}
	if rigIssue == nil {
		return fmt.Errorf("%s has no rig row; run 'gt doctor --fix' (agent-beads-exist) first", id)
	}
	townIssue, _, err := townBd.GetAgentBeadInStoreOnly(id)
	if err != nil {
		return fmt.Errorf("reading town row %s: %w", id, err)
	}
	if townIssue == nil {
		return fmt.Errorf("%s has no legacy town row; nothing to reconcile", id)
	}
	// Belt-and-suspenders: two rows genuinely occupying different databases
	// never share both timestamps (they were created and last touched at
	// different times). If they match, the "town" read above resolved to
	// the exact same row as the rig read — refuse rather than risk merging
	// or deleting a row against itself.
	if rigIssue.CreatedAt == townIssue.CreatedAt && rigIssue.UpdatedAt == townIssue.UpdatedAt {
		return fmt.Errorf("refusing to reconcile %s: rig and town reads returned identical created_at/updated_at, meaning they resolved to the same row", id)
	}

	// A nuked town row is a dead incarnation: it carries no information
	// about the current (possibly respawned) rig row, so it must never be
	// merged in — by severity or by recency. Force delete-only (gt-1361).
	if fields := beads.ParseAgentFields(townIssue.Description); fields != nil && fields.AgentState == "nuked" && !deleteOnly {
		fmt.Fprintln(out, "town row is a nuked incarnation; fields ignored")
		deleteOnly = true
	}

	var updates beads.AgentFieldUpdates
	if !deleteOnly {
		exists := func(ref string) bool {
			_, err := beads.New(townRoot).Show(ref)
			return err == nil
		}
		var rows []beads.ReconcileRow
		updates, rows = beads.MergeLegacyAgentBead(rigIssue, townIssue, exists)

		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintf(tw, "field\trig\ttown\twinner\n")
		fmt.Fprintf(tw, "updated_at\t%v\t%v\t\n", rigIssue.UpdatedAt, townIssue.UpdatedAt)
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Field, orNull(r.Rig), orNull(r.Town), r.Winner)
		}
		if len(rows) == 0 {
			fmt.Fprintln(tw, "(no differing fields; --apply is archive+delete only)")
		}
		tw.Flush()
	} else {
		fmt.Fprintln(out, "delete-only: skipping merge; archive + delete + verify only.")
	}

	if !apply {
		fmt.Fprintln(out, "dry-run: nothing written. Re-run with --apply to merge, archive and delete.")
		return nil
	}

	if !deleteOnly && updates != (beads.AgentFieldUpdates{}) {
		if err := rigBd.UpdateAgentDescriptionFields(id, updates); err != nil {
			return fmt.Errorf("step 1/4 update rig row: %w", err)
		}
	}
	archivePath := filepath.Join(townBeadsDir, "archive", "agent-bead-legacy.jsonl")
	if err := appendJSONLine(archivePath, townIssue); err != nil {
		return fmt.Errorf("step 2/4 archive town row: %w", err)
	}
	if err := townBd.DeleteLegacyAgentBead(id); err != nil {
		return fmt.Errorf("step 3/4 delete town row (archived at %s): %w", archivePath, err)
	}
	// GetAgentBeadInStoreOnly, not Show: once the town row is gone, `bd
	// show` from the town directory falls back to routes.jsonl and finds
	// the rig row instead of reporting not-found, which would make this
	// verify falsely fail with "still exists" (gt-1361).
	if stillThere, _, _ := townBd.GetAgentBeadInStoreOnly(id); stillThere != nil {
		return fmt.Errorf("step 4/4 verify: town row %s still exists after delete", id)
	}
	after, _, err := rigBd.GetAgentBeadInStoreOnly(id)
	if err != nil || after == nil {
		return fmt.Errorf("step 4/4 verify: rig row %s unreadable after merge: %v", id, err)
	}
	fmt.Fprintf(out, "reconciled %s: rig row updated, town row archived to %s and deleted\n", id, archivePath)
	return nil
}

func orNull(s string) string {
	if s == "" {
		return "null"
	}
	return s
}

func appendJSONLine(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}
