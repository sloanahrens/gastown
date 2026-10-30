package bumpversion

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

// Env is every seam the run touches: the tree it edits, git, the processes it
// starts, the install location, the release date and the output stream. The command
// wires the real ones; the tests wire fakes over a fixture tree, so no run touches
// the working copy or a real tool. Root is the repository root. The command makes it
// absolute once, so no step depends on the directory the command was started in.
type Env struct {
	FS         FS
	Git        Git
	Exec       Exec
	Root       string
	Home       string
	HomeErr    error  // why Home is unknown; the install check then reports unverified
	Date       string // the release date, YYYY-MM-DD, written to CHANGELOG.md
	Executable func(path string) bool
	Out        io.Writer
}

// Run is the command after its command line is read. It edits the version files,
// checks they hold the new version, installs when asked, then makes the commit and
// tag under --commit and --tag. Each step that fails returns before any later step, so
// a refused run leaves no commit and no tag behind. A run that fails after its first
// edit prints the command that restores the edited files, or, when one of them had
// uncommitted work before the run, a warning and the diff to review instead. The
// summary is the last block a run that finishes prints; a run that fails prints its
// error, and the restore note above, instead. Run never pushes.
//
// ACCEPTED-RISK: a mismatch or an unverified gt after make install is a warning, and
// the run still commits and tags (gt-tpg0p.74). A hard stop there would leave the edits
// uncommitted over a problem the operator can read, so the summary repeats the warning
// as the last lines the command prints.
func Run(env Env, opts Options) (err error) {
	// Parse has checked the version already; Run is also called with Options built
	// by hand, so it holds the same rule before it reads or edits anything.
	if !versionPattern.MatchString(opts.Version) {
		return &VersionError{Version: opts.Version}
	}
	var edited []string
	var dirty map[string]bool
	defer func() {
		if err != nil && len(edited) > 0 {
			restoreNote(env.Out, edited, dirty)
		}
	}()

	out := env.Out
	versionGo, err := env.FS.ReadFile(VersionGoPath)
	if errors.Is(err, fs.ErrNotExist) {
		return &Refusal{Msg: "Error: Must run from repository root"}
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", VersionGoPath, err)
	}
	old := CurrentVersion(string(versionGo))
	if old == "" {
		return &Refusal{Msg: "Error: no Version = line in " + VersionGoPath}
	}
	newVersion := opts.Version
	nix := hasNix(env.Exec)

	fmt.Fprintf(out, "Bumping version: %s → %s\n\n", old, newVersion)
	if err := CheckTree(env.Git, env.Root, opts, out); err != nil {
		return err
	}
	// Before the first edit, record which files the run may edit already differ from
	// HEAD. A restore of one of them would destroy that work.
	dirty = map[string]bool{}
	for _, name := range editable(nix) {
		differs, err := isDirty(env.Git, env.Root, name)
		if err != nil {
			return err
		}
		dirty[name] = differs
	}

	fmt.Fprintln(out, "Updating version files...")
	fmt.Fprintln(out, "  • "+VersionGoPath)
	if err := EditFile(env.FS, VersionGoPath, func(t string) string { return SetVersionGo(t, old, newVersion) }); err != nil {
		return err
	}
	edited = append(edited, VersionGoPath)
	fmt.Fprintln(out, "  • "+PackageJSONPath)
	if err := EditFile(env.FS, PackageJSONPath, func(t string) string { return SetPackageJSON(t, old, newVersion) }); err != nil {
		return err
	}
	edited = append(edited, PackageJSONPath)
	if nix {
		fmt.Fprintln(out, "  • flake.nix (version)")
		if err := EditFile(env.FS, FlakePath, func(t string) string { return SetFlakeVersion(t, old, newVersion) }); err != nil {
			return err
		}
		edited = append(edited, FlakePath)
		fmt.Fprintln(out, "  • flake.nix (vendorHash)")
		if _, err := UpdateVendorHash(env.FS, env.Exec, env.Root, out); err != nil {
			return fmt.Errorf("failed to update vendorHash: %w", err)
		}
	} else {
		fmt.Fprintln(out, "  • "+NixSkippedNote)
	}
	fmt.Fprintln(out, "  • "+ChangelogPath)
	warning, err := UpdateChangelogFile(env.FS, newVersion, env.Date)
	if err != nil {
		return err
	}
	if warning != "" {
		fmt.Fprintln(out, warning)
	} else {
		edited = append(edited, ChangelogPath)
	}

	fmt.Fprintf(out, "\n✓ Version updated to %s\n\n", newVersion)
	stat, _, err := env.Exec.Run(env.Root, nil, "git", "diff", "--stat")
	if err != nil {
		return fmt.Errorf("git diff --stat: %w", err)
	}
	fmt.Fprintln(out, "Changed files:")
	fmt.Fprintf(out, "%s", stat)
	fmt.Fprintln(out)

	fmt.Fprintln(out, "Verifying version consistency...")
	if err := Verify(env.FS, newVersion, nix); err != nil {
		return err
	}
	fmt.Fprintf(out, "✓ All versions match: %s\n\n", newVersion)

	var installed InstallResult
	if opts.Install {
		fmt.Fprintln(out, "Rebuilding and installing gt...")
		inst := Installer{Exec: env.Exec, Executable: env.Executable, Home: env.Home, HomeErr: env.HomeErr, Root: env.Root}
		res, err := inst.Install(newVersion)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "✓ gt installed")
		res.Report(out)
		fmt.Fprintln(out)
		installed = res
	}

	if opts.Commit {
		if err := Finish(env.Git, env.FS, env.Root, opts, Release{Old: old, New: newVersion, Nix: nix}, out); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(out, "Review the changes above.")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "To commit and release:")
		fmt.Fprintf(out, "  bump-version %s --commit --tag --install\n", newVersion)
	}
	summary(out, opts, newVersion, installed)
	return nil
}

