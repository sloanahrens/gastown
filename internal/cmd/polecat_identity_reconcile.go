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
)

var polecatIdentityReconcileApply bool

var polecatIdentityReconcileCmd = &cobra.Command{
	Use:   "reconcile <rig>/<name>",
	Short: "Merge a legacy town copy of an agent bead into its rig row, archive, and delete it",
	Long: `Completes the gt-8we migration for ONE agent bead (gt-a6g).

Dry-run (default) prints a field table: rig value, town value, winner.
--apply then (1) writes the merged fields to the rig row, (2) appends the
full town row to <town>/.beads/archive/agent-bead-legacy.jsonl, (3) deletes
the town row, (4) re-reads both stores and fails loudly on any mismatch.
Run it one ID at a time from an operator session; never from a patrol.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		rigName, name, ok := strings.Cut(args[0], "/")
		if !ok {
			return fmt.Errorf("expected <rig>/<name>, got %q", args[0])
		}
		townRoot, err := findTownRoot()
		if err != nil {
			return err
		}
		return runReconcile(os.Stdout, townRoot, rigName, name, polecatIdentityReconcileApply)
	},
}

func init() {
	polecatIdentityReconcileCmd.Flags().BoolVar(&polecatIdentityReconcileApply, "apply", false, "perform the merge, archive and delete (default is dry-run)")
	polecatIdentityCmd.AddCommand(polecatIdentityReconcileCmd)
}

// runReconcile is the testable core of `gt polecat identity reconcile`.
func runReconcile(out io.Writer, townRoot, rigName, name string, apply bool) error {
	id := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(townRoot, rigName), rigName, name)
	townBeadsDir := beads.GetTownBeadsPath(townRoot)
	rigBeadsDir := beads.ResolveBeadsDirForID(townBeadsDir, id)
	if beads.ResolveBeadsDir(rigBeadsDir) == beads.ResolveBeadsDir(townBeadsDir) {
		return fmt.Errorf("%s routes to the town database; nothing to reconcile", id)
	}
	rigBd := beads.NewRigLocal(filepath.Dir(rigBeadsDir))
	townBd := beads.NewRigLocal(townRoot)

	rigIssue, _, err := rigBd.GetAgentBead(id)
	if err != nil {
		return fmt.Errorf("reading rig row %s: %w", id, err)
	}
	if rigIssue == nil {
		return fmt.Errorf("%s has no rig row; run 'gt doctor --rig %s --fix' (agent-beads-exist) first", id, rigName)
	}
	townIssue, _, err := townBd.GetAgentBead(id)
	if err != nil {
		return fmt.Errorf("reading town row %s: %w", id, err)
	}
	if townIssue == nil {
		return fmt.Errorf("%s has no legacy town row; nothing to reconcile", id)
	}

	exists := func(ref string) bool {
		_, err := beads.New(townRoot).Show(ref)
		return err == nil
	}
	updates, rows := beads.MergeLegacyAgentBead(rigIssue, townIssue, exists)

	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "field\trig(%s)\ttown\twinner\n", rigName)
	fmt.Fprintf(tw, "updated_at\t%v\t%v\t\n", rigIssue.UpdatedAt, townIssue.UpdatedAt)
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Field, orNull(r.Rig), orNull(r.Town), r.Winner)
	}
	if len(rows) == 0 {
		fmt.Fprintln(tw, "(no differing fields; --apply is archive+delete only)")
	}
	tw.Flush()
	if !apply {
		fmt.Fprintln(out, "dry-run: nothing written. Re-run with --apply to merge, archive and delete.")
		return nil
	}

	if updates != (beads.AgentFieldUpdates{}) {
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
	if _, err := townBd.Show(id); err == nil {
		return fmt.Errorf("step 4/4 verify: town row %s still exists after delete", id)
	}
	after, _, err := rigBd.GetAgentBead(id)
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
