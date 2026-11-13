// Command changedpkgs prints the Go packages a branch changed, one "./dir"
// per line, for `make presubmit` (gt-ssyxd). It reads
// `git diff --name-status -z -M <base>...HEAD`, so a branch is judged by its
// own commits, never by what landed on the base after it branched.
//
// -guards prints instead the arguments for a second `go test` run over the
// tree-wide guard tests the branch's changed paths trigger and the guards that
// always run (gt-ydzwb, gt-t4rchq), "-run <union of test names> ./pkg ...",
// and nothing for an empty diff.
// The guard tests run under -run so presubmit does not pay for the whole
// suite of the package that hosts one: internal/cmd and internal/polecat take
// minutes each, and the guard test inside them takes a second.
//
// The pod-engine inventory (gt-tk6l7k) pins file lines in
// docs/design/pod-engine.md, and internal/podengine's tests fail when a pinned
// line moves. A file is pinned when the doc's inventory File column names it,
// or when the doc cites a Go file with a line anywhere (a prose line ref or an
// Evidence cell). The package list names ./internal/podengine when the branch
// changes a pinned file. The doc must be readable: a branch never passes
// presubmit because the pins it may touch could not be read. -podengine-recovery
// runs the inventory tests and, when a pinned file moved and they fail, prints
// the one-line recovery. presubmit calls it once, after its changed-package
// tests fail, so the hint prints once and names the renumber steps.
//
// The list also names the direct importers of the changed packages
// (gt-cwkzxk): a change to internal/rig tests internal/townstate, whose test
// imports it, so a break in an importer fails presubmit rather than the
// landing gate.
// The heavy packages among them (land.SelectImporters) are left out and named
// on stderr with the reason. A graph that cannot be read fails the run.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/presubmit"
)

// options are the mode flags of one run.
type options struct {
	guards   bool
	recovery bool
}

// seams are what a run reaches outside the process: the inventory document, the
// inventory package's tests, and the check for Go in a directory. main wires
// the real ones, and the tests inject fakes.
type seams struct {
	readDoc      func() ([]byte, error)
	runInventory func() error
	hasGo        func(dir string) bool
	importers    func(dirs []string) (land.ImporterSelection, error)
}

func main() {
	base := flag.String("base", "origin/main", "ref the branch is compared against")
	guards := flag.Bool("guards", false, "print the go test arguments for the tree-wide guard tests the branch triggers")
	recovery := flag.Bool("podengine-recovery", false, "print the pod-engine recovery line when the branch changes a file the inventory pins and its tests fail")
	flag.Parse()

	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "changedpkgs: not in a git work tree:", err)
		os.Exit(1)
	}
	repoRoot := string(root[:len(root)-1])

	diff := exec.Command("git", land.NameStatusArgs(*base+"...HEAD")...)
	diff.Stderr = os.Stderr
	out, err := diff.Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "changedpkgs: git diff %s...HEAD failed: %v\n", *base, err)
		os.Exit(1)
	}
	s := seams{
		// The inventory is read from the tree the branch is checked out on.
		readDoc: func() ([]byte, error) { return os.ReadFile(filepath.Join(repoRoot, presubmit.PodEngineDoc)) },
		runInventory: func() error {
			return exec.Command("go", "test", "-count=1", "./"+presubmit.PodEngineDir).Run()
		},
		hasGo: func(d string) bool { return land.PackageDirHasGo(repoRoot, d) },
		importers: func(dirs []string) (land.ImporterSelection, error) {
			return land.SelectImporters(runCommand, repoRoot, "HEAD", dirs)
		},
	}
	os.Exit(run(options{guards: *guards, recovery: *recovery}, string(out), s, os.Stdout, os.Stderr))
}

// run writes the selection for nameStatus to stdout, a refusal to stderr, and
// returns the exit status.
func run(o options, nameStatus string, s seams, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintln(stderr, "changedpkgs:", err)
		return 1
	}
	if o.guards {
		sel, err := land.GuardSelects(nameStatus, s.hasGo)
		if err != nil {
			return fail(err)
		}
		printGuards(stdout, sel)
		return 0
	}
	// A document that cannot be read fails the run: a branch must not pass
	// presubmit because the pins it may touch could not be read.
	raw, err := s.readDoc()
	if err != nil {
		return fail(fmt.Errorf("reading %s: %w", presubmit.PodEngineDoc, err))
	}
	doc := string(raw)
	if o.recovery {
		line, err := presubmit.RecoveryHint(nameStatus, doc, s.runInventory)
		if err != nil {
			return fail(err)
		}
		if line != "" {
			fmt.Fprintln(stdout, line)
		}
		return 0
	}
	dirs, err := land.ChangedPackages(nameStatus, s.hasGo)
	if err != nil {
		return fail(err)
	}
	dirs, err = presubmit.PodEngineDirs(dirs, nameStatus, doc)
	if err != nil {
		return fail(err)
	}
	// The direct importers of the changed packages are tested too (gt-cwkzxk).
	// A graph that cannot be read fails the run: the changed packages alone
	// would let a change to a widely imported package pass presubmit.
	sel, err := s.importers(dirs)
	if err != nil {
		return fail(err)
	}
	for _, l := range sel.LeftOut {
		fmt.Fprintf(stderr, "changedpkgs: left out direct importer %s: %s\n", l.Dir, l.Reason)
	}
	listed := map[string]bool{}
	for _, dir := range dirs {
		listed[dir] = true
	}
	for _, dir := range sel.Add {
		if !listed[dir] {
			listed[dir] = true
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		fmt.Fprintln(stdout, "./"+dir)
	}
	return 0
}

// runCommand runs argv in the working directory and returns its standard
// output, with the tool's own diagnostics left on the terminal. The error is
// the command's failure, never its output, which this cannot judge for secrets.
// The selector reads only git and go, so the program is named literally and
// anything else is refused.
func runCommand(argv []string) ([]byte, error) {
	var cmd *exec.Cmd
	switch {
	case len(argv) > 0 && argv[0] == "git":
		cmd = exec.Command("git", argv[1:]...)
	case len(argv) > 0 && argv[0] == "go":
		cmd = exec.Command("go", argv[1:]...)
	default:
		return nil, errors.New("refusing to run a program other than git or go")
	}
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

// printGuards writes one line of `go test` arguments, or nothing when the
// branch triggers no guard. The union of the test names is safe to expand
// unquoted: a Go test name holds no space and no shell metacharacter.
func printGuards(w io.Writer, sel []land.GuardSelect) {
	if len(sel) == 0 {
		return
	}
	names := map[string]bool{}
	pkgs := make([]string, 0, len(sel))
	for _, s := range sel {
		pkgs = append(pkgs, "./"+s.Package)
		for _, name := range s.Tests {
			names[name] = true
		}
	}
	union := make([]string, 0, len(names))
	for name := range names {
		union = append(union, name)
	}
	sort.Strings(union)
	sort.Strings(pkgs)
	fmt.Fprintf(w, "-run ^(%s)$ %s\n", strings.Join(union, "|"), strings.Join(pkgs, " "))
}
