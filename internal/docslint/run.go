package docslint

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// The exit codes of the docs-lint command.
const (
	// ExitClean means every check ran and found nothing, or a listing or count
	// was printed.
	ExitClean = 0
	// ExitFindings means every check ran and at least one rule was broken.
	ExitFindings = 1
	// ExitFailed means a refused command line or a check that could not run. It
	// is never read as clean, and it is never exit 1 without a finding.
	ExitFailed = 2
)

// DefaultCeiling is the word ceiling of the agent-facing tier when
// DOCS_LINT_CEILING names none.
const DefaultCeiling = 1500

// Env is what Run takes from its process, so a test hands it a checkout and a
// git log in memory.
type Env struct {
	// Getenv reads DOCS_LINT_ROOT and DOCS_LINT_CEILING.
	Getenv func(string) string
	// TopLevel is the git top level of the working directory, the root when
	// DOCS_LINT_ROOT names none.
	TopLevel func() (string, error)
	// NewFS returns the checkout at root.
	NewFS func(root string) FS
	// NewGit returns the commit-date source for the checkout at root.
	NewGit func(root string) Git
}

// check is one rule's walk over the checkout.
type check struct {
	name string
	run  func(FS, int) ([]Finding, error)
}

// checks lists the rules in the order Run runs them and reports their errors.
func checks() []check {
	return []check{
		{"dead links", func(f FS, _ int) ([]Finding, error) { return CheckDeadLinks(f) }},
		{"dead make targets", func(f FS, _ int) ([]Finding, error) { return CheckDeadMakeTargets(f) }},
		{"status headers", func(f FS, _ int) ([]Finding, error) { return CheckStatusHeaders(f) }},
		{"status conflicts", func(f FS, _ int) ([]Finding, error) { return CheckStatusConflict(f) }},
		{"stray markup", func(f FS, _ int) ([]Finding, error) { return CheckStrayMarkup(f) }},
		{"polecat main push", func(f FS, _ int) ([]Finding, error) { return CheckPolecatMainPush(f) }},
		{"word ceiling", func(f FS, ceiling int) ([]Finding, error) { return CheckWordCeiling(f, ceiling) }},
	}
}

// Run is the docs-lint command: args are the words after its name. It prints a
// listing, a count or the findings to stdout and the reason for any refusal to
// stderr, and returns the exit code.
//
//	docs-lint                 run all checks; one line per finding; exit 1 on any
//	docs-lint --list <tier>   agent-facing | reference | historical | go
//	docs-lint --slice N M     N docs and M go files, least recently committed first
//	docs-lint --words         total words across the agent-facing tier
//
// A check that cannot run exits 2 with the reason on stderr, after the findings
// the other checks found: an erroring check with no findings is never exit 1 and
// never silent (gt-et39).
func Run(args []string, env Env, stdout, stderr io.Writer) int {
	req, err := parseArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return ExitFailed
	}
	ceiling := DefaultCeiling
	if req.mode == modeLint {
		if v := env.Getenv("DOCS_LINT_CEILING"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				fmt.Fprintf(stderr, "docs-lint: DOCS_LINT_CEILING '%s' is not a number of words\n", v)
				return ExitFailed
			}
			ceiling = n
		}
	}
	root := env.Getenv("DOCS_LINT_ROOT")
	if root == "" {
		top, err := env.TopLevel()
		if err != nil {
			fmt.Fprintf(stderr, "docs-lint: cannot find the repository root (set DOCS_LINT_ROOT): %v\n", err)
			return ExitFailed
		}
		root = top
	}
	fsys := env.NewFS(root)
	switch req.mode {
	case modeList:
		paths, err := List(fsys, req.tier)
		return printPaths(paths, err, stdout, stderr)
	case modeSlice:
		paths, err := Slice(fsys, env.NewGit(root), req.n, req.m)
		return printPaths(paths, err, stdout, stderr)
	case modeWords:
		total, err := Words(fsys)
		if err != nil {
			fmt.Fprintln(stderr, withPrefix(err))
			return ExitFailed
		}
		fmt.Fprintln(stdout, total)
		return ExitClean
	}
	return runChecks(fsys, ceiling, stdout, stderr)
}

// printPaths prints one path per line, or the reason it cannot.
func printPaths(paths []string, err error, stdout, stderr io.Writer) int {
	if err != nil {
		fmt.Fprintln(stderr, withPrefix(err))
		return ExitFailed
	}
	for _, p := range paths {
		fmt.Fprintln(stdout, p)
	}
	return ExitClean
}

// runChecks runs every check, prints the findings sorted, and reports each check
// that could not run. A check that errors does not stop the others, so the
// findings of the rest are still printed.
func runChecks(fsys FS, ceiling int, stdout, stderr io.Writer) int {
	var findings []Finding
	var failures []error
	for _, c := range checks() {
		got, err := c.run(fsys, ceiling)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		findings = append(findings, got...)
	}
	sortFindings(findings)
	for _, f := range findings {
		fmt.Fprintln(stdout, f)
	}
	for _, err := range failures {
		fmt.Fprintln(stderr, withPrefix(err))
	}
	switch {
	case len(failures) > 0:
		return ExitFailed
	case len(findings) > 0:
		return ExitFindings
	}
	return ExitClean
}

// sortFindings orders findings by path bytewise, then by line number, then by
// the rest of the line bytewise, so the order is the same on every run and host.
func sortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		a, b := findings[i], findings[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.String() < b.String()
	})
}

// withPrefix is err's text under the command's name, once.
func withPrefix(err error) string {
	msg := err.Error()
	if strings.HasPrefix(msg, "docs-lint:") {
		return msg
	}
	return "docs-lint: " + msg
}

type mode int

const (
	modeLint mode = iota
	modeList
	modeSlice
	modeWords
)

// request is a parsed command line.
type request struct {
	mode mode
	tier string
	n, m int
}

// parseArgs turns the words after the command's name into a request, or the
// usage error that refuses them. Anything that is not a documented option is refused
// rather than ignored.
func parseArgs(args []string) (request, error) {
	if len(args) == 0 {
		return request{mode: modeLint}, nil
	}
	rest := args[1:]
	switch args[0] {
	case "--list":
		if len(rest) != 1 || rest[0] == "" {
			return request{}, fmt.Errorf("docs-lint: --list needs one tier: agent-facing, reference, historical or go")
		}
		return request{mode: modeList, tier: rest[0]}, nil
	case "--slice":
		if len(rest) != 2 {
			return request{}, fmt.Errorf("docs-lint: --slice needs two counts: N docs and M go files")
		}
		n, errN := strconv.Atoi(rest[0])
		m, errM := strconv.Atoi(rest[1])
		if errN != nil || errM != nil || n < 0 || m < 0 {
			return request{}, fmt.Errorf("docs-lint: --slice counts must be whole numbers, got '%s' and '%s'", rest[0], rest[1])
		}
		return request{mode: modeSlice, n: n, m: m}, nil
	case "--words":
		if len(rest) != 0 {
			return request{}, fmt.Errorf("docs-lint: --words takes no argument, got '%s'", rest[0])
		}
		return request{mode: modeWords}, nil
	}
	return request{}, fmt.Errorf("docs-lint: unknown option '%s'", args[0])
}
