package templates

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// ComposeSpec is the identity of one town's daemon service, which
// RenderDaemonCompose writes into the compose file the container supervisor
// drives. Every field is per town: a scratch town passes its own project,
// container name and root, so its render shares nothing with the live one.
type ComposeSpec struct {
	// Project is the compose project the supervisor passes to `docker compose
	// -p`. The file itself does not carry it, so the flag is the only place
	// the project is named; it is still required here so a caller cannot
	// render a file that belongs to no project.
	Project string

	// ContainerName is the container's name, the daemon's owner id and the
	// `docker inspect` target (internal/daemon's owner, internal/cmd's
	// supervisor), so all three always name one container.
	ContainerName string

	// Image is the tag the deployer built on this host, gt-daemon:<rev>.
	Image string

	// TownRoot is the directory bind-mounted at its own absolute path, so the
	// daemon inside the container reads and writes the town's own files.
	TownRoot string

	// StopGrace is docker's stop_grace_period: the time between SIGTERM and
	// SIGKILL on `docker compose stop` and on a restart. It may not be shorter
	// than the daemon's own worst-case graceful shutdown; see MinimumStopGrace.
	StopGrace time.Duration

	// DoltHost is the host the daemon reaches the town's Dolt through from
	// inside the container. The rendered file does not carry it: the daemon
	// reads the town config over the bind mount, which keeps the endpoint in
	// one source.
	//
	// It is the resolved host, not town.json's container_host alone: the caller
	// passes container_host when the town sets it and dolt.host when it does
	// not, because only the town config holds both. It is required here, and
	// must name the Mac on the far side of the mount. An empty host is refused
	// because town.json's dolt.host may be empty, and an empty one means the
	// local machine, which inside the container is the container itself; so is
	// a loopback address, for the same reason.
	DoltHost string

	// Scratch renders the scratch project: its own engine, networks, volumes and
	// Dolt server, and the --scratch-town flag that lets the container
	// entrypoint install the scratch town. No name in it is one the live render
	// carries.
	Scratch bool

	// ScratchDoltImage is the Dolt server image the scratch project runs. It is
	// required when Scratch is set; the scratch spec passes DoltServerImage.
	ScratchDoltImage string

	// LiveTownRoot is the live town's root, which a scratch spec must not
	// mount. It is required when Scratch is set, so a scratch render is always
	// checked against the live town it could otherwise reach.
	LiveTownRoot string
}

// MinimumStopGrace is the shortest stop_grace_period RenderDaemonCompose
// accepts, and the shortest that is ever correct: a grace period shorter than
// the daemon's own graceful shutdown gets it SIGKILLed mid-shutdown on every
// stop, which for the Dolt SQL server step risks its append-only journal
// instead of letting it exit cleanly.
//
// The value is daemon.ShutdownBudget. internal/daemon imports this package, so
// the renderer cannot read that constant here; TestMinimumStopGrace_CoversShutdownBudget
// pins the two together, and a change to either that unties them fails it.
const MinimumStopGrace = 30 * time.Second

// The sweep engine's names are fixed, not per town: the compose project scopes
// them, which is what keeps a scratch render off the live town's engine (E1
// renders its own).
const (
	dindService  = "sweep-dind"
	dindNetwork  = "sweep"
	dindVolume   = "sweep_dind_data"
	dindDataPath = "/var/lib/docker"

	// defaultNetwork is compose's implicit network, declared here because a
	// service that lists its networks explicitly gets no implicit one, and the
	// daemon needs a way out to host.docker.internal.
	defaultNetwork = "default"
)

// composeNames are the names one project's engine and networks take. The live
// project's are the sweep names above. A scratch project takes its own, so no
// name in its render is one the live render carries.
type composeNames struct {
	engine  string // the engine's service name, which DOCKER_HOST reaches
	network string // the internal network the engine, the daemon and the scratch Dolt share
	volume  string // the engine's data volume
	egress  string // the network the daemon reaches the outside through
}

var (
	liveNames    = composeNames{engine: dindService, network: dindNetwork, volume: dindVolume, egress: defaultNetwork}
	scratchNames = composeNames{engine: "scratch-dind", network: "scratch-internal", volume: "scratch_dind_data", egress: "scratch-egress"}
)

// The scratch project's Dolt server, which only the scratch render has. It
// joins the internal network alone and publishes no port.
const (
	scratchDoltService  = "scratch-dolt"
	scratchDoltVolume   = "scratch_dolt_data"
	scratchDoltDataPath = "/var/lib/dolt"
)

