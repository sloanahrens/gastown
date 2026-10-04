package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
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

The stamp is the freshness field readers age out, because bd leaves the
bead's updated_at untouched on a label-only write.

EXAMPLES:
  # Check current idle count
  gt agents state hq-deacon

  # Reset idle counter after finding work
  gt agents state hq-deacon --set idle=0

  # Increment idle counter on timeout
  gt agents state hq-deacon --incr idle

  # Get state as JSON
  gt agents state hq-deacon --json`,
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
		return modifyAgentState(beads.NewPinned(beadsDir), os.Stdout, agentBead, agentLabelOps{set: agentStateSet, incr: agentStateIncr, del: agentStateDel}, time.Now())
	}

	// Query mode
	return queryAgentState(agentBead, beadsDir)
}

// queryAgentState retrieves and displays labels from an agent bead.
func queryAgentState(agentBead, beadsDir string) error {
	labels, err := getAgentLabels(beads.NewPinned(beadsDir), agentBead)
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

// agentLabelOps are gt agent state's --set, --incr and --del operations.
type agentLabelOps struct {
	set  []string
	incr string
	del  []string
}

// modifyAgentState modifies labels on an agent bead in db, stamping now as
// its heartbeat.
// Uses read-modify-write pattern: read current labels, apply changes, write back all.
func modifyAgentState(db beads.Client, w io.Writer, agentBead string, ops agentLabelOps, now time.Time) error {
	// Read current labels
	allLabels, err := getAllAgentLabels(db, agentBead)
	if err != nil {
		return err
	}

	stateLabels := parseStateLabels(allLabels)
	if err := applyLabelOperations(stateLabels, ops.set, ops.incr, ops.del); err != nil {
		return err
	}

	finalLabels := buildAgentStateLabels(allLabels, stateLabels, now)
	if err := db.Update(agentBead, beads.UpdateOptions{SetLabels: finalLabels}); err != nil {
		return fmt.Errorf("updating agent state: %w", err)
	}

	fmt.Fprintf(w, "%s Updated agent state for %s\n", style.Bold.Render("✓"), agentBead)

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
// freshness reader (gt-dq5z).
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
func getAgentLabels(db beads.Client, agentBead string) (map[string]string, error) {
	allLabels, err := getAllAgentLabels(db, agentBead)
	if err != nil {
		return nil, err
	}

	return parseStateLabels(allLabels), nil
}

// getAllAgentLabels retrieves all labels (including non-state) from an agent bead.
func getAllAgentLabels(db beads.Client, agentBead string) ([]string, error) {
	issue, err := db.Show(agentBead)
	if errors.Is(err, beads.ErrNotFound) {
		return nil, fmt.Errorf("agent bead not found: %s", agentBead)
	}
	if err != nil {
		return nil, fmt.Errorf("querying agent bead: %w", err)
	}
	return issue.Labels, nil
}

// agentBeadsDB opens the agent-bead database at beadsDir through open, or
// pinned to it (beads.NewPinned) when open is nil. Unit tests pass an open
// that returns a beadsfake.
func agentBeadsDB(open func(beadsDir string) beads.Client, beadsDir string) beads.Client {
	if open == nil {
		return beads.NewPinned(beadsDir)
	}
	return open(beadsDir)
}
