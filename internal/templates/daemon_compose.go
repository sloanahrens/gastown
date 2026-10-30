package templates

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
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
	//
	// A scratch spec does not set DoltHost, and a scratch render ignores it. The
	// scratch daemon's Dolt host comes from the scratch town's own settings under
	// the bind mount, which the spec does not carry. The scratch render is tested
	// to carry none of the live render's names, the live Dolt's among them.
	DoltHost string

	// Scratch renders the scratch project: its own engine, networks, volumes and
	// Dolt server, and the --scratch-town flag that lets the container
	// entrypoint install the scratch town. No name in it is one the live render
	// carries.
	Scratch bool

	// ScratchDoltImage is the Dolt server image the scratch project runs. It is
	// required when Scratch is set; the scratch spec passes DoltServerImage.
	ScratchDoltImage string

	// OMTokenFile is the path, inside the container, of the file the om
	// subscription token is read from. When set, the scratch render passes it to
	// the entrypoint as --om-token-file beside --scratch-town. Only the scratch
	// render takes it: the live project's command line stays empty, and a live
	// spec that sets it is refused (gt-qpeei.36).
	OMTokenFile string

	// LiveTownRoot is the live town's root, which a scratch spec must not
	// mount, nor mount anything under. It is required when Scratch is set, and
	// absolute, so a scratch render is always checked against the live town it
	// could otherwise reach.
	LiveTownRoot string

	// ScratchForge adds a throwaway Forgejo stack to the scratch project: the
	// forge, its Postgres, a runner and the engine the runner drives, for the L2
	// window. Only a scratch spec takes it. Its names and volumes are the scratch
	// project's own, and its two host ports are the fields below, so the stack
	// never takes the live Forgejo's 3000 and 2222.
	ScratchForge bool

	// ScratchForgeHTTPPort and ScratchForgeSSHPort are the host ports the scratch
	// Forgejo publishes, on 127.0.0.1 as the live project does. They are required
	// when ScratchForge is set, must be valid, distinct ports, and may not be the
	// live Forgejo's 3000 and 2222: a start on those ports would fight the live
	// forge for them, or worse, find it already there.
	ScratchForgeHTTPPort int
	ScratchForgeSSHPort  int
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

// ScratchDoltService is the scratch Dolt server's name on the internal network,
// and the only DoltHost a scratch spec may name: the live Dolt is on the Mac.
const ScratchDoltService = "scratch-dolt"

// The scratch project's Dolt server, which only the scratch render has. It
// joins the internal network alone and publishes no port.
const (
	scratchDoltVolume   = "scratch_dolt_data"
	scratchDoltDataPath = "/var/lib/dolt"
)

// The live Forgejo project's host ports. A scratch forge may take neither.
const (
	liveForgeHTTPPort = 3000
	liveForgeSSHPort  = 2222
)

// The scratch Forgejo stack's service names, which a compose command takes to
// start them: the forge, its database, the engine the runner drives and the
// runner. Each is the scratch project's own: none is a name the live Forgejo
// project declares (the list is the test's, in
// daemon_compose_scratch_forge_test.go, so it is written once), and the compose
// project scopes them again.
const (
	ScratchForgejoService     = "scratch-forgejo"
	ScratchForgejoDBService   = "scratch-forgejo-db"
	ScratchForgeDindService   = "scratch-forge-dind"
	ScratchForgeRunnerService = "scratch-forge-runner"
)

// ScratchForgeServices are the services the scratch forge adds to the file, in
// the order a compose command names them to start them: none is a dependency of
// daemon, so naming daemon alone would leave the forge stopped. The list is
// empty unless s.ScratchForge is set, the only case that renders the body, and
// daemon_compose_scratch_forge_test.go holds it to the body's service set.
func ScratchForgeServices(s ComposeSpec) []string {
	if !s.ScratchForge {
		return nil
	}
	return []string{
		ScratchForgejoService,
		ScratchForgejoDBService,
		ScratchForgeDindService,
		ScratchForgeRunnerService,
	}
}