// scratchDaemonCommand is the daemon's command in the scratch project: the
// container entrypoint's --scratch-town flag, the one way a container asks for
// its scratch town's install. The live project passes nothing. After a failed
// install every later start refuses until the operator removes
// .gt-install-incomplete and what the install left, from the host side of the
// scratch directory.
const scratchDaemonCommand = `    # After a failed scratch install every start refuses until the operator
    # removes .gt-install-incomplete and what the install left, from the host
    # side of the scratch directory.
    command: ["--scratch-town"]
`

// scratchDoltBody is the scratch project's Dolt server service. It has no
// ports, so the only way in is the internal network the daemon also joins.
const scratchDoltBody = `  %[1]s:
    image: %[2]q
    restart: unless-stopped
    volumes:
      - type: volume
        source: %[3]s
        target: %[4]s
    networks:
      - %[5]s
`

// daemonComposeBody renders the daemon service and the Docker engine its sweep
// drives. Every spec value takes a %q, quoted so a town root with a space or a
// colon cannot change the YAML's shape, and passes through composeLiteral so a
// dollar sign in one is not interpolated away; the remaining lines are fixed by
// the design and not per town. GT_DAEMON_TAKEOVER is the file's one intentional
// reference to the environment, which is why it alone keeps a single "${...}".
//
// The engine listens without TLS by design, so its only guard is the network:
// declared internal, no published port, and joined by the daemon alone. The
// daemon keeps the project's default network beside it because an internal
// network has no way out, and the daemon reaches the town's Dolt through
// host.docker.internal.
//
// The daemon waits on the engine's health rather than on its start: compose
// starts a dependency's container and moves on, so a plain depends_on can hand
// the daemon an engine whose dockerd is not listening yet, and the first sweep
// then fails on a connection refused. `docker info` answers only once dockerd
// is up, so the health condition holds the daemon until the engine can take a
// request.
const daemonComposeBody = `# Generated by gt for the container supervisor; every deploy rewrites this file.
services:
  daemon:
    image: %[1]q
    container_name: %[2]q
    restart: unless-stopped
    stop_grace_period: %[3]q
    working_dir: %[4]q
%[12]s    depends_on:
      %[5]s:
        condition: service_healthy
    environment:
      DOCKER_HOST: "tcp://%[5]s:2375"
      GT_DAEMON_CONTAINER_NAME: %[2]q
      GT_DAEMON_SUPERVISOR: %[6]q
      GT_DAEMON_TAKEOVER: "${GT_DAEMON_TAKEOVER:-}"
      GT_TOWN_ROOT: %[4]q
    volumes:
      - type: bind
        source: %[4]q
        target: %[4]q
    networks:
      - %[11]s
      - %[7]s
  %[5]s:
    image: %[8]q
    restart: unless-stopped
    privileged: true
    command: ["--tls=false"]
    environment:
      DOCKER_TLS_CERTDIR: ""
    healthcheck:
      test: ["CMD", "docker", "info"]
      interval: 5s
      timeout: 3s
      retries: 10
      start_period: 30s
    volumes:
      - type: volume
        source: %[9]s
        target: %[10]s
    networks:
      - %[7]s
%[13]snetworks:
  %[11]s:
  %[7]s:
    internal: true
volumes:
  %[9]s:
%[14]s`

// RenderDaemonCompose renders the compose file the container supervisor
// starts, stops and inspects: the daemon service and the Docker engine its
// tier sweep drives.
//
// Neither service publishes a port and neither mounts a Docker socket: the
// daemon reaches a Docker engine only through the sweep's own engine on the
// project's internal network (C2). That engine is privileged, so the internal
// network is its only guard and only the daemon joins it. GT_DAEMON_SUPERVISOR
// puts the process in container mode, so it writes no pid file, and
// GT_DAEMON_CONTAINER_NAME is the owner id it records.
// GT_DAEMON_TAKEOVER is passed through empty, so a `--takeover` start is the
// operator's one start and never a standing setting. working_dir and
// GT_TOWN_ROOT both name the mount point, as the launchd plist and the systemd
// unit do, so the process finds the town whichever of the two it reads.
//
// It refuses a spec whose project or container name is empty rather than
// falling back to the live town's defaults (gt-ix1kz): a scratch render that
// inherited them could stop or replace the live daemon's container.
func RenderDaemonCompose(s ComposeSpec) ([]byte, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}

	names := liveNames
	var command, dolt, doltVolume string
	if s.Scratch {
		names = scratchNames
		command = scratchDaemonCommand
		dolt = fmt.Sprintf(scratchDoltBody,
			scratchDoltService,
			composeLiteral(s.ScratchDoltImage),
			scratchDoltVolume,
			scratchDoltDataPath,
			names.network,
		)
		doltVolume = "  " + scratchDoltVolume + ":\n"
	}

	out := fmt.Sprintf(daemonComposeBody,
		composeLiteral(s.Image),
		composeLiteral(s.ContainerName),
		s.StopGrace.String(),
		composeLiteral(s.TownRoot),
		names.engine,
		config.SupervisorContainer,
		names.network,
		composeLiteral(DindImage),
		names.volume,
		dindDataPath,
		names.egress,
		command,
		dolt,
		doltVolume,
	)
	return []byte(out), nil
}

