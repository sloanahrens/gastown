package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/steveyegge/gastown/internal/doctor"
	"github.com/steveyegge/gastown/internal/doltbackup"
	"github.com/steveyegge/gastown/internal/doltpause"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/style"
)

// doltComposeProject is the compose project the town's data unit runs as
// (docs/design/bd-container.md, "The compose unit").
const doltComposeProject = "gt-data"

// doltComposeRelPath is the compose file, under the rig's gastown directory
// that also holds the values file.
const doltComposeRelPath = "deploy/data/docker-compose.yml"

// doltComposeWaitSeconds bounds the wait for both services to report healthy
// after up. An unhealthy service ends the start with compose's error, not a
// wait with no end.
const doltComposeWaitSeconds = "120"

// doltUseComposeUnit is set by --compose-unit on gt dolt start, stop and
// status. Off, those commands run the single-container path they always ran.
var doltUseComposeUnit bool

// doltComposeDeps is what the compose path reaches outside the town: the docker
// call, the environment that call inherits and the backup root. A test supplies
// fakes, so it runs no docker and reads neither the live environment nor HOME.
type doltComposeDeps struct {
	// run runs one docker call with argv and the environment env, and returns
	// what it wrote to stdout.
	run        func(argv, env []string) ([]byte, error)
	environ    func() []string
	backupRoot func() (string, error)
}

// doltComposeRealDeps is doltComposeDeps on the real machine.
func doltComposeRealDeps() doltComposeDeps {
	return doltComposeDeps{
		run:        runDoltComposeCommand,
		environ:    os.Environ,
		backupRoot: doltbackup.DefaultRoot,
	}
}

// runDoltComposeCommand runs one docker call. Its stderr goes to the terminal,
// where compose's progress and errors belong.
func runDoltComposeCommand(argv, env []string) ([]byte, error) {
	c := exec.Command(argv[0], argv[1:]...)
	c.Env = env
	c.Stderr = os.Stderr
	out, err := c.Output()
	if err != nil {
		return nil, doltComposeRunError(err)
	}
	return out, nil
}

// doltComposeRunError names a failed docker call by its exit status only: what
// the call wrote can carry host detail. A call that never started has no status.
func doltComposeRunError(err error) error {
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) {
		return fmt.Errorf("docker exited with status %d", exit.ExitCode())
	}
	return errors.New("the docker command could not be run")
}

// doltComposeUnit is what the compose unit runs: its file, and the host paths
// and bd image the file reads from its environment.
type doltComposeUnit struct {
	file       string
	dataDir    string
	backupDir  string
	configPath string
	bdImage    string
	tokenDir   string
}

// argv is a docker compose call for this unit: the project, the file, then verb.
func (u doltComposeUnit) argv(verb ...string) []string {
	return append([]string{"docker", "compose", "-p", doltComposeProject, "-f", u.file}, verb...)
}

// env is base with the variables the compose file reads from the environment
// that starts it. Entries appended last win, so the values file's bd image and
// the town's token directory are the ones compose sees even when base names
// another.
func (u doltComposeUnit) env(base []string) []string {
	return append(append([]string{}, base...),
		"GT_DOLT_DATA="+u.dataDir,
		"GT_DOLT_BACKUPS="+u.backupDir,
		"GT_DOLT_CONFIG="+u.configPath,
		"BD_IMAGE="+u.bdImage,
		"GT_BD_TOKEN_DIR="+u.tokenDir,
	)
}

// doltComposeUnitFor reads what the compose unit runs for townRoot. It refuses
// by name when the values file is missing or names no bd image: a unit without
// the bd service is not the unit the design starts.
func doltComposeUnitFor(townRoot string, deps doltComposeDeps) (doltComposeUnit, error) {
	rig := filepath.Join(townRoot, "gastown")
	values, ok, err := imagevalues.LoadIn(rig)
	if err != nil {
		return doltComposeUnit{}, fmt.Errorf("reading %s: %w", imagevalues.PathIn(rig), err)
	}
	if !ok {
		return doltComposeUnit{}, fmt.Errorf("no %s: the compose unit takes its bd image from that file, so gt refuses to run it", imagevalues.PathIn(rig))
	}
	if values.Bd == "" {
		return doltComposeUnit{}, fmt.Errorf("%s has no bd key: the compose unit would start without the bd service, so gt refuses to run it", imagevalues.PathIn(rig))
	}
	backup, err := deps.backupRoot()
	if err != nil {
		return doltComposeUnit{}, fmt.Errorf("backup directory: %w", err)
	}
	dataDir := doltserver.DefaultConfig(townRoot).DataDir
	return doltComposeUnit{
		file:       filepath.Join(rig, doltComposeRelPath),
		dataDir:    dataDir,
		backupDir:  backup,
		configPath: filepath.Join(dataDir, "config.yaml"),
		bdImage:    values.Bd,
		tokenDir:   doctor.DataUnitTokenDir(townRoot),
	}, nil
}

