package bumpversion

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
)

// Env is every seam the run touches: the tree it edits, git, the processes it
// starts, the install location, the release date and the output stream. The command
// wires the real ones; the tests wire fakes over a fixture tree, so no run touches
// the working copy or a real tool.
type Env struct {
	FS         FS
	Git        Git
	Exec       Exec
	Root       string
	Home       string
	Date       string // the release date, YYYY-MM-DD, written to CHANGELOG.md
	Executable func(path string) bool
	Out        io.Writer
}

// Run is the command after its command line is read. It edits the version files,
// checks they hold the new version, installs when asked, then makes the commit and
// tag under --commit and --tag. The order is the script's. Each step that fails
// returns before any later step, so a refused run leaves no commit and no tag behind.
// Run never pushes.
func Run(env Env, opts Options) error {
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

	fmt.Fprintln(out, "Updating version files...")
	fmt.Fprintln(out, "  • "+VersionGoPath)
	if err := EditFile(env.FS, VersionGoPath, func(t string) string { return SetVersionGo(t, old, newVersion) }); err != nil {
		return err
	}
	fmt.Fprintln(out, "  • "+PackageJSONPath)
	if err := EditFile(env.FS, PackageJSONPath, func(t string) string { return SetPackageJSON(t, old, newVersion) }); err != nil {
		return err
	}
	if nix {
		fmt.Fprintln(out, "  • flake.nix (version)")
		if err := EditFile(env.FS, FlakePath, func(t string) string { return SetFlakeVersion(t, old, newVersion) }); err != nil {
			return err
		}
		fmt.Fprintln(out, "  • flake.nix (vendorHash)")
		if _, err := UpdateVendorHash(env.FS, env.Exec, env.Root, out); err != nil {
			return fmt.Errorf("Failed to update vendorHash: %w", err)
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

	if opts.Install {
		fmt.Fprintln(out, "Rebuilding and installing gt...")
		inst := Installer{Exec: env.Exec, Executable: env.Executable, Home: env.Home, Root: env.Root}
		res, err := inst.Install(newVersion)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, "✓ gt installed")
		res.Report(out)
		fmt.Fprintln(out)
	}

	if opts.Commit {
		return Finish(env.Git, env.FS, env.Root, opts, Release{Old: old, New: newVersion, Nix: nix}, out)
	}
	fmt.Fprintln(out, "Review the changes above.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "To commit and release:")
	fmt.Fprintf(out, "  bump-version %s --commit --tag --install\n", newVersion)
	return nil
}

// hasNix reports whether nix is on PATH, which is what the script's command -v nix
// checks. It decides whether flake.nix is edited and named in the commit.
func hasNix(run Exec) bool {
	_, err := run.LookPath("nix")
	return err == nil
}
