// Command gt-wrap is installed once under the names gt, bd, om and hm and runs
// the tool it was invoked as inside the daemon image. The target is a config
// setting: docker runs the tool in the daemon container, and kubernetes execs into
// the daemon pod of a Deployment.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/term"

	"github.com/steveyegge/gastown/internal/gtwrap"
)

const (
	// defaultTarget is the target an absent config file names.
	defaultTarget = "docker"
	// kubernetesTarget is the target that execs into the daemon pod.
	kubernetesTarget = "kubernetes"
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
	// newKubectl builds the kubernetes target for a town, reaching the Deployment
	// named by kube, and tool is the name the wrapper was invoked as.
	newKubectl func(town string, kube kubeConfig, tool string) target
	// executable is the wrapper binary install copies into each tool's place.
	executable func() (string, error)
	// files is the filesystem the cutover changes; nil uses the system's.
	files gtwrap.Files
	// newDoctor builds install's doctor for a town.
	newDoctor func(town string) gtwrap.Doctor
	// native runs the calls the config names for the Mac; nil refuses them.
	native gtwrap.Process
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
		newKubectl: func(town string, kube kubeConfig, tool string) target {
			return gtwrap.Kubectl{
				Town:       town,
				Context:    kube.Context,
				Namespace:  kube.Namespace,
				Deployment: kube.Deployment,
				Container:  kube.Container,
				Tool:       tool,
				Shell:      gtwrap.OSShell{},
				LookPath:   exec.LookPath,
				Note:       os.Stderr,
			}
		},
		executable: os.Executable,
		newDoctor: func(town string) gtwrap.Doctor {
			return cutoverDoctor{town: town, shell: gtwrap.OSShell{}, read: os.ReadFile, report: os.Stderr}
		},
		native: gtwrap.OSProcess{},
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
	cfg, err := configTarget(s)
	if err != nil {
		return fail(s.stderr, err)
	}
	args := argv[1:]
	nat, native := cfg.Native[name]
	if native && diagnose {
		if status := reportNative(s.stdout, nat, s.stat); status != 0 {
			return status
		}
	}
	if native && nat.Covers(args) {
		return runNative(nat, args, s)
	}
	town, err := townFor(cfg, s)
	if err != nil {
		// --doctor still answers when there is no town: a diagnosis of down with
		// the town error as its reason, rather than a refusal with no report.
		if diagnose {
			return reportMissingTown(s.stdout, err)
		}
		return fail(s.stderr, err)
	}
	t, err := openTarget(cfg, town, name, s)
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

// targetConfig is what the config file names: the target, and for the kubernetes
// target the Deployment it reaches.
type targetConfig struct {
	Target string
	Kube   kubeConfig
	// Native holds the tools the config runs on the Mac, by tool name.
	Native map[string]gtwrap.Native
}

// kubeConfig is the kubernetes target's fields in the config file. Only that
// target reads them, and each must be named.
type kubeConfig struct {
	Context    string `json:"context"`
	Namespace  string `json:"namespace"`
	Deployment string `json:"deployment"`
	Container  string `json:"container"`
}

// configTarget reads the target the config file names, and docker when the file
// is absent. A file that cannot be read or parsed refuses: a broken setting must
// not be read as its default. A kubernetes target refuses when a field it needs is
// missing, naming the field.
func configTarget(s surface) (targetConfig, error) {
	home := s.getenv("HOME")
	if home == "" {
		return targetConfig{}, errors.New("gtwrap: HOME is not set, so the target config file cannot be located")
	}
	path := filepath.Join(home, ".config", "gt", "wrap.json")
	data, err := s.readFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return targetConfig{Target: defaultTarget}, nil
	}
	if err != nil {
		return targetConfig{}, fmt.Errorf("gtwrap: read the target config file %s: %w", path, err)
	}
	var cfg struct {
		Target string                 `json:"target"`
		Native map[string]nativeEntry `json:"native"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return targetConfig{}, fmt.Errorf("gtwrap: parse the target config file %s: %w", path, err)
	}
	if cfg.Target == "" {
		return targetConfig{}, fmt.Errorf("gtwrap: the target config file %s names no target; write {\"target\": %q} there, or delete the file to use %q", path, defaultTarget, defaultTarget)
	}
	native, err := nativeTools(cfg.Native, path)
	if err != nil {
		return targetConfig{}, err
	}
	out := targetConfig{Target: cfg.Target, Native: native}
	if cfg.Target != kubernetesTarget {
		return out, nil
	}
	if err := json.Unmarshal(data, &out.Kube); err != nil {
		return targetConfig{}, fmt.Errorf("gtwrap: parse the kubernetes target fields in %s: %w", path, err)
	}
	for _, field := range []struct{ name, value string }{
		{"context", out.Kube.Context},
		{"namespace", out.Kube.Namespace},
		{"deployment", out.Kube.Deployment},
		{"container", out.Kube.Container},
	} {
		if field.value == "" {
			return targetConfig{}, fmt.Errorf("gtwrap: the target config file %s names the kubernetes target but no %q; add %q there", path, field.name, field.name)
		}
	}
	return out, nil
}

// openTarget builds the target the config names. An unknown name refuses rather
// than falling back: a guessed target would run the tool somewhere the operator
// did not choose. tool is the name the wrapper was invoked as.
func openTarget(cfg targetConfig, town, tool string, s surface) (target, error) {
	switch cfg.Target {
	case defaultTarget:
		return s.newDocker(town), nil
	case kubernetesTarget:
		return s.newKubectl(town, cfg.Kube, tool), nil
	}
	return nil, fmt.Errorf("gtwrap: the target config names %q; the targets are docker and kubernetes", cfg.Target)
}

// townFor is the town root the target is built for. The docker target runs from
// the town's compose file, so it finds the town. The kubernetes target only maps
// the working directory onto the pod, and the path it maps under is $HOME/gt. That
// needs no town marker, and a directory outside it is mapped with a note, not
// refused, so the target runs from any directory.
func townFor(cfg targetConfig, s surface) (string, error) {
	if cfg.Target != kubernetesTarget {
		return findTown(s)
	}
	home := s.getenv("HOME")
	if home == "" {
		return "", errors.New("gtwrap: HOME is not set, so the kubernetes target cannot map the working directory")
	}
	return filepath.Join(home, "gt"), nil
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
