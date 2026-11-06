package gtwrap

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// preWrapInfix separates a tool's path from the stamp its replaced file
	// keeps.
	preWrapInfix = ".pre-wrap-"
	// wrapperInfix separates a tool's path from the stamp the wrapper it
	// replaced keeps when rollback moves it aside.
	wrapperInfix = ".wrapper-"
	// stampLayout is the stamp form install and rollback share: the day and
	// minute the cutover ran.
	stampLayout = "20060102-1504"
)

// cutoverPrechecks is the operator's list of conditions this command cannot
// check for itself. Install prints it, and
// docs/design/gt-in-container-cutover.md repeats each line word for word;
// TestCutoverRunbookNamesEveryPrecheck holds the two lists equal, so an edit to
// one alone fails the test (gt-yjzepk.12).
var cutoverPrechecks = []string{
	"the dry run against every Mac caller passed",
	"the compose environment names the Dolt host, so bd in the container reaches Dolt",
	"the .beads mount is read-write in the daemon container",
	"docker-exec load was measured with the monitors running",
}

// Target is one tool the cutover replaces: the name the wrapper answers as and
// the file it replaces.
type Target struct {
	Name string
	Path string
}

// Files is the filesystem the cutover reads and changes. The command uses the
// operating system's; a test injects one that records every operation.
type Files interface {
	Stat(path string) (fs.FileInfo, error)
	ReadFile(path string) ([]byte, error)
	CreateTemp(dir, pattern string) (TempFile, error)
	Rename(oldpath, newpath string) error
	Remove(path string) error
}

// TempFile is the new file the wrapper is written to before it is renamed into
// place. Creating it new is what keeps the copy off the path a running binary
// holds (gt-yjzepk.12).
type TempFile interface {
	io.Writer
	Name() string
	Chmod(mode fs.FileMode) error
	Close() error
}

// Doctor answers install's two questions: whether a tool answers inside the
// image the wrapper reaches, before anything changes, and whether the file
// install placed answers --doctor as the tool it replaced.
type Doctor interface {
	// InImage reports why name does not answer inside the image, or nil when it
	// does.
	InImage(name string) error
	// Installed runs path as name with --doctor and reports a failure.
	Installed(name, path string) error
}

// osFiles is Files on the operating system.
type osFiles struct{}

func (osFiles) Stat(path string) (fs.FileInfo, error)            { return os.Stat(path) }
func (osFiles) ReadFile(path string) ([]byte, error)             { return os.ReadFile(path) }
func (osFiles) CreateTemp(dir, pattern string) (TempFile, error) { return os.CreateTemp(dir, pattern) }
func (osFiles) Rename(oldpath, newpath string) error             { return os.Rename(oldpath, newpath) }
func (osFiles) Remove(path string) error                         { return os.Remove(path) }

// InstallConfig is one cutover run.
type InstallConfig struct {
	// Source is the wrapper binary install copies into each tool's place.
	Source string
	// Stamp dates the files install keeps and names the rollback command.
	Stamp string
	// Dir holds every tool. Install replaces each of the four it finds there
	// and notes those it does not. Targets replaces Dir.
	Dir string
	// Targets names the tools and files when they live in different
	// directories.
	Targets []Target
	// Files is the filesystem; nil uses the operating system's.
	Files Files
	// Doctor checks the image before any change and each placed file after it.
	// A nil doctor refuses: nothing would verify the image.
	Doctor Doctor
	// Out is where install reports what it did; nil uses standard output.
	Out io.Writer
}

