package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/cmdtree"
)

// TestCommandTokensResolve is the command-tree lint (gt-fcxe9.5, deep review
// G4-03/B1-08/B5-11). Formulas, role and message templates, plugins, hook
// templates and scripts, role configs, repo scripts and git hooks, agent
// wrappers, the repo's agent commands and skills, and exec.Command/BdCmd literals name
// gt and bd commands as untyped strings; nothing else notices when one of
// them is renamed or removed, and an agent handed "unknown command"
// improvises. Every gt invocation must resolve against this binary's cobra
// tree and every bd invocation against internal/cmdtree/bd-command-tree.json
// (refresh: make bd-command-tree). bd sync is deny-listed outright.
//
// Fix a failure by correcting the file that names the command, never by
// loosening the lint.
func TestCommandTokensResolve(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}

	bdTree, snap, err := cmdtree.LoadBdTree()
	if err != nil {
		t.Fatalf("LoadBdTree: %v", err)
	}
	gtTree := cmdtree.FromCobra(rootCmd, gtTakesArgs)
	markRequireSubcommandHelpOnly(gtTree, rootCmd)
	trees := map[string]*cmdtree.Tree{
		"gt": gtTree,
		"bd": bdTree,
	}

	refs, err := cmdtree.ScanRepo(root)
	if err != nil {
		t.Fatalf("ScanRepo: %v", err)
	}
	// A scanner that goes blind on one kind of file would pass silently, so
	// each source must still yield a floor of invocations. The floors sit near
	// half of the 2026-09-29 counts (formulas 772, templates 583, plugins 234,
	// go 153, hooks 17, role configs 3; templates jumped from 332 once
	// {{ cmd }} counted as gt); lower one only when files that call gt/bd were
	// really removed. The witness/deacon deletion (gt-4k3fj.6.1) took formulas
	// to 319 and the role configs to none, so both floors dropped with it.
	// The sling/convoy conversion (gt-z56xs.5) deleted dead bd-calling code
	// and took go to 69. Repo scripts, git hooks and agent wrappers (44) and
	// the repo's .claude/.cursor commands and skills (20) joined on 2026-09-30
	// (gt-fd2cu.4). The hooks floor went with the non-Claude hook templates
	// (D4): the Claude settings templates call {{GT_BIN}}, which the scanner
	// does not read as gt. The dog-job conversion (gt-4k3fj.8.1/.8.5) deleted
	// the compactor, dolt-backup, dolt-log-rotate, github-sheriff, git-hygiene,
	// gitignore-reconcile, submodule-commit, dolt-archive and dolt-snapshots
	// plugins and took plugins to 69. The dispatch/install conversion
	// (gt-4k3fj.8.6) deleted seat-refill and rebuild-gt — the last two
	// dispatch-shaped scripts — and took plugins to 5, tool-updater alone.
	// The one-client migration
	// (gt-7iwy0.4.1) keeps moving raw bd argv onto typed beads methods, which
	// the scanner does not read; go was 34 after the handoff/hook/rollback move,
	// 18 after the sling formula/duplicate/done move, and 17 after the
	// scheduler-run leaf (gt-638go.9) deleted the daemon's `gt scheduler run`.
	floors := map[string]int{"formulas": 160, "templates": 290, "plugins": 3, "go": 17, "scripts": 22, "agent": 10}
	counts := map[string]int{}
	for _, r := range refs {
		counts[refSource(r.File)]++
	}
	for src, floor := range floors {
		if counts[src] < floor {
			t.Errorf("ScanRepo found %d gt/bd invocations in %s (floor %d); the scanner has stopped reading those files", counts[src], src, floor)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	violations := cmdtree.Check(refs, trees)
	if len(violations) == 0 {
		return
	}
	lines := make([]string, len(violations))
	for i, v := range violations {
		lines[i] = v.String()
	}
	t.Fatalf("%d gt/bd invocation(s) do not resolve against the command trees (gt: this binary; bd: %s, contract_version %d):\n%s",
		len(violations), snap.Source, snap.ContractVersion, strings.Join(lines, "\n"))
}

// isRequireSubcommand reports whether c's RunE is requireSubcommand.
func isRequireSubcommand(c *cobra.Command) bool {
	return c.RunE != nil && reflect.ValueOf(c.RunE).Pointer() == reflect.ValueOf(requireSubcommand).Pointer()
}

// markRequireSubcommandHelpOnly marks every requireSubcommand parent
// help-only in tree. cmdtree.FromCobra only marks parents cobra cannot run;
// a requireSubcommand parent runs, but only to fail, so an invocation that
// stops on one is as broken as one that stops on a help-only parent.
func markRequireSubcommandHelpOnly(tree *cmdtree.Tree, root *cobra.Command) {
	var walk func(c *cobra.Command, path []string)
	walk = func(c *cobra.Command, path []string) {
		for _, child := range c.Commands() {
			p := append(append([]string(nil), path...), child.Name())
			if child.HasSubCommands() && isRequireSubcommand(child) {
				tree.MarkHelpOnly(p)
			}
			walk(child, p)
		}
	}
	walk(root, nil)
}

// refSource buckets a repo-relative path by the scanner input it belongs to.
func refSource(file string) string {
	switch {
	case strings.HasSuffix(file, ".go"):
		return "go"
	case strings.HasPrefix(file, "internal/formula/formulas/"):
		return "formulas"
	case strings.HasPrefix(file, "internal/templates/"), strings.HasPrefix(file, "templates/"):
		return "templates"
	case strings.HasPrefix(file, "plugins/"):
		return "plugins"
	case strings.HasPrefix(file, "internal/config/roles/"):
		return "roles"
	case strings.HasPrefix(file, "scripts/"), strings.HasPrefix(file, ".githooks/"):
		return "scripts"
	case strings.HasPrefix(file, ".claude/"):
		return "agent"
	}
	return "other"
}

// gtTakesArgs reports whether a gt command accepts positional arguments. A
// parent whose RunE is requireSubcommand, or that is not runnable, accepts
// none, so a plain word after it must name one of its subcommands. So does a
// command whose Args validator accepts no arguments but rejects one.
func gtTakesArgs(c *cobra.Command) bool {
	if !c.Runnable() {
		return false
	}
	if isRequireSubcommand(c) {
		return false
	}
	if c.Args != nil && c.Args(c, nil) == nil && c.Args(c, []string{"x"}) != nil {
		return false
	}
	return true
}

func TestGtTakesArgs(t *testing.T) {
	t.Parallel()
	if gtTakesArgs(&cobra.Command{Use: "p", RunE: requireSubcommand}) {
		t.Error("a requireSubcommand parent must be argument-free")
	}
	if gtTakesArgs(&cobra.Command{Use: "p"}) {
		t.Error("a non-runnable parent must be argument-free")
	}
	if !gtTakesArgs(&cobra.Command{Use: "p", Run: func(*cobra.Command, []string) {}}) {
		t.Error("a runnable leaf with no Args validator takes arguments")
	}
	if gtTakesArgs(&cobra.Command{Use: "p", Args: cobra.NoArgs, Run: func(*cobra.Command, []string) {}}) {
		t.Error("a cobra.NoArgs leaf must be argument-free")
	}
	if !gtTakesArgs(&cobra.Command{Use: "p", Args: cobra.ExactArgs(1), Run: func(*cobra.Command, []string) {}}) {
		t.Error("an ExactArgs(1) leaf takes arguments")
	}
}

// TestMarkRequireSubcommandHelpOnly drives the failing branch the lint relies
// on: an invocation that stops on a requireSubcommand parent (by name or by
// alias) must resolve as help-only, while its subcommands and a runnable
// leaf still resolve.
func TestMarkRequireSubcommandHelpOnly(t *testing.T) {
	t.Parallel()
	root := &cobra.Command{Use: "gt"}
	parent := &cobra.Command{Use: "p", Aliases: []string{"pp"}, RunE: requireSubcommand}
	parent.AddCommand(&cobra.Command{Use: "child", Run: func(*cobra.Command, []string) {}})
	root.AddCommand(parent, &cobra.Command{Use: "leaf", Run: func(*cobra.Command, []string) {}})

	tree := cmdtree.FromCobra(root, gtTakesArgs)
	if r := tree.Resolve([]string{"p"}); !r.OK || r.HelpOnly {
		t.Fatalf("before marking, Resolve(p) = %+v; FromCobra alone must not flag a runnable parent", r)
	}
	markRequireSubcommandHelpOnly(tree, root)
	for _, words := range [][]string{{"p"}, {"pp"}} {
		if r := tree.Resolve(words); r.OK || !r.HelpOnly {
			t.Errorf("Resolve(%v) = %+v, want a help-only failure", words, r)
		}
	}
	for _, words := range [][]string{{"p", "child"}, {"pp", "child"}, {"leaf"}} {
		if r := tree.Resolve(words); !r.OK {
			t.Errorf("Resolve(%v) = %+v, want OK", words, r)
		}
	}
}
