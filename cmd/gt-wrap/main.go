// Command gt-wrap is installed once under the names gt, bd, om and hm and runs
// the tool it was invoked as inside the daemon image. Docker is the target
// today; the target is a config setting so a kubectl target can join it later.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/term"

	"github.com/steveyegge/gastown/internal/gtwrap"
)

const (
	// defaultTarget is the target an absent config file names.
	defaultTarget = "docker"
	// doctorFlag asks the wrapper to report its ladder instead of running the
	// tool. It is the wrapper's own flag, read only as the first argument, so a
	// tool's own arguments are never taken for it.
	doctorFlag = "--doctor"
	// townMarker is the file that marks a town root.
	townMarker = "mayor/town.json"
	// exitFailure is the wrapper's own refusal: the tool never ran, so there is
	// no tool status to pass through.
	exitFailure = 1
	// cutoverName is the name the binary runs its own commands under. The four
	// tool names are taken, so the operator runs gt-wrap install.
	cutoverName = "gt-wrap"
	installCmd  = "install"
	rollbackCmd = "rollback"
)

// target is what the wrapper needs from a target: the ladder that runs a call
// (refusing with the doctor message when no rung holds) and the doctor that
// reports the ladder without running anything.
type target interface {
	Launch(gtwrap.Call) (int, error)
	Doctor() (gtwrap.Diagnosis, error)
}

// surface is the process a run reads. A test supplies its own, so the wiring is
// driven with a fake target and a temp town instead of the operator's.
type surface struct {
	// getenv reads HOME, which names the config file, and GT_TOWN_ROOT.
	getenv func(string) string
	// environ is the environment the plan filters and forwards.
	environ  func() []string
	getwd    func() (string, error)
	readFile func(string) ([]byte, error)
	// stat reports whether a town marker exists, and refuses an unreadable one.
	stat func(string) (os.FileInfo, error)
	// stdinTTY says stdin is a terminal, so the container gets a terminal.
	stdinTTY       bool
	stdout, stderr io.Writer
	// newDocker builds the docker target for a town.
	newDocker func(town string) target
	// executable is the wrapper binary install copies into each tool's place.
	executable func() (string, error)
	// files is the filesystem the cutover changes; nil uses the system's.
	files gtwrap.Files
	// newDoctor builds install's doctor for a town.
	newDoctor func(town string) gtwrap.Doctor
}

func main() {
	os.Exit(run(os.Args, osSurface()))
}

// osSurface is the real process.
func osSurface() surface {
	return surface{
		getenv:   os.Getenv,
		environ:  os.Environ,
		getwd:    os.Getwd,
		readFile: os.ReadFile,
		stat:     os.Stat,
		stdinTTY: term.IsTerminal(int(os.Stdin.Fd())),
		stdout:   os.Stdout,
		stderr:   os.Stderr,
		newDocker: func(town string) target {
			return gtwrap.Docker{Town: town, Shell: gtwrap.OSShell{}, ReadFile: os.ReadFile, Stat: os.Stat}
		},
		executable: os.Executable,
		newDoctor: func(town string) gtwrap.Doctor {
			return cutoverDoctor{town: town, shell: gtwrap.OSShell{}, read: os.ReadFile, report: os.Stderr}
		},
	}
}

// run is main with its process injected. argv[0] is the installed name: the
// four tools run the tool, gt-wrap runs the cutover commands, and --doctor asks
// either for the ladder a call would take.
func run(argv []string, s surface) int {
	name := filepath.Base(argv[0])
	diagnose := len(argv) > 1 && argv[1] == doctorFlag
	if name == cutoverName && !diagnose {
		return cutover(argv[1:], s)
	}
	targetName, err := configTarget(s)
	if err != nil {
		return fail(s.stderr, err)
	}
	args := argv[1:]
	town, err := findTown(s)
	if err != nil {
		// --doctor still answers when there is no town: a diagnosis of down with
		// the town error as its reason, rather than a refusal with no report.
		if diagnose {
			return reportMissingTown(s.stdout, err)
		}
		return fail(s.stderr, err)
	}
	t, err := openTarget(targetName, town, s)
	if err != nil {
		return fail(s.stderr, err)
	}
	if diagnose {
		return doctor(t, s.stdout, s.stderr)
	}
	dir, err := s.getwd()
	if err != nil {
		return fail(s.stderr, fmt.Errorf("gtwrap: get the working directory to run the tool in: %w", err))
	}
	call, err := gtwrap.Plan(name, args, s.environ(), dir, s.stdinTTY)
	if err != nil {
		return fail(s.stderr, err)
	}
	exit, err := t.Launch(call)
	if err != nil {
		fmt.Fprintln(s.stderr, err)
		if exit == 0 {
			exit = exitFailure
		}
	}
	return exit
}

