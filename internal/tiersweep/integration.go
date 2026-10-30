package tiersweep

import (
	"bytes"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"
)

// TierDocker is the untagged container run: the docker.txt packages, which the
// container trigger may skip. It is the tier name the trigger takes.
const TierDocker = "integration-docker"

const (
	// dockerList is the container package list, relative to the repository root.
	dockerList = "internal/testpolicy/docker.txt"
	// triggerPkg is the container trigger the sweep runs as a child process.
	triggerPkg = "./internal/tiertrigger/cmd/tiertrigger"
)

// taggedArgs are the go test arguments for the integration build.
var taggedArgs = []string{"-tags", "integration"}

// Sweep is one tier-sweep run's options and seams. Exec runs go, gt and git,
// Src reads the repository, and Now is the clock, so a tier runs under fakes
// with no toolchain.
type Sweep struct {
	// Opts are the run's resolved options.
	Opts Options
	// Exec runs every command. ProcessExec in production.
	Exec Exec
	// Src lists and reads the repository's files, relative to the directory
	// it is asked from.
	Src Source
	// Environ is the environment the container trigger runs in. The sweep adds
	// its TIER_SWEEP_* values to it, so it must hold the whole process
	// environment, PATH included.
	Environ []string
	// Stderr receives the progress lines, the failure excerpts and the
	// trigger's own failure lines.
	Stderr io.Writer
	// Now is the clock a tier's elapsed time is read from, and the one each
	// package's wall time is read from.
	Now func() time.Time
	// Timings receives one line per package a tier runs, and holds the lines for
	// TIER_SWEEP_TIMINGS. Run sets it; nil writes no timings.
	Timings *Timings
}

// integrationPlan is what the integration tier runs.
type integrationPlan struct {
	// root is the root module's tagged packages, without the nested modules'.
	root []string
	// docker is the docker.txt packages, each as ./pkg.
	docker []string
	// modules holds each nested module's tagged packages, module-relative.
	modules []moduleRun
}

// moduleRun is one nested module's packages, run in the module's directory.
type moduleRun struct {
	dir  string
	pkgs []string
}

// Integration runs the integration tier and returns its summary line and
// whether it is RED. The root's tagged packages run first, then the container
// packages, then each nested module's tagged packages in its own directory.
// The names keep that order, so "untagged:" labels only the container group.
func (s *Sweep) Integration() (line string, red bool) {
	start := s.Now()
	elapsed := func() int { return int(s.Now().Sub(start) / time.Second) }
	tested := s.headCommit()
	skipList := s.Opts.SkipList
	// Without a commit to name, no container package can be shown as due, so
	// the trigger is not asked and every container package runs.
	if tested != "" {
		if due := s.dueSkips(tested); due != "" {
			skipList = strings.TrimSpace(skipList + " " + due)
		}
	}
	plan, err := s.planIntegration()
	if err != nil {
		// A list that cannot be read runs nothing, so the tier fails closed:
		// RED with no names, the same as an empty tier.
		fmt.Fprintf(s.Stderr, "%s: %s: %v\n", Name, TierIntegration, err)
		return Summary(TierIntegration, s.Opts.LogDir, 0, 0, 0, "", elapsed())
	}
	runner := &Runner{
		Exec:     s.Exec,
		LogDir:   s.Opts.LogDir,
		Timeout:  s.Opts.Timeout,
		SkipList: skipList,
		Env:      s.exportEnv(tested, skipList),
		Stderr:   s.Stderr,
		Timings:  s.Timings,
		Now:      s.Now,
	}
	root := runner.Tier(TierIntegration, ".", slotPrefix(s.Opts.Nice), taggedArgs, plan.root)
	docker := runner.Tier(TierDocker, ".", slotPrefix(s.Opts.Nice), nil, plan.docker)
	// Only a passing container run at a named commit earns the record. A red
	// one leaves the state alone, so the next sweep runs those packages again,
	// and a commit that cannot be named is never stamped.
	if docker.Failed == 0 && tested != "" {
		s.record(tested, skipList)
	}
	passed := root.Passed + docker.Passed
	failed := root.Failed + docker.Failed
	skipped := root.Skipped + docker.Skipped
	var names []string
	names = append(names, root.FailedNames...)
	for _, m := range plan.modules {
		c := runner.Tier(TierIntegration, m.dir, slotPrefix(s.Opts.Nice), taggedArgs, m.pkgs)
		passed += c.Passed
		failed += c.Failed
		skipped += c.Skipped
		names = append(names, c.FailedNames...)
	}
	if len(docker.FailedNames) > 0 {
		names = append(names, "untagged:"+docker.FailedNames[0])
		names = append(names, docker.FailedNames[1:]...)
	}
	return Summary(TierIntegration, s.Opts.LogDir, passed, failed, skipped, strings.Join(names, " "), elapsed())
}

