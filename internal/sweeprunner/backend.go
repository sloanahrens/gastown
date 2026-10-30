package sweeprunner

// Backend renders a Spec into the steps that run it on one engine: the Docker
// Backend renders CLI argv, and the Kubernetes one renders a Job (gt-7zy61).
type Backend interface {
	// Name is the backend's name in the daemon's config and its logs.
	Name() string
	// Render returns the plan that runs spec, or the Validate error that
	// refuses it.
	Render(spec Spec) (Plan, error)
}

// Plan is one rendered run: the object it applies, and the steps a caller runs
// in the order it runs them.
//
// A caller applies Job when it is set, then runs the steps whose Teardown is
// false in order and stops at the first failure, then runs Teardown whether or
// not a step before it failed. A run that fails at its own start step has still
// made the network, the volume and the sidecar, so a teardown that waited for
// success would leak them.
type Plan struct {
	// Job is the batch/v1 Job a Kubernetes backend applies, and nil for a
	// backend that renders argv alone.
	Job *Job
	// Steps are the run's own steps, then its teardown.
	Steps []Step
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
	// Teardown marks a step that removes what an earlier step made; it runs
	// even when an earlier step failed.
	Teardown bool
}
