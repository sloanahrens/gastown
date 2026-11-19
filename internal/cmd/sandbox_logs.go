package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/workspace"
)

// `gt sandbox logs` (gt-fldibt) prints the sandbox-source log lines a seat's
// gateway keeps — the supervisor and OCSF lines that reach no other gt surface.
// The seat's sandbox record names the sandbox and the gateway it runs on, so
// the command asks that gateway about that sandbox and prints what it answers.

// sandboxLogsDefaultSince is how far back the command reads when --since is
// not given.
const sandboxLogsDefaultSince = 15 * time.Minute

var sandboxLogsSince time.Duration

var sandboxLogsCmd = &cobra.Command{
	Use:   "logs <rig>/<polecat>",
	Short: "Print a sandbox seat's log lines from the last interval",
	Long: `Print the log lines a sandbox seat's gateway keeps for it: the supervisor's
and the OCSF lines that reach no other gt surface.

The seat is named <rig>/<polecat>. Its sandbox record says which sandbox and
gateway it runs on, so a seat that runs on host tmux has no record and is
refused rather than read from another sandbox. The lines go to stdout
unchanged; a seat that cannot be resolved, or a gateway that cannot answer, is
an error naming the seat.

--since says how far back to read (default 15m).`,
	Args: cobra.ExactArgs(1),
	RunE: runSandboxLogsCmd,
}

func init() {
	sandboxLogsCmd.Flags().DurationVar(&sandboxLogsSince, "since", sandboxLogsDefaultSince, "How far back to read the log lines")
	sandboxCmd.AddCommand(sandboxLogsCmd)
}

func runSandboxLogsCmd(cmd *cobra.Command, args []string) error {
	return runSandboxLogsCmdWith(cmd, args, workspace.FindFromCwdOrError)
}

// runSandboxLogsCmdWith is runSandboxLogsCmd with the town lookup injected, so
// a test can fail it.
func runSandboxLogsCmdWith(cmd *cobra.Command, args []string, findTown func() (string, error)) error {
	townRoot, err := findTown()
	if err != nil {
		return fmt.Errorf("sandbox logs: %w", err)
	}
	return executeSandboxLogs(cmd.Context(), cmd.OutOrStdout(), townRoot, args[0], sandboxLogsSince, nil)
}

// executeSandboxLogs prints the log lines the gateway of addr's seat keeps
// since, and returns an error naming the seat when it cannot resolve the seat
// or read its lines. run is the openshell runner; nil runs the binary.
func executeSandboxLogs(ctx context.Context, w io.Writer, townRoot, addr string, since time.Duration, run sandbox.Runner) error {
	rig, polecat, err := splitSandboxSeatAddress(addr)
	if err != nil {
		return err
	}
	seat := rig + "/" + polecat
	name, err := sandboxSeatSessionName(townRoot, rig, polecat)
	if err != nil {
		return fmt.Errorf("sandbox logs %s: %w", seat, err)
	}
	rec, ok, err := session.ReadSandboxRecord(townRoot, name)
	if err != nil {
		return fmt.Errorf("sandbox logs %s: reading its sandbox record: %w", seat, err)
	}
	if !ok {
		return fmt.Errorf("sandbox logs %s: no sandbox record: the seat is not in a sandbox", seat)
	}
	if rec.Session != name {
		return fmt.Errorf("sandbox logs %s: its sandbox record names sandbox %q, not %q", seat, rec.Session, name)
	}
	text, err := (&sandbox.CLI{Gateway: rec.Gateway, Run: run}).Logs(ctx, name, since)
	if err != nil {
		return fmt.Errorf("sandbox logs %s: %w", seat, err)
	}
	_, err = io.WriteString(w, text)
	return err
}

// splitSandboxSeatAddress splits a <rig>/<polecat> address. A seat has exactly
// these two parts, so a missing or extra slash names no seat.
func splitSandboxSeatAddress(addr string) (rig, polecat string, err error) {
	rig, polecat, ok := strings.Cut(addr, "/")
	if !ok || rig == "" || polecat == "" || strings.Contains(polecat, "/") {
		return "", "", fmt.Errorf("sandbox logs %s: unknown seat: want <rig>/<polecat>", addr)
	}
	return rig, polecat, nil
}

// sandboxSeatSessionName is the session, and so the sandbox, name of rig's
// polecat: the rig's prefix from the town's rig registry, then the polecat. A
// registry that cannot be read, or one that does not name the rig, is an
// error, never the "gt" fallback that would address another rig's records. A
// rig the registry gives no beads prefix runs its sessions under the default
// prefix, as the registry itself answers for it.
func sandboxSeatSessionName(townRoot, rig, polecat string) (string, error) {
	cfg, err := config.LoadRigsConfig(constants.MayorRigsPath(townRoot))
	if err != nil {
		return "", fmt.Errorf("reading the town's rig registry: %w", err)
	}
	entry, ok := cfg.Rigs[rig]
	if !ok {
		return "", fmt.Errorf("unknown seat: the town's rig registry does not name rig %q", rig)
	}
	prefix := session.DefaultPrefix
	if entry.BeadsConfig != nil && entry.BeadsConfig.Prefix != "" {
		prefix = strings.TrimSuffix(entry.BeadsConfig.Prefix, "-")
	}
	return session.PolecatSessionName(prefix, polecat), nil
}
