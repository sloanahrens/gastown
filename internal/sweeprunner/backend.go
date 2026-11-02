package sweeprunner

import "strings"

// The sidecar engine's address and port, which both backends hand the runner
// (DOCKER_HOST) and the sidecar (--host).
const (
	// dindAddress is where the runner reaches the sidecar: the runner's own
	// loopback, because it shares the sidecar's network namespace (the pod's,
	// with the Kubernetes backend; the sidecar container's, which the Docker
	// backend's runner joins). The sidecar binds this address too; a bind on
	// 0.0.0.0 would answer at the pod IP or the run network, where another
	// workload could drive the privileged engine (gt-7zy61).
	dindAddress = "127.0.0.1"

	// dindPort is the engine's plain-TCP port: the sidecar runs with --tls=false
	// on an internal network and publishes nothing.
	dindPort = "2375"
)

// Backend renders a Spec into the steps that run it on one engine: the Docker
// Backend renders CLI argv, and the Kubernetes one renders a Job (gt-7zy61).
type Backend interface {
	// Name is the backend's name in the daemon's config and its logs.
	Name() string
	// Render returns the plan that runs spec, or the Validate error that
	// refuses it.
	Render(spec Spec) (Plan, error)
}

// Plan is one rendered run: the objects it applies, the steps a caller runs in
// the order it runs them, and how a caller learns the run has ended.
//
// A caller runs Guards first and applies nothing while one refuses, then Apply
// in order, then the steps whose Teardown is false in order, and stops at the
// first failure, then runs Teardown whether or not a step before it failed. A
// run that fails at its own start step has still made the network, the volume,
// the claim and the sidecar, so a teardown that waited for success would leak
// them.
type Plan struct {
	// Job is the batch/v1 Job a Kubernetes backend applies, and nil for a
	// backend that renders argv alone. Apply carries the document it applies.
	Job *Job
	// Guards are the checks a caller runs before it applies anything: each
	// lists objects that must not share the engine with this run, and its
	// Refuse reads the listing and reports the objects that refuse the run.
	Guards []Guard
	// Apply are the steps that create the run's objects before its start
	// steps, each with the document it applies as Stdin. Nil for a backend
	// that renders argv alone.
	Apply []Step
	// Steps are the run's own steps, then its teardown.
	Steps []Step
	// Poll is how a caller learns the run has ended and gets its result out,
	// for a backend whose result stays on the engine. Nil when the run writes
	// its result into the output directory itself.
	Poll *Poll
}

// Guard is one check a caller runs before a Plan's Apply steps: a program that
// lists objects on the engine, and what its listing refuses.
type Guard struct {
	// Step is the program that lists the objects, run directly like any step.
	Step Step
	// Refuse reads Step's stdout and returns the error naming the objects that
	// make the run unsafe to start, or nil when its listing names none. An
	// output it cannot read is an error too: a check that cannot run is not a
	// pass.
	Refuse func(stdout []byte) error
}

// Poll is how a caller learns that a run has ended and brings its result out.
type Poll struct {
	// Status is the step that reads the run's state. Its stdout is handed to
	// Ended.
	Status Step
	// Ended reports from Status's stdout whether the run has ended, whether it
	// succeeded or not.
	Ended func(stdout []byte) bool
	// Collect brings the run's result, and the logs the backend keeps on its
	// engine, into the output directory. A caller runs it once Ended reports
	// true, in order, and stops at the first failure a Step does not tolerate.
	Collect []Step
}

// Teardown returns the steps that take the run down, in the order to run them:
// the caller runs them after Steps even when a start step failed.
func (p Plan) Teardown() []Step {
	var steps []Step
	for _, step := range p.Steps {
		if step.Teardown {
			steps = append(steps, step)
		}
	}
	return steps
}

// Step is one program with its arguments.
type Step struct {
	// Argv is the program and its arguments, run directly and never through a
	// shell.
	Argv []string
	// Stdin is the input the step reads, when it reads one: the document an
	// apply takes. Nil for a step that reads nothing.
	Stdin []byte
	// Teardown marks a step that removes what an earlier step made; it runs
	// even when an earlier step failed.
	Teardown bool
	// MissingSourcePath names the source whose absence is not a failure: a
	// run that wrote no logs leaves /out/logs absent, and the copy of it then
	// fails. Empty for a step that tolerates no absence. The caller tolerates
	// only a failure whose output names this source; any other failure is
	// still the run's (gt-4lj2q).
	MissingSourcePath string
}

// missingSourceMarker is what a failed copy prints when its source is not
// there: kubectl cp reports tar's "No such file or directory" (gt-4lj2q).
const missingSourceMarker = "no such file"

// MissingSource reports whether a failed step is the absent source it
// tolerates: the step names a MissingSourcePath and a line of its output says
// that path is not there. A step that failed another way, or one that names no
// source, reports false (gt-4lj2q).
func (s Step) MissingSource(output []byte) bool {
	if s.MissingSourcePath == "" {
		return false
	}
	source := strings.ToLower(s.MissingSourcePath)
	for _, line := range strings.Split(strings.ToLower(string(output)), "\n") {
		if strings.Contains(line, missingSourceMarker) && namesPath(line, source) {
			return true
		}
	}
	return false
}

// namesPath reports whether line names path as a whole path, rather than a
// longer path that merely starts or ends with it: /out/logs-old and
// /out/logs/integration do not name /out/logs.
func namesPath(line, path string) bool {
	for at := 0; at <= len(line)-len(path); {
		i := strings.Index(line[at:], path)
		if i < 0 {
			return false
		}
		i += at
		end := i + len(path)
		if (i == 0 || !pathByte(line[i-1])) && (end == len(line) || !pathByte(line[end])) {
			return true
		}
		at = i + 1
	}
	return false
}

// pathByte reports whether c is one a path is made of, so a name beside it is
// part of a longer path.
func pathByte(c byte) bool {
	return c == '/' || c == '-' || c == '_' || c == '.' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
