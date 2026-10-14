package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/workspace"
)

// bead_search.go is `gt bead search`: one bd search per rig database, printed
// grouped by rig, so "did we ever see X" is a single command. It reads only;
// it never writes a bead and never starts a service.

// beadSearchRigTimeout bounds one rig's bd search. A rig that overruns it is
// reported unavailable and the next rig is searched, so one wedged database
// cannot hold the whole answer.
const beadSearchRigTimeout = 5 * time.Second

var (
	beadSearchLimit  int
	beadSearchStatus string
)

var beadSearchCmd = &cobra.Command{
	Use:   "search <words...>",
	Short: "Search every rig's beads for words",
	Long: `Run 'bd search' with the same words against each rig's beads database and
print the matches grouped by rig. Closed beads are included.

Each match is one line:  <rig>: <id> [P2] status - title
A rig that cannot be read prints  rig <name>: unavailable (<reason>)  instead
of being left out, so a silent rig is never mistaken for "no match".

--limit caps the matches printed per rig. --status narrows to one status, as
'bd search --status' does. Read-only.

Examples:
  gt bead search restart nudge
  gt bead search --status open overrun guard
  gt bead search --limit 5 dolt timeout`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		townRoot, err := workspace.FindFromCwdOrError()
		if err != nil {
			return fmt.Errorf("not in a Gas Town workspace: %w", err)
		}
		rigs, err := beadSearchRigs(townRoot)
		if err != nil {
			return err
		}
		return runBeadSearch(cmd.OutOrStdout(), rigs, searchRigBD, args, beadSearchLimit, beadSearchStatus)
	},
}

func init() {
	f := beadSearchCmd.Flags()
	f.IntVar(&beadSearchLimit, "limit", 20, "Most matches printed per rig")
	f.StringVar(&beadSearchStatus, "status", "", "Only beads in this status (open, in_progress, closed, ...)")
}

// beadSearchRig is one rig's database to search.
type beadSearchRig struct {
	name     string
	beadsDir string
}

// beadSearchFunc runs bd with args against one rig's database and returns its
// stdout. It is the seam tests replace so no bd runs.
type beadSearchFunc func(rig beadSearchRig, args []string) ([]byte, error)

// beadSearchRigs lists the town's registered rigs, sorted by name, from the
// registry gt rig list reads. Each rig's database is its resolved .beads, so a
// rig whose .beads redirects to mayor/rig/.beads is searched there.
func beadSearchRigs(townRoot string) ([]beadSearchRig, error) {
	rigsConfig, err := config.LoadRigsConfigOrEmpty(filepath.Join(townRoot, "mayor", "rigs.json"))
	if err != nil {
		return nil, fmt.Errorf("loading rigs config: %w", err)
	}
	names := make([]string, 0, len(rigsConfig.Rigs))
	for name := range rigsConfig.Rigs {
		names = append(names, name)
	}
	sort.Strings(names)
	rigs := make([]beadSearchRig, 0, len(names))
	for _, name := range names {
		rigs = append(rigs, beadSearchRig{name: name, beadsDir: beads.ResolveBeadsDir(filepath.Join(townRoot, name))})
	}
	return rigs, nil
}

// searchRigBD is the production beadSearchFunc: bd pinned to the rig's
// database (no prefix routing, so a rig is never answered from another's
// store) and killed at beadSearchRigTimeout.
func searchRigBD(rig beadSearchRig, args []string) ([]byte, error) {
	env := append(os.Environ(), "BEADS_DIR="+rig.beadsDir)
	return beads.NewPlain(filepath.Dir(rig.beadsDir), env).WithTimeout(beadSearchRigTimeout).Run(args...)
}

// beadSearchArgs is the bd argv for one rig: the flags first, then the words
// after "--" so a word that starts with a dash is searched for, not parsed.
// It asks for one match more than limit, which is how the caller knows the
// rig had more than it prints.
func beadSearchArgs(words []string, limit int, status string) []string {
	args := []string{"search", "--json", "--limit", strconv.Itoa(limit + 1)}
	if status != "" {
		args = append(args, "--status", status)
	}
	args = append(args, "--")
	return append(args, words...)
}

// runBeadSearch searches each rig in turn and writes the grouped matches to
// out. It fails only when no rig could be read at all.
func runBeadSearch(out io.Writer, rigs []beadSearchRig, search beadSearchFunc, words []string, limit int, status string) error {
	if limit < 1 {
		return fmt.Errorf("--limit must be at least 1, got %d", limit)
	}
	if len(rigs) == 0 {
		return errors.New("no rigs registered in this town")
	}
	args := beadSearchArgs(words, limit, status)
	matches, failed := 0, 0
	for _, rig := range rigs {
		issues, err := searchOneRig(search, rig, args)
		if err != nil {
			failed++
			fmt.Fprintf(out, "rig %s: unavailable (%s)\n", rig.name, searchFailureReason(err))
			continue
		}
		for i, is := range issues {
			if i == limit {
				fmt.Fprintf(out, "%s: more matches not shown (--limit %d)\n", rig.name, limit)
				break
			}
			fmt.Fprintf(out, "%s: %s [P%d] %s - %s\n", rig.name, is.ID, is.Priority, is.Status, is.Title)
			matches++
		}
	}
	if failed == len(rigs) {
		return fmt.Errorf("no rig could be searched (%d unavailable)", failed)
	}
	if matches == 0 && failed == 0 {
		fmt.Fprintln(out, "no matches in any rig")
	}
	return nil
}

func searchOneRig(search beadSearchFunc, rig beadSearchRig, args []string) ([]*beads.Issue, error) {
	raw, err := search(rig, args)
	if err != nil {
		return nil, err
	}
	var issues []*beads.Issue
	if err := json.Unmarshal(raw, &issues); err != nil {
		return nil, fmt.Errorf("unreadable bd search output: %w", err)
	}
	return issues, nil
}

// searchFailureReason is one line saying why a rig could not be searched: a
// timeout names itself, anything else is bd's first line of output.
func searchFailureReason(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out after " + beadSearchRigTimeout.String()
	}
	msg := bdErrOutput(err)
	if line, _, _ := strings.Cut(strings.TrimSpace(msg), "\n"); line != "" {
		msg = line
	}
	const maxReason = 160
	if len(msg) > maxReason {
		msg = msg[:maxReason] + "..."
	}
	return msg
}
