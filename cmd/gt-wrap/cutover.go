package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/steveyegge/gastown/internal/gtwrap"
)

// cutover runs gt-wrap's own commands. They are never tool arguments: `gt
// install` still runs gt inside the daemon image.
func cutover(args []string, s surface) int {
	if len(args) == 0 {
		return fail(s.stderr, errors.New("gtwrap: gt-wrap install puts the wrapper in place and gt-wrap rollback takes it out"))
	}
	switch args[0] {
	case installCmd:
		return install(args[1:], s)
	case rollbackCmd:
		return rollback(args[1:], s)
	}
	return fail(s.stderr, fmt.Errorf("gtwrap: %q is not a gt-wrap command; the commands are install and rollback", args[0]))
}

// install replaces the tools install names with the wrapper. It is
// operator-gated: the command lands, the operator runs it.
func install(args []string, s surface) int {
	options, err := parseCutover(installCmd, args)
	if err != nil {
		return fail(s.stderr, err)
	}
	town, err := findTown(s)
	if err != nil {
		return fail(s.stderr, err)
	}
	source, err := s.executable()
	if err != nil {
		return fail(s.stderr, fmt.Errorf("gtwrap: find the wrapper binary install copies: %w", err))
	}
	err = gtwrap.Install(gtwrap.InstallConfig{
		Source:  source,
		Stamp:   options.stamp,
		Dir:     options.dir,
		Targets: options.targets,
		Files:   s.files,
		Doctor:  s.newDoctor(town),
		Out:     s.stdout,
	})
	if err != nil {
		return fail(s.stderr, err)
	}
	return 0
}

// rollback restores the files an install kept. It changes no container, so it
// needs no town and no doctor.
func rollback(args []string, s surface) int {
	options, err := parseCutover(rollbackCmd, args)
	if err != nil {
		return fail(s.stderr, err)
	}
	err = gtwrap.Rollback(gtwrap.RollbackConfig{
		Stamp:   options.stamp,
		Dir:     options.dir,
		Targets: options.targets,
		Files:   s.files,
		Out:     s.stdout,
	})
	if err != nil {
		return fail(s.stderr, err)
	}
	return 0
}

// cutoverOptions is what install or rollback was asked to change.
type cutoverOptions struct {
	stamp   string
	dir     string
	targets []gtwrap.Target
}

// parseCutover reads the flags both commands share. A repeated --target is read
// here; the library checks each name and path before anything changes.
func parseCutover(command string, args []string) (cutoverOptions, error) {
	set := flag.NewFlagSet(cutoverName+" "+command, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	var options cutoverOptions
	var named targetList
	set.StringVar(&options.stamp, "stamp", "", "the stamp the dated files and the rollback use (yyyymmdd-hhmm)")
	set.StringVar(&options.dir, "dir", "", "the directory holding the four tools")
	set.Var(&named, "target", "name=path; the file one tool replaces; repeatable")
	if err := set.Parse(args); err != nil {
		return options, fmt.Errorf("gtwrap: %s: %w", command, err)
	}
	if set.NArg() > 0 {
		return options, fmt.Errorf("gtwrap: %s: unexpected argument %q", command, set.Arg(0))
	}
	if options.dir != "" && len(named) > 0 {
		return options, fmt.Errorf("gtwrap: %s names both --dir and --target; name one", command)
	}
	for _, value := range named {
		target, err := parseTarget(value)
		if err != nil {
			return options, err
		}
		options.targets = append(options.targets, target)
	}
	return options, nil
}

// targetList collects repeated --target values in the order they are given.
type targetList []string

func (t *targetList) String() string { return strings.Join(*t, " ") }

func (t *targetList) Set(value string) error {
	*t = append(*t, value)
	return nil
}

// parseTarget reads one --target value, splitting at the first =, so a path
// holding one keeps it.
func parseTarget(value string) (gtwrap.Target, error) {
	name, path, ok := strings.Cut(value, "=")
	if !ok || name == "" || path == "" {
		return gtwrap.Target{}, fmt.Errorf("gtwrap: --target %q is not name=path", value)
	}
	return gtwrap.Target{Name: name, Path: path}, nil
}

// cutoverDoctor is install's doctor on the host: the docker ladder's answer on
// whether a tool answers in the image, and the placed file's own --doctor. The
// placed file's report goes to the operator's stderr, so install's stdout lists
// only what it changed.
type cutoverDoctor struct {
	town   string
	shell  gtwrap.Shell
	read   func(string) ([]byte, error)
	report io.Writer
	// run runs argv as a process; nil runs it on the host. Tests inject it so
	// the doctor's invocation is checked without starting a process.
	run func(argv ...string) error
}

// InImage runs the wrapper's tool check through the docker ladder.
func (d cutoverDoctor) InImage(name string) error {
	return gtwrap.Docker{Town: d.town, Shell: d.shell, ReadFile: d.read}.ToolExists(name)
}

// Installed runs the file install placed as the tool it replaced.
func (d cutoverDoctor) Installed(name, path string) error {
	run := d.run
	if run == nil {
		run = d.process
	}
	if err := run(path, doctorFlag); err != nil {
		return fmt.Errorf("%s --doctor at %s: %w", name, path, err)
	}
	return nil
}

// process runs argv on the host, sending its output to the report writer when
// one is set.
func (d cutoverDoctor) process(argv ...string) error {
	command := exec.Command(argv[0], argv[1:]...) //nolint:gosec // G204: argv[0] is the tool file install just placed
	if d.report != nil {
		command.Stdout, command.Stderr = d.report, d.report
	}
	return command.Run()
}