// Install replaces each target's file with the wrapper, keeping the old file
// under a dated name, and prints the rollback command and the dated files. It
// is transactional. Every target is checked and the wrapper is staged beside
// each one before any target is touched, so a refusal or a failed staging step
// changes nothing. The tools are then placed one at a time, each checked with
// its --doctor. A failed step puts back every tool install changed, in reverse
// order, removes what is still staged, and prints the outcome of each tool.
func Install(cfg InstallConfig) error {
	if cfg.Doctor == nil {
		return errors.New("gtwrap: install has no doctor, so the image and each placed file would go unchecked")
	}
	if err := checkStamp(cfg.Stamp); err != nil {
		return err
	}
	if cfg.Source == "" {
		return errors.New("gtwrap: install has no wrapper binary to place")
	}
	files := cfg.files()
	out := cfg.out()

	targets, skipped, err := resolveTargets(files, cfg.Dir, cfg.Targets)
	if err != nil {
		return err
	}
	for _, t := range skipped {
		fmt.Fprintf(out, "skipped %s: no file at %s\n", t.Name, t.Path)
	}
	if err := preflight(files, cfg.Doctor, cfg.Stamp, targets); err != nil {
		return err
	}
	data, mode, err := readWrapper(files, cfg.Source)
	if err != nil {
		return err
	}

	printPrechecks(out)
	staged, err := stageAll(files, data, mode, targets)
	if err != nil {
		return err
	}
	return placeAll(files, cfg.Doctor, out, cfg.Stamp, targets, staged)
}

// RollbackConfig is one rollback run.
type RollbackConfig struct {
	// Stamp names the dated files to restore and the wrapper files to keep
	// aside.
	Stamp string
	// Dir holds every tool; Targets replaces it, as for Install.
	Dir string
	// Targets names the tools and files to restore.
	Targets []Target
	// Files is the filesystem; nil uses the operating system's.
	Files Files
	// Out is where rollback reports what it did; nil uses standard output.
	Out io.Writer
}

// Rollback restores every target's dated file and keeps the wrapper it replaces
// aside under the same stamp. A target whose dated file is missing or unusable
// is refused, naming it, and left as it is; the other targets are restored.
func Rollback(cfg RollbackConfig) error {
	if err := checkStamp(cfg.Stamp); err != nil {
		return err
	}
	files := cfg.files()
	out := cfg.out()

	targets, skipped, err := resolveTargets(files, cfg.Dir, cfg.Targets)
	if err != nil {
		return err
	}
	for _, t := range skipped {
		fmt.Fprintf(out, "skipped %s: no file at %s\n", t.Name, t.Path)
	}

	restored := 0
	var refused []string
	for _, t := range targets {
		if reason := restorable(files, t, cfg.Stamp); reason != "" {
			refused = append(refused, reason)
			continue
		}
		if err := restore(files, t, cfg.Stamp); err != nil {
			refused = append(refused, fmt.Sprintf("%s: %v", t.Name, err))
			continue
		}
		restored++
		fmt.Fprintf(out, "restored %s from %s; the wrapper is at %s\n", t.Path, dated(t, cfg.Stamp), aside(t, cfg.Stamp))
	}
	if len(refused) > 0 {
		return fmt.Errorf("gtwrap: rollback with stamp %s restored %d of %d tools and refused:\n  %s",
			cfg.Stamp, restored, len(targets), strings.Join(refused, "\n  "))
	}
	return nil
}

// resolveTargets is the files install or rollback changes: the named targets,
// else the four tools present in dir. A tool absent from dir is skipped and
// returned so the caller can note it; a named target is never skipped.
func resolveTargets(files Files, dir string, named []Target) (targets, skipped []Target, err error) {
	switch {
	case dir != "" && len(named) > 0:
		return nil, nil, errors.New("gtwrap: both a directory and targets are named; name one")
	case dir != "":
		for _, name := range tools {
			t := Target{Name: name, Path: filepath.Join(dir, name)}
			if _, err := files.Stat(t.Path); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					skipped = append(skipped, t)
					continue
				}
				return nil, nil, fmt.Errorf("gtwrap: look for %s: %w", t.Path, err)
			}
			targets = append(targets, t)
		}
		if len(targets) == 0 {
			return nil, nil, fmt.Errorf("gtwrap: %s holds none of %s, so there is nothing to change", dir, toolList())
		}
	case len(named) > 0:
		targets = append([]Target(nil), named...)
	default:
		return nil, nil, errors.New("gtwrap: no directory and no target is named; name one")
	}
	checked, err := checkTargets(targets)
	if err != nil {
		return nil, nil, err
	}
	return checked, skipped, nil
}

