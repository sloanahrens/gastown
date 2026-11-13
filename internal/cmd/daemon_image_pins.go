package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
)

// daemonImagePinsFlags are the values `gt daemon image pins` parsed: the image
// pins file, the four source checkouts whose origin/main tips the refresh reads,
// the forge owner the pins' {owner} expands to, and the seed-pins file the
// refresh writes beside the image pins.
type daemonImagePinsFlags struct {
	daemonImageFlags
	seedPins string // the absolute seed-pins file whose beads and devops lines follow the image pins
}

// daemonImagePinsDeps is what the refresh takes from the host: git, to read each
// source checkout's origin/main, how a path resolves, and where output goes. A
// test supplies a fake git, so no unit test runs git or touches a network.
type daemonImagePinsDeps struct {
	git          bdbuild.Git
	evalSymlinks func(path string) (string, error)
	out          io.Writer
}

// newDaemonImagePinsCmd builds `gt daemon image pins` over run, which receives
// the flags the command parsed.
func newDaemonImagePinsCmd(run func(out io.Writer, f daemonImagePinsFlags) error) *cobra.Command {
	var f daemonImagePinsFlags
	cmd := &cobra.Command{
		Use:   "pins",
		Short: "Move the image pins that trail their source's origin/main to its tip (a dry run unless --apply)",
		Long: `Move the bd, om, hm and devops pins to each source's origin/main tip, and write
the image-pins file and the seed-pins file so they agree.

A pin is moved when its commit is an ancestor of its source's origin/main and
at least 20 commits behind it, half the 40-commit bound the pin-drift guard
enforces. A pin nearer the tip keeps its line, so the diff stays quiet. The tip
is read from each --<name>-src checkout's origin/main through git; nothing is
fetched. A pin that is not an ancestor of its tip (ahead or diverged) is
refused: set it by hand.

Only the commit column of each line changes: comments, URLs, spacing and line
order stay as they are. The seed-pins file's beads and devops lines take the
image file's bd and devops commits, so TestCommittedPinsAgree holds after a
refresh.

--pins, --seed-pins and the four --<name>-src checkouts are required: nothing is
inferred from the live town, except the forge owner a {owner} placeholder needs,
which --forge-owner or --town-root names as gt daemon image does. Every file is
read and every tip checked before anything is written, so a refusal writes
neither file. Without --apply the command prints the changes and writes nothing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.OutOrStdout(), f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.pins, "pins", "", "absolute image-pins file naming the bd, om, hm and devops commits")
	fl.StringVar(&f.seedPins, "seed-pins", "", "absolute seed-pins file whose beads and devops lines take the image commits")
	fl.StringVar(&f.bdSrc, "bd-src", "", "absolute beads checkout whose origin/main is the bd tip")
	fl.StringVar(&f.omSrc, "om-src", "", "absolute om checkout whose origin/main is the om tip")
	fl.StringVar(&f.hmSrc, "hm-src", "", "absolute hm checkout whose origin/main is the hm tip")
	fl.StringVar(&f.devopsSrc, "devops-src", "", "absolute devops checkout whose origin/main is the devops tip")
	fl.StringVar(&f.forgeOwner, "forge-owner", "", "Forgejo owner the pins' URLs name; empty takes --town-root's forge.owner, else the setting's default")
	fl.StringVar(&f.townRoot, "town-root", "", "town whose forge.owner names the owner when --forge-owner is empty; empty reads no town")
	fl.BoolVar(&f.apply, "apply", false, "write both files; without it only print the changes")
	return cmd
}

var daemonImagePinsCmd = newDaemonImagePinsCmd(func(out io.Writer, f daemonImagePinsFlags) error {
	return runDaemonImagePins(f, daemonImagePinsDeps{
		git:          bdbuild.ExecGit,
		evalSymlinks: filepath.EvalSymlinks,
		out:          out,
	})
})

func init() {
	daemonImageCmd.AddCommand(daemonImagePinsCmd)
}

// runDaemonImagePins validates the flags, reads both files and each source's
// tip, and plans the refresh. It prints the plan, and with --apply writes the
// two files. Every refusal happens before the first write, so a refusal leaves
// both files as they were.
func runDaemonImagePins(f daemonImagePinsFlags, d daemonImagePinsDeps) error {
	if err := validateDaemonImagePinsFlags(f, d.evalSymlinks); err != nil {
		return err
	}
	if f.forgeOwner != "" {
		if err := config.ValidForgeOwner(f.forgeOwner); err != nil {
			return fmt.Errorf("forge owner from --forge-owner: %w", err)
		}
	}
	imageRaw, pins, err := readImagePinsFile(f.pins, func() (string, error) {
		return config.ResolveForgeOwnerFor(f.forgeOwner, f.townRoot)
	})
	if err != nil {
		return err
	}
	seedRaw, err := os.ReadFile(f.seedPins)
	if err != nil {
		return fmt.Errorf("--seed-pins %q cannot be read: %w", f.seedPins, err)
	}
	changes, err := daemonimage.PlanPinRefresh(pins, pinsMainOf(f.daemonImageFlags), d.git)
	if err != nil {
		return err
	}
	commits := make(map[string]string, len(pins))
	for _, p := range pins {
		commits[p.Name] = p.Commit
	}
	moved := make(map[string]string, len(changes))
	for _, c := range changes {
		moved[c.Name] = c.To
		commits[c.Name] = c.To
	}
	newImage := []byte(imageRaw)
	if len(moved) != 0 {
		if newImage, err = daemonimage.RewritePinCommits(newImage, moved, f.pins); err != nil {
			return err
		}
	}
	newSeed, err := daemonimage.RefreshSeedPins(seedRaw, daemonimage.SeedCommits(commits), f.seedPins)
	if err != nil {
		return err
	}
	imageChanged := !bytes.Equal(newImage, []byte(imageRaw))
	seedChanged := !bytes.Equal(newSeed, seedRaw)
	printPinsRefresh(d.out, pins, changes, seedChanged, f.seedPins)
	if !f.apply {
		fmt.Fprintln(d.out, "dry run: nothing was written. Pass --apply to write both files.")
		return nil
	}
	if !imageChanged && !seedChanged {
		fmt.Fprintf(d.out, "nothing to refresh: no pin trails its origin/main by %d commits or more, and the two files already agree.\n", daemonimage.PinRefreshBehind)
		return nil
	}
	if imageChanged {
		if err := os.WriteFile(f.pins, newImage, 0o644); err != nil {
			return fmt.Errorf("--pins %q cannot be written, so neither file was changed: %w", f.pins, err)
		}
	}
	if seedChanged {
		if err := os.WriteFile(f.seedPins, newSeed, 0o644); err != nil {
			return fmt.Errorf("--pins %q was written but --seed-pins %q cannot be, so the two files disagree until it is fixed: %w", f.pins, f.seedPins, err)
		}
	}
	fmt.Fprintf(d.out, "wrote %s and %s\n", f.pins, f.seedPins)
	return nil
}

// printPinsRefresh prints one line per image pin, saying which moves and to what,
// and whether the seed-pins file changes.
func printPinsRefresh(out io.Writer, pins []daemonimage.Pin, changes []daemonimage.PinChange, seedChanges bool, seedPath string) {
	moved := make(map[string]daemonimage.PinChange, len(changes))
	for _, c := range changes {
		moved[c.Name] = c
	}
	for _, p := range pins {
		c, ok := moved[p.Name]
		if !ok {
			fmt.Fprintf(out, "%s: pin %s stays\n", p.Name, shortCommit(p.Commit))
			continue
		}
		fmt.Fprintf(out, "%s: pin %s -> %s (%d commits behind origin/main)\n", p.Name, shortCommit(c.From), shortCommit(c.To), c.Behind)
	}
	if seedChanges {
		fmt.Fprintf(out, "%s: beads and devops lines take the image commits\n", seedPath)
	} else {
		fmt.Fprintf(out, "%s: already agrees with the image commits\n", seedPath)
	}
}

// validateDaemonImagePinsFlags refuses, before anything is read, a missing
// required flag, a relative or missing file, a checkout that is not an existing
// directory, and a checkout that is or lies inside a canonical clone.
func validateDaemonImagePinsFlags(f daemonImagePinsFlags, evalSymlinks func(string) (string, error)) error {
	for _, req := range []struct{ flag, value string }{
		{"--pins", f.pins}, {"--seed-pins", f.seedPins},
		{"--bd-src", f.bdSrc}, {"--om-src", f.omSrc},
		{"--hm-src", f.hmSrc}, {"--devops-src", f.devopsSrc},
	} {
		if strings.TrimSpace(req.value) == "" {
			return fmt.Errorf("%s is required: the refresh reads each source's tip from its checkout, and nothing is inferred from the live town", req.flag)
		}
	}
	for _, p := range []struct{ flag, value string }{
		{"--pins", f.pins}, {"--seed-pins", f.seedPins},
	} {
		if !filepath.IsAbs(p.value) {
			return fmt.Errorf("%s %q is not absolute: name the file outright", p.flag, p.value)
		}
		if info, err := os.Stat(p.value); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("%s %q is not an existing file", p.flag, p.value)
		}
	}
	for _, p := range []struct{ flag, value string }{
		{"--bd-src", f.bdSrc}, {"--om-src", f.omSrc},
		{"--hm-src", f.hmSrc}, {"--devops-src", f.devopsSrc},
	} {
		if !filepath.IsAbs(p.value) {
			return fmt.Errorf("%s %q is not absolute: a checkout is named by its absolute path", p.flag, p.value)
		}
		resolved, err := evalSymlinks(p.value)
		if err != nil {
			return fmt.Errorf("%s %q cannot be resolved: %w", p.flag, p.value, err)
		}
		if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
			return fmt.Errorf("%s %q is not an existing directory", p.flag, p.value)
		}
		if daemonimage.IsCanonicalClone(resolved) {
			return fmt.Errorf("%s %q is or lies inside a canonical clone (.../mayor/rig): the refresh reads that checkout's origin/main, and the canonical clone is never touched", p.flag, p.value)
		}
	}
	return nil
}
