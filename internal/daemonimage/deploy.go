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
// Deploy asks it for the start argv before it writes anything, so a town its
// settings refuse is refused with its compose file untouched.
type Supervisor interface {
	// StartArgv builds the argv that starts the daemon's container. `up -d
	// daemon` also recreates the container, which is how a new image reaches
	// it; nothing removes the old container first.
	StartArgv() ([]string, error)
	// ContainerName is the container the town's settings name, the one
	// StartArgv recreates. A render for any other container is refused.
	ContainerName() string
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
// The start command must name composePath as its only compose file and spec's
// Project as its only compose project, and spec's ContainerName must be the
// container the supervisor's settings name. A command or render that disagrees
// would start or replace a container this deploy did not render for, so each
// mismatch is refused before anything is written.
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
	files, projects := composeGlobals(argv)
	if len(files) != 1 || files[0] != composePath {
		return fmt.Errorf("daemonimage: the start command must name %s as its only compose file, the one Deploy writes; nothing was written or started", composePath)
	}
	if len(projects) != 1 || projects[0] != spec.Project {
		return fmt.Errorf("daemonimage: the start command must run compose project %q, the project the render is for; nothing was written or started", spec.Project)
	}
	if container := sup.ContainerName(); spec.ContainerName != container {
		return fmt.Errorf("daemonimage: the render is for container %q but the town's settings name %q; nothing was written or started", spec.ContainerName, container)
	}
	content, err := templates.RenderDaemonCompose(spec)
	if err != nil {
		return fmt.Errorf("daemonimage: cannot render the compose file for %s; the old file is unchanged and nothing was started: %w", composePath, err)
	}
	if err := WriteComposeFile(composePath, content); err != nil {
		return fmt.Errorf("daemonimage: cannot write %s; the old compose file is unchanged and nothing was started: %w", composePath, err)
	}
	if err := d.Run(ctx, filepath.Dir(composePath), dockerEnv(d.Environ), argv); err != nil {
		return fmt.Errorf("daemonimage: starting the container failed: %w", err)
	}
	return nil
}

// composeGlobals returns the compose files and projects argv names before its
// compose subcommand: the value of each -f or --file, and of each -p or
// --project-name, whether the value follows the flag or is joined to it with =.
// Flags after the subcommand belong to it and are not read. A flag with no value
// yields an empty name, which matches no file or project.
func composeGlobals(argv []string) (files, projects []string) {
	i := 0
	for ; i < len(argv) && argv[i] != "compose"; i++ {
		if argv[i] == "--context" {
			i++
		}
	}
	for i++; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "-f" || a == "--file":
			files = append(files, valueAfter(argv, i))
			i++
		case strings.HasPrefix(a, "--file="):
			files = append(files, strings.TrimPrefix(a, "--file="))
		case strings.HasPrefix(a, "-f="):
			files = append(files, strings.TrimPrefix(a, "-f="))
		case a == "-p" || a == "--project-name":
			projects = append(projects, valueAfter(argv, i))
			i++
		case strings.HasPrefix(a, "--project-name="):
			projects = append(projects, strings.TrimPrefix(a, "--project-name="))
		case strings.HasPrefix(a, "-p="):
			projects = append(projects, strings.TrimPrefix(a, "-p="))
		default:
			return files, projects
		}
	}
	return files, projects
}

// valueAfter is the argument after the flag at argv[i], or empty when the flag
// is the last argument.
func valueAfter(argv []string, i int) string {
	if i+1 < len(argv) {
		return argv[i+1]
	}
	return ""
}

// WriteComposeFile replaces path with content in one rename, so a reader of the
// file sees either the old render or the new one, never a partly written file.
// The render is written to a temporary file beside path, because a rename is
// atomic only within one filesystem; its unique name keeps concurrent writers
// apart. The temporary file is removed unless the rename puts it in place.
//
// `gt daemon compose write` renders the live town's file through this too, so
// every writer of a compose file replaces it the same way.
func WriteComposeFile(path string, content []byte) error {
	return writeComposeFileWith(path, content, createComposeTemp)
}

// ImageTagRev returns the rev an image tag names: the commit ImageTag stamps.
// It refuses any other tag, so a tag the builder could not have made never
// reaches a rendered compose file. The shape is read with the builder's own
// check, so the two cannot drift.
func ImageTagRev(tag string) (string, error) {
	rev, ok := strings.CutPrefix(strings.TrimSpace(tag), "gt-daemon:")
	if !ok || !isCommit(rev) {
		return "", fmt.Errorf("daemonimage: image tag %q is not gt-daemon:<7 or more hex digits>; build one with gt daemon image", tag)
	}
	return rev, nil
}

// composeTemp is the temporary file a render is written to. *os.File is one;
// a test stands in a file that fails a step, to drive that step's failure.
type composeTemp interface {
	Write(p []byte) (int, error)
	Chmod(mode os.FileMode) error
	Close() error
	Name() string
}

// createComposeTemp makes the temporary file WriteComposeFile writes through.
func createComposeTemp(dir, pattern string) (composeTemp, error) {
	return os.CreateTemp(dir, pattern)
}

// writeComposeFileWith is WriteComposeFile with the temporary file's source
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