// checkTargets validates the tools and files a run names before anything is
// read or changed, and returns them with each path cleaned.
func checkTargets(targets []Target) ([]Target, error) {
	names := map[string]bool{}
	paths := map[string]bool{}
	checked := make([]Target, 0, len(targets))
	for _, t := range targets {
		if _, err := toolFor(t.Name); err != nil {
			return nil, fmt.Errorf("gtwrap: %q is not one of the tools the wrapper answers to (%s)", t.Name, toolList())
		}
		if t.Path == "" {
			return nil, fmt.Errorf("gtwrap: %s names no file", t.Name)
		}
		path := filepath.Clean(t.Path)
		if names[t.Name] {
			return nil, fmt.Errorf("gtwrap: %s is named twice", t.Name)
		}
		if paths[path] {
			return nil, fmt.Errorf("gtwrap: %s is named twice", path)
		}
		names[t.Name], paths[path] = true, true
		checked = append(checked, Target{Name: t.Name, Path: path})
	}
	return checked, nil
}

// preflight refuses when a change cannot be made safely: every target must hold
// a regular file to replace, no dated file may exist, no target's path may be
// another target's dated or aside name, and every tool must answer in the image.
func preflight(files Files, doctor Doctor, stamp string, targets []Target) error {
	paths := map[string]bool{}
	for _, t := range targets {
		paths[filepath.Clean(t.Path)] = true
	}
	for _, t := range targets {
		info, err := files.Stat(t.Path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("gtwrap: %s does not exist, so there is no file for %s to replace", t.Path, t.Name)
		case err != nil:
			return fmt.Errorf("gtwrap: read %s: %w", t.Path, err)
		case info.IsDir():
			return fmt.Errorf("gtwrap: %s is a directory, not a file", t.Path)
		case !info.Mode().IsRegular():
			return fmt.Errorf("gtwrap: %s is not a regular file or a symlink to one", t.Path)
		}
		for _, name := range []string{dated(t, stamp), aside(t, stamp)} {
			if paths[filepath.Clean(name)] {
				return fmt.Errorf("gtwrap: %s would be the file another target replaces", name)
			}
		}
		if _, err := files.Stat(dated(t, stamp)); err == nil {
			return fmt.Errorf("gtwrap: %s already exists, so a cutover with stamp %s already ran; name a new stamp", dated(t, stamp), stamp)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("gtwrap: look for %s: %w", dated(t, stamp), err)
		}
	}
	for _, t := range targets {
		if err := doctor.InImage(t.Name); err != nil {
			return fmt.Errorf("gtwrap: %s does not answer in the daemon image: %w", t.Name, err)
		}
	}
	return nil
}

// readWrapper reads the binary install copies and the mode it keeps.
func readWrapper(files Files, source string) ([]byte, fs.FileMode, error) {
	info, err := files.Stat(source)
	if err != nil {
		return nil, 0, fmt.Errorf("gtwrap: read the wrapper %s: %w", source, err)
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("gtwrap: the wrapper %s is not a regular file", source)
	}
	data, err := files.ReadFile(source)
	if err != nil {
		return nil, 0, fmt.Errorf("gtwrap: read the wrapper %s: %w", source, err)
	}
	return data, info.Mode().Perm(), nil
}

// stageAll writes the wrapper to a new file beside each target before any target
// is touched. A failure removes the files already staged, so it changes nothing.
func stageAll(files Files, data []byte, mode fs.FileMode, targets []Target) ([]string, error) {
	staged := make([]string, 0, len(targets))
	for _, t := range targets {
		path, err := stage(files, data, mode, t)
		if err != nil {
			return nil, unstage(files, staged, err)
		}
		staged = append(staged, path)
	}
	return staged, nil
}

