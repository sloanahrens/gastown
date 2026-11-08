package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/gatemodel"
	"github.com/steveyegge/gastown/internal/land"
)

// gateGitTimeout bounds one git call and gateListTimeout one go list of the
// tree.
const (
	gateGitTimeout  = time.Minute
	gateListTimeout = 5 * time.Minute
)

var (
	gateModelLogs string
	gateModelBase string
	gateModelLast int
	gateModelCPUs int
	gateModelJSON bool
)

var gateCmd = &cobra.Command{
	Use:     "gate",
	GroupID: GroupDiag,
	Short:   "Measure a selective landing gate without changing it",
	Long: `Measure what a selective landing gate would run, and what it would save.

The model is hypothetical: it runs no test and changes no gate. For each
landing commit under --base it reads the landing's diff and the repository's
go list import graph, and computes the packages the gate would test: the
changed packages, the packages whose code or tests import them, and the
always-run set (internal/guardlint, internal/makegate, internal/testpolicy,
plus the guard packages land.GuardSelects names for the changed paths). A
change to go.mod, go.sum, go.work, the Makefile, scripts/, .forgejo/, deploy/,
a nested module, or a path the rules cannot place falls back to the full run.

Gate job logs are saved by a separate step; this command calls no Forgejo. A
log named <sha>.log belongs to that landing. Every log in --logs supplies per
package walls from its "tierwall:" lines, and a package a log omits takes the
largest wall any log reports, or 0 with a note.

The modeled unit-tier wall is
max(longest selected package, sum of package walls / cpus).
The summary reports the landings, the fallbacks, the median and p90 modeled
saving, and the hub landings that select 60 or more packages.

Examples:
  gt gate model --logs .runtime/gate-logs --base origin/main --last 40
  gt gate model --logs .runtime/gate-logs --cpus 8 --json`,
	RunE: requireSubcommand,
}

var gateModelCmd = &cobra.Command{
	Use:   "model",
	Short: "Recompute the savings of a selective landing gate from logs, diffs and the import graph",
	Long: `Print, per landing, the changed packages, the dependents, the always-run set,
the fallback reason if any, and the modeled unit-tier wall beside the wall the
landing's own gate log reports, then the summary. --json prints the same as
JSON. The report goes to standard output; the command writes no file.`,
	Args: cobra.NoArgs,
	RunE: runGateModel,
}

func init() {
	gateModelCmd.Flags().StringVar(&gateModelLogs, "logs", "", "directory of saved gate job logs (required)")
	gateModelCmd.Flags().StringVar(&gateModelBase, "base", "HEAD", "ref whose landing commits are read, walking back")
	gateModelCmd.Flags().IntVar(&gateModelLast, "last", 40, "number of landing commits to model")
	gateModelCmd.Flags().IntVar(&gateModelCPUs, "cpus", 0, "cpus the modeled gate runs on (0: this host's count)")
	gateModelCmd.Flags().BoolVar(&gateModelJSON, "json", false, "print the report as JSON")

	gateCmd.AddCommand(gateModelCmd)
	rootCmd.AddCommand(gateCmd)
}

// gateModelOptions are the flags of `gt gate model`.
type gateModelOptions struct {
	Logs string
	Base string
	Last int
	CPUs int
	JSON bool
}

// runGateModel is the cobra entry point: it hands the parsed flags to
// gateModel.
func runGateModel(cmd *cobra.Command, args []string) error {
	return gateModel(cmd, gateModelOptions{
		Logs: gateModelLogs,
		Base: gateModelBase,
		Last: gateModelLast,
		CPUs: gateModelCPUs,
		JSON: gateModelJSON,
	})
}

