package gtwrap

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// doltHostVar is the variable bd reads for the host that runs Dolt.
const doltHostVar = "BEADS_DOLT_SERVER_HOST"

// doltHostGateway is the name a container uses for the host. Inside the daemon
// container 127.0.0.1 is the container itself, so bd there reaches Dolt by this
// name. It replaces any value the caller forwarded, since a forwarded 127.0.0.1
// would send bd to the wrong place.
const doltHostGateway = "host.docker.internal"

// composeService is the compose service that runs the daemon.
const composeService = "daemon"

// composeProject is the compose project the town's daemon runs under. It appears
// only in the start command downText names.
const composeProject = "gt-live"

// probeTimeout is the bound on each probe when Docker.ProbeTimeout is unset. A
// hung engine then refuses the call instead of hanging the caller. Attach is not
// bounded: a tool session has no fixed end.
const probeTimeout = 10 * time.Second

// exitDown is the status returned when the call cannot run: no rung holds, or
// the docker command did not start.
const exitDown = 1

// Shell runs the docker command lines the Docker runner plans. The real one runs
// the docker binary; a test one records the lines. Each argv starts with "docker".
type Shell interface {
	// Attach runs argv with the caller's stdin, stdout and stderr attached and
	// returns the status it exited with. An error means it did not run at all.
	Attach(argv []string) (exit int, err error)
	// Capture runs argv and returns its stdout and exit status. ctx bounds the
	// run: when it ends, the command is stopped. An error means it did not run.
	Capture(ctx context.Context, argv []string) (stdout string, exit int, err error)
}

// Docker reaches the daemon image through the docker CLI. Exec and Run are the
// first two rungs of the ladder; Launch chooses between them and refuses when
// Docker is down.
type Docker struct {
	// Town is the town root. It is mounted at the same path inside the container,
	// and the compose file the image tag comes from sits under it.
	Town string
	// Context names the Docker context every command runs in, so the town's VM is
	// chosen by name rather than by the shell's current context. Empty means the
	// current context.
	Context string
	// Shell runs the docker command lines.
	Shell Shell
	// ReadFile reads the compose file. The file is read on every call so an edit
	// to it takes effect without restarting the wrapper.
	ReadFile func(path string) ([]byte, error)
	// ProbeTimeout bounds each probe. Zero means probeTimeout.
	ProbeTimeout time.Duration
}

// Launch runs call by the first rung that holds: exec into the running daemon
// container; a throwaway run of the same image when that container is stopped;
// and, when Docker itself is down, an error that names what is down and the
// command that starts it. It refuses, rather than guessing, when the probes
// cannot answer.
func (d Docker) Launch(call Call) (int, error) {
	svc, err := d.daemonService()
	if err != nil {
		return exitDown, err
	}
	if err := checkCall(call); err != nil {
		return exitDown, err
	}
	rung, reason := d.rung(svc)
	switch rung {
	case RungExec:
		return d.Exec(call)
	case RungRun:
		return d.Run(call)
	default:
		return exitDown, fmt.Errorf("gtwrap: %s", reason)
	}
}

// rung chooses the step of the ladder that runs a call and the reason for it,
// running nothing in the container. Launch and Doctor both come through here,
// so the doctor cannot report a different step from the one a call takes.
func (d Docker) rung(svc daemonService) (Rung, string) {
	if _, exit, err := d.probe("info"); err != nil || exit != 0 {
		return RungDown, d.downText(svc)
	}
	running, exit, err := d.probe("ps", "-q", "--filter", "name=^"+svc.ContainerName+"$")
	if err != nil || exit != 0 {
		return RungDown, fmt.Sprintf("could not list the containers, so it is not known whether %s is running; the call did not run", svc.ContainerName)
	}
	if strings.TrimSpace(running) != "" {
		return RungExec, "the daemon container " + svc.ContainerName + " is running"
	}
	return RungRun, "the daemon container " + svc.ContainerName + " is not running, so a throwaway container of the image runs the tool"
}

// Exec runs call in the running daemon container: rung 1.
func (d Docker) Exec(call Call) (int, error) {
	svc, err := d.daemonService()
	if err != nil {
		return exitDown, err
	}
	if err := checkCall(call); err != nil {
		return exitDown, err
	}
	args := []string{"exec"}
	args = append(args, stdioFlags(call)...)
	args = append(args, callFlags(call)...)
	args = append(args, svc.ContainerName)
	args = append(args, call.Argv...)
	return d.Shell.Attach(d.dockerArgv(args...))
}

// Run runs call in a throwaway container of the daemon image with the town
// mounted at its own path: rung 2. The entrypoint is replaced by the tool,
// because the image's entrypoint is the daemon's, not the tool's.
func (d Docker) Run(call Call) (int, error) {
	svc, err := d.daemonService()
	if err != nil {
		return exitDown, err
	}
	if err := checkCall(call); err != nil {
		return exitDown, err
	}
	args := []string{"run", "--rm"}
	args = append(args, stdioFlags(call)...)
	args = append(args, callFlags(call)...)
	args = append(args, "-v", d.Town+":"+d.Town, "--entrypoint", call.Argv[0], svc.Image)
	args = append(args, call.Argv[1:]...)
	return d.Shell.Attach(d.dockerArgv(args...))
}

// toolCheck is the shell command ToolExists runs in the container: command -v is
// a shell builtin, so the image needs sh and nothing else.
const toolCheck = `command -v "$1"`

