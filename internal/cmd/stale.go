package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/version"
)

var staleJSON bool
var staleQuiet bool

var staleCmd = &cobra.Command{
	Use:     "stale",
	GroupID: GroupDiag,
	Short:   "Check if the gt binary is stale",
	Long: `Check if the gt binary was built from an older commit than a build ref.

This command compares the commit hash embedded in the binary at build time
with the resolved build branch of the gastown repository (main/master/carry/*).

Examples:
  gt stale              # Human-readable output
  gt stale --json       # Machine-readable JSON output
  gt stale --quiet      # Exit code only (0=stale, 1=fresh, 2=undetermined)

Exit codes:
  0 - Binary is stale
  1 - Binary is fresh (up to date)
  2 - Error or skipped (could not determine staleness)`,
	RunE: runStale,
}

func init() {
	staleCmd.Flags().BoolVar(&staleJSON, "json", false, "Output as JSON")
	staleCmd.Flags().BoolVarP(&staleQuiet, "quiet", "q", false, "Exit code only (0=stale, 1=fresh, 2=undetermined)")
	rootCmd.AddCommand(staleCmd)
}

// StaleOutput represents the JSON output structure.
type StaleOutput struct {
	Stale         bool   `json:"stale"`
	Forward       bool   `json:"forward"`
	OnMainBranch  bool   `json:"on_main_branch"`
	SafeToRebuild bool   `json:"safe_to_rebuild"`
	BinaryCommit  string `json:"binary_commit"`
	RepoCommit    string `json:"repo_commit"`
	CompareRef    string `json:"compare_ref,omitempty"`
	CommitsBehind int    `json:"commits_behind,omitempty"`
	// BinaryAffecting is how many of the commits behind change what the build
	// produces; meaningful only when BinaryAffectingKnown.
	BinaryAffecting      int    `json:"binary_affecting,omitempty"`
	BinaryAffectingKnown bool   `json:"binary_affecting_known,omitempty"`
	Skipped              bool   `json:"skipped,omitempty"`
	SkipReason           string `json:"skip_reason,omitempty"`
	Error                string `json:"error,omitempty"`
}

func runStale(cmd *cobra.Command, args []string) error {
	// Find the gastown repo
	repoRoot, err := version.GetRepoRoot()
	if err != nil {
		if staleQuiet {
			return NewSilentExit(2)
		}
		if staleJSON {
			return outputStaleJSON(StaleOutput{Error: err.Error()})
		}
		return fmt.Errorf("cannot find gastown repo: %w", err)
	}

	// Check staleness against a freshly-refreshed remote-tracking ref
	// (gt-cq0): repoRoot's cached origin/main can lag if nobody has fetched
	// it recently, which would otherwise report a stale binary as "fresh".
	info := version.CheckStaleBinaryFresh(repoRoot)

	// Handle errors
	if info.Error != nil {
		if staleQuiet {
			return NewSilentExit(2)
		}
		if staleJSON {
			return outputStaleJSON(StaleOutput{Error: info.Error.Error()})
		}
		return fmt.Errorf("staleness check failed: %w", info.Error)
	}

	// Quiet mode: just exit with appropriate code
	if staleQuiet {
		return NewSilentExit(staleQuietExitCode(info))
	}

	// Build output
	// SafeToRebuild requires: stale + forward-only + on a build branch.
	safeToRebuild := info.IsStale && info.IsForward && info.OnMainBranch
	output := StaleOutput{
		Stale:                info.IsStale,
		Forward:              info.IsForward,
		OnMainBranch:         info.OnMainBranch,
		SafeToRebuild:        safeToRebuild,
		BinaryCommit:         info.BinaryCommit,
		RepoCommit:           info.RepoCommit,
		CompareRef:           info.CompareRef,
		CommitsBehind:        info.CommitsBehind,
		BinaryAffecting:      info.BinaryAffecting,
		BinaryAffectingKnown: info.BinaryAffectingKnown,
		Skipped:              info.Skipped,
		SkipReason:           info.SkipReason,
	}

	if staleJSON {
		return outputStaleJSON(output)
	}

	return outputStaleText(os.Stdout, output)
}

func staleQuietExitCode(info *version.StaleBinaryInfo) int {
	if info.Skipped {
		return 2
	}
	if info.IsStale {
		return 0
	}
	return 1
}

func outputStaleJSON(output StaleOutput) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(output)
}

func outputStaleText(w io.Writer, output StaleOutput) error {
	if output.Skipped {
		fmt.Fprintf(w, "%s Binary staleness check skipped\n", style.Dim.Render("•"))
		fmt.Fprintf(w, "  %s\n", output.SkipReason)
		fmt.Fprintf(w, "  Binary: %s\n", version.ShortCommit(output.BinaryCommit))
		return nil
	}
	if output.Stale {
		fmt.Fprintf(w, "%s Binary is stale\n", style.Warning.Render("⚠"))
		fmt.Fprintf(w, "  Binary:   %s\n", version.ShortCommit(output.BinaryCommit))
		fmt.Fprintf(w, "  Build ref (%s): %s\n", output.CompareRef, version.ShortCommit(output.RepoCommit))
		if output.CommitsBehind > 0 {
			note := version.BinaryNote(output.BinaryAffecting, output.CommitsBehind, output.BinaryAffectingKnown)
			fmt.Fprintf(w, "  %s\n", style.Dim.Render(fmt.Sprintf("(%d commits behind %s%s)", output.CommitsBehind, output.CompareRef, note)))
		}
		if !output.Forward {
			fmt.Fprintf(w, "  %s %s is NOT a descendant of binary commit (diverged or older)\n", style.Error.Render("✗"), output.CompareRef)
		}
		if !output.OnMainBranch {
			fmt.Fprintf(w, "  %s source worktree is not on a build branch (compared against %s)\n", style.Warning.Render("⚠"), output.CompareRef)
		}
		if output.SafeToRebuild {
			fmt.Fprintf(w, "\n  Safe to rebuild: run 'make install'\n")
		} else {
			fmt.Fprintf(w, "\n  %s NOT safe for automated rebuild (forward=%v, build_branch=%v)\n",
				style.Error.Render("✗"), output.Forward, output.OnMainBranch)
		}
	} else {
		// Every commit the build ref is ahead changes nothing the build reads:
		// installing would produce the same binary, so it is current for the
		// build while still being behind the ref by the raw count (gt-p62ku).
		nonBinaryAhead := output.CommitsBehind > 0 && output.BinaryAffectingKnown && output.BinaryAffecting == 0
		head := "Binary is fresh"
		if nonBinaryAhead {
			head = "Binary is current for the build"
		}
		fmt.Fprintf(w, "%s %s\n", style.Success.Render("✓"), head)
		fmt.Fprintf(w, "  Commit: %s\n", version.ShortCommit(output.BinaryCommit))
		if output.CompareRef != "" {
			fmt.Fprintf(w, "  %s\n", style.Dim.Render(fmt.Sprintf("(compared against %s)", output.CompareRef)))
		}
		if nonBinaryAhead {
			fmt.Fprintf(w, "  %s\n", style.Dim.Render(fmt.Sprintf("(%d documentation-only commits ahead of %s)", output.CommitsBehind, output.CompareRef)))
		}
	}
	return nil
}
