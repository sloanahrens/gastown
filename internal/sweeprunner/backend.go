package sweeprunner

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
// A caller runs Apply in order, then the steps whose Teardown is false in order,
// and stops at the first failure, then runs Teardown whether or not a step
// before it failed. A run that fails at its own start step has still made the
// network, the volume, the claim and the sidecar, so a teardown that waited for
// success would leak them.
type Plan struct {
	// Job is the batch/v1 Job a Kubernetes backend applies, and nil for a
	// backend that renders argv alone. Apply carries the document it applies.
	Job *Job
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

// Poll is how a caller learns that a run has ended and brings its result out.
type Poll struct {
	// Status is the step that reads the run's state. Its stdout is handed to
	// Ended.
	Status Step
	// Ended reports from Status's stdout whether the run has ended, whether it
	// succeeded or not.
	Ended func(stdout []byte) bool
	// Collect copies the result into the output directory. A caller runs it
	// once Ended reports true, in order, and stops at the first failure.
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
}