// composeLiteral makes a spec value a literal in the compose file. `docker
// compose` interpolates $NAME and ${NAME} in every value it reads, before the
// daemon sees the file, so a town root holding a dollar sign would reach the
// container as whatever the environment names instead of itself. A doubled $$
// is compose's escape for one literal $.
func composeLiteral(v string) string {
	return strings.ReplaceAll(v, "$", "$$")
}

// validate reports the first field that would make the rendered file wrong or
// dangerous to apply. An empty name is never filled with a default here; the
// defaults belong to the town config (config.DaemonPatrolConfig).
func (s ComposeSpec) validate() error {
	switch {
	case s.Project == "":
		return fmt.Errorf("compose spec: Project is empty; a render never falls back to the live town's compose project")
	case s.ContainerName == "":
		return fmt.Errorf("compose spec: ContainerName is empty; a render never falls back to the live town's container name")
	case s.Image == "":
		return fmt.Errorf("compose spec: Image is empty; the daemon's image is the local tag gt-daemon:<rev> the deployer built")
	case s.TownRoot == "":
		return fmt.Errorf("compose spec: TownRoot is empty; the bind mount has no directory to mount")
	case !filepath.IsAbs(s.TownRoot):
		return fmt.Errorf("compose spec: TownRoot %q is not absolute; the mount names the same path inside the container, where a relative one resolves against docker's working directory", s.TownRoot)
	case s.StopGrace < MinimumStopGrace:
		return fmt.Errorf("compose spec: StopGrace %s is below MinimumStopGrace %s; a shorter grace period SIGKILLs the daemon mid-shutdown",
			s.StopGrace, MinimumStopGrace)
	case s.DoltHost == "":
		return fmt.Errorf("compose spec: DoltHost is empty; pass town.json's dolt.container_host or dolt.host resolved, and an empty dolt.host is the local machine, which inside the container is the container itself")
	}
	if err := checkDoltHost(s.DoltHost); err != nil {
		return err
	}
	if s.Scratch {
		return s.validateScratch()
	}
	return nil
}

// validateScratch refuses a scratch spec that could reach the live town: the
// live town's root, container name or compose project. The container name and
// project are the config defaults a live daemon runs under. The live root is
// the caller's, passed as LiveTownRoot, because no constant names it, so an
// empty one is refused rather than compared as "no root".
func (s ComposeSpec) validateScratch() error {
	switch {
	case s.LiveTownRoot == "":
		return fmt.Errorf("compose spec: LiveTownRoot is empty; a scratch spec is checked against the live town root, so the live root must be named")
	case strings.EqualFold(filepath.Clean(s.TownRoot), filepath.Clean(s.LiveTownRoot)):
		return fmt.Errorf("compose spec: scratch TownRoot %q is the live town root; the scratch project mounts its own directory, never the live one", s.TownRoot)
	case s.ContainerName == config.DefaultContainerName:
		return fmt.Errorf("compose spec: scratch ContainerName %q is the live daemon's container name", s.ContainerName)
	case s.Project == config.DefaultComposeProject:
		return fmt.Errorf("compose spec: scratch Project %q is the live compose project", s.Project)
	case s.ScratchDoltImage == "":
		return fmt.Errorf("compose spec: ScratchDoltImage is empty; the scratch project runs its own Dolt server, at DoltServerImage")
	}
	return nil
}

// checkDoltHost refuses a loopback endpoint: the container's own loopback is
// the container, so the daemon would fail to reach the town's Dolt while the
// file looked right. An empty host is refused in validate, before this, for the
// same reason.
func checkDoltHost(host string) error {
	loopback := strings.EqualFold(host, "localhost")
	if ip := net.ParseIP(host); ip != nil {
		loopback = loopback || ip.IsLoopback()
	}
	if loopback {
		return fmt.Errorf("compose spec: DoltHost %q is a loopback address; inside the container that is the container itself, so set dolt.container_host to the Mac's name in the Docker VM (host.docker.internal)", host)
	}
	return nil
}
