package gtwrap

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// This file is the bddocker target: it runs one bd call inside the local bd
// container by `docker exec <container> bdgw exec`, with the same town-relative
// translation as the bdgateway target, so a Mac call is checked and logged by the
// gateway's own `exec` mode. The design is docs/design/bd-container.md, "Call
// path from each caller" and task G10 of the plan.

// bdDockerContainerRE matches the container names the target accepts. Docker
// names hold letters, digits, dots, dashes and underscores and start with a
// letter or digit; the name comes from the config file and reaches argv and the
// ps filter, so a name that could be read as a flag or as a regular expression is
// refused before either is built.
var bdDockerContainerRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// BdDocker is the bddocker target. It runs one bd call inside the local bd
// container through `docker exec`, and passes the container command's exit status
// back unchanged.
type BdDocker struct {
	// Town is the town root the caller runs in: BEADS_DIR and the working
	// directory are translated relative to it, so no absolute path is sent.
	Town string
	// Container is the bd container's name, gt-bd in the compose unit.
	Container string
	// Context names the Docker context every command runs in. Empty means the
	// current context.
	Context string
	// Caller is the name bdgw exec records in the call log.
	Caller string
	// TimeoutMS is the call's requested limit. Zero means gatewayDefaultTimeoutMS.
	TimeoutMS int
	// Shell runs the docker command lines.
	Shell Shell
	// ProbeTimeout bounds each probe. Zero means probeTimeout.
	ProbeTimeout time.Duration
	// Note receives the explanation when the working directory is outside the
	// town and runs in the town root instead. Nil refuses the call, so a mapped
	// directory is never silent.
	Note io.Writer
}

// Launch runs call inside the bd container and returns the container command's
// exit status. It refuses with the doctor's reason when the ladder finds no
// running, healthy container: there is no throwaway rung for bd, because a fresh
// container has neither the workspace volume nor the token files a call needs.
func (b BdDocker) Launch(call Call) (int, error) {
	if err := b.validate(); err != nil {
		return exitDown, err
	}
	if err := checkCall(call); err != nil {
		return exitDown, err
	}
	if call.Argv[0] != "bd" {
		return exitDown, fmt.Errorf("gtwrap: the bddocker target runs bd, but the call names %q", call.Argv[0])
	}
	rig, err := beadRig(b.Town, call.Env)
	if err != nil {
		return exitDown, err
	}
	env, err := gatewayEnv(call.Env)
	if err != nil {
		return exitDown, err
	}
	if rung, reason := b.rung(); rung != RungExec {
		return exitDown, fmt.Errorf("gtwrap: %s", reason)
	}
	return b.attach(call, rig, dirRelative(b.Town, call.Dir, b.Note), env)
}

// attach runs the call in the container: the bd arguments after `bdgw exec --`,
// the rig and the town-relative directory named on its flags, the screened
// environment forwarded by name, and the call's stdin attached.
func (b BdDocker) attach(call Call, rig, dir string, env map[string]string) (int, error) {
	flags, dockerEnv := b.envFlags(env)
	args := []string{"exec"}
	args = append(args, stdioFlags(call)...)
	args = append(args, flags...)
	args = append(args, b.Container, "bdgw", "exec")
	if rig != "" {
		args = append(args, "-rig", rig)
	}
	args = append(args, "-cwd", dir, "-timeout-ms", strconv.Itoa(b.timeoutMS()), "-caller", b.caller(), "--")
	args = append(args, call.Argv[1:]...)
	return b.Shell.AttachStdin(dockerCommand(b.Context, args...), dockerEnv, call.Stdin)
}

// envFlags carries the screened environment into the container. Each name is
// passed by `-e NAME` and its value in the docker process's environment, so the
// value stays out of argv, where a process listing would show it.
func (b BdDocker) envFlags(env map[string]string) (flags, dockerEnv []string) {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		flags = append(flags, "-e", name)
		dockerEnv = append(dockerEnv, name+"="+env[name])
	}
	return flags, dockerEnv
}

// Doctor reports the first step of the ladder that fails: Docker answering, the
// container existing, the container running, and its health check passing. The
// container is named so the operator knows which one the call would reach.
func (b BdDocker) Doctor() (Diagnosis, error) {
	if err := b.validate(); err != nil {
		return Diagnosis{}, err
	}
	rung, reason := b.rung()
	return Diagnosis{Rung: rung, Container: b.Container, Reason: reason}, nil
}