// summary is the block every run prints last. It repeats a mismatch or an unverified
// install, so the warning is the final thing the operator reads, and it says whether
// the run made a commit and a tag.
func summary(out io.Writer, opts Options, version string, installed InstallResult) {
	fmt.Fprintln(out, "Summary:")
	switch installed.Status {
	case Mismatch:
		fmt.Fprintf(out, "⚠ installed gt reports %s, wanted %s\n", installed.Got, installed.Want)
	case Unverified:
		fmt.Fprintf(out, "⚠ installed gt unverified (%s), wanted %s\n", installed.Reason, installed.Want)
	}
	switch {
	case opts.Commit && opts.Tag:
		fmt.Fprintf(out, "Committed and tagged v%s.\n", version)
	case opts.Commit:
		fmt.Fprintf(out, "Committed %s; no tag made.\n", version)
	default:
		fmt.Fprintln(out, "No commit or tag made.")
	}
}

// hasNix reports whether nix is on PATH. It decides whether flake.nix is edited and
// named in the commit.
func hasNix(run Exec) bool {
	_, err := run.LookPath("nix")
	return err == nil
}

// editable is every file the run may edit, in the order it edits them. flake.nix is
// in the list only when nix is on PATH, since that is when the run edits it.
func editable(nix bool) []string {
	files := []string{VersionGoPath, PackageJSONPath}
	if nix {
		files = append(files, FlakePath)
	}
	return append(files, ChangelogPath)
}

// restoreNote is the line a failed run prints after its first edit. When no edited file
// had uncommitted work before the run, it names the command that puts the files back to
// HEAD in the index and the work tree, which undoes a git add as well as the edit. When
// one did, a restore would destroy that work, so the note names that file, warns, and
// points at the diff to review, and it prints no restore command.
func restoreNote(out io.Writer, edited []string, dirty map[string]bool) {
	var had []string
	for _, name := range edited {
		if dirty[name] {
			had = append(had, name)
		}
	}
	files := strings.Join(edited, " ")
	if len(had) == 0 {
		fmt.Fprintf(out, "Edited before this failure. Restore with: git restore --source=HEAD --staged --worktree -- %s\n", files)
		return
	}
	fmt.Fprintf(out, "Edited before this failure: %s.\n", files)
	fmt.Fprintf(out, "Had uncommitted changes before this run: %s. A restore would destroy that work, so no restore command is printed.\n", strings.Join(had, " "))
	fmt.Fprintf(out, "Review the edits with: git diff HEAD -- %s, then revert them by hand.\n", files)
}
