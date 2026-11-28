package cmd

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/openshellprov"
)

// `gt sandbox per-seat <profile> on|off` switches the per-seat Forgejo push.
// off (gt-dmeph9) reads no gateway, so it works while the gateway is down. on
// (gt-oatim0) writes only when every seat name the town may hand out has its
// provider on the profile's gateway.

var sandboxPerSeatCmd = &cobra.Command{
	Use:   "per-seat <profile> <state>",
	Short: "Switch a sandbox profile's per-seat Forgejo push on or off",
	Long: `Set forgejo_per_seat on a named sandbox profile in settings/config.json,
keeping a .bak- copy. off asks nothing. on asks the profile's gateway whether every
seat name the town may hand out has its forgejo-seat provider, and refuses when
one is missing. A numbered overflow name is not covered, and a hand edit of
settings/config.json skips the check.

gt sandbox per-seat polecat-k8s on`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runSandboxPerSeatCmd(cmd.OutOrStdout(), processSetResourcesTown, townSeatNames, gatewayMissingSeatProviders, args[0], args[1])
	},
}

// perSeatValue is the value a state sets.
func perSeatValue(state string) (bool, error) {
	switch state {
	case "off":
		return false, nil
	case "on":
		return true, nil
	}
	return false, fmt.Errorf("sandbox per-seat: unknown state %q: want on or off", state)
}

// runSandboxPerSeatCmd reads the state and, for on, checks the seat names first.
func runSandboxPerSeatCmd(w io.Writer, resolve setResourcesTownResolver, seatNames func(string) ([]string, error), providers func(string, []string) ([]string, error), name, state string) error {
	value, err := perSeatValue(state)
	if err != nil {
		return err
	}
	townRoot, err := resolve("")
	if err != nil {
		return err
	}
	path := config.TownSettingsPath(townRoot)
	if value {
		if err := checkSeatProviders(path, townRoot, name, seatNames, providers); err != nil {
			return err
		}
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	was, copied, err := writeSandboxPerSeat(path, stamp, name, value, config.BackupFile)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "sandboxes.%s.forgejo_per_seat: %t -> %t\nprevious file kept at %s\n", name, was, value, copied)
	printSandboxTown(w, townRoot, path)
	return nil
}

// checkSeatProviders asks the profile's gateway which town seat names have no
// provider; a failed read, list or question is an error, and nothing is written.
func checkSeatProviders(path, townRoot, name string, seatNames func(string) ([]string, error), providers func(string, []string) ([]string, error)) error {
	ts, err := config.LoadTownSettings(path)
	if err != nil {
		return fmt.Errorf("sandbox per-seat: %w", err)
	}
	profile, ok := config.SandboxProfile{}, false
	if ts != nil {
		profile, ok = ts.Sandboxes[name]
	}
	if !ok {
		return fmt.Errorf("sandbox per-seat: %q is not a sandbox profile", name)
	}
	gateway := profile.WithDefaults().Gateway
	names, err := seatNames(townRoot)
	if err != nil {
		return fmt.Errorf("sandbox per-seat: %w", err)
	}
	missing, err := providers(gateway, names)
	if err != nil {
		return fmt.Errorf("sandbox per-seat: %w", err)
	}
	if len(missing) > 0 {
		return fmt.Errorf("sandbox per-seat: %d seat name(s) have no provider on gateway %q: %s", len(missing), gateway, strings.Join(missing, ", "))
	}
	return nil
}

// gatewayMissingSeatProviders asks the gateway for the names with no provider.
func gatewayMissingSeatProviders(gateway string, names []string) ([]string, error) {
	client := openshellprov.Client{Run: openshellprov.ExecRunner{}, Gateway: gateway}
	return client.MissingSeatProviders(names)
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
