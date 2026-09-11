package cmd

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

var tapGuardPatrolLoopCmd = &cobra.Command{
	Use:   "patrol-loop",
	Short: "Block patrol anti-patterns (pouring patrol formulas, deacon batch loops)",
	Long: `Block patrol-loop anti-patterns via Claude Code PreToolUse hooks.

This guard reads tool_input.command from stdin and inspects the actual
command itself, rather than relying on the settings.json "if" permission-glob
to filter which commands reach it. Claude Code's "if" glob evaluator treats a
compound command it cannot statically resolve (an argument-position $(...) or
${#...} substitution, a long "cd ... && ...; ... | ..." line) as matching any
pattern that begins with "*" — so the leading-* globs this guard replaces
(Bash(*bd mol pour*patrol*), Bash(*for *seq*), Bash(*while true*), etc.) fired
on ordinary unrelated commands, blocking a large share of witness/deacon/
refinery patrol traffic (gt-qqfy). Registering this guard unconditionally
(no "if") and doing the real check here means it blocks only what it
actually recognizes, no matter what confused the outer glob matcher.

This guard blocks:
  - bd mol pour <formula>, when <formula> names a patrol formula (any name
    containing "patrol", or mol-witness*/mol-deacon*/mol-refinery*) — patrol
    formulas must run as wisps (bd mol wisp), not persistent molecules, in
    every patrol role (witness, deacon, refinery).
  - for ... seq ... and while true / while : — open-ended loop shapes,
    deacon only, so a deacon can't batch multiple patrol cycles into one
    Bash call.

Exit codes:
  0 - Operation allowed
  2 - Operation BLOCKED`,
	RunE: runTapGuardPatrolLoop,
}

func init() {
	tapGuardCmd.AddCommand(tapGuardPatrolLoopCmd)
}

func runTapGuardPatrolLoop(cmd *cobra.Command, args []string) error {
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		return nil // fail open
	}
	command := extractCommand(input)
	if command == "" {
		return nil
	}
	tokens := shellTokenize(command)

	if matchesBdMolPourPatrol(tokens) {
		printPatrolPourBlock()
		return NewSilentExit(2)
	}

	if isDeaconRole() {
		if matchesForSeqLoop(tokens) {
			printDeaconLoopBlock("Deacon must not batch patrol cycles with for/seq loops.")
			return NewSilentExit(2)
		}
		if matchesOpenEndedWhileLoop(tokens) {
			printDeaconLoopBlock("Deacon must not run open-ended patrol loops.")
			return NewSilentExit(2)
		}
	}

	return nil
}

// isDeaconRole reports whether the current process is running as the deacon
// role, mirroring isRefineryRole: GT_DEACON is checked directly (matching
// isGasTownAgentContext's existing envVars list, in case a future session
// sets it), falling back to parsing GT_ROLE.
func isDeaconRole() bool {
	if os.Getenv("GT_DEACON") != "" {
		return true
	}
	role, _, _ := parseRoleString(os.Getenv("GT_ROLE"))
	return role == RoleDeacon
}

// patrolPourMarkers are substrings that, found anywhere among the arguments
// following "bd mol pour", identify the target as a patrol formula that must
// be poured as a wisp instead of a persistent molecule. Collapses the four
// leading-* "if" globs this guard replaces (Bash(*bd mol pour*patrol*),
// Bash(*bd mol pour *mol-witness*), Bash(*bd mol pour *mol-deacon*),
// Bash(*bd mol pour *mol-refinery*)) into one token-based check.
var patrolPourMarkers = []string{"patrol", "mol-witness", "mol-deacon", "mol-refinery"}

// matchesBdMolPourPatrol reports whether tokens invoke "bd mol pour" with a
// patrol-formula argument. tokens must be shell-aware tokens (see
// shellTokenize) so a marker word quoted inside unrelated text (a bead
// title, a mail body) is never mistaken for the actual pour target.
func matchesBdMolPourPatrol(tokens []string) bool {
	for i := 0; i+2 < len(tokens); i++ {
		if !strings.EqualFold(tokens[i], "bd") || !strings.EqualFold(tokens[i+1], "mol") || !strings.EqualFold(tokens[i+2], "pour") {
			continue
		}
		for _, arg := range tokens[i+3:] {
			lower := strings.ToLower(arg)
			for _, marker := range patrolPourMarkers {
				if strings.Contains(lower, marker) {
					return true
				}
			}
		}
	}
	return false
}

// seqWordPattern matches "seq" as a whole word inside a token, so it still
// finds "seq" glued onto a command-substitution opener with no space (the
// realistic shape: "$(seq 1 5)" tokenizes as "$(seq", "1", "5)" — shlex has
// no notion of "$(" grouping, so "seq" is never its own bare token there).
var seqWordPattern = regexp.MustCompile(`(?i)\bseq\b`)

// matchesForSeqLoop reports whether tokens contain a "for ... seq" shape — a
// for-loop whose iteration is (eventually) sourced from "seq" — replacing
// the "if" glob Bash(*for *seq*). Deacon-only: a deacon must run one patrol
// cycle per Bash call, not batch several into a for/seq loop.
func matchesForSeqLoop(tokens []string) bool {
	sawFor := false
	for _, t := range tokens {
		if strings.EqualFold(t, "for") {
			sawFor = true
			continue
		}
		if sawFor && seqWordPattern.MatchString(t) {
			return true
		}
	}
	return false
}

// matchesOpenEndedWhileLoop reports whether tokens contain "while true" or
// "while :", replacing the "if" globs Bash(*while true*) and
// Bash(*while :*). Deacon-only, same rationale as matchesForSeqLoop.
func matchesOpenEndedWhileLoop(tokens []string) bool {
	for i, t := range tokens {
		if strings.ToLower(t) != "while" || i+1 >= len(tokens) {
			continue
		}
		next := tokens[i+1]
		if strings.EqualFold(next, "true") || next == ":" {
			return true
		}
	}
	return false
}

// printPatrolPourBlock prints the standard block banner for a patrol
// formula poured as a persistent molecule instead of a wisp.
func printPatrolPourBlock() {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(os.Stderr, "║  ❌ PATROL FORMULA POUR BLOCKED                                  ║")
	fmt.Fprintln(os.Stderr, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintln(os.Stderr, "║  Patrol formulas must use wisps, not persistent molecules.      ║")
	fmt.Fprintln(os.Stderr, "║                                                                  ║")
	fmt.Fprintln(os.Stderr, "║  Use: bd mol wisp mol-*-patrol                                  ║")
	fmt.Fprintln(os.Stderr, "║  Not:  bd mol pour mol-*-patrol                                 ║")
	fmt.Fprintln(os.Stderr, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(os.Stderr, "")
}

// printDeaconLoopBlock prints the standard block banner for a deacon
// open-ended or batched patrol loop. reason is the specific loop shape
// detected.
func printDeaconLoopBlock(reason string) {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "╔══════════════════════════════════════════════════════════════════╗")
	fmt.Fprintln(os.Stderr, "║  ❌ PATROL LOOP BLOCKED                                          ║")
	fmt.Fprintln(os.Stderr, "╠══════════════════════════════════════════════════════════════════╣")
	fmt.Fprintf(os.Stderr, "║  %-64s║\n", truncateStr(reason, 64))
	fmt.Fprintf(os.Stderr, "║  %-64s║\n", "Run one patrol cycle, then use gt patrol report or gt handoff.")
	fmt.Fprintln(os.Stderr, "╚══════════════════════════════════════════════════════════════════╝")
	fmt.Fprintln(os.Stderr, "")
}
