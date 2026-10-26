// Command integrationvet type-checks every package the integration build tag
// adds files to, so an integration-only file that stops type-checking fails the
// gate instead of the post-merge rehearsal (gt-w3aby).
//
//	go run ./internal/testpolicy/cmd/integrationvet
//
// It runs go vet with the tag and no test binary: vet type-checks each selected
// package and its tests, and analyzes them, but it builds nothing it runs.
// Before changing the gate stage that runs it, read docs/testing.md, "The
// gate": that section is the one description of the stage.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/testpolicy"
	"github.com/steveyegge/gastown/internal/util"
)

// maxJobs caps the packages go vet works on at once in one module. The unit
// tier is already compiling the same tree beside it, so a wider fan-out buys
// little wall and costs the host.
const maxJobs = 4

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stderr))
}

// run returns 0 when every selected package type-checks, 1 when one does not,
// 2 when the selection or the report file could not be made, and 130 when the
// run was interrupted.
func run(ctx context.Context, args []string, out io.Writer) int {
	return runWith(ctx, args, out, runCommand, createSpool)
}

// runner runs one program in dir and returns its combined output and its error.
// runWith takes it so a test sees the argv and the working directory the go
// binary would get; runCommand is the real one.
type runner func(ctx context.Context, dir, name string, args ...string) (string, error)

// runWith is run with the program's invocation and its report file injectable.
func runWith(ctx context.Context, args []string, out io.Writer, run runner, newSpool func() (*spool, error)) (code int) {
	fs := flag.NewFlagSet("integrationvet", flag.ContinueOnError)
	fs.SetOutput(out)
	root := fs.String("root", ".", "directory to search for packages the integration tag adds files to")
	jobs := fs.Int("jobs", defaultJobs(), "packages go vet type-checks at once in one module")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *jobs < 1 {
		fmt.Fprintf(out, "integration vet: -jobs must be 1 or more, got %d\n", *jobs)
		return 2
	}
	report, err := newSpool()
	if err != nil {
		fmt.Fprintf(out, "integration vet: creating the report file: %v\n", err)
		return 2
	}
	// The report prints as one block when the run ends, so the unit tier's
	// lines running beside it never split a module's diagnostics, and every
	// exit path removes the file, an interrupted run included (gt-w3aby).
	defer func() {
		if err := report.copyTo(out); err != nil {
			fmt.Fprintf(out, "integration vet: writing the report: %v\n", err)
		}
		if err := report.remove(); err != nil {
			fmt.Fprintf(out, "integration vet: removing the report file %s: %v\n", report.path, err)
		}
	}()

	cleanRoot := filepath.Clean(*root)
	// make polecat-image stages whole rig checkouts here for its build cache;
	// they are copies of other trees, not this module.
	pkgs, err := testpolicy.IntegrationPackages(cleanRoot, "deploy/polecat/seed")
	if err != nil {
		fmt.Fprintf(report, "integration vet: selecting packages: %v\n", err)
		return 2
	}
	if len(pkgs) == 0 {
		fmt.Fprintf(report, "integration vet: no package under %s adds files with the integration tag; the selection or -root is wrong\n", cleanRoot)
		return 2
	}
	modules, err := groupByModule(cleanRoot, pkgs)
	if err != nil {
		fmt.Fprintf(report, "integration vet: %v\n", err)
		return 2
	}

	start := time.Now()
	results := vetAll(ctx, cleanRoot, modules, *jobs, run)
	wall := time.Since(start).Round(100 * time.Millisecond)
	if ctx.Err() != nil {
		fmt.Fprintf(report, "integration vet: interrupted after %s\n", wall)
		return 130
	}
	failed := 0
	for _, r := range results {
		if r.err == nil {
			continue
		}
		failed++
		fmt.Fprintf(report, "--- %s (%d packages, go vet -tags integration)\n%s", r.module, len(r.pkgs), r.output)
		if r.output != "" && r.output[len(r.output)-1] != '\n' {
			fmt.Fprintln(report)
		}
	}
	if failed > 0 {
		fmt.Fprintf(report, "integration vet: FAILED in %d of %d modules after %s\n", failed, len(results), wall)
		return 1
	}
	fmt.Fprintf(report, "integration vet: %d packages in %d modules type-check with -tags integration in %s\n", len(pkgs), len(results), wall)
	return 0
}

// spool is the temp file a run writes its report to and copies to the gate's
// stream when it ends.
type spool struct {
	path string
	f    *os.File
}

