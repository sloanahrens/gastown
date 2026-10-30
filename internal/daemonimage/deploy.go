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
// The start command must name composePath as its only compose file, since that
// is the file written here; a command that named another would start a
// container this deploy never rendered.
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
	if files := composeFiles(argv); len(files) != 1 || files[0] != composePath {
		return fmt.Errorf("daemonimage: the start command must name %s as its only compose file, the one Deploy writes; nothing was written or started", composePath)
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

// composeFiles returns the compose files argv names: the value of each -f or
// --file flag, whether the value follows the flag or is joined to it with =.
// A flag with no value yields an empty name, which no compose file matches.
func composeFiles(argv []string) []string {
	var files []string
	for i := 0; i < len(argv); i++ {
		switch a := argv[i]; {
		case a == "-f" || a == "--file":
			name := ""
			if i+1 < len(argv) {
				name = argv[i+1]
			}
			files = append(files, name)
			i++
		case strings.HasPrefix(a, "--file="):
			files = append(files, strings.TrimPrefix(a, "--file="))
		case strings.HasPrefix(a, "-f="):
			files = append(files, strings.TrimPrefix(a, "-f="))
		}
	}
	return files
}

// writeComposeFile replaces path with content in one rename, so a reader of the
// file sees either the old render or the new one, never a partly written file.
// The render goes to a new temporary file beside path, since a rename is atomic
// only within one filesystem, and the name is unique so concurrent writers never
// share it. The temporary file is removed unless the rename puts it in place.
func writeComposeFile(path string, content []byte) error {
	return writeComposeFileWith(path, content, createComposeTemp)
}

// composeTemp is the temporary file a render is written to. *os.File is one;
// a test stands in a file that fails a step, to drive that step's failure.
type composeTemp interface {
	Write(p []byte) (int, error)
	Chmod(mode os.FileMode) error
	Close() error
	Name() string
}

// createComposeTemp makes the temporary file writeComposeFile writes through.
func createComposeTemp(dir, pattern string) (composeTemp, error) {
	return os.CreateTemp(dir, pattern)
}

// writeComposeFileWith is writeComposeFile with the temporary file's source
// injected, so each step's failure can be driven.
func writeComposeFileWith(path string, content []byte, create func(dir, pattern string) (composeTemp, error)) (err error) {
	f, err := create(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(content); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Chmod(0o644); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
