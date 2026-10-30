package sweeprunner

import (
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/templates"
)

// Platform is the architecture the runner image is built for: the Mac hosts a
// sweep runs off are arm64.
const Platform = "linux/arm64"

// Where a run reads its repo and writes its result, and what it reads the run
// from. cmd/sweep-run maps the Env* values onto its own options and writes under
// OutputDir, so these are the contract between the two beads, not a private
// detail of the Docker argv.
const (
	// RepoMount is where the rig's bare repository is mounted, read-only.
	RepoMount = "/repo.git"
	// OutputDir is where the run writes result.json and its logs.
	OutputDir = "/out"

	// EnvRig is the rig whose sweep the runner runs.
	EnvRig = "GT_SWEEP_RIG"
	// EnvSHA is the commit the runner tests.
	EnvSHA = "GT_SWEEP_SHA"
	// EnvTiers is the tiers to run, comma-separated, in order.
	EnvTiers = "GT_SWEEP_TIERS"
	// EnvJobs is how many packages the runner tests at once.
	EnvJobs = "GT_SWEEP_JOBS"
)

const (
	// program is the CLI every step runs.
	program = "docker"
	// dindAlias names the sidecar on the run's private network; the runner
	// resolves its engine by this name, as the daemon's compose project does.
	dindAlias = "sweep-dind"
	// dindPort is the engine's plain-TCP port: the sidecar runs with --tls=false
	// on an internal network and publishes nothing.
	dindPort = "2375"
	// dindDataPath is where the sidecar's engine keeps its state, on the run's
	// own volume.
	dindDataPath = "/var/lib/docker"
)

// Docker renders a sweep run as docker CLI argv against the Spec's context.
type Docker struct{}

// Compile-time check that Docker satisfies the Backend the daemon renders with.
var _ Backend = Docker{}

// Name is the backend's name in the daemon's config and its logs.
func (Docker) Name() string { return "docker" }

// Render returns the steps that start the run and the steps that take it down:
// the private network and the engine's volume, the sidecar, the runner, then the
// objects removed in reverse.
func (Docker) Render(spec Spec) (Plan, error) {
	if err := spec.Validate(); err != nil {
		return Plan{}, err
	}
	var start, teardown []Step
	if spec.Dind {
		start = append(start,
			dockerStep(spec, "network", "create", "--internal", networkName(spec)),
			dockerStep(spec, "volume", "create", volumeName(spec)),
			// The engine is the plan's only privileged container, and it is
			// reached over plain TCP on the internal network: dockerd runs the
			// packages' containers, and nothing else needs a capability. The
			// step returns before dockerd listens, so the run's entrypoint
			// waits for `docker info` before a package that needs it.
			dockerStep(spec, "run", "-d",
				"--name", dindName(spec),
				"--privileged",
				"--network", networkName(spec),
				"--network-alias", dindAlias,
				"--env", "DOCKER_TLS_CERTDIR=",
				"--volume", volumeName(spec)+":"+dindDataPath,
				templates.DindImage,
				"--tls=false",
			),
		)
	}
	start = append(start, dockerStep(spec, runnerArgv(spec)...))
	teardown = append(teardown, teardownStep(spec, "rm", "-f", runnerName(spec)))
	if spec.Dind {
		teardown = append(teardown,
			teardownStep(spec, "rm", "-f", dindName(spec)),
			teardownStep(spec, "volume", "rm", volumeName(spec)),
			teardownStep(spec, "network", "rm", networkName(spec)),
		)
	}
	return Plan{Steps: append(start, teardown...)}, nil
}

// runnerArgv is the runner container's docker run: the run's limits, its two
// mounts, and what it runs. It starts detached, so the caller waits for
// result.json rather than for the command; teardown removes the container.
func runnerArgv(spec Spec) []string {
	argv := []string{
		"run", "-d",
		"--name", runnerName(spec),
		"--platform", Platform,
		"--cpus", strconv.Itoa(spec.CPUs),
		"--memory", strconv.Itoa(spec.MemoryGB) + "G",
	}
	if spec.Dind {
		argv = append(argv,
			"--network", networkName(spec),
			"--env", "DOCKER_HOST=tcp://"+dindAlias+":"+dindPort,
		)
	}
	return append(argv,
		"--volume", spec.RepoDir+":"+RepoMount+":ro",
		"--volume", spec.OutDir+":"+OutputDir,
		"--env", EnvRig+"="+spec.Rig,
		"--env", EnvSHA+"="+spec.SHA,
		"--env", EnvTiers+"="+strings.Join(spec.TiersOrDefault(), ","),
		"--env", EnvJobs+"="+strconv.Itoa(spec.Jobs),
		spec.Image,
	)
}

// dockerStep is one docker command against the Spec's context: the flag is
// explicit so a step reaches the engine the Spec names, never the one the
// caller's environment happens to select.
func dockerStep(spec Spec, args ...string) Step {
	return Step{Argv: append([]string{program, "--context", spec.Context}, args...)}
}

// teardownStep is a docker command that removes an object an earlier step made.
func teardownStep(spec Spec, args ...string) Step {
	step := dockerStep(spec, args...)
	step.Teardown = true
	return step
}

// The names below carry the run id as well as the rig and the sha: teardown
// removes objects by name and runs even after a failed start, so a name keyed to
// the rig and sha alone would let one run's teardown force-remove another run's
// container, network or volume (gt-74ix9).

// runnerName is the container the sweep runs in.
func runnerName(spec Spec) string {
	return "gt-sweep-" + spec.Rig + "-" + spec.SHA[:8] + "-" + spec.RunID
}

// dindName is the sidecar's container name.
func dindName(spec Spec) string {
	return "gt-sweep-dind-" + spec.Rig + "-" + spec.SHA[:8] + "-" + spec.RunID
}

// networkName is the run's private network, which only the sidecar and the
// runner join.
func networkName(spec Spec) string {
	return "gt-sweep-net-" + spec.Rig + "-" + spec.SHA[:8] + "-" + spec.RunID
}

// volumeName is the sidecar's engine state. docker volume create reuses a
// volume of an existing name without a word, so the name must belong to one run
// (gt-74ix9).
func volumeName(spec Spec) string {
	return "gt-sweep-dind-data-" + spec.Rig + "-" + spec.SHA[:8] + "-" + spec.RunID
}
