package cmd

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/townstate"
)

var townStateRewriteCmd = &cobra.Command{
	Use:   "rewrite",
	Short: "Rewrite the town root and the Forgejo owner in a claim's config and git pointer files",
	Long: `Replace the old town root with the new one, and the old Forgejo owner with the
new one, in the files of one town directory. The files it changes are listed in
internal/townstate (RewriteClasses); everything else is left alone, and the
summary names what was skipped.

Run it on a copy: the claim mounted in a helper Pod, or a scratch directory.
Dry run is the default and prints each file with the number of replacements it
would make. With --apply each changed file is written atomically, its original
bytes kept as <file>.pre-rewrite beside it, and the command then scans the
classes and exits non-zero naming any file that still holds an old value. With
--undo the kept copies are restored byte for byte, and a rewritten file that is
missing is created again from its copy; --undo takes no root or owner flags.
Any run, dry or applied, refuses while the town holds a file named
<file>.pre-rewrite outside its git and Dolt trees, so a second --apply refuses
and writes nothing until that copy is undone or removed.

A root matches only as a whole path, so a directory whose name starts with the
old root's characters is not rewritten. The owner matches only in the two
repository forms forgejo:3000/<owner>/ and "<owner>/.

Examples:
  gt town state rewrite --town /volume --from-root /Users/me/gt --to-root /town
  gt town state rewrite --town /volume --from-root /Users/me/gt --to-root /town \
    --from-owner me --to-owner gastown --apply
  gt town state rewrite --town /volume --undo`,
	Args: cobra.NoArgs,
	RunE: runTownStateRewrite,
}

func init() {
	townStateCmd.AddCommand(townStateRewriteCmd)
	townStateRewriteCmd.Flags().String("town", "", "the town directory to rewrite (required)")
	townStateRewriteCmd.Flags().String("from-root", "", "the old town root to replace, an absolute path (required unless --undo)")
	townStateRewriteCmd.Flags().String("to-root", "", "the new town root, an absolute path (required unless --undo)")
	townStateRewriteCmd.Flags().String("from-owner", "", "the old Forgejo owner to replace (optional, with --to-owner)")
	townStateRewriteCmd.Flags().String("to-owner", "", "the new Forgejo owner (optional, with --from-owner)")
	townStateRewriteCmd.Flags().Bool("apply", false, "write the changes, keeping each original as <file>.pre-rewrite")
	townStateRewriteCmd.Flags().Bool("undo", false, "restore every kept copy instead of rewriting")
}

// townStateRewriteEnv is what the rewrite command reads from its session.
type townStateRewriteEnv struct {
	town      string
	fromRoot  string
	toRoot    string
	fromOwner string
	toOwner   string
	apply     bool
	undo      bool
	// scanClasses is the list the post-apply scan reads; nil is the pass's own
	// list. A test adds a class the pass leaves out to drive the non-zero exit.
	scanClasses []townstate.RewriteClass
	out         io.Writer
}

func runTownStateRewrite(cmd *cobra.Command, _ []string) error {
	env := townStateRewriteEnv{out: cmd.OutOrStdout()}
	flags := []struct {
		name string
		dst  *string
	}{
		{"town", &env.town},
		{"from-root", &env.fromRoot},
		{"to-root", &env.toRoot},
		{"from-owner", &env.fromOwner},
		{"to-owner", &env.toOwner},
	}
	for _, f := range flags {
		v, err := cmd.Flags().GetString(f.name)
		if err != nil {
			return fmt.Errorf("reading --%s: %w", f.name, err)
		}
		*f.dst = v
	}
	var err error
	if env.apply, err = cmd.Flags().GetBool("apply"); err != nil {
		return fmt.Errorf("reading --apply: %w", err)
	}
	if env.undo, err = cmd.Flags().GetBool("undo"); err != nil {
		return fmt.Errorf("reading --undo: %w", err)
	}
	return runTownStateRewriteIn(env)
}

func runTownStateRewriteIn(env townStateRewriteEnv) error {
	if env.apply && env.undo {
		return errors.New("rewrite: --apply and --undo do different things; give one")
	}
	if env.undo {
		if env.fromRoot != "" || env.toRoot != "" || env.fromOwner != "" || env.toOwner != "" {
			return errors.New("rewrite: --undo restores the kept copies and takes no root or owner flags")
		}
		report, err := townstate.Undo(env.town)
		if err != nil {
			return err
		}
		for _, r := range report.Undone {
			if r.Recreated {
				fmt.Fprintf(env.out, "restored %s (the rewritten file was missing)\n", r.Path)
				continue
			}
			fmt.Fprintf(env.out, "restored %s\n", r.Path)
		}
		fmt.Fprintf(env.out, "rewrite: restored %d file(s)\n", len(report.Undone))
		return nil
	}
	q := townstate.RewriteQuery{
		FromRoot:  env.fromRoot,
		ToRoot:    env.toRoot,
		FromOwner: env.fromOwner,
		ToOwner:   env.toOwner,
	}
	report, err := townstate.Rewrite(env.town, q, env.apply)
	if err != nil {
		return err
	}
	verb := "would change"
	if env.apply {
		verb = "changed"
	}
	replacements := 0
	for _, f := range report.Files {
		fmt.Fprintf(env.out, "%s %s (%d)\n", verb, f.Path, f.Replacements)
		replacements += f.Replacements
	}
	fmt.Fprintf(env.out, "rewrite: %s %d file(s), %d replacement(s)\n", verb, len(report.Files), replacements)
	fmt.Fprintf(env.out, "classes: %s\n", strings.Join(report.Classes, "; "))
	// The kept-copy and left-alone lines print before the scan, so a scan that
	// fails still tells the operator where the originals are.
	if env.apply && len(report.Files) > 0 {
		fmt.Fprintf(env.out, "rewrite: each original is kept beside its file as <file>%s\n", townstate.RewriteBackupSuffix)
	}
	fmt.Fprintf(env.out, "left alone: %s\n", strings.Join(townstate.RewriteSkipped, "; "))
	if !env.apply {
		return nil
	}
	remaining, err := townstate.Scan(env.town, q, env.scanClasses)
	if err != nil {
		return err
	}
	if len(remaining) > 0 {
		return fmt.Errorf("rewrite: after the pass these files still hold an old value: %s", joinRewritePaths(remaining))
	}
	return nil
}

// joinRewritePaths names at most ten files and counts the rest, so a class the
// list missed reads as one line rather than a page.
func joinRewritePaths(paths []string) string {
	const shown = 10
	if len(paths) <= shown {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(paths[:shown], ", "), len(paths)-shown)
}
