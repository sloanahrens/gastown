package shelltier

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// postLandInputs is the shell tier manifest's trigger union: a change on main
// under one of these paths can move a shell test's verdict, so the post-land
// check runs the tier for it. TestPostLandShellInputsCoverTheManifestTriggers
// holds it to the manifest trigger by trigger, so a trigger added there cannot
// leave this skip behind (gt-vt2g4). The live script's INPUTS line repeats this
// until the overseer switches the setting; TestPostLandScriptInputsLine holds
// the two equal.
const postLandInputs = `^(scripts/|plugins/|\.githooks/|deploy/|Makefile$|go\.mod$|go\.sum$|internal/|cmd/)`

// postLandWindow is how far back main's history is read when --window names
// none, in git's --since syntax.
const postLandWindow = "2 hours"

// postLandListMax caps how many matching paths the check prints. The list is
// sorted before the cap, so the first 20 in sort order are the ones named.
const postLandListMax = 20

// postLandUsageExit is the code for a command line the check cannot read.
const postLandUsageExit = 2

// postLandRE is postLandInputs compiled as POSIX ERE, the syntax the grep the
// check replaces reads (gt-r2u9g).
var postLandRE = regexp.MustCompilePOSIX(postLandInputs)

// Git runs one git command from the repository root, writes its output to out
// and returns its exit code. A non-zero code means the command failed.
type Git func(argv []string, out io.Writer) int

// Tier runs the shell tier and returns its exit code.
type Tier func() int

// PostLand is the post-landing check. It reads main's first-parent history
// inside the window and runs the shell tier only when a changed path matches
// the trigger union. It prints the matching paths, or the skip line when none
// match, and returns the tier's exit code when the tier runs, 0 when it is
// skipped, and 2 for a command line it cannot read. A history it cannot read
// runs the tier: no skip without evidence.
//
// args are the arguments after the subcommand name; the only one is --window.
func PostLand(args []string, stdout, stderr io.Writer, git Git, tier Tier) int {
	fs := flag.NewFlagSet("post-land", flag.ContinueOnError)
	fs.SetOutput(stderr)
	window := fs.String("window", postLandWindow, "how far back main's history is read, in git's --since syntax")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return postLandUsageExit
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "post-land: unexpected argument %q\n", fs.Arg(0))
		return postLandUsageExit
	}
	if *window == "" {
		fmt.Fprintln(stderr, "post-land: --window needs a git --since value")
		return postLandUsageExit
	}
	history, ok := readMainHistory(git, *window)
	if !ok {
		fmt.Fprintln(stdout, "post-land-shell: cannot read main's history; running the shell tier")
		return tier()
	}
	hits := inputPaths(history)
	if len(hits) == 0 {
		fmt.Fprintf(stdout, "post-land-shell: no shell-test input changed on main in the last %s; shell tier skipped\n", *window)
		return 0
	}
	fmt.Fprintf(stdout, "post-land-shell: shell-test inputs changed on main in the last %s:\n", *window)
	if len(hits) > postLandListMax {
		hits = hits[:postLandListMax]
	}
	for _, p := range hits {
		fmt.Fprintf(stdout, "  %s\n", p)
	}
	return tier()
}

// readMainHistory returns the paths of main's first-parent commits inside the
// window, one per line, and whether git could read them.
func readMainHistory(git Git, window string) (string, bool) {
	var out bytes.Buffer
	argv := []string{"git", "log", "--first-parent", "--since=" + window, "--name-only", "--format=", "HEAD"}
	if git(argv, &out) != 0 {
		return "", false
	}
	return out.String(), true
}

// inputPaths returns the distinct paths in history that match the trigger
// union, sorted.
func inputPaths(history string) []string {
	seen := make(map[string]bool)
	var hits []string
	for _, line := range strings.Split(history, "\n") {
		if seen[line] || !postLandRE.MatchString(line) {
			continue
		}
		seen[line] = true
		hits = append(hits, line)
	}
	sort.Strings(hits)
	return hits
}
