package templates

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ContainerDockerContext is the --context every container argv carries: the
// Docker Desktop context a container town's daemon runs in. Never the global
// context, which nothing here may depend on.
const ContainerDockerContext = "desktop-linux"

// ContainerInspectTimeout bounds one container reading. A docker that hangs
// must fail the reading as unknown at the deadline, not hold a doctor check or
// a status line open for as long as it runs.
const ContainerInspectTimeout = 10 * time.Second

// containerInspectFormat asks docker for the two fields the container
// supervisor's state is made of: the state it reports for the container and
// the image it runs.
const containerInspectFormat = "{{.State.Status}} {{.Config.Image}}"

// containerAbsentMarker is what docker writes to stderr for a container it
// knows nothing about. That container is absent, which is docker's answer and
// not a read that failed.
const containerAbsentMarker = "No such object"

// ContainerDocker runs one docker argv and returns its stdout. It must return
// once ctx is done, so a hung docker fails the reading at its deadline, and
// its error must keep the command's stderr apart from the stdout the reading
// parses. Production is RunDockerInspect; a test supplies a fake and starts no
// process.
type ContainerDocker func(ctx context.Context, argv []string) ([]byte, error)

// RunDockerInspect is the production ContainerDocker: one docker command under
// ctx, whose stderr is kept out of the stdout a reading parses.
func RunDockerInspect(ctx context.Context, argv []string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // fixed argv from ContainerJobState
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &DockerInspectError{StderrText: stderr.String(), Err: err}
	}
	return stdout.Bytes(), nil
}

// DockerInspectError is a failed docker run's exit with its stderr kept aside,
// so a diagnostic is never read as state. Error() carries the exit alone,
// because captured output can hold a secret.
type DockerInspectError struct {
	StderrText string // the command's stderr; never in Error() for the reason above
	Err        error  // the command's exit
}

func (e *DockerInspectError) Error() string  { return e.Err.Error() }
func (e *DockerInspectError) Unwrap() error  { return e.Err }
func (e *DockerInspectError) Stderr() string { return e.StderrText }

// stderrCarrier is a runner error that kept the command's stderr. Only stderr
// can say the container is absent: stdout is the inspect format's own output,
// and a status printed there is not docker's verdict on the container.
type stderrCarrier interface {
	error
	Stderr() string
}

// ContainerJobState is the state reader of the container named containerName,
// as one docker inspect under dockerContext reports it.
func ContainerJobState(run ContainerDocker, dockerContext, containerName string) SupervisorReader {
	argv := []string{"docker", "--context", dockerContext, "inspect", "-f", containerInspectFormat, containerName}
	return func(kind string) SupervisorState {
		st := SupervisorState{Kind: kind, LastExit: -1, Container: containerName}
		ctx, cancel := context.WithTimeout(context.Background(), ContainerInspectTimeout)
		defer cancel()
		out, err := run(ctx, argv)
		if err != nil {
			var carrier stderrCarrier
			if errors.As(err, &carrier) && strings.Contains(carrier.Stderr(), containerAbsentMarker) {
				return st
			}
			// Unknown, never "not running": the daemon may well be up, and of
			// the two wrong answers only one reports a failure the operator
			// can see.
			st.Err = fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
			return st
		}
		// A successful run that printed no status and image is a read this
		// reader cannot interpret, not a container that is absent.
		fields := strings.Fields(string(out))
		if len(fields) < 2 {
			st.Err = fmt.Errorf("%s: docker printed no status and image for %s", strings.Join(argv, " "), containerName)
			return st
		}
		st.Loaded = true
		st.Status = fields[0]
		st.Image = fields[1]
		return st
	}
}