// doltComposeStart brings the compose unit up, with bd started after Dolt is
// healthy. It refuses while the Dolt pause marker holds, as the old start does,
// and while the managed config file is absent: compose would bind-mount a
// missing file into the container as a directory.
func doltComposeStart(townRoot string, deps doltComposeDeps) error {
	if p := doltpause.Current(townRoot, time.Now()); p != nil {
		return &doltpause.Error{Marker: *p}
	}
	u, err := doltComposeUnitFor(townRoot, deps)
	if err != nil {
		return err
	}
	if _, err := os.Stat(u.configPath); err != nil {
		return fmt.Errorf("the Dolt config %s is not readable (%w): the compose unit mounts it, and a missing file would be created as a directory", u.configPath, err)
	}
	if _, err := deps.run(u.argv("up", "-d", "--wait", "--wait-timeout", doltComposeWaitSeconds), u.env(deps.environ())); err != nil {
		return fmt.Errorf("docker compose up for %s: %w", doltComposeProject, err)
	}
	fmt.Printf("%s Dolt compose unit %s is up (bd image %s)\n",
		style.Bold.Render("✓"), doltComposeProject, u.bdImage)
	return nil
}

// doltComposeStop stops the compose unit's services. The containers and the
// network stay, so a later start is the same unit.
func doltComposeStop(townRoot string, deps doltComposeDeps) error {
	u, err := doltComposeUnitFor(townRoot, deps)
	if err != nil {
		return err
	}
	if _, err := deps.run(u.argv("stop"), u.env(deps.environ())); err != nil {
		return fmt.Errorf("docker compose stop for %s: %w", doltComposeProject, err)
	}
	fmt.Printf("%s Dolt compose unit %s stopped\n", style.Bold.Render("✓"), doltComposeProject)
	return nil
}

// doltComposeStatus writes the state of each service in the compose unit to w.
func doltComposeStatus(townRoot string, deps doltComposeDeps, w io.Writer) error {
	u, err := doltComposeUnitFor(townRoot, deps)
	if err != nil {
		return err
	}
	out, err := deps.run(u.argv("ps", "--all", "--format", doctor.DataUnitPsFormat), u.env(deps.environ()))
	if err != nil {
		return fmt.Errorf("docker compose ps for %s: %w", doltComposeProject, err)
	}
	if _, err := fmt.Fprintf(w, "Dolt compose unit %s:\n%s", doltComposeProject, out); err != nil {
		return fmt.Errorf("writing the status: %w", err)
	}
	return nil
}

// doltComposePsFor returns the status reader the data unit check runs: docker
// compose ps for the unit, with the unit's own argv and environment, so the
// check reads the project and file that gt dolt start brings up.
func doltComposePsFor(deps doltComposeDeps) func(townRoot string) ([]byte, error) {
	return func(townRoot string) ([]byte, error) {
		u, err := doltComposeUnitFor(townRoot, deps)
		if err != nil {
			return nil, err
		}
		out, err := deps.run(u.argv("ps", "--all", "--format", doctor.DataUnitPsFormat), u.env(deps.environ()))
		if err != nil {
			return nil, fmt.Errorf("docker compose ps for %s: %w", doltComposeProject, err)
		}
		return out, nil
	}
}

// registerDataUnitCheck registers the data unit check on d, reading the unit with
// deps. It is opt-in by name, like mac-crew-hooks: named in names, it reads the
// compose unit through docker; otherwise it reports SKIPPED and calls nothing.
func registerDataUnitCheck(d *doctor.Doctor, names []string, deps doltComposeDeps) {
	d.Register(doctor.NewDataUnitCheck(slices.Contains(names, doctor.DataUnitCheckName), doltComposePsFor(deps)))
}