// planIntegration lists what the integration tier runs. A list that cannot be
// read is an error, not an empty list: an empty list would pass without a run.
func (s *Sweep) planIntegration() (integrationPlan, error) {
	goMods, err := s.Src.LsFiles(".", "*/go.mod")
	if err != nil {
		return integrationPlan{}, fmt.Errorf("list nested modules: %w", err)
	}
	modules := NestedModules(goMods)
	testFiles, err := s.Src.LsFiles(".", "*_test.go")
	if err != nil {
		return integrationPlan{}, fmt.Errorf("list test files: %w", err)
	}
	var plan integrationPlan
	for _, pkg := range TaggedDirs(testFiles, s.Src.ReadFile) {
		if !InsideModule(pkg, modules) {
			plan.root = append(plan.root, pkg)
		}
	}
	text, err := s.Src.ReadFile(dockerList)
	if err != nil {
		return integrationPlan{}, fmt.Errorf("read %s: %w", dockerList, err)
	}
	plan.docker = DockerPackages(string(text))
	for _, dir := range modules {
		run, err := s.planModule(dir, modules)
		if err != nil {
			return integrationPlan{}, err
		}
		if len(run.pkgs) > 0 {
			plan.modules = append(plan.modules, run)
		}
	}
	return plan, nil
}

// planModule lists one nested module's tagged packages. A package inside a
// module nested deeper is left to that module's own run.
func (s *Sweep) planModule(dir string, modules []string) (moduleRun, error) {
	files, err := s.Src.LsFiles(dir, "*_test.go")
	if err != nil {
		return moduleRun{}, fmt.Errorf("list test files in %s: %w", dir, err)
	}
	var deeper []string
	for _, o := range modules {
		if strings.HasPrefix(o, dir+"/") {
			deeper = append(deeper, o)
		}
	}
	read := func(f string) ([]byte, error) { return s.Src.ReadFile(path.Join(dir, f)) }
	run := moduleRun{dir: dir}
	for _, pkg := range TaggedDirs(files, read) {
		if !InsideModule(packageName(dir, pkg), deeper) {
			run.pkgs = append(run.pkgs, pkg)
		}
	}
	return run, nil
}

// headCommit is the commit the run tests, empty when git cannot name it.
func (s *Sweep) headCommit() string {
	var out bytes.Buffer
	if s.Exec(".", nil, []string{"git", "rev-parse", "HEAD"}, &out) != 0 {
		return ""
	}
	return strings.TrimSpace(out.String())
}

// dueSkips asks the container trigger which container packages are not due and
// returns them as TIER_SWEEP_SKIP entries. A trigger that fails leaves the list
// as it was: it writes its own line to stderr, so every container package runs.
func (s *Sweep) dueSkips(tested string) string {
	var out bytes.Buffer
	if s.Exec(".", s.triggerEnv(tested, s.Opts.SkipList), triggerArgv("skips"), &out) != 0 {
		fmt.Fprintf(s.Stderr, "%s", out.String())
		return ""
	}
	return strings.TrimSpace(out.String())
}

// record stamps the container packages as passed at tested. The trigger's exit
// is not checked: its own line names the failure, and the tier stays GREEN.
func (s *Sweep) record(tested, skipList string) {
	s.Exec(".", s.triggerEnv(tested, skipList), triggerArgv("record"), s.Stderr)
}

// exportEnv is the environment the sweep exports to its children, as the script
// exports TIER_SWEEP_TESTED and TIER_SWEEP_SKIP: the process's, with the commit
// under test and the skip list added. Test runs and trigger children both
// inherit it.
func (s *Sweep) exportEnv(tested, skipList string) []string {
	env := append([]string(nil), s.Environ...)
	return append(env, EnvTested+"="+tested, EnvSkip+"="+skipList)
}

// triggerEnv is the environment a trigger child runs in: the exported one, with
// TIER_SWEEP_TRIGGER added. The trigger's exit code and output are its own;
// this only sets its inputs.
func (s *Sweep) triggerEnv(tested, skipList string) []string {
	env := s.exportEnv(tested, skipList)
	if s.Opts.Trigger != "" {
		env = append(env, EnvTrigger+"="+s.Opts.Trigger)
	}
	return env
}

// triggerArgv is the command for one trigger verb on the container tier.
func triggerArgv(verb string) []string {
	return []string{"go", "run", triggerPkg, verb, TierDocker}
}

// slotPrefix runs a package under gt slot run with the container tests on, and
// with --nice when TIER_SWEEP_NICE set one. A nil nice passes no flag, so gt
// slot run keeps its own default.
func slotPrefix(nice *int) []string {
	prefix := []string{"gt", "slot", "run"}
	if nice != nil {
		prefix = append(prefix, "--nice", strconv.Itoa(*nice))
	}
	return append(prefix, "--", "env", "GT_TEST_DOCKER=1")
}
