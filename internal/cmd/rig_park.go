package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/townconfig"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	rigParkReason  string
	rigParkMigrate bool
)

var rigParkCmd = &cobra.Command{
	Use:   "park [<rig>...]",
	Short: "Park one or more rigs (dispatch and the daemon start nothing for them)",
	Long: `Park rigs to take them out of service.

Parking a rig writes a "parked" record {since, by, reason} into the rig's
entry in mayor/rigs.json. Dispatch (gt sling, convoys, gt dispatch) refuses
the rig and the daemon auto-starts nothing for it until 'gt rig unpark'.
A rig that is already parked keeps its original record.

gt rig park and gt rig unpark are the only writers of that record. Every
reader fails closed: if mayor/rigs.json (or any town config file) does not
load, every rig reads as parked.

--migrate moves park records left by older gt versions (a "status" value in
.beads-wisp/config/<rig>.json) into mayor/rigs.json and removes them. Until
it runs, such a rig reads as parked.

Examples:
  gt rig park gastown --reason "rubric rework"
  gt rig park beads gastown
  gt rig park --migrate`,
	RunE: runRigPark,
}

var rigUnparkCmd = &cobra.Command{
	Use:   "unpark <rig>...",
	Short: "Unpark one or more rigs (allow dispatch and daemon auto-start)",
	Long: `Unpark rigs to resume normal operation.

Unparking a rig removes its "parked" record from mayor/rigs.json (and any
legacy record in .beads-wisp/config/<rig>.json). It does NOT start agents;
use 'gt rig start' for that.

Examples:
  gt rig unpark gastown
  gt rig unpark beads gastown`,
	Args: cobra.MinimumNArgs(1),
	RunE: runRigUnpark,
}

func init() {
	rigParkCmd.Flags().StringVar(&rigParkReason, "reason", "", "Why the rig is parked (recorded in mayor/rigs.json)")
	rigParkCmd.Flags().BoolVar(&rigParkMigrate, "migrate", false, "Move legacy .beads-wisp park records into mayor/rigs.json")
	rigCmd.AddCommand(rigParkCmd)
	rigCmd.AddCommand(rigUnparkCmd)
}

func runRigPark(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	if rigParkMigrate {
		if len(args) > 0 {
			return fmt.Errorf("--migrate takes no rig names: it migrates every registered rig")
		}
		return migrateParkedRigs(os.Stdout, townRoot, detectActor())
	}
	if len(args) == 0 {
		return fmt.Errorf("name at least one rig to park (or pass --migrate)")
	}
	rec := config.RigParked{Since: time.Now().UTC(), By: detectActor(), Reason: rigParkReason}
	return parkRigs(os.Stdout, townRoot, args, rec)
}

func runRigUnpark(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	return unparkRigs(os.Stdout, townRoot, args)
}

// parkRigs writes rec as each rig's park record.
func parkRigs(w io.Writer, townRoot string, rigNames []string, rec config.RigParked) error {
	failed := 0
	for _, rigName := range rigNames {
		kept, err := townconfig.Park(townRoot, rigName, rec)
		if err != nil {
			fmt.Fprintf(w, "%s %s: %v\n", style.Error.Render("✗"), rigName, err)
			failed++
			continue
		}
		fmt.Fprintf(w, "%s Rig %s %s\n", style.Success.Render("✓"), rigName, townconfig.Describe(&kept))
	}
	if failed > 0 {
		return fmt.Errorf("failed to park %d rig(s)", failed)
	}
	return nil
}

// unparkRigs clears each rig's park record.
func unparkRigs(w io.Writer, townRoot string, rigNames []string) error {
	failed := 0
	for _, rigName := range rigNames {
		was, err := townconfig.Unpark(townRoot, rigName)
		if err != nil {
			fmt.Fprintf(w, "%s %s: %v\n", style.Error.Render("✗"), rigName, err)
			failed++
			continue
		}
		if !was {
			fmt.Fprintf(w, "%s Rig %s was not parked\n", style.Dim.Render("○"), rigName)
			continue
		}
		fmt.Fprintf(w, "%s Rig %s unparked\n", style.Success.Render("✓"), rigName)
		fmt.Fprintf(w, "  Use '%s' to start agents now\n", style.Dim.Render("gt rig start "+rigName))
	}
	if failed > 0 {
		return fmt.Errorf("failed to unpark %d rig(s)", failed)
	}
	return nil
}

// migrateParkedRigs moves legacy wisp park records into mayor/rigs.json.
func migrateParkedRigs(w io.Writer, townRoot, by string) error {
	migrated, err := townconfig.MigrateLegacyParked(townRoot, by)
	if len(migrated) > 0 {
		fmt.Fprintf(w, "%s Migrated park records into mayor/rigs.json: %s\n", style.Success.Render("✓"), strings.Join(migrated, ", "))
	} else if err == nil {
		fmt.Fprintf(w, "%s No legacy park records to migrate\n", style.Dim.Render("○"))
	}
	return err
}

// IsRigParked reports whether a rig is parked, failing closed: a rig whose
// park state cannot be read is parked. The record lives in mayor/rigs.json
// and is read through the config kernel (townconfig.IsParked).
func IsRigParked(townRoot, rigName string) bool {
	parked, _ := townconfig.IsParked(townRoot, rigName)
	return parked
}