// gateModel reads the landings, the graph and the logs, then prints the
// report. An input it cannot read fails closed: the graph and a landing's
// diff become the full run, while a missing log directory or an unreadable
// nested-module tree fails the command rather than reporting a model it did
// not compute.
func gateModel(cmd *cobra.Command, opts gateModelOptions) error {
	if opts.Logs == "" {
		return fmt.Errorf("--logs is required: the model reads per-package walls from saved gate job logs")
	}
	if opts.Last < 1 {
		return fmt.Errorf("--last must be at least 1, got %d", opts.Last)
	}
	cpus := opts.CPUs
	if cpus == 0 {
		cpus = runtime.NumCPU()
	}
	if cpus < 1 {
		return fmt.Errorf("--cpus must be at least 1, got %d", opts.CPUs)
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	src := gateSources{git: gateGit, graph: gateGraph, nested: gateNestedModules}
	return gateModelRun(ctx, src, opts, cpus, cmd.OutOrStdout())
}

// gateGitRunner runs one git command in dir and returns its stdout.
type gateGitRunner func(ctx context.Context, dir string, args ...string) (string, error)

// gateSources are the reads gateModelRun makes of the repository. gateModel
// passes the real ones; a test passes fakes, so no git or go tool runs.
type gateSources struct {
	git    gateGitRunner
	graph  func(ctx context.Context, root string) *gatemodel.Graph
	nested func(root string) ([]string, error)
}

// gateModelRun reads the repository through src, the logs in opts.Logs, and
// prints the report to out. It is gateModel without the flag checks and the
// real reads.
func gateModelRun(ctx context.Context, src gateSources, opts gateModelOptions, cpus int, out io.Writer) error {
	root, err := gateRepoRoot(ctx, src.git)
	if err != nil {
		return err
	}
	landings, err := gateLandings(ctx, src.git, root, opts.Base, opts.Last)
	if err != nil {
		return err
	}
	g := src.graph(ctx, root)
	logs, err := gatemodel.ParseLogs(opts.Logs, g)
	if err != nil {
		return err
	}
	nested, err := src.nested(root)
	if err != nil {
		return err
	}
	hasGo := func(dir string) bool { return land.PackageDirHasGo(root, dir) }
	rep, err := gatemodel.Build(landings, g, hasGo, nested, logs, cpus)
	if err != nil {
		return err
	}
	if opts.JSON {
		return gatemodel.RenderJSON(out, rep)
	}
	return gatemodel.RenderText(out, rep)
}

// gateRepoRoot is the repository the model reads.
func gateRepoRoot(ctx context.Context, git gateGitRunner) (string, error) {
	out, err := git(ctx, ".", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("gt gate model must run in a git work tree: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// gateLandings reads up to last landing commits at base, each with its diff
// against its parent. A ref beginning with '-' is refused: it would be read by
// git as an option.
func gateLandings(ctx context.Context, git gateGitRunner, root, base string, last int) ([]gatemodel.Landing, error) {
	if base == "" || strings.HasPrefix(base, "-") {
		return nil, fmt.Errorf("refusing --base %q: it must name a ref and not start with -", base)
	}
	out, err := git(ctx, root, "log", "--format=%H%x00%h%x00%s%x00",
		"--grep=^land: ", "--max-count="+strconv.Itoa(last), base, "--")
	if err != nil {
		return nil, fmt.Errorf("reading the landing commits: %w", err)
	}
	landings, err := parseLandingRefs(out)
	if err != nil {
		return nil, err
	}
	for i := range landings {
		sha := landings[i].SHA
		diff, err := git(ctx, root, land.NameStatusArgs(sha+"^", sha)...)
		if err != nil {
			// An unreadable diff makes the model fall back to the full run.
			landings[i].Unreadable = fmt.Sprintf("git diff %s^ %s: %v", sha, sha, err)
			continue
		}
		landings[i].NameStatus = diff
	}
	return landings, nil
}

// parseLandingRefs reads the sha/short/subject triples of `git log
// --format=%H%x00%h%x00%s%x00` output. Output that is not a whole number of
// records is an error, not fewer landings: a dropped record would report a
// saving the gate would not deliver.
func parseLandingRefs(out string) ([]gatemodel.Landing, error) {
	fields := gateNULFields(out)
	if len(fields)%3 != 0 {
		return nil, fmt.Errorf("git log returned %d fields, not a whole number of sha/short/subject records", len(fields))
	}
	landings := make([]gatemodel.Landing, 0, len(fields)/3)
	for i := 0; i < len(fields); i += 3 {
		// The sha reaches git as a revision, so it is validated as one: a
		// stream that substituted another word must not become an argument.
		if !isFullSHA(fields[i]) {
			return nil, fmt.Errorf("git log named %q where a full sha belongs", fields[i])
		}
		landings = append(landings, gatemodel.Landing{SHA: fields[i], Subject: fields[i+2]})
	}
	return landings, nil
}

// isFullSHA reports whether s is a 40-character lowercase hex sha.
func isFullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// gateNULFields splits NUL-separated git format output, dropping the newline
// git writes between records and any empty field.
func gateNULFields(out string) []string {
	var fields []string
	for _, f := range strings.Split(out, "\x00") {
		f = strings.TrimLeft(f, "\r\n")
		if f != "" {
			fields = append(fields, f)
		}
	}
	return fields
}

// gateGraph lists the repository's import graph.
func gateGraph(ctx context.Context, root string) *gatemodel.Graph {
	ctx, cancel := context.WithTimeout(ctx, gateListTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-e", "-json", "./...")
	cmd.Dir = root
	out, err := cmd.Output()
	return parseGateGraph(out, err)
}

// parseGateGraph reads one go list run. A graph it cannot parse, or a run that
// exited non-zero, is returned as nil with a warning: the model then falls back
// to the full run and never reports a selection it did not compute. A run that
// did not finish is unreadable as a whole, even when it printed some packages,
// because a package it did not print takes its dependents with it. go list's
// stderr is not echoed: captured output can hold a module path or a token, and
// the exit status is what the warning reports.
func parseGateGraph(out []byte, runErr error) *gatemodel.Graph {
	if runErr != nil {
		fmt.Fprintln(os.Stderr, "gt gate model: go list did not finish; modeling the full run")
		return nil
	}
	g, err := gatemodel.ParseGraph(bytes.NewReader(out))
	if err != nil {
		fmt.Fprintf(os.Stderr, "gt gate model: the import graph did not parse (%v); modeling the full run\n", err)
		return nil
	}
	return g
}

// gateNestedModules lists the directories below the repository root that hold
// a go.mod: `go test ./...` from the root does not build them. A walk that
// fails is an error, because a nested module the model missed could select
// nothing where the gate runs everything.
func gateNestedModules(root string) ([]string, error) {
	return nestedModules(os.DirFS(root))
}

// nestedModules is gateNestedModules over the tree fsys holds, its names
// slash-separated and relative to the root.
func nestedModules(fsys fs.FS) ([]string, error) {
	var dirs []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || p == "." {
			return nil
		}
		switch d.Name() {
		case ".git", "vendor", "node_modules", "testdata":
			return fs.SkipDir
		}
		if strings.HasPrefix(d.Name(), ".") {
			return fs.SkipDir
		}
		if _, err := fs.Stat(fsys, p+"/go.mod"); err != nil {
			// A stat error that is not "no go.mod" is reported, not read as
			// one: a missed nested module would select nothing.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		dirs = append(dirs, p)
		return fs.SkipDir
	})
	if err != nil {
		return nil, fmt.Errorf("scanning for nested modules: %w", err)
	}
	sort.Strings(dirs)
	return dirs, nil
}

// gateGit runs one git command in dir and returns its stdout.
func gateGit(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gateGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
