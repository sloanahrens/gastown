package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/steveyegge/gastown/internal/daemon"
)

// printRepackPlan writes the plan a person reads before the repack; it runs nothing.
func printRepackPlan(w io.Writer, repo string, plan daemon.RepackPlan) error {
	due := "no"
	if plan.Due {
		due = "yes"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "due: %s\nreason: %s\n", due, plan.Reason)
	if plan.Due {
		fmt.Fprintf(&b, "%s\n", strings.Join(append([]string{"git", "-C", repo}, plan.Args...), " "))
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("write repack plan: %w", err)
	}
	return nil
}
