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
// templates and scripts, role configs, and exec.Command/BdCmd literals name
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
	trees := map[string]*cmdtree.Tree{
		"gt": cmdtree.FromCobra(rootCmd, gtTakesArgs),
		"bd": bdTree,
	}

	refs, err := cmdtree.ScanRepo(root)
	if err != nil {
		t.Fatalf("ScanRepo: %v", err)
	}
	if len(refs) < 100 {
		// A scanner regression that finds nothing would pass silently.
		t.Fatalf("ScanRepo found only %d gt/bd invocations; the scanner is not reading the repo", len(refs))
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

// gtTakesArgs reports whether a gt command accepts positional arguments. A
// parent whose RunE is requireSubcommand, or that is not runnable, accepts
// none, so a plain word after it must name one of its subcommands. So does a
// command whose Args validator accepts no arguments but rejects one.
func gtTakesArgs(c *cobra.Command) bool {
	if !c.Runnable() {
		return false
	}
	if c.RunE != nil && reflect.ValueOf(c.RunE).Pointer() == reflect.ValueOf(requireSubcommand).Pointer() {
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
