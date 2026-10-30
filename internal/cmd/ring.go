// The One's verbs: gt ring forge, renew, status and remove (gt-6mlik.2,
// claude-aq9.5). The Ring is the town's signed lease in mayor/town.json. forge
// writes a new one, renew extends a standing one, status reads it and remove
// deletes it. Each verb signs or verifies under the key settings/config.json's
// ring.key names, and none of them prints that key.
package cmd

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/ring"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Exit codes gt ring status reports: a valid Ring exits 0, a waned one 2, and
// an absent one 3.
const (
	ringWanedExit  = 2
	ringAbsentExit = 3
)

// ringDeps is what a Ring verb reads from outside the process: the town, the
// environment its key reference resolves through, the instant it judges the
// Ring at, and the session actor that forges a record.
type ringDeps struct {
	townRoot string
	getenv   func(string) string
	now      time.Time
	actor    string
}

// key resolves the signing key. A town with no resolvable key is refused
// before any verb reads or writes the Ring.
func (d ringDeps) key() ([]byte, error) {
	settings, err := config.LoadTownSettings(config.TownSettingsPath(d.townRoot))
	if err != nil {
		return nil, fmt.Errorf("reading settings/config.json: %w", err)
	}
	key, err := settings.RingKeyFrom(d.getenv)
	if err != nil {
		return nil, err
	}
	return []byte(key), nil
}

// lease signs and stores a record forged now by the session actor, valid for
// one ring.Lease from now.
func (d ringDeps) lease(key []byte) (ring.Record, error) {
	rec, err := ring.Sign(ring.Record{ForgedAt: d.now, ExpiresAt: d.now.Add(ring.Lease), ForgedBy: d.actor}, key)
	if err != nil {
		return ring.Record{}, err
	}
	if err := ring.Write(d.townRoot, rec); err != nil {
		return ring.Record{}, err
	}
	return rec, nil
}

func runRingForge(d ringDeps, out io.Writer) error {
	key, err := d.key()
	if err != nil {
		return err
	}
	return d.writeLease(key, out, "forged")
}

func runRingRenew(d ringDeps, out io.Writer) error {
	key, err := d.key()
	if err != nil {
		return err
	}
	st, err := ring.Read(d.townRoot, key, d.now)
	if err != nil {
		return fmt.Errorf("reading the Ring: %w", err)
	}
	if st.Kind == ring.Absent {
		return fmt.Errorf("no Ring; forge it (%s)", st.Reason)
	}
	return d.writeLease(key, out, "renewed")
}

// writeLease stores a lease forged now and reports its new expiry as verb.
func (d ringDeps) writeLease(key []byte, out io.Writer, verb string) error {
	rec, err := d.lease(key)
	if err != nil {
		return fmt.Errorf("writing the Ring: %w", err)
	}
	fmt.Fprintf(out, "%s: the Ring is valid until %s\n", verb, rec.ExpiresAt.Format(time.RFC3339))
	return nil
}

func runRingStatus(d ringDeps, out io.Writer) error {
	key, err := d.key()
	if err != nil {
		return err
	}
	st, err := ring.Read(d.townRoot, key, d.now)
	if err != nil {
		return fmt.Errorf("reading the Ring: %w", err)
	}
	fmt.Fprintf(out, "ring: %s\n", st)
	switch st.Kind {
	case ring.Valid:
		fmt.Fprintf(out, "wanes in %s\n", ringSpan(st.Record.ExpiresAt.Sub(d.now)))
		return nil
	case ring.Waned:
		fmt.Fprintf(out, "waned %s ago\n", ringSpan(d.now.Sub(st.Record.ExpiresAt)))
		return NewSilentExit(ringWanedExit)
	default:
		return NewSilentExit(ringAbsentExit)
	}
}

func runRingRemove(d ringDeps, out io.Writer) error {
	if _, err := d.key(); err != nil {
		return err
	}
	removed, err := ring.Remove(d.townRoot)
	if err != nil {
		return fmt.Errorf("removing the Ring: %w", err)
	}
	if !removed {
		fmt.Fprintln(out, "no Ring to remove")
		return nil
	}
	fmt.Fprintln(out, "removed the Ring")
	return nil
}

// ringSpan formats d to the minute as hours and minutes, e.g. "35h59m".
func ringSpan(d time.Duration) string {
	d = d.Round(time.Minute)
	return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int((d%time.Hour)/time.Minute))
}

// ringVerb is the cobra RunE for a Ring verb. It finds the town with find
// (workspace.FindFromCwdOrError in production), takes the process environment,
// the wall clock and the session's actor, and hands the verb the command's
// stdout.
func ringVerb(find func() (string, error), run func(ringDeps, io.Writer) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		townRoot, err := find()
		if err != nil {
			return fmt.Errorf("not in a Gas Town workspace: %w", err)
		}
		d := ringDeps{townRoot: townRoot, getenv: os.Getenv, now: time.Now(), actor: detectActor()}
		return run(d, cmd.OutOrStdout())
	}
}

var ringCmd = &cobra.Command{
	Use:     "ring",
	GroupID: GroupConfig,
	Short:   "Manage the One: the town's signed lease",
	RunE:    requireSubcommand,
	Long: `Manage the One, the town's signed lease.

  gt ring forge    Forge a new Ring, valid for 36 hours
  gt ring renew    Extend the Ring by 36 hours from now, valid or waned
  gt ring status   Print the Ring's state (exit 0 valid, 2 waned, 3 absent)
  gt ring remove   Remove the Ring from mayor/town.json

The Ring is signed with the key settings/config.json names in ring.key, for
example "${GT_RING_KEY}", resolved from the daemon environment. A verb that
cannot resolve the key refuses and writes nothing. No verb prints the key.`,
}

var ringForgeCmd = &cobra.Command{
	Use:   "forge",
	Short: "Forge a new Ring, valid for 36 hours",
	Args:  cobra.NoArgs,
	RunE:  ringVerb(workspace.FindFromCwdOrError, runRingForge),
}

var ringRenewCmd = &cobra.Command{
	Use:   "renew",
	Short: "Extend the Ring by 36 hours from now, valid or waned",
	Args:  cobra.NoArgs,
	RunE:  ringVerb(workspace.FindFromCwdOrError, runRingRenew),
}

var ringStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Print the Ring's state: exit 0 valid, 2 waned, 3 absent",
	Args:  cobra.NoArgs,
	RunE:  ringVerb(workspace.FindFromCwdOrError, runRingStatus),
}

var ringRemoveCmd = &cobra.Command{
	Use:   "remove",
	Short: "Remove the Ring from mayor/town.json",
	Args:  cobra.NoArgs,
	RunE:  ringVerb(workspace.FindFromCwdOrError, runRingRemove),
}

func init() {
	ringCmd.AddCommand(ringForgeCmd, ringRenewCmd, ringStatusCmd, ringRemoveCmd)
	rootCmd.AddCommand(ringCmd)
}
