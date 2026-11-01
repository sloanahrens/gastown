package sweeprunner

import (
	"strconv"
	"strings"

	"github.com/steveyegge/gastown/internal/templates"
)

// RunnerArgv is the command the Docker backend starts the runner container with.
// The container starts as root so that its first process (the init) is root's, as
// a seat's is: a test that signals pid 1 needs another user's process there. The
// sweep itself runs as the image's unprivileged polecat user, which setpriv (util-linux,
// in the image's base; `command -v setpriv` answers /usr/bin/setpriv in the image
// built at 534b955e8) drops to. The image's own default command is bash, which
// exits at once in a container with no terminal, so the command is named. HOME is
// /tmp because the user's own home belongs to another user.
//
// The Kubernetes backend does not use it: the pod's shared process namespace has a
// root pause process as pid 1, so the runner there is the unprivileged user from
// the start and holds no root at all (runnerContainer).
func RunnerArgv() []string {
	return []string{"setpriv", "--reuid", runnerID, "--regid", runnerID, "--clear-groups", "env", "HOME=/tmp", RunnerCommand}
}

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
	// RunnerCommand is the program the runner container runs: the sweep-run the
	// polecat image carries (deploy/polecat/Dockerfile copies it to this path). Both
	// backends name it, because the image's own default is bash, which exits at once
	// in a container with no terminal.
	RunnerCommand = "/usr/local/bin/sweep-run"
	// runnerID is the user and group the sweep runs as: the image's unprivileged
	// polecat user.
	runnerID = "10001"
	// runnerUser is runnerID as the number a pod's securityContext takes.
	runnerUser int64 = 10001

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
	// dindDataPath is where the sidecar's engine keeps its state, on the run's
	// own volume.
	dindDataPath = "/var/lib/docker"
	// SeedVolume is the engine state a seed run (RenderSeed) fills with the images
	// the packages' tests start. Each run copies it into the run's own volume
	// before the sidecar starts, so the sidecar's engine has the images without
	// any network and the seed itself is never written by a run.
	SeedVolume = "gt-sweep-dind-seed"
	// seedMount is where the copy step reads the seed, read-only.
	seedMount = "/seed"
	// ryukDisabled turns off the testcontainers reaper in the runner. The reaper
	// is a container image the sidecar's engine cannot pull, and the sidecar's
	// teardown removes every container the tests started.
	ryukDisabledName  = "TESTCONTAINERS_RYUK_DISABLED"
	ryukDisabledValue = "true"
	// ContainerPrefix starts the name of every container a run makes (the runner,
	// the sidecar), so a caller can tell whether a sweep is running.
	ContainerPrefix = "gt-sweep-"
)

// seedCopyScript copies the seed into the run's volume, and fails the run when
// the seed is empty: a seed that was never filled would otherwise copy nothing and
// every Dolt-backed package would fail later on a missing image.
const seedCopyScript = `[ -n "$(ls -A ` + seedMount + `)" ] || { echo "the seed volume ` + SeedVolume + ` is empty: run sweep-seed" >&2; exit 1; }; cp -a ` + seedMount + `/. ` + dindDataPath + `/`

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
			// The run's volume starts as a copy of the seed: the sidecar sits on
			// an internal network and cannot pull the images the tests start, so
			// they come from the seed filled once by RenderSeed. The seed is
			// mounted read-only, and a seed that was never filled is an empty
			// copy: the packages that need an image then fail on it by name.
			dockerStep(spec, "run", "--rm", "--network", "none",
				"--entrypoint", "sh",
				"--volume", SeedVolume+":"+seedMount+":ro",
				"--volume", volumeName(spec)+":"+dindDataPath,
				templates.SweepDindImage,
				"-c", seedCopyScript,
			),
			// The engine is the plan's only privileged container, and it is
			// reached over plain TCP on the run's own loopback: dockerd runs the
			// packages' containers, and nothing else needs a capability. The
			// step returns before dockerd listens, so the run's entrypoint
			// waits for `docker info` before a package that needs it.
			dockerStep(spec, "run", "-d",
				"--name", dindName(spec),
				"--privileged",
				"--network", networkName(spec),
				"--env", "DOCKER_TLS_CERTDIR=",
				"--volume", volumeName(spec)+":"+dindDataPath,
				templates.SweepDindImage,
				// dockerd named as the command: the image's entrypoint adds its
				// own host on 0.0.0.0:2375 to any arguments that do not start
				// with it, which collides with the loopback host asked for here.
				"dockerd",
				"--host=tcp://"+dindAddress+":"+dindPort,
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
		// An init as the container's first process reaps the orphans the tests
		// kill; without one a killed grandchild stays a zombie and still answers
		// kill(pid, 0). It runs as root, with the user the command drops to.
		"--init",
		"--user", "0:0",
	}
	if spec.Dind {
		// The runner shares the sidecar's network namespace. The engine is then
		// on its loopback, and so is every test container the engine publishes:
		// internal/testutil dials the published Dolt port at 127.0.0.1, which two
		// containers on a bridge network cannot answer (gt-9lmam). The sidecar
		// step above must run first: a network namespace can be joined only
		// once its container exists.
		argv = append(argv,
			"--network", "container:"+dindName(spec),
			"--env", "DOCKER_HOST=tcp://"+dindAddress+":"+dindPort,
		)
	}
	return append(append(argv,
		"--volume", spec.RepoDir+":"+RepoMount+":ro",
		"--volume", spec.OutDir+":"+OutputDir,
		"--env", EnvRig+"="+spec.Rig,
		"--env", EnvSHA+"="+spec.SHA,
		"--env", EnvTiers+"="+strings.Join(spec.TiersOrDefault(), ","),
		"--env", EnvJobs+"="+strconv.Itoa(spec.Jobs),
		"--env", ryukDisabledName+"="+ryukDisabledValue,
		spec.Image,
	), RunnerArgv()...)
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
	return ContainerPrefix + spec.Rig + "-" + spec.SHA[:8] + "-" + spec.RunID
}

// dindName is the sidecar's container name.
func dindName(spec Spec) string {
	return ContainerPrefix + "dind-" + spec.Rig + "-" + spec.SHA[:8] + "-" + spec.RunID
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