// configTarget reads the target the config file names, and docker when the file
// is absent. A file that cannot be read or parsed refuses: a broken setting must
// not be read as its default.
func configTarget(s surface) (string, error) {
	home := s.getenv("HOME")
	if home == "" {
		return "", errors.New("gtwrap: HOME is not set, so the target config file cannot be located")
	}
	path := filepath.Join(home, ".config", "gt", "wrap.json")
	data, err := s.readFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return defaultTarget, nil
	}
	if err != nil {
		return "", fmt.Errorf("gtwrap: read the target config file %s: %w", path, err)
	}
	var cfg struct {
		Target string `json:"target"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("gtwrap: parse the target config file %s: %w", path, err)
	}
	if cfg.Target == "" {
		return "", fmt.Errorf("gtwrap: the target config file %s names no target; write {\"target\": %q} there, or delete the file to use %q", path, defaultTarget, defaultTarget)
	}
	return cfg.Target, nil
}

// openTarget builds the target the config names. An unknown name refuses rather
// than falling back: a guessed target would run the tool somewhere the operator
// did not choose.
func openTarget(name, town string, s surface) (target, error) {
	switch name {
	case "docker":
		return s.newDocker(town), nil
	}
	return nil, fmt.Errorf("gtwrap: the target config names %q; the targets are docker", name)
}

// findTown is the town root the wrapper reads the compose file from and mounts:
// GT_TOWN_ROOT when it names a town, else the first town above the working
// directory, else $HOME/gt, the host's default town path. A marker that cannot
// be read refuses rather than being walked past, since a skipped ancestor could
// resolve a different town.
func findTown(s surface) (string, error) {
	if root := s.getenv("GT_TOWN_ROOT"); root != "" {
		ok, err := s.hasTownMarker(root)
		if err != nil {
			return "", fmt.Errorf("gtwrap: look for %s under GT_TOWN_ROOT %s: %w", townMarker, root, err)
		}
		if ok {
			return root, nil
		}
		return "", fmt.Errorf("gtwrap: GT_TOWN_ROOT is %q, which holds no %s", root, townMarker)
	}
	dir, err := s.getwd()
	if err != nil {
		return "", fmt.Errorf("gtwrap: get the working directory to find the town: %w", err)
	}
	start := dir
	for {
		ok, err := s.hasTownMarker(dir)
		if err != nil {
			return "", fmt.Errorf("gtwrap: look for %s under %s: %w", townMarker, dir, err)
		}
		if ok {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if home := s.getenv("HOME"); home != "" {
		root := filepath.Join(home, "gt")
		ok, err := s.hasTownMarker(root)
		if err != nil {
			return "", fmt.Errorf("gtwrap: look for %s under %s: %w", townMarker, root, err)
		}
		if ok {
			return root, nil
		}
	}
	return "", fmt.Errorf("gtwrap: no town root above %s and GT_TOWN_ROOT is unset; run the tool from a town or set GT_TOWN_ROOT", start)
}

// hasTownMarker reports whether dir is a town root.
func (s surface) hasTownMarker(dir string) (bool, error) {
	_, err := s.stat(filepath.Join(dir, townMarker))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, err
	}
}

// doctor prints the rung, container, image and reason --doctor asks for, and
// exits 0 when a rung holds and 1 when it is down or the diagnosis cannot be
// made.
func doctor(t target, stdout, stderr io.Writer) int {
	diag, err := t.Doctor()
	if err != nil {
		// The target's error already names the wrapper, so it is printed as
		// given rather than wrapped a second time.
		return fail(stderr, err)
	}
	return report(stdout, diag)
}

// reportMissingTown is --doctor's answer when no town root is found: down, with
// the town error as the reason. The container and the image are read from the
// compose file under the town, so with no town there is neither to name.
func reportMissingTown(stdout io.Writer, err error) int {
	return report(stdout, gtwrap.Diagnosis{
		Rung:      gtwrap.RungDown,
		Container: "unknown",
		Image:     "unknown",
		Reason:    err.Error(),
	})
}

// report prints a diagnosis in the doctor's four-line form and returns the
// doctor's status for it.
func report(stdout io.Writer, diag gtwrap.Diagnosis) int {
	fmt.Fprintf(stdout, "rung: %s\ncontainer: %s\nimage: %s\nreason: %s\n", diag.Rung, diag.Container, diag.Image, diag.Reason)
	if diag.Rung == gtwrap.RungDown {
		return exitFailure
	}
	return 0
}

// fail reports a refusal and returns the wrapper's failure status.
func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, err)
	return exitFailure
}
