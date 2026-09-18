package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/steveyegge/gastown/internal/style"
)

// heavyTestPackages are the packages whose full test run costs minutes on
// this host (measured 2026-09-18, alone: internal/cmd 183s, internal/polecat
// 175s, internal/daemon 385s; under contention 500-700s). A polecat has no
// reason to run one whole: the refinery runs the full suite once per
// submission (and `gt done` tests the light changed packages). Five polecats doing it anyway
// put ~81 of 185 agent-minutes into the test loop in one afternoon
// (gt-pxlg), and the formula text asking them not to was not enough.
var heavyTestPackages = map[string]bool{
	"internal/cmd":      true,
	"internal/daemon":   true,
	"internal/polecat":  true,
	"internal/refinery": true,
}

// isPolecatContext reports whether the current process runs as a polecat —
// the only role this rule applies to. The refinery MUST run whole packages
// (that is its job) and crew are operators.
func isPolecatContext() bool {
	if os.Getenv("GT_POLECAT") != "" {
		return true
	}
	// GT_ROLE is the signal every spawn carries ("gastown/polecats/topaz");
	// GT_POLECAT is set by AgentEnv alongside it but a hook that inherited
	// only the role must still recognise the polecat.
	if role := os.Getenv("GT_ROLE"); strings.Contains(role, "/polecats/") || role == "polecat" {
		return true
	}
	cwd, err := os.Getwd()
	return err == nil && strings.Contains(cwd, "/polecats/")
}

// evaluatePolecatTestScope blocks, per shell segment, a `go test` that (a)
// targets the whole repo, or (b) names a heavy package with no -run filter.
// Wrapping in gt slot run does not exempt it: the slot protects Docker, not
// the host's CPU. A -run filter on any number of packages is allowed — the
// cost of a filtered run is the compile, seconds not minutes.
func evaluatePolecatTestScope(command string) (reason string, matched []string) {
	tokens := shellTokenize(strings.TrimSpace(command))
	var segment []string
	for _, tok := range tokens {
		if shellCommandSeparators[tok] {
			if r, m := evaluatePolecatTestScopeSegment(segment); r != "" {
				return r, m
			}
			segment = nil
			continue
		}
		segment = append(segment, tok)
	}
	return evaluatePolecatTestScopeSegment(segment)
}

func evaluatePolecatTestScopeSegment(tokens []string) (reason string, matched []string) {
	lower := make([]string, len(tokens))
	for i, t := range tokens {
		lower[i] = strings.ToLower(t)
	}
	i := findAdjacentPair(lower, "go", "test")
	if i < 0 {
		return "", nil
	}
	rest := tokens[i+2:]
	hasRun := false
	for _, t := range rest {
		if t == "--" {
			break
		}
		if t == "-run" || strings.HasPrefix(t, "-run=") || t == "-test.run" || strings.HasPrefix(t, "-test.run=") {
			hasRun = true
		}
	}
	var heavy []string
	for _, arg := range goTestPackageArgs(rest) {
		norm := normalizeGoPackageArg(arg)
		if norm == "" {
			return "polecat 'go test' of the whole repo", []string{arg}
		}
		switch {
		case heavyTestPackages[norm] || heavyTestPackages[topTwoPathSegments(norm)]:
			heavy = append(heavy, norm)
		case strings.HasSuffix(arg, "/...") || strings.HasSuffix(arg, "..."):
			// An ancestor wildcard (./internal/...) runs every heavy package
			// beneath it; name them so the message says what it caught.
			for hp := range heavyTestPackages {
				if strings.HasPrefix(hp, norm+"/") {
					heavy = append(heavy, hp)
				}
			}
		}
	}
	if len(heavy) > 0 && !hasRun {
		return "polecat 'go test' of a whole heavy package with no -run filter", heavy
	}
	return "", nil
}

// topTwoPathSegments reduces "internal/cmd/sub" to "internal/cmd" so a
// subpackage of a heavy package is judged with its parent.
func topTwoPathSegments(p string) string {
	parts := strings.SplitN(p, "/", 3)
	if len(parts) < 2 {
		return p
	}
	return parts[0] + "/" + parts[1]
}

func printPolecatTestScopeBlock(reason, command string, matched []string) {
	fmt.Fprintln(os.Stderr, style.Bold.Render("❌ TEST SCOPE (polecat)"))
	fmt.Fprintf(os.Stderr, "  %s\n", reason)
	if len(matched) > 0 {
		fmt.Fprintf(os.Stderr, "  packages: %s\n", strings.Join(matched, " "))
	}
	fmt.Fprintf(os.Stderr, "  command:  %s\n", command)
	fmt.Fprintln(os.Stderr, "  Whole-package runs of internal/cmd, daemon, polecat or refinery take 3-10 minutes")
	fmt.Fprintln(os.Stderr, "  each and there is no need for one: the refinery runs the full suite once per")
	fmt.Fprintln(os.Stderr, "  submission (gt done covers the light changed packages). Iterate on what you touched:")
	fmt.Fprintln(os.Stderr, "    go test ./internal/<pkg>/ -run 'TestOne|TestTwo'")
	fmt.Fprintln(os.Stderr, "  (gt-pxlg)")
}
