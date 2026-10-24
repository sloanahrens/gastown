package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/workspace"
)

// bdCreateStoreRefusal is the line a polecat reads when its bd create would
// file into the town store.
const bdCreateStoreRefusal = "file it in your rig: cd to your worktree"

var tapGuardBdCreateStoreCmd = &cobra.Command{
	Use:   "bd-create-store",
	Short: "Block a polecat's bd create that would file into the town (hq) store",
	Long: `Block a polecat's bd create when the effective working directory resolves to
the town's beads store.

bd picks its store from the directory it runs in: inside a polecat worktree it
is the rig's store, at the town root (~/gt on a host, /sandbox/gt in a sandbox)
it is the town's hq store. A work bug filed there is invisible to the rig's
dispatcher. The effective directory is the hook payload's cwd moved by every
literal cd earlier in the same command; a cd whose target cannot be resolved
before a bd create is refused too. gt escalate, gt ask and gt done create town
beads inside gt itself and never reach this hook.

Exit codes:
  0 - Allowed (not a polecat, not a bd create, or the store is the rig's)
  2 - BLOCKED`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return tapGuardBdCreateStore(os.Stdin, os.Stderr, realGuardProcess())
	},
}

func init() {
	tapGuardCmd.AddCommand(tapGuardBdCreateStoreCmd)
}

// bdCreateVerbs are the bd subcommands that file a new bead.
var bdCreateVerbs = map[string]bool{"create": true, "new": true, "q": true}

func tapGuardBdCreateStore(stdin io.Reader, stderr io.Writer, proc guardProcess) error {
	input, err := io.ReadAll(stdin)
	if err != nil {
		return nil
	}
	command := extractCommand(input)
	if command == "" || !isPolecatContext(proc) {
		return nil
	}
	cwd := payloadCwd(input)
	if !filepath.IsAbs(cwd) {
		if cwd, err = proc.getwd(); err != nil {
			return nil
		}
	}
	townRoot, err := workspace.Find(cwd)
	if err != nil || strings.TrimSpace(townRoot) == "" {
		return nil
	}
	g := bdCreateStoreGuard{proc: proc, townStore: filepath.Join(townRoot, ".beads")}
	reason := g.walk(command, cwd, 0)
	if reason == "" {
		return nil
	}
	fmt.Fprintf(stderr, "\n❌ BD CREATE BLOCKED: %s\n   %s (a bead filed in the town store is invisible to your rig)\n\n", reason, bdCreateStoreRefusal)
	return NewSilentExit(2)
}

type bdCreateStoreGuard struct {
	proc      guardProcess
	townStore string
}

// walk judges every bd create in command, following literal cd words. cwd ""
// means a cd target was unresolvable.
func (g bdCreateStoreGuard) walk(command, cwd string, depth int) string {
	if depth > 3 {
		return "command nesting too deep to follow"
	}
	stripped := stripHeredocBodies(strings.TrimSpace(command))
	var segment []string
	judge := func() string {
		reason, next := g.segment(segment, cwd, depth)
		cwd = next
		return reason
	}
	for _, tok := range shellTokenize(stripped) {
		if shellCommandSeparators[tok] {
			if reason := judge(); reason != "" {
				return reason
			}
			segment = nil
			continue
		}
		segment = append(segment, tok)
	}
	if reason := judge(); reason != "" {
		return reason
	}
	for _, body := range commandSubstitutions(stripped) {
		if reason := g.walk(body, cwd, depth+1); reason != "" {
			return reason
		}
	}
	return ""
}

// segment judges one simple command and returns the directory later segments
// run in.
func (g bdCreateStoreGuard) segment(segment []string, cwd string, depth int) (reason, next string) {
	beadsDir := ""
	for _, tok := range segment {
		if !isEnvAssignment(tok) {
			break
		}
		if v, ok := strings.CutPrefix(tok, "BEADS_DIR="); ok {
			beadsDir = v
		}
	}
	tokens := bdCloseCommandTokens(segment)
	if len(tokens) == 0 {
		return "", cwd
	}
	word, args := tokens[0], tokens[1:]
	base := filepath.Base(word)
	switch {
	case base == "cd" || base == "pushd":
		return "", g.followCd(args, cwd)
	case isBdCommandWord(word):
		if i := bdSubcommandIndex(args); i >= 0 && bdCreateVerbs[args[i]] {
			return g.judgeCreate(cwd, beadsDir), cwd
		}
	default:
		if payload, ok := shellPayload(base, args); ok {
			return g.walk(payload, cwd, depth+1), cwd
		}
	}
	return "", cwd
}

func (g bdCreateStoreGuard) judgeCreate(cwd, beadsDir string) string {
	if beadsDir == "" {
		beadsDir = g.proc.getenv("BEADS_DIR")
	}
	if beadsDir == "" {
		if cwd == "" {
			return "a cd before this bd create has a target this guard cannot resolve; use a literal path, or run the bd create in its own Bash call"
		}
		beadsDir = cwd
	}
	if sameDir(beads.ResolveBeadsDir(beadsDir), g.townStore) {
		return "this bd create resolves to the town (hq) store"
	}
	return ""
}

// followCd returns the directory a cd leaves the shell in, "" when the target
// is not a plain path (an unset variable, a substitution, cd -).
func (g bdCreateStoreGuard) followCd(args []string, cwd string) string {
	target := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") || a == "-" {
			target = a
			break
		}
	}
	home, _ := g.proc.homeDir()
	switch {
	case target == "" || target == "~":
		target = home
	case strings.HasPrefix(target, "~/"):
		target = filepath.Join(home, target[2:])
	case strings.HasPrefix(target, "$HOME/"):
		target = filepath.Join(g.proc.getenv("HOME"), target[len("$HOME/"):])
	}
	if target == "" || target == "-" || strings.ContainsAny(target, "$`*?[") {
		return ""
	}
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	if cwd == "" {
		return ""
	}
	return filepath.Join(cwd, target)
}

// sameDir compares two directories, following symlinks that exist (a macOS
// temp dir is under /var, a symlink to /private/var).
func sameDir(a, b string) bool {
	if r, err := filepath.EvalSymlinks(a); err == nil {
		a = r
	}
	if r, err := filepath.EvalSymlinks(b); err == nil {
		b = r
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
