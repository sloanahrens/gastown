package seed

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
)

// Guard is the pin-drift check (gt-n3ewt): a committed pin for a rig other
// than the build rig must be that rig's main, so the image seeds the modules
// main's code needs. A pin that is behind main fails, naming the rig, the pin,
// the main sha and any nested go.mod module the pin lacks. The build rig takes
// the build commit and has no committed pin, so it is skipped.
//
// The main sha is read from the rig's canonical checkout (origin/main as it
// stands there); the guard never fetches.
func Guard(pins []Source, buildRig, town string, git Git) error {
	for _, src := range pins {
		if src.Rig == buildRig {
			continue
		}
		dir := path.Join(town, src.Rig, "mayor", "rig")
		out, err := git(dir, "rev-parse", "--verify", "origin/main^{commit}")
		if err != nil {
			return fmt.Errorf("pin-drift guard: cannot read origin/main of rig %s in %s (%v): fetch that checkout through the town's path, then rerun", src.Rig, dir, err)
		}
		main := strings.TrimSpace(string(out))
		if src.Commit == main {
			continue
		}
		missing, err := missingModules(git, dir, src.Commit, main)
		if err != nil {
			return fmt.Errorf("pin-drift guard: rig %s pin %s is not readable in its canonical checkout, so it cannot be compared with main %s (%v): fetch or fix that checkout, then rerun", src.Rig, short(src.Commit), short(main), err)
		}
		msg := fmt.Sprintf("pin-drift guard: rig %s pin %s is behind its main %s; run make polecat-seed-pins on the canonical checkout at main, then commit the pin", src.Rig, short(src.Commit), short(main))
		if len(missing) > 0 {
			msg += "; the pin lacks: " + strings.Join(missing, ", ")
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
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