// stage writes the wrapper to a new file in the target's own directory and
// returns its name. The file is new, so the copy never touches the path a
// running binary holds.
func stage(files Files, data []byte, mode fs.FileMode, t Target) (string, error) {
	temp, err := files.CreateTemp(filepath.Dir(t.Path), "."+t.Name+".gt-wrap-*")
	if err != nil {
		return "", fmt.Errorf("gtwrap: make a temporary file beside %s: %w", t.Path, err)
	}
	name := temp.Name()
	if _, err := temp.Write(data); err != nil {
		return "", dropStaged(files, temp, fmt.Errorf("gtwrap: write the wrapper beside %s: %w", t.Path, err))
	}
	if err := temp.Chmod(mode); err != nil {
		return "", dropStaged(files, temp, fmt.Errorf("gtwrap: set the wrapper's mode beside %s: %w", t.Path, err))
	}
	if err := temp.Close(); err != nil {
		return "", errors.Join(fmt.Errorf("gtwrap: finish the wrapper beside %s: %w", t.Path, err), removeTemp(files, name))
	}
	return name, nil
}

// dropStaged closes and removes a staged file a failed write or mode change
// left behind. The step's error is returned joined with any failure to do so.
func dropStaged(files Files, temp TempFile, cause error) error {
	return errors.Join(cause, temp.Close(), removeTemp(files, temp.Name()))
}