// The stack's networks, the alias the runner reaches its engine by, and its
// volumes: no command names one from outside this package, so they stay
// unexported.
const (
	scratchForgeNetwork   = "scratch-forge"
	scratchForgeCINetwork = "scratch-forge-ci"
	scratchForgeDindAlias = "docker"

	scratchForgeDBVolume     = "scratch_forgejo_pgdata"
	scratchForgeDataVolume   = "scratch_forgejo_data"
	scratchForgeCertsVolume  = "scratch_forge_certs"
	scratchForgeDindVolume   = "scratch_forge_dind_data"
	scratchForgeRunnerVolume = "scratch_forge_runner_data"
)

// scratchForgeBody is the throwaway Forgejo stack. %[1]d and %[2]d are the host
// ports for http and ssh, bound to 127.0.0.1 as the live project binds its own.
// SSH_PORT is the host port, as the live project's is: it is the port clone URLs
// advertise, and the port a client reaches the forge's ssh on is the host one.
//
// Three networks keep the privileged engine away from the daemon. The forge and
// its Postgres share an internal one, so the database has no way in or out but
// the forge. The forge, the runner and the runner's engine share the CI network,
// which is not internal because the engine pulls its job images through it; the
// daemon is not on it. The forge also joins the scratch egress network, the
// daemon's, so the daemon reaches the forge by its service name without ever
// reaching the engine. Postgres trusts connections on its network because the
// stack holds nothing but scratch data, and a password would be one more secret
// for the render to carry or interpolate. The engine speaks TLS with its own
// certs volume, as the live one does.
//
// The runner reaches its engine as "docker": the engine's cert is valid for that
// name, and the CI network gives the engine that alias explicitly, because a
// container's hostname is not a name compose's DNS resolves.
//
// The runner names this forge only: the one forge URL the file carries is
// http://scratch-forgejo:3000/. compose starts it with the rest of the project;
// without a config.yml in its data volume it exits, and restart "no" leaves it
// stopped. The L2 window registers it against this forge and starts it again.
const scratchForgeBody = `  ` + ScratchForgejoDBService + `:
    image: %[3]q
    restart: unless-stopped
    environment:
      POSTGRES_USER: forgejo
      POSTGRES_DB: forgejo
      POSTGRES_HOST_AUTH_METHOD: trust
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U forgejo -d forgejo"]
      interval: 5s
      timeout: 3s
      retries: 20
    volumes:
      - type: volume
        source: ` + scratchForgeDBVolume + `
        target: /var/lib/postgresql/data
    networks:
      - ` + scratchForgeNetwork + `
  ` + ScratchForgejoService + `:
    image: %[4]q
    restart: unless-stopped
    depends_on:
      ` + ScratchForgejoDBService + `:
        condition: service_healthy
    environment:
      FORGEJO__database__DB_TYPE: postgres
      FORGEJO__database__HOST: ` + ScratchForgejoDBService + `:5432
      FORGEJO__database__NAME: forgejo
      FORGEJO__database__USER: forgejo
      FORGEJO__server__DOMAIN: ` + ScratchForgejoService + `
      FORGEJO__server__ROOT_URL: http://` + ScratchForgejoService + `:3000/
      FORGEJO__server__SSH_DOMAIN: ` + ScratchForgejoService + `
      FORGEJO__server__SSH_PORT: "%[2]d"
      FORGEJO__security__INSTALL_LOCK: "true"
      FORGEJO__service__DISABLE_REGISTRATION: "true"
      FORGEJO__actions__ENABLED: "true"
      FORGEJO__repository__DEFAULT_REPO_UNITS: "repo.code,repo.actions,repo.pulls,repo.issues"
    volumes:
      - type: volume
        source: ` + scratchForgeDataVolume + `
        target: /data
    ports:
      - "127.0.0.1:%[1]d:3000"
      - "127.0.0.1:%[2]d:22"
    networks:
      - ` + scratchForgeNetwork + `
      - ` + scratchForgeCINetwork + `
      - %[6]s
  ` + ScratchForgeDindService + `:
    image: %[5]q
    restart: unless-stopped
    privileged: true
    command: ["--insecure-registry=` + ScratchForgejoService + `:3000"]
    environment:
      DOCKER_TLS_CERTDIR: /certs
    volumes:
      - type: volume
        source: ` + scratchForgeCertsVolume + `
        target: /certs
      - type: volume
        source: ` + scratchForgeDindVolume + `
        target: /var/lib/docker
    networks:
      ` + scratchForgeCINetwork + `:
        aliases:
          - ` + scratchForgeDindAlias + `
  ` + ScratchForgeRunnerService + `:
    image: %[7]q
    restart: "no"
    depends_on:
      ` + ScratchForgejoService + `:
        condition: service_started
      ` + ScratchForgeDindService + `:
        condition: service_started
    working_dir: /data
    command: ["forgejo-runner", "--config", "config.yml", "daemon"]
    environment:
      DOCKER_HOST: tcp://` + scratchForgeDindAlias + `:2376
      DOCKER_CERT_PATH: /certs/client
      DOCKER_TLS_VERIFY: "1"
    volumes:
      - type: volume
        source: ` + scratchForgeRunnerVolume + `
        target: /data
      - type: volume
        source: ` + scratchForgeCertsVolume + `
        target: /certs
    networks:
      - ` + scratchForgeCINetwork + `
`