// ToolExists checks that name answers inside the image the ladder reaches: the
// running container's command, or a throwaway container of the same image when
// the daemon is stopped. It refuses when the rung is down, so a cutover never
// runs against a Docker that cannot answer.
func (d Docker) ToolExists(name string) error {
	if _, err := toolFor(name); err != nil {
		return err
	}
	svc, err := d.daemonService()
	if err != nil {
		return err
	}
	rung, reason := d.rung(svc)
	switch rung {
	case RungExec:
		args := []string{"exec", svc.ContainerName, "sh", "-c", toolCheck, "sh", name}
		return d.checkTool(args, name, svc.Image)
	case RungRun:
		args := []string{"run", "--rm", "--entrypoint", "sh", svc.Image, "-c", toolCheck, "sh", name}
		return d.checkTool(args, name, svc.Image)
	default:
		return fmt.Errorf("gtwrap: %s", reason)
	}
}

// checkTool runs one tool check and refuses when it cannot answer or finds
// nothing on PATH.
func (d Docker) checkTool(args []string, name, image string) error {
	out, exit, err := d.probe(args...)
	if err != nil {
		return fmt.Errorf("gtwrap: check that %s answers in the image %s: %w", name, image, err)
	}
	if exit != 0 || strings.TrimSpace(out) == "" {
		return fmt.Errorf("gtwrap: %s does not answer in the image %s: the check exited %d", name, image, exit)
	}
	return nil
}

// daemonService is the daemon service of the compose file: the image a throwaway
// run starts and the container name exec and the probe use. Both are read, never
// hard-coded, and a file that does not supply both is an error.
type daemonService struct {
	Image         string
	ContainerName string
}

// composePath is the compose file the container supervisor drives, which is
// config.ContainerComposeFile. The wrapper does not import internal/config, which
// would pull the whole town into a binary that has to stay small.
func (d Docker) composePath() string {
	return filepath.Join(d.Town, "daemon", "container", "compose.yml")
}

func (d Docker) daemonService() (daemonService, error) {
	if d.Town == "" {
		return daemonService{}, errors.New("gtwrap: no town root, so the daemon compose file cannot be found")
	}
	path := d.composePath()
	data, err := d.ReadFile(path)
	if err != nil {
		return daemonService{}, fmt.Errorf("gtwrap: read the daemon compose file: %w", err)
	}
	var file struct {
		Services map[string]struct {
			Image         string `yaml:"image"`
			ContainerName string `yaml:"container_name"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &file); err != nil {
		return daemonService{}, fmt.Errorf("gtwrap: parse the daemon compose file %s: %w", path, err)
	}
	s := file.Services[composeService]
	if s.Image == "" || s.ContainerName == "" {
		return daemonService{}, fmt.Errorf("gtwrap: the %s service in %s names no image or container_name", composeService, path)
	}
	return daemonService{Image: s.Image, ContainerName: s.ContainerName}, nil
}

// downText names what is down and the commands that start it: Docker Desktop,
// which takes the container down with it, and the compose command that starts
// the daemon once Docker answers.
func (d Docker) downText(svc daemonService) string {
	start := strings.Join(d.dockerArgv("compose", "-p", composeProject, "-f", d.composePath(), "up", "-d", composeService), " ")
	return fmt.Sprintf("Docker is down: docker info did not answer, so the container %s cannot be reached. Start Docker Desktop with `open -a Docker`, then start the daemon with `%s`, and run the command again", svc.ContainerName, start)
}

// probe runs one docker command that only reads state, bounded by ProbeTimeout.
func (d Docker) probe(args ...string) (string, int, error) {
	limit := d.ProbeTimeout
	if limit <= 0 {
		limit = probeTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	return d.Shell.Capture(ctx, d.dockerArgv(args...))
}

// dockerArgv is the docker command line for args, in the configured context.
func (d Docker) dockerArgv(args ...string) []string {
	argv := []string{"docker"}
	if d.Context != "" {
		argv = append(argv, "--context", d.Context)
	}
	return append(argv, args...)
}

// checkCall refuses a call with no tool to run: Run needs the tool for its
// entrypoint, and Exec would hand docker no command.
func checkCall(call Call) error {
	if len(call.Argv) == 0 {
		return errors.New("gtwrap: the call names no tool to run")
	}
	return nil
}

// stdioFlags attach stdin always, and a terminal only when stdin is one.
func stdioFlags(call Call) []string {
	if call.TTY {
		return []string{"-i", "-t"}
	}
	return []string{"-i"}
}

// callFlags carry the caller's directory and environment into the container. The
// Dolt host is set last and replaces any forwarded value of the same name.
func callFlags(call Call) []string {
	flags := []string{"-w", call.Dir}
	for _, entry := range call.Env {
		name, _, _ := strings.Cut(entry, "=")
		if name == doltHostVar {
			continue
		}
		flags = append(flags, "-e", entry)
	}
	return append(flags, "-e", doltHostVar+"="+doltHostGateway)
}

// OSShell runs docker from the host's PATH: the Shell for a Docker that talks to
// the real engine, where a test uses a fake.
type OSShell struct{}

// Attach runs argv with the wrapper's own stdin, stdout and stderr.
func (OSShell) Attach(argv []string) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return exitStatus(cmd.Run())
}

// Capture runs argv and returns its stdout. A probe stopped by ctx exits with a
// non-zero status, which the caller reads as a refusal.
func (OSShell) Capture(ctx context.Context, argv []string) (string, int, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.Output()
	return probeResult(out, err)
}

// probeResult reads a finished probe: its stdout and exit status, or an error when
// it did not run at all.
func probeResult(out []byte, err error) (string, int, error) {
	exit, err := exitStatus(err)
	return string(out), exit, err
}

// exitStatus turns a finished command's error into its exit status. An error that
// is not an exit status means the command did not run at all.
func exitStatus(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var failed *exec.ExitError
	if errors.As(err, &failed) {
		return failed.ExitCode(), nil
	}
	return exitDown, err
}