// createSpool writes the report in the system temp directory.
func createSpool() (*spool, error) {
	return createSpoolIn("")
}

// createSpoolIn writes the report in dir, which is "" for the system temp
// directory.
func createSpoolIn(dir string) (*spool, error) {
	f, err := os.CreateTemp(dir, "integration-vet-*.log")
	if err != nil {
		return nil, err
	}
	return &spool{path: f.Name(), f: f}, nil
}

func (s *spool) Write(p []byte) (int, error) {
	return s.f.Write(p)
}

// copyTo writes what the run wrote, once, so the report reaches out as one
// block.
func (s *spool) copyTo(out io.Writer) error {
	if _, err := s.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err := io.Copy(out, s.f)
	return err
}

func (s *spool) remove() error {
	if err := s.f.Close(); err != nil {
		return err
	}
	return os.Remove(s.path)
}

// module is one go.mod directory and the selected packages under it, as paths
// relative to the module directory.
type module struct {
	dir  string
	pkgs []string
}

// groupByModule assigns each selected package to the module go vet must run
// in, so a nested module is vetted from its own root.
func groupByModule(root string, pkgs []string) ([]module, error) {
	byDir := map[string][]string{}
	for _, pkg := range pkgs {
		abs, err := moduleRoot(root, pkg)
		if err != nil {
			return nil, err
		}
		dir, err := filepath.Rel(root, abs)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(abs, filepath.Join(root, pkg))
		if err != nil {
			return nil, err
		}
		byDir[filepath.ToSlash(dir)] = append(byDir[filepath.ToSlash(dir)], filepath.ToSlash(rel))
	}
	modules := make([]module, 0, len(byDir))
	for dir, ps := range byDir {
		sort.Strings(ps)
		modules = append(modules, module{dir: dir, pkgs: ps})
	}
	sort.Slice(modules, func(i, j int) bool { return modules[i].dir < modules[j].dir })
	return modules, nil
}

// moduleRoot finds the nearest directory at or above pkg, and not above root,
// that holds a go.mod.
func moduleRoot(root, pkg string) (string, error) {
	dir := filepath.Join(root, pkg)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		if dir == root {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("package %s is not in a module under %s", pkg, root)
}

// result is one module's go vet output, kept per module so failures print in a
// stable order however the modules interleave.
type result struct {
	module string
	pkgs   []string
	output string
	err    error
}

// vetAll vets every module, one go vet invocation per module so go's own
// parallelism and result cache serve its packages. Each runs from its own
// module directory, so a nested module resolves its own go.mod.
func vetAll(ctx context.Context, root string, modules []module, jobs int, run runner) []result {
	var wg sync.WaitGroup
	results := make([]result, len(modules))
	for i, m := range modules {
		wg.Add(1)
		go func() {
			defer wg.Done()
			output, err := run(ctx, filepath.Join(root, m.dir), "go", vetArgs(m, jobs)...)
			results[i] = result{module: m.dir, pkgs: m.pkgs, output: output, err: err}
		}()
	}
	wg.Wait()
	sort.Slice(results, func(i, j int) bool { return results[i].module < results[j].module })
	return results
}

// runCommand is the real runner: the named program in dir, its output
// combined.
func runCommand(ctx context.Context, dir, name string, args ...string) (string, error) {
	output, err := command(ctx, dir, name, args...).CombinedOutput()
	return string(output), err
}

// command builds one vet invocation in its own process group, so cancellation
// stops the vet tool go vet spawns, not only go vet. The WaitDelay bounds the
// wait on a descendant that escaped the group and kept the output pipe open.
// cancelGroup is what cancellation calls on the vet's command: util's group kill (SIGTERM,
// then SIGKILL after the grace). A variable so a test sees the hook is wired through it.
var cancelGroup = util.KillProcessGroup

func command(ctx context.Context, dir, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.WaitDelay = util.ProcessGroupKillGrace
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return cancelGroup(cmd) }
	return cmd
}

// vetArgs is the go vet command line for one module: its selected packages,
// as patterns go resolves from the module directory.
func vetArgs(m module, jobs int) []string {
	args := []string{"vet", "-tags", "integration", "-p", strconv.Itoa(jobs)}
	for _, pkg := range m.pkgs {
		if pkg == "." {
			args = append(args, ".")
			continue
		}
		args = append(args, "./"+pkg)
	}
	return args
}

// defaultJobs is maxJobs or the CPU count at most.
func defaultJobs() int {
	if n := runtime.NumCPU(); n < maxJobs {
		return n
	}
	return maxJobs
}