// scratchForgeNetworkDecl and scratchForgeVolumeDecls are the stack's entries in
// the file's networks and volumes sections.
const scratchForgeNetworkDecl = "  " + scratchForgeNetwork + ":\n    internal: true\n" +
	"  " + scratchForgeCINetwork + ":\n"

var scratchForgeVolumeDecls = "  " + scratchForgeDBVolume + ":\n" +
	"  " + scratchForgeDataVolume + ":\n" +
	"  " + scratchForgeCertsVolume + ":\n" +
	"  " + scratchForgeDindVolume + ":\n" +
	"  " + scratchForgeRunnerVolume + ":\n"

// scratchOperatorNote is what the scratch daemon's compose file tells the
// operator about a failed install. It is written once, here: scratchDaemonCommand
// turns each line into a YAML comment, so the file cannot carry a second copy
// that drifts from this one.
var scratchOperatorNote = []string{
	"After a failed scratch install the entrypoint refuses every start until",
	"the operator removes .gt-install-incomplete and what the install left,",
	"from the host side of the scratch directory.",
}

// scratchDaemonCommand is the daemon's command in the scratch project: the
// operator note, then the flags the container entrypoint is passed: --scratch-town,
// the one way a container asks for its scratch town's install, and, when
// omTokenFile is set, --om-token-file <path> beside it. The entrypoint is the
// image's ENTRYPOINT, so the command carries its flags alone. The live project
// passes nothing.
func scratchDaemonCommand(omTokenFile string) string {
	var b strings.Builder
	for _, line := range scratchOperatorNote {
		b.WriteString("    # " + line + "\n")
	}
	if omTokenFile == "" {
		fmt.Fprintf(&b, "    command: [%q]\n", "--"+config.ScratchTownFlag)
		return b.String()
	}
	fmt.Fprintf(&b, "    command: [%q, %q, %q]\n", "--"+config.ScratchTownFlag, "--"+config.OMTokenFileFlag, composeLiteral(omTokenFile))
	return b.String()
}

