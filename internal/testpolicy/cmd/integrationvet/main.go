// Command integrationvet type-checks every package the integration build tag
// adds files to, so an integration-only file that stops type-checking fails the
// gate instead of the post-merge rehearsal (gt-w3aby).
//
//	go run ./internal/testpolicy/cmd/integrationvet
//
// `make gate` runs it beside the unit tier. It runs go vet with the tag and no
// test binary: vet type-checks each selected package and its tests, and
// analyzes them, but it builds nothing it runs.
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
)

// maxJobs caps the packages go vet works on at once in one module. The gate
// runs this beside the unit tier, which is already compiling the same tree, so
// a wider fan-out buys little wall and costs the host.
const maxJobs = 4

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stderr))
}

// run returns 0 when every selected package type-checks, 1 when one does not,
// and 2 when the selection itself could not be made.
func run(ctx context.Context, args []string, out io.Writer) int {
	return runWith(ctx, args, out, runCommand)
}

// runner runs one program in dir and returns its combined output and its error.
// runWith takes it so a test sees the argv and the working directory the go
// binary would get; runCommand is the real one.
type runner func(ctx context.Context, dir, name string, args ...string) (string, error)

// runWith is run with the program's invocation injectable.
func runWith(ctx context.Context, args []string, out io.Writer, run runner) int {
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
	cleanRoot := filepath.Clean(*root)
	// make polecat-image stages whole rig checkouts here for its build cache;
	// they are copies of other trees, not this module.
	pkgs, err := testpolicy.IntegrationPackages(cleanRoot, "deploy/polecat/seed")
	if err != nil {
		fmt.Fprintf(out, "integration vet: selecting packages: %v\n", err)
		return 2
	}
	if len(pkgs) == 0 {
		fmt.Fprintf(out, "integration vet: no package under %s adds files with the integration tag; the selection or -root is wrong\n", cleanRoot)
		return 2
	}
	modules, err := groupByModule(cleanRoot, pkgs)
	if err != nil {
		fmt.Fprintf(out, "integration vet: %v\n", err)
		return 2
	}

	start := time.Now()
	results := vetAll(ctx, cleanRoot, modules, *jobs, run)
	wall := time.Since(start).Round(100 * time.Millisecond)
	if ctx.Err() != nil {
		fmt.Fprintf(out, "integration vet: interrupted after %s\n", wall)
		return 130
	}
	failed := 0
	for _, r := range results {
		if r.err == nil {
			continue
		}
		failed++
		fmt.Fprintf(out, "--- %s (%d packages, go vet -tags integration)\n%s", r.module, len(r.pkgs), r.output)
		if r.output != "" && r.output[len(r.output)-1] != '\n' {
			fmt.Fprintln(out)
		}
	}
	if failed > 0 {
		fmt.Fprintf(out, "integration vet: FAILED in %d of %d modules after %s\n", failed, len(results), wall)
		return 1
	}
	fmt.Fprintf(out, "integration vet: %d packages in %d modules type-check with -tags integration in %s\n", len(pkgs), len(results), wall)
	return 0
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
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	return string(output), err
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
