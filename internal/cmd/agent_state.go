package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
)

var (
	agentStateSet  []string
	agentStateIncr string
	agentStateDel  []string
	agentStateJSON bool
)

var agentStateCmd = &cobra.Command{
	Use:   "state <agent-bead>",
	Short: "Get or set operational state on agent beads",
	Long: `Get or set label-based operational state on agent beads.

Agent beads store operational state (like idle cycle counts) as labels.
This command provides a convenient interface for reading and modifying
these labels without affecting other bead properties.

LABEL FORMAT:
Labels are stored as key:value pairs (e.g., idle:3, backoff:2m).

OPERATIONS:
  Get all labels (default):
    gt agents state <agent-bead>

  Set a label:
    gt agents state <agent-bead> --set idle=0
    gt agents state <agent-bead> --set idle=0 --set backoff=30s

  Increment a numeric label:
    gt agents state <agent-bead> --incr idle
    (Creates label with value 1 if not present)

  Delete a label:
    gt agents state <agent-bead> --del idle

COMMON LABELS:
  idle:<n>           - Consecutive idle patrol cycles
  backoff:<duration> - Current backoff interval
  last_activity:<ts> - Last activity timestamp
  heartbeat:<epoch>  - Liveness stamp: every operation stamps it with the
                       current time, overriding any value passed to --set

The stamp is the freshness field readers age out — the deacon's HEALTH_CHECK
primary signal reads it — because bd leaves the bead's updated_at untouched on
a label-only write.

EXAMPLES:
  # Check current idle count
  gt agents state gt-gastown-witness

  # Reset idle counter after finding work
  gt agents state gt-gastown-witness --set idle=0

  # Increment idle counter on timeout
  gt agents state gt-gastown-witness --incr idle

  # Get state as JSON
  gt agents state gt-gastown-witness --json`,
	Args: cobra.ExactArgs(1),
	RunE: runAgentState,
}

func init() {
	agentStateCmd.Flags().StringArrayVar(&agentStateSet, "set", nil,
		"Set label value (format: key=value, repeatable)")
	agentStateCmd.Flags().StringVar(&agentStateIncr, "incr", "",
		"Increment numeric label (creates with value 1 if missing)")
	agentStateCmd.Flags().StringArrayVar(&agentStateDel, "del", nil,
		"Delete label (repeatable)")
	agentStateCmd.Flags().BoolVar(&agentStateJSON, "json", false,
		"Output as JSON")

	// Add as subcommand of agents
	agentsCmd.AddCommand(agentStateCmd)
}

// agentStateResult holds the state query result.
type agentStateResult struct {
	AgentBead string            `json:"agent_bead"`
	Labels    map[string]string `json:"labels"`
}

func runAgentState(cmd *cobra.Command, args []string) error {
	agentBead := args[0]

	beadsDir, err := resolveAgentTrackingBeadsDir()
	if err != nil {
		return fmt.Errorf("not in a beads workspace: %w", err)
	}

	// Determine operation mode
	hasSet := len(agentStateSet) > 0
	hasIncr := agentStateIncr != ""
	hasDel := len(agentStateDel) > 0

	if hasSet || hasIncr || hasDel {
		// Modification mode
		return modifyAgentState(agentBead, beadsDir)
	}

	// Query mode
	return queryAgentState(agentBead, beadsDir)
}

// queryAgentState retrieves and displays labels from an agent bead.
func queryAgentState(agentBead, beadsDir string) error {
	labels, err := getAgentLabels(agentBead, beadsDir)
	if err != nil {
		return err
	}

	result := &agentStateResult{
		AgentBead: agentBead,
		Labels:    labels,
	}

	if agentStateJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}

	// Human-readable output
	fmt.Printf("%s Agent: %s\n\n", style.Bold.Render("📊"), agentBead)

	if len(labels) == 0 {
		fmt.Printf("  %s\n", style.Dim.Render("(no operational state labels)"))
		return nil
	}

	for key, value := range labels {
		fmt.Printf("  %s: %s\n", key, value)
	}

	return nil
}

// modifyAgentState modifies labels on an agent bead.
// Uses read-modify-write pattern: read current labels, apply changes, write back all.
func modifyAgentState(agentBead, beadsDir string) error {
	// Read current labels
	allLabels, err := getAllAgentLabels(agentBead, beadsDir)
	if err != nil {
		return err
	}

	stateLabels := parseStateLabels(allLabels)
	if err := applyLabelOperations(stateLabels, agentStateSet, agentStateIncr, agentStateDel); err != nil {
		return err
	}

	finalLabels := buildAgentStateLabels(allLabels, stateLabels, time.Now())

	// Build update command with --set-labels to replace all
	args := []string{"update", agentBead}
	for _, label := range finalLabels {
		args = append(args, "--set-labels="+label)
	}

	ctx, cancel := context.WithTimeout(context.Background(), bdCallTimeout)
	defer cancel()

	cmd := beads.CommandContext(ctx, filepath.Dir(beadsDir), beadsDir, beads.MutationPinned, args...)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return fmt.Errorf("%s", errMsg)
		}
		return fmt.Errorf("updating agent state: %w", err)
	}

	fmt.Printf("%s Updated agent state for %s\n", style.Bold.Render("✓"), agentBead)

	return nil
}