// scratchDoltBody is the scratch project's Dolt server service. It has no
// ports, so the only way in is the internal network the daemon also joins. Its
// healthcheck holds the daemon until the server accepts a connection, which is
// the wait the engine's own healthcheck gives the daemon (gt-3bp5q): the port
// is the Dolt server's default, 3306, which the service does not change.
//
// The probe runs bash's /dev/tcp, so it needs bash in the image. The pinned
// DoltServerImage has it: a container run from that digest on 2026-10-30
// (linux/arm64) printed /usr/bin/bash and /usr/local/bin/dolt on a Debian 12
// base. The live L1 check still runs the probe before the scratch project is
// trusted.
const scratchDoltBody = `  %[1]s:
    image: %[2]q
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/3306"]
      interval: 5s
      timeout: 3s
      retries: 10
      start_period: 30s
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
// request. A scratch daemon waits the same way on the scratch Dolt server, which
// has its own healthcheck, so the daemon never starts before its Dolt answers.
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
%[15]s    environment:
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
%[16]svolumes:
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
	var command, dolt, doltVolume, doltWait, forgeNetwork string
	if s.Scratch {
		names = scratchNames
		command = scratchDaemonCommand(s.OMTokenFile)
		dolt = fmt.Sprintf(scratchDoltBody,
			ScratchDoltService,
			composeLiteral(s.ScratchDoltImage),
			scratchDoltVolume,
			scratchDoltDataPath,
			names.network,
		)
		doltVolume = "  " + scratchDoltVolume + ":\n"
		doltWait = "      " + ScratchDoltService + ":\n        condition: service_healthy\n"
		if s.ScratchForge {
			dolt += fmt.Sprintf(scratchForgeBody,
				s.ScratchForgeHTTPPort,
				s.ScratchForgeSSHPort,
				composeLiteral(ForgePostgresImage),
				composeLiteral(ForgejoImage),
				composeLiteral(DindImage),
				names.egress,
				composeLiteral(ForgejoRunnerImage),
			)
			doltVolume += scratchForgeVolumeDecls
			forgeNetwork = scratchForgeNetworkDecl
		}
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
		doltWait,
		forgeNetwork,
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
	case !s.Scratch && (s.ScratchForge || s.ScratchForgeHTTPPort != 0 || s.ScratchForgeSSHPort != 0):
		return fmt.Errorf("compose spec: the scratch forge is set on a live spec; only the scratch render carries a throwaway Forgejo")
	case s.OMTokenFile != "" && !s.Scratch:
		return fmt.Errorf("compose spec: OMTokenFile is set on a live spec; only the scratch render passes --%s, and the live project's command line stays empty", config.OMTokenFileFlag)
	case s.OMTokenFile != "" && !filepath.IsAbs(s.OMTokenFile):
		return fmt.Errorf("compose spec: OMTokenFile %q is not absolute; it names a path inside the container", s.OMTokenFile)
	case strings.ContainsFunc(s.OMTokenFile, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return fmt.Errorf("compose spec: OMTokenFile %q holds a control character, which a command line entry cannot carry", s.OMTokenFile)
	case !s.Scratch && s.DoltHost == "":
		return fmt.Errorf("compose spec: DoltHost is empty; pass town.json's dolt.container_host or dolt.host resolved, and an empty dolt.host is the local machine, which inside the container is the container itself")
	}
	if s.Scratch {
		return s.validateScratch()
	}
	return checkDoltHost(s.DoltHost)
}

// validateScratch refuses a scratch spec that could reach the live town: the
// live town's root, container name or compose project. The container name and
// project are the config defaults a live daemon runs under. The live root is the
// caller's, passed as LiveTownRoot, because no constant names it, so an empty
// one is refused rather than compared as "no root". The scratch spec carries no
// DoltHost: its daemon's Dolt host comes from the scratch town's own settings.
func (s ComposeSpec) validateScratch() error {
	switch {
	case s.LiveTownRoot == "":
		return fmt.Errorf("compose spec: LiveTownRoot is empty; a scratch spec is checked against the live town root, so the live root must be named")
	case !filepath.IsAbs(s.LiveTownRoot):
		return fmt.Errorf("compose spec: LiveTownRoot %q is not absolute; the root check compares absolute paths", s.LiveTownRoot)
	case s.ContainerName == config.DefaultContainerName:
		return fmt.Errorf("compose spec: scratch ContainerName %q is the live daemon's container name", s.ContainerName)
	case s.Project == config.DefaultComposeProject:
		return fmt.Errorf("compose spec: scratch Project %q is the live compose project", s.Project)
	case s.ScratchDoltImage == "":
		return fmt.Errorf("compose spec: ScratchDoltImage is empty; the scratch project runs its own Dolt server, at DoltServerImage")
	}
	if err := s.checkScratchForge(); err != nil {
		return err
	}
	return s.checkScratchRoot()
}

// checkScratchForge refuses a forge spec whose ports would reach, or collide
// with, the live Forgejo: the live ports themselves, a port that is not one, or
// one port named for both services. Ports set without ScratchForge are refused
// too, because a render that ignored them would let a caller believe a forge was
// running on them.
func (s ComposeSpec) checkScratchForge() error {
	if !s.ScratchForge {
		if s.ScratchForgeHTTPPort != 0 || s.ScratchForgeSSHPort != 0 {
			return fmt.Errorf("compose spec: scratch forge ports are set but ScratchForge is not; the render would ignore them")
		}
		return nil
	}
	for _, p := range []struct {
		name string
		port int
	}{{"ScratchForgeHTTPPort", s.ScratchForgeHTTPPort}, {"ScratchForgeSSHPort", s.ScratchForgeSSHPort}} {
		switch {
		case p.port < 1 || p.port > 65535:
			return fmt.Errorf("compose spec: %s %d is not a port; a scratch forge names both its host ports", p.name, p.port)
		case p.port == liveForgeHTTPPort || p.port == liveForgeSSHPort:
			return fmt.Errorf("compose spec: %s %d is a host port the live Forgejo publishes (%d and %d); the scratch forge takes its own", p.name, p.port, liveForgeHTTPPort, liveForgeSSHPort)
		}
	}
	if s.ScratchForgeHTTPPort == s.ScratchForgeSSHPort {
		return fmt.Errorf("compose spec: ScratchForgeHTTPPort and ScratchForgeSSHPort are both %d; the two services need different host ports", s.ScratchForgeHTTPPort)
	}
	return nil
}

// checkScratchRoot refuses a scratch town root that resolves to the live root, to
// a path under it, or to an ancestor of it: mounting an ancestor exposes the live
// town just as mounting it does. The check runs on resolved paths, because a
// symlink to the live root, or a directory under one that does not exist yet,
// names the live town under another path. A path that cannot be resolved is
// refused rather than taken as clear.
func (s ComposeSpec) checkScratchRoot() error {
	// Both roots must be absolute here as well as in validate: a relative path
	// cannot be compared with an absolute one, and this check never takes "cannot
	// compare" for "clear".
	if !filepath.IsAbs(s.TownRoot) || !filepath.IsAbs(s.LiveTownRoot) {
		return fmt.Errorf("compose spec: scratch TownRoot %q and LiveTownRoot %q must both be absolute for the root check", s.TownRoot, s.LiveTownRoot)
	}
	scratch, err := resolvePath(s.TownRoot)
	if err != nil {
		return fmt.Errorf("compose spec: resolving scratch TownRoot %q: %w", s.TownRoot, err)
	}
	live, err := resolvePath(s.LiveTownRoot)
	if err != nil {
		return fmt.Errorf("compose spec: resolving LiveTownRoot %q: %w", s.LiveTownRoot, err)
	}
	if within(scratch, live) {
		return fmt.Errorf("compose spec: scratch TownRoot %q is the live town root %q or lies under it; the scratch project mounts its own directory, never the live one", s.TownRoot, s.LiveTownRoot)
	}
	if within(live, scratch) {
		return fmt.Errorf("compose spec: scratch TownRoot %q contains the live town root %q; mounting it would expose the live town, so the scratch project mounts its own directory", s.TownRoot, s.LiveTownRoot)
	}
	return nil
}

// within reports whether path is root or lies under it. Letter case is folded,
// as the default macOS volumes do, so the same directory spelled in another case
// is still the same directory; on a case-sensitive filesystem that folding can
// refuse two distinct directories whose names differ only by case, which is the
// safe side of the error for a check that guards the live town. A pair that
// cannot be compared (filepath.Rel errors when one path is absolute and the
// other is not) counts as inside, so the caller refuses: the answer "cannot
// tell" is never "clear".
func within(path, root string) bool {
	rel, err := filepath.Rel(strings.ToLower(root), strings.ToLower(path))
	if err != nil {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolvePath returns path with its symlinks resolved. A path that does not
// exist yet resolves through its nearest existing ancestor and keeps the rest
// of its name, so a directory a scratch install has not created yet, under a
// link to the live root, still resolves to the live root. A dangling link is an
// entry that exists but whose target does not, so it is an error, never a name
// to keep lexically: its target could be the live root. Any other error stops
// the resolution.
func resolvePath(path string) (string, error) {
	clean := filepath.Clean(path)
	dir, rest := clean, ""
	for {
		_, err := os.Lstat(dir)
		parent := filepath.Dir(dir)
		if errors.Is(err, fs.ErrNotExist) && parent != dir {
			rest = filepath.Join(filepath.Base(dir), rest)
			dir = parent
			continue
		}
		if err != nil {
			return "", err
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return "", err
		}
		return filepath.Join(resolved, rest), nil
	}
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