// unstage removes the files staged before a staging step failed, so nothing is
// left beside a target. The cause is returned first, joined with any failure to
// remove a file.
func unstage(files Files, staged []string, cause error) error {
	errs := []error{cause}
	for _, path := range staged {
		if err := removeTemp(files, path); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// removeTemp removes a temporary file a failed step left behind. It returns nil
// when the file is gone.
func removeTemp(files Files, path string) error {
	if err := files.Remove(path); err != nil {
		return fmt.Errorf("gtwrap: remove the temporary file %s: %w", path, err)
	}
	return nil
}

// installRun is one install's state once the wrapper is staged, so the steps
// that stop it share it.
type installRun struct {
	files   Files
	out     io.Writer
	stamp   string
	targets []Target
	// staged is the staged file of each target, in the order of targets; an
	// entry is "" once that target's staged file has been placed.
	staged []string
	// placed is the targets install has placed, in order.
	placed []Target
}

// placeAll places each target's staged file in turn, and checks each placed file
// with its --doctor. The first step that fails stops the run.
func placeAll(files Files, doctor Doctor, out io.Writer, stamp string, targets []Target, staged []string) error {
	run := &installRun{files: files, out: out, stamp: stamp, targets: targets, staged: staged}
	for i, t := range targets {
		if err := swapIn(files, t, staged[i], stamp); err != nil {
			return run.stop(t, err)
		}
		staged[i] = ""
		run.placed = append(run.placed, t)
		if err := doctor.Installed(t.Name, t.Path); err != nil {
			return run.stop(t, err)
		}
	}
	for _, t := range targets {
		fmt.Fprintf(out, "placed %s at %s; old file %s\n", t.Name, t.Path, dated(t, stamp))
	}
	printRollback(out, stamp, targets)
	return nil
}

// swapIn puts the staged file in place of the target. The old file is renamed to
// its dated name first, so the path is empty when the staged file is renamed
// onto it: os.Rename would replace an existing destination on POSIX, and this
// never does. The dated name is only a rename target after preflight has found
// it absent, so a backup is never replaced.
func swapIn(files Files, t Target, staged, stamp string) error {
	kept := dated(t, stamp)
	if err := files.Rename(t.Path, kept); err != nil {
		return fmt.Errorf("gtwrap: keep %s as %s: %w", t.Path, kept, err)
	}
	if err := files.Rename(staged, t.Path); err != nil {
		return fmt.Errorf("gtwrap: rename the wrapper into place at %s: %w", t.Path, err)
	}
	return nil
}

// stop undoes the install after failed, the target whose step returned cause. It
// puts back every target install changed, in reverse order: the targets already
// placed, and failed when its old file had moved aside. It removes what is still
// staged, and prints the outcome of each target, one line each, with the manual
// command for any target it could not put back.
func (r *installRun) stop(failed Target, cause error) error {
	changed := append([]Target(nil), r.placed...)
	if !containsTarget(r.placed, failed) {
		if _, err := r.files.Stat(dated(failed, r.stamp)); !errors.Is(err, fs.ErrNotExist) {
			changed = append(changed, failed)
		}
	}

	var problems []error
	for _, path := range r.staged {
		if path != "" {
			if err := removeTemp(r.files, path); err != nil {
				problems = append(problems, err)
			}
		}
	}

	outcome := map[string]string{}
	var unrestored []Target
	for i := len(changed) - 1; i >= 0; i-- {
		t := changed[i]
		if err := putBack(r.files, t, r.stamp); err != nil {
			unrestored = append(unrestored, t)
			outcome[t.Name] = fmt.Sprintf("could not restore %s at %s: %v; put the original file back by hand: mv -f %s %s",
				t.Name, t.Path, err, shellQuote(dated(t, r.stamp)), shellQuote(t.Path))
			continue
		}
		outcome[t.Name] = fmt.Sprintf("restored %s at %s from %s", t.Name, t.Path, dated(t, r.stamp))
	}

	for _, t := range r.targets {
		line, ok := outcome[t.Name]
		switch {
		case t.Name == failed.Name && ok:
			fmt.Fprintf(r.out, "failed %s at %s: %v; %s\n", t.Name, t.Path, cause, line)
		case t.Name == failed.Name:
			fmt.Fprintf(r.out, "failed %s at %s: %v; left as it was\n", t.Name, t.Path, cause)
		case ok:
			fmt.Fprintln(r.out, line)
		default:
			fmt.Fprintf(r.out, "untouched %s at %s\n", t.Name, t.Path)
		}
	}
	if len(unrestored) > 0 {
		printRollback(r.out, r.stamp, unrestored)
	}

	err := fmt.Errorf("gtwrap: install stopped at %s: %w", failed.Name, cause)
	if len(unrestored) == 0 {
		err = fmt.Errorf("%w; every tool it changed was put back", err)
	} else {
		err = fmt.Errorf("%w; %s could not be put back, see the manual commands above", err, toolNames(unrestored))
	}
	return errors.Join(append([]error{err}, problems...)...)
}

// putBack puts the old file back over the wrapper at the target: the dated file
// is renamed onto the path. It replaces the wrapper install placed there, and is
// the only rename install makes onto a path that holds a file.
func putBack(files Files, t Target, stamp string) error {
	kept := dated(t, stamp)
	if err := files.Rename(kept, t.Path); err != nil {
		return fmt.Errorf("gtwrap: rename %s back over %s: %w", kept, t.Path, err)
	}
	return nil
}

// containsTarget reports whether targets names the tool t names.
func containsTarget(targets []Target, t Target) bool {
	for _, x := range targets {
		if x.Name == t.Name {
			return true
		}
	}
	return false
}

// toolNames is the tool names in targets, comma separated, for an error.
func toolNames(targets []Target) string {
	list := make([]string, 0, len(targets))
	for _, t := range targets {
		list = append(list, t.Name)
	}
	return strings.Join(list, ", ")
}

// restorable is rollback's per-tool refusal before any change is made: it names
// why the tool cannot be restored, or returns "" when it can. The wrapper at the
// target and the dated file must both be regular files, or symlinks to them: os.Stat
// follows a symlink, so a symlink is judged by what it points to.
func restorable(files Files, t Target, stamp string) string {
	wrapper, err := files.Stat(t.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Sprintf("%s: %s is missing, so there is no wrapper to keep aside", t.Name, t.Path)
	case err != nil:
		return fmt.Sprintf("%s: read %s: %v", t.Name, t.Path, err)
	case !wrapper.Mode().IsRegular():
		return fmt.Sprintf("%s: %s is %s, not a regular file or a symlink to one, so it was not moved aside", t.Name, t.Path, pathType(wrapper))
	}
	info, err := files.Stat(dated(t, stamp))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Sprintf("%s: %s does not exist, so %s was not restored", t.Name, dated(t, stamp), t.Path)
	case err != nil:
		return fmt.Sprintf("%s: read %s: %v", t.Name, dated(t, stamp), err)
	case !info.Mode().IsRegular():
		return fmt.Sprintf("%s: %s is %s, not a regular file or a symlink to one, so %s was not restored", t.Name, dated(t, stamp), pathType(info), t.Path)
	}
	if _, err := files.Stat(aside(t, stamp)); err == nil {
		return fmt.Sprintf("%s: %s already exists, so %s was not restored", t.Name, aside(t, stamp), t.Path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Sprintf("%s: look for %s: %v", t.Name, aside(t, stamp), err)
	}
	return ""
}

// pathType names what a path holds, for a refusal that has to say why.
func pathType(info fs.FileInfo) string {
	mode := info.Mode()
	switch {
	case mode.IsDir():
		return "a directory"
	case mode&fs.ModeSocket != 0:
		return "a socket"
	case mode&fs.ModeNamedPipe != 0:
		return "a named pipe"
	case mode&fs.ModeDevice != 0:
		return "a device"
	case mode.IsRegular():
		return "a regular file"
	}
	return "a special file"
}

// restore moves the wrapper at the target aside and the dated file back. It
// puts the wrapper back when the dated file cannot be renamed into place, so a
// failed restore leaves the tool as it was.
func restore(files Files, t Target, stamp string) error {
	kept, moved := dated(t, stamp), aside(t, stamp)
	if err := files.Rename(t.Path, moved); err != nil {
		return fmt.Errorf("move %s aside: %w", t.Path, err)
	}
	if err := files.Rename(kept, t.Path); err != nil {
		if backErr := files.Rename(moved, t.Path); backErr != nil {
			return fmt.Errorf("rename %s back: %v; putting the wrapper back at %s also failed: %v", kept, err, t.Path, backErr)
		}
		return fmt.Errorf("rename %s back: %w", kept, err)
	}
	return nil
}

// checkStamp refuses a stamp that is not yyyymmdd-hhmm: the stamp is part of
// every dated and aside name, so anything else could name a path it should not.
func checkStamp(stamp string) error {
	if stamp == "" {
		return errors.New("gtwrap: no stamp is named; name one as yyyymmdd-hhmm")
	}
	if _, err := time.Parse(stampLayout, stamp); err != nil {
		return fmt.Errorf("gtwrap: stamp %q is not a day and minute as yyyymmdd-hhmm", stamp)
	}
	return nil
}

// dated is the name install keeps the file it replaces under.
func dated(t Target, stamp string) string { return t.Path + preWrapInfix + stamp }

// aside is the name rollback keeps the wrapper under.
func aside(t Target, stamp string) string { return t.Path + wrapperInfix + stamp }

// files is the filesystem a run changes: the injected one, else the operating
// system's.
func (cfg InstallConfig) files() Files {
	if cfg.Files != nil {
		return cfg.Files
	}
	return osFiles{}
}

func (cfg InstallConfig) out() io.Writer {
	if cfg.Out != nil {
		return cfg.Out
	}
	return os.Stdout
}

func (cfg RollbackConfig) files() Files {
	if cfg.Files != nil {
		return cfg.Files
	}
	return osFiles{}
}

func (cfg RollbackConfig) out() io.Writer {
	if cfg.Out != nil {
		return cfg.Out
	}
	return os.Stdout
}

// printPrechecks lists what the operator must confirm before the cutover, so an
// install run cannot be mistaken for the whole of the decision.
func printPrechecks(out io.Writer) {
	fmt.Fprintln(out, "pre-checks (docs/design/gt-in-container-cutover.md):")
	for _, check := range cutoverPrechecks {
		fmt.Fprintf(out, "  - %s\n", check)
	}
}

// printRollback prints the exact command that undoes this run and the files it
// kept, one per tool.
func printRollback(out io.Writer, stamp string, targets []Target) {
	fmt.Fprintf(out, "rollback: %s\n", rollbackCommand(stamp, targets))
	fmt.Fprintln(out, "dated files:")
	for _, t := range targets {
		fmt.Fprintf(out, "  %s\n", dated(t, stamp))
	}
}

// rollbackCommand is the gt-wrap rollback command that restores targets.
func rollbackCommand(stamp string, targets []Target) string {
	command := "gt-wrap rollback --stamp " + shellQuote(stamp)
	for _, t := range targets {
		command += " --target " + shellQuote(t.Name+"="+t.Path)
	}
	return command
}

// shellQuote renders s as one POSIX shell word, so a path holding a space or a
// quote can be pasted back into a shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