// rung chooses the step of the ladder that runs a call and the reason for it,
// running nothing in the container. Launch and Doctor both come through here, so
// the doctor cannot report a different step from the one a call takes. There is
// no run rung: when the container does not exist or is stopped or unhealthy, the
// call is refused rather than run in a throwaway container.
func (b BdDocker) rung() (Rung, string) {
	if _, exit, err := b.probe("info"); err != nil || exit != 0 {
		return RungDown, b.downText()
	}
	present, exit, err := b.probe("ps", "-a", "-q", "--filter", "name=^"+b.Container+"$")
	if err != nil || exit != 0 {
		return RungDown, fmt.Sprintf("could not list the containers, so it is not known whether %s exists; the call did not run", b.Container)
	}
	if strings.TrimSpace(present) == "" {
		return RungDown, fmt.Sprintf("the bd container %s does not exist, so the call did not run; start it with `gt dolt start`", b.Container)
	}
	running, exit, err := b.probe("ps", "-q", "--filter", "name=^"+b.Container+"$")
	if err != nil || exit != 0 {
		return RungDown, fmt.Sprintf("could not list the containers, so it is not known whether %s is running; the call did not run", b.Container)
	}
	if strings.TrimSpace(running) == "" {
		return RungDown, fmt.Sprintf("the bd container %s is stopped, so the call did not run; start it with `gt dolt start`", b.Container)
	}
	health, exit, err := b.probe("inspect", "--format", "{{.State.Health.Status}}", b.Container)
	if err != nil || exit != 0 {
		return RungDown, fmt.Sprintf("the health of the bd container %s cannot be read, so the call did not run; its compose service must define the `bdgw ready` health check", b.Container)
	}
	switch status := strings.TrimSpace(health); status {
	case "healthy":
		return RungExec, fmt.Sprintf("the bd container %s is running and healthy", b.Container)
	case "starting":
		return RungDown, fmt.Sprintf("the bd container %s is starting and not healthy yet, so the call did not run; run the command again when it is healthy", b.Container)
	case "unhealthy":
		return RungDown, fmt.Sprintf("the bd container %s is unhealthy, so its `bdgw ready` check is failing and the call did not run; check the container's log and the workspace it was pushed", b.Container)
	default:
		return RungDown, fmt.Sprintf("the bd container %s reports health %q, which is not a healthy state, so the call did not run", b.Container, status)
	}
}

// downText names what is down and the commands that start it: Docker Desktop,
// which takes the container down with it, and the command that starts the
// compose unit the bd container runs in.
func (b BdDocker) downText() string {
	return fmt.Sprintf("Docker is down: docker info did not answer, so the bd container %s cannot be reached. Start Docker Desktop with `open -a Docker`, then start the bd container with `gt dolt start`, and run the command again", b.Container)
}

// validate refuses a target that is not fully set up. A field that is empty or
// would be read as a flag is refused by name, and so is a dependency left
// unwired, since an unwired check is not a pass.
func (b BdDocker) validate() error {
	switch {
	case b.Town == "" || !filepath.IsAbs(b.Town):
		return fmt.Errorf("gtwrap: the bddocker target's town root is %q; it must be an absolute path", b.Town)
	case !bdDockerContainerRE.MatchString(b.Container):
		return fmt.Errorf("gtwrap: the bddocker target's container is %q; it must be a container name of letters, digits, dots, dashes and underscores, not starting with a dash", b.Container)
	case b.Shell == nil:
		return errors.New("gtwrap: no shell is wired for the bddocker target, so the call is refused")
	case b.Note == nil:
		return errors.New("gtwrap: no note writer is wired for the bddocker target, so a mapped directory would be silent; the call is refused")
	}
	return nil
}

// probe runs one docker command that only reads state, bounded by ProbeTimeout.
func (b BdDocker) probe(args ...string) (string, int, error) {
	return probeDocker(b.Shell, b.ProbeTimeout, dockerCommand(b.Context, args...))
}

// timeoutMS is the call's requested limit, with the default for an unset one.
func (b BdDocker) timeoutMS() int {
	if b.TimeoutMS > 0 {
		return b.TimeoutMS
	}
	return gatewayDefaultTimeoutMS
}

// caller is the caller name bdgw exec logs: the configured name, or the default.
func (b BdDocker) caller() string {
	if b.Caller != "" {
		return b.Caller
	}
	return gatewayDefaultCaller
}
