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
// docs/design/gt-in-container-cutover.md repeats each line word for word, so
// the runbook and the command cannot drift (gt-yjzepk.12).
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
// refuses before any change when a precondition fails; when a placed file fails
// its doctor, it undoes that tool, stops, and leaves the earlier tools in place.
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
	placed := make([]Target, 0, len(targets))
	for _, t := range targets {
		if err := place(files, data, mode, t, cfg.Stamp); err != nil {
			return leftInstalled(out, cfg.Stamp, placed, err)
		}
		if err := cfg.Doctor.Installed(t.Name, t.Path); err != nil {
			return leftInstalled(out, cfg.Stamp, placed, stopped(t, placed, err, restore(files, t, cfg.Stamp)))
		}
		placed = append(placed, t)
		fmt.Fprintf(out, "installed %s at %s; old file %s\n", t.Name, t.Path, dated(t, cfg.Stamp))
	}
	printRollback(out, cfg.Stamp, placed)
	return nil
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

// place writes the wrapper to a new file in the target's own directory and
// renames it into place, after renaming the file it replaces to the dated name.
// The old file moves first, so the rename into place has no existing file to
// replace: os.Rename would overwrite one on POSIX.
func place(files Files, data []byte, mode fs.FileMode, t Target, stamp string) error {
	temp, err := files.CreateTemp(filepath.Dir(t.Path), "."+t.Name+".gt-wrap-*")
	if err != nil {
		return fmt.Errorf("gtwrap: make a temporary file beside %s: %w", t.Path, err)
	}
	tempPath := temp.Name()
	discard := func() {
		_ = temp.Close()
		_ = files.Remove(tempPath)
	}
	if _, err := temp.Write(data); err != nil {
		discard()
		return fmt.Errorf("gtwrap: write the wrapper beside %s: %w", t.Path, err)
	}
	if err := temp.Chmod(mode); err != nil {
		discard()
		return fmt.Errorf("gtwrap: set the wrapper's mode beside %s: %w", t.Path, err)
	}
	if err := temp.Close(); err != nil {
		_ = files.Remove(tempPath)
		return fmt.Errorf("gtwrap: finish the wrapper beside %s: %w", t.Path, err)
	}
	kept := dated(t, stamp)
	if err := files.Rename(t.Path, kept); err != nil {
		_ = files.Remove(tempPath)
		return fmt.Errorf("gtwrap: keep %s as %s: %w", t.Path, kept, err)
	}
	if err := files.Rename(tempPath, t.Path); err != nil {
		backErr := files.Rename(kept, t.Path)
		_ = files.Remove(tempPath)
		if backErr != nil {
			return fmt.Errorf("gtwrap: rename the wrapper into place at %s: %v; putting %s back also failed: %v", t.Path, err, kept, backErr)
		}
		return fmt.Errorf("gtwrap: rename the wrapper into place at %s: %w", t.Path, err)
	}
	return nil
}

// restorable is rollback's per-tool refusal before any change is made: it names
// why the tool cannot be restored, or returns "" when it can. The wrapper at the
// target and the dated file are both checked, so rollback only ever renames
// regular files (or symlinks to them).
func restorable(files Files, t Target, stamp string) string {
	wrapper, err := files.Stat(t.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Sprintf("%s: %s does not exist, so there is no wrapper to keep aside", t.Name, t.Path)
	case err != nil:
		return fmt.Sprintf("%s: read %s: %v", t.Name, t.Path, err)
	case !wrapper.Mode().IsRegular():
		return fmt.Sprintf("%s: the wrapper %s is not a regular file or a symlink to one, so it was not moved aside", t.Name, t.Path)
	}
	info, err := files.Stat(dated(t, stamp))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Sprintf("%s: %s does not exist, so %s was not restored", t.Name, dated(t, stamp), t.Path)
	case err != nil:
		return fmt.Sprintf("%s: read %s: %v", t.Name, dated(t, stamp), err)
	case !info.Mode().IsRegular():
		return fmt.Sprintf("%s: %s is not a regular file or a symlink to one, so %s was not restored", t.Name, dated(t, stamp), t.Path)
	}
	if _, err := files.Stat(aside(t, stamp)); err == nil {
		return fmt.Sprintf("%s: %s already exists, so %s was not restored", t.Name, aside(t, stamp), t.Path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Sprintf("%s: look for %s: %v", t.Name, aside(t, stamp), err)
	}
	return ""
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

// leftInstalled is install's refusal after a step fails with some tools already
// placed: it prints the rollback command and the dated files for those tools,
// and names the command in the error, so the operator can undo them. With no
// tool placed it returns cause unchanged.
func leftInstalled(out io.Writer, stamp string, placed []Target, cause error) error {
	if len(placed) == 0 {
		return cause
	}
	command := rollbackCommand(stamp, placed)
	printRollback(out, stamp, placed)
	return fmt.Errorf("%w; the tools still installed can be undone with: %s", cause, command)
}

// stopped is install's refusal when a placed file fails its doctor: it names
// the tool that failed, what its undo did, and the tools left installed.
func stopped(failed Target, placed []Target, doctorErr, undoErr error) error {
	msg := fmt.Sprintf("gtwrap: %s failed its --doctor at %s: %v", failed.Name, failed.Path, doctorErr)
	if undoErr != nil {
		return fmt.Errorf("%s; undoing it also failed: %v", msg, undoErr)
	}
	msg += fmt.Sprintf("; it was undone, so %s holds the file it replaced", failed.Path)
	if len(placed) == 0 {
		return errors.New(msg + "; no tool was cut over")
	}
	names := make([]string, 0, len(placed))
	for _, t := range placed {
		names = append(names, t.Name)
	}
	return errors.New(msg + "; still installed: " + strings.Join(names, ", "))
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
func printRollback(out io.Writer, stamp string, placed []Target) {
	fmt.Fprintf(out, "rollback: %s\n", rollbackCommand(stamp, placed))
	fmt.Fprintln(out, "dated files:")
	for _, t := range placed {
		fmt.Fprintf(out, "  %s\n", dated(t, stamp))
	}
}

// rollbackCommand is the gt-wrap rollback command that restores placed.
func rollbackCommand(stamp string, placed []Target) string {
	command := "gt-wrap rollback --stamp " + shellQuote(stamp)
	for _, t := range placed {
		command += " --target " + shellQuote(t.Name+"="+t.Path)
	}
	return command
}

// shellQuote renders s as one POSIX shell word, so a path holding a space or a
// quote can be pasted back into a shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