// parseStateLabels returns the key:value labels from allLabels as a map.
// Labels without a : separator are not state and are left out.
func parseStateLabels(allLabels []string) map[string]string {
	labels := make(map[string]string)
	for _, label := range allLabels {
		parts := strings.SplitN(label, ":", 2)
		if len(parts) == 2 {
			labels[parts[0]] = parts[1]
		}
	}
	return labels
}

// applyLabelOperations applies the increment, set, and delete operations to
// stateLabels, in that order. A set operation without a = is the only error.
func applyLabelOperations(stateLabels map[string]string, setOps []string, incrKey string, delKeys []string) error {
	if incrKey != "" {
		currentValue := 0
		if valStr, ok := stateLabels[incrKey]; ok {
			if v, err := strconv.Atoi(valStr); err == nil {
				currentValue = v
			}
		}
		stateLabels[incrKey] = strconv.Itoa(currentValue + 1)
	}

	for _, setOp := range setOps {
		parts := strings.SplitN(setOp, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid set format: %s (expected key=value)", setOp)
		}
		stateLabels[parts[0]] = parts[1]
	}

	for _, delKey := range delKeys {
		delete(stateLabels, delKey)
	}

	return nil
}

// buildAgentStateLabels returns the complete label list to write back:
// colon-free labels preserved, state labels from stateLabels, and a heartbeat
// stamp of now that replaces any older one.
//
// The stamp carries evidence a reader can act on: a state change means the agent
// is alive and processing, and bd writes labels to their own table, leaving the
// issue row's updated_at stale. Without the stamp the change is invisible to any
// freshness reader, the deacon's HEALTH_CHECK primary signal included (gt-dq5z).
func buildAgentStateLabels(allLabels []string, stateLabels map[string]string, now time.Time) []string {
	finalLabels := make([]string, 0, len(allLabels)+1)

	// Colon-free labels first, then state.
	for _, label := range allLabels {
		if !strings.Contains(label, ":") {
			finalLabels = append(finalLabels, label)
		}
	}
	for key, value := range stateLabels {
		if key == heartbeatLabelKey {
			continue // replaced by the stamp below
		}
		finalLabels = append(finalLabels, key+":"+value)
	}

	return append(finalLabels, fmt.Sprintf("%s:%d", heartbeatLabelKey, now.Unix()))
}

// getAgentLabels retrieves an agent bead's state labels.
func getAgentLabels(agentBead, beadsDir string) (map[string]string, error) {
	allLabels, err := getAllAgentLabels(agentBead, beadsDir)
	if err != nil {
		return nil, err
	}

	return parseStateLabels(allLabels), nil
}

// bdCallTimeout is the per-call timeout for bd subprocess invocations in agent-bead
// helpers. bd commands should be fast against a local Dolt server, but can hang
// indefinitely if Dolt is unresponsive (e.g., connection pool exhausted). A 30s
// ceiling prevents await-event/await-signal from stalling past the patrol timeout.
const bdCallTimeout = 30 * time.Second

// getAllAgentLabels retrieves all labels (including non-state) from an agent bead.
func getAllAgentLabels(agentBead, beadsDir string) ([]string, error) {
	args := []string{"show", agentBead, "--json"}

	ctx, cancel := context.WithTimeout(context.Background(), bdCallTimeout)
	defer cancel()

	cmd := beads.CommandContext(ctx, filepath.Dir(beadsDir), beadsDir, beads.ReadOnlyPinned, args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if strings.Contains(errMsg, "not found") {
			return nil, fmt.Errorf("agent bead not found: %s", agentBead)
		}
		if errMsg != "" {
			return nil, fmt.Errorf("%s", errMsg)
		}
		return nil, fmt.Errorf("querying agent bead: %w", err)
	}

	return parseAgentBeadLabels(stdout.Bytes(), stderr.Bytes(), agentBead)
}

// parseAgentBeadLabels parses the JSON output from bd show --json and extracts labels.
// This is separated from getAllAgentLabels to enable unit testing.
func parseAgentBeadLabels(stdout, stderr []byte, agentBead string) ([]string, error) {
	// Check for empty stdout before parsing - can happen with daemon mismatch
	// or other errors that don't set exit code
	if len(stdout) == 0 {
		errMsg := strings.TrimSpace(string(stderr))
		if errMsg != "" {
			return nil, fmt.Errorf("%s", errMsg)
		}
		return nil, fmt.Errorf("agent bead query returned no output: %s", agentBead)
	}

	// Parse JSON output - bd show --json returns an array
	var issues []struct {
		Labels []string `json:"labels"`
	}

	if err := json.Unmarshal(stdout, &issues); err != nil {
		return nil, fmt.Errorf("parsing agent bead response: %w", err)
	}

	if len(issues) == 0 {
		return nil, fmt.Errorf("agent bead not found: %s", agentBead)
	}

	return issues[0].Labels, nil
}
