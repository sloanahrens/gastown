package cmd

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
)

// `gt sandbox per-seat <profile> off` (gt-dmeph9) is the way back of the per-seat
// Forgejo switch. It reads no gateway and no rig, so it works while the gateway is
// down, and it writes the file the way sandbox set-resources does.

var sandboxPerSeatCmd = &cobra.Command{
	Use:   "per-seat <profile> <state>",
	Short: "Switch a sandbox profile's per-seat Forgejo push off",
	Long: `Set forgejo_per_seat to false on a named sandbox profile in settings/config.json,
keeping a .bak- copy of the file. Only off is built: on refuses and writes nothing.

gt sandbox per-seat polecat-k8s off`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSandboxPerSeatCmd(cmd.OutOrStdout(), processSetResourcesTown, args[0], args[1])
	},
}

// perSeatValue is the value a state sets. on sets true but is refused: it has no
// build yet, so it fails before the town is read.
func perSeatValue(state string) (bool, error) {
	switch state {
	case "off":
		return false, nil
	case "on":
		return true, errors.New("sandbox per-seat on is not built yet: only off is")
	}
	return false, fmt.Errorf("sandbox per-seat: unknown state %q: want off", state)
}

// runSandboxPerSeatCmd reads the state, names the town, and writes.
func runSandboxPerSeatCmd(w io.Writer, resolve setResourcesTownResolver, name, state string) error {
	value, err := perSeatValue(state)
	if err != nil {
		return err
	}
	townRoot, err := resolve("")
	if err != nil {
		return err
	}
	path := config.TownSettingsPath(townRoot)
	stamp := time.Now().UTC().Format("20060102T150405Z")
	was, copied, err := writeSandboxPerSeat(path, stamp, name, value, config.BackupFile)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "sandboxes.%s.forgejo_per_seat: %t -> %t\nprevious file kept at %s\n", name, was, value, copied)
	printSandboxTown(w, townRoot, path)
	return nil
}

// writeSandboxPerSeat plans under the file's lock and copies the file before it
// replaces the profile, so a refusal or a failed copy writes nothing.
func writeSandboxPerSeat(path, stamp, name string, value bool, backup sandboxBackup) (bool, string, error) {
	var was bool
	var copied string
	err := config.UpdateConfigJSON(path, 0o600, func(ts *config.TownSettings, _ bool) error {
		before, ok := ts.Sandboxes[name]
		if !ok {
			return fmt.Errorf("%q is not a sandbox profile: the profiles are %s", name, sandboxProfileNames(ts))
		}
		if before.ForgejoPerSeat == value {
			return fmt.Errorf("nothing to change: sandboxes.%s already has forgejo_per_seat %t", name, value)
		}
		after := before
		after.ForgejoPerSeat = value
		check := after
		if sandboxProfileSeated(ts, name) {
			check = after.WithDefaults()
		}
		if err := check.Validate(name); err != nil {
			return err
		}
		var err error
		if copied, err = backup(config.WritePath(path), stamp); err != nil {
			return err
		}
		ts.Sandboxes[name] = after
		was = before.ForgejoPerSeat
		return nil
	})
	if err != nil {
		return false, "", fmt.Errorf("sandbox per-seat: %w", err)
	}
	return was, copied, nil
}

func init() {
	sandboxCmd.AddCommand(sandboxPerSeatCmd)
}
