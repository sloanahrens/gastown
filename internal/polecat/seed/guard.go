package seed

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
)

// ReadOnlyGitVerbs is every git verb the guard runs. The guard reads only: it
// never fetches or writes, and its no-fetch test checks each call against this
// list.
var ReadOnlyGitVerbs = []string{"rev-parse", "ls-tree", "show", "merge-base", "rev-list"}

// Guard is the pin-drift check (gt-n3ewt): a committed pin for a rig other than
// the build rig must carry every module that rig's main requires, so the image
// can build the code main runs. A pin that is behind main but has the same module
// set passes, with a note: a live rig's main moves every few minutes, and a
// behind pin with no coverage gap does not break a build. A pin whose modules
// are missing a module main requires fails, naming the rig, the pin, the main
// sha and the missing modules. The build rig takes the build commit and has no
// committed pin, so it is skipped.
//
// The main sha is read from the rig's .repo.git bare repository, from its main
// branch (the bare repo keeps no origin/main); the guard never fetches. It
// returns one note per rig that is behind main but has no coverage gap.
//
// Signature change: Guard now returns the notes as well as the error (it was
// error only, when it failed on any pin behind main). The only caller in the
// repository is the stage command (internal/polecat/seed/cmd/stage), which prints
// the notes; no other package imports internal/polecat/seed's Guard.
func Guard(pins []Source, buildRig, town string, git Git) (notes []string, err error) {
	for _, src := range pins {
		if src.Rig == buildRig {
			continue
		}
		dir, err := bareRepo(town, src.Rig)
		if err != nil {
			return nil, err
		}
		out, err := git(dir, "--git-dir="+dir, "rev-parse", "--verify", "main^{commit}")
		if err != nil {
			return nil, fmt.Errorf("pin-drift guard: cannot read main of rig %s from its .repo.git bare repo %s (%v): fetch that rig's bare repo, then rerun", src.Rig, dir, err)
		}
		main := strings.TrimSpace(string(out))
		note, err := CheckAgainstMain(src.Rig, src.Commit, main, "the .repo.git bare repo "+dir, dir, git)
		if err != nil {
			return nil, err
		}
		if note != "" {
			notes = append(notes, note)
		}
	}
	return notes, nil
}

// CheckAgainstMain is Guard's rule for one rig: the pin must carry every module
// main requires, both commits read from dir, a tree that holds the pin and main
// and is named by where in the refusal. A pin at main returns no note. A pin
// behind main with no coverage gap returns the note Guard prints. A pin whose
// modules lack one main requires is refused. The caller reads main; this never
// fetches.
func CheckAgainstMain(rig, pin, main, where, dir string, git Git) (note string, err error) {
	if pin == main {
		return "", nil
	}
	missing, err := missingModules(git, dir, pin, main)
	if err != nil {
		return "", fmt.Errorf("pin-drift guard: rig %s pin %s is not readable in %s, so it cannot be compared with main %s (%v): fetch or fix that repository, then rerun", rig, short(pin), where, short(main), err)
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("pin-drift guard: rig %s pin %s lacks modules its main %s requires: %s; main is read from %s; run make polecat-seed-pins, then commit the pin", rig, short(pin), short(main), strings.Join(missing, ", "), where)
	}
	return fmt.Sprintf("pin-drift guard: rig %s pin %s is %s, main %s: no module coverage gap", rig, short(pin), behind(git, dir, pin, main), short(main)), nil
}

// behind describes where the pin stands against main: how many commits behind
// when the pin is an ancestor of main, and "ahead or diverged" otherwise.
func behind(git Git, dir, pin, main string) string {
	if _, err := git(dir, "merge-base", "--is-ancestor", pin, main); err != nil {
		return "not an ancestor of its main (ahead or diverged)"
	}
	out, err := git(dir, "rev-list", "--count", pin+".."+main)
	if err != nil {
		return "behind its main"
	}
	return fmt.Sprintf("%s commits behind its main", strings.TrimSpace(string(out)))
}

// missingModules lists the modules main's go.mod files require that the pin's
// go.mod files do not, each as "<module> (in <dir>)", sorted. A nested go.mod
// (tracker/go.mod, say) counts under its own directory.
func missingModules(git Git, dir, pin, main string) ([]string, error) {
	have, err := requiredModules(git, dir, pin)
	if err != nil {
		return nil, err
	}
	want, err := requiredModules(git, dir, main)
	if err != nil {
		return nil, err
	}
	var missing []string
	for key := range want {
		if !have[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	return missing, nil
}

// requiredModules returns "<module> (in <dir>)" for every require in every
// go.mod in the commit's tree, keyed so the same module under two go.mod files
// is two entries.
func requiredModules(git Git, dir, commit string) (map[string]bool, error) {
	files, err := git(dir, "ls-tree", "-r", "--name-only", commit)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, name := range strings.Split(strings.TrimSpace(string(files)), "\n") {
		if path.Base(name) != "go.mod" {
			continue
		}
		body, err := git(dir, "show", commit+":"+name)
		if err != nil {
			return nil, err
		}
		f, err := modfile.Parse(name, body, nil)
		if err != nil {
			return nil, fmt.Errorf("%s at %s: %w", name, short(commit), err)
		}
		where := path.Dir(name)
		for _, r := range f.Require {
			set[fmt.Sprintf("%s (in %s)", r.Mod.Path, where)] = true
		}
	}
	return set, nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
