package daemonimage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/templates"
)

// Supervisor is the daemon supervisor that replaces a container town's daemon.
// It offers its start argv the way the landed supervisor does (gt-qpeei.10), as
// a builder called when needed rather than a fixed slice: the container
// supervisor refuses a town that does not set both daemon.container_name and
// daemon.compose_project there, and Deploy asks for the argv before it writes
// anything, so a town that would act on the live container is refused with its
// compose file untouched.
type Supervisor interface {
	// StartArgv builds the argv that starts the daemon's container. `up -d
	// daemon` also recreates the container, which is how a new image reaches
	// it; nothing removes the old container first.
	StartArgv() ([]string, error)
}

// Deployer replaces a container town's daemon with a freshly built image: it
// writes the town's compose file and runs the supervisor's start command.
type Deployer struct {
	// Run runs the supervisor's start command; it is the package's only way to
	// run anything, as it is for the builds.
	Run Runner
	// Environ is the host environment the command starts from; nil reads the
	// host's. It goes through dockerEnv for the same reason the image build
	// does: the argv carries the explicit --context, so a shell's DOCKER_HOST
	// or DOCKER_CONTEXT must not reach the command and pick another engine.
	Environ []string
}

// Deploy writes spec's render to composePath, then runs sup's start command.
// The render is written to a temporary file beside composePath and renamed over
// it, and the rename happens before the command runs, so a container that
// starts always starts from the file on disk, never from a half-written one.
//
// The start argv is asked for first and run last. Asking first is what makes a
// refused town harmless: an unset daemon.container_name or daemon.compose_project
// refuses there, before the render replaces the file. A render or write error
// then leaves the old compose file as it was and runs no container command.
func (d Deployer) Deploy(ctx context.Context, sup Supervisor, composePath string, spec templates.ComposeSpec) error {
	if d.Run == nil {
		return errors.New("daemonimage: no runner to start the container with")
	}
	if sup == nil {
		return errors.New("daemonimage: no supervisor to start the container with")
	}
	composePath = strings.TrimSpace(composePath)
	if composePath == "" {
		return errors.New("daemonimage: no compose file to write")
	}
	if !filepath.IsAbs(composePath) {
		return fmt.Errorf("daemonimage: compose file path %q is not absolute; the supervisor's argv names this file and docker resolves a relative -f against the directory it runs in, not the caller's", composePath)
	}
	argv, err := sup.StartArgv()
	if err != nil {
		return fmt.Errorf("daemonimage: the supervisor refused to start the container: %w", err)
	}
	content, err := templates.RenderDaemonCompose(spec)
	if err != nil {
		return fmt.Errorf("daemonimage: cannot render the compose file for %s; the old file is unchanged and nothing was started: %w", composePath, err)
	}
	if err := writeComposeFile(composePath, content); err != nil {
		return fmt.Errorf("daemonimage: cannot write %s; the old compose file is unchanged and nothing was started: %w", composePath, err)
	}
	if err := d.Run(ctx, filepath.Dir(composePath), dockerEnv(d.Environ), argv); err != nil {
		return fmt.Errorf("daemonimage: starting the container failed: %w", err)
	}
	return nil
}

// writeComposeFile replaces path with content in one rename, so a reader of the
// file — the start command, a status check — sees either the old render or the
// new one, never a partly written file. The temporary file is path with .tmp
// appended: in the same directory, because a rename is atomic only within one
// filesystem. A deploy is the only writer of a town's compose file, and one
// that found a previous deploy's leftover temp file replaces it rather than
// reads it.
//
// path is untouched until the rename, so both failures leave the old render in
// place. A write that fails may leave a partial temporary file, which the next
// deploy overwrites; a rename that fails removes it here, since it is the only
// copy of the new render and nothing would ever read it.
func writeComposeFile(path string, content []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
