package daemonimage

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
	polecatseed "github.com/steveyegge/gastown/internal/polecat/seed"
)

// PinRefreshBehind is how far a pin must trail its source's origin/main before
// `gt daemon image pins` moves it: half the drift guard's bound, so a pin a few
// commits behind keeps its line and the diff stays quiet.
const PinRefreshBehind = MaxPinBehind / 2

// seedRigOf names the seed-pins rig that is the same repository as each
// image pins source the seed file also pins: the seed names beads where the
// image names bd.
var seedRigOf = map[string]string{
	"bd":     "beads",
	"devops": "devops",
}

// PinChange is one image pin `gt daemon image pins` moves: the commit it
// trailed, the origin/main commit it moves to, and how far behind it was.
type PinChange struct {
	Name   string
	From   string
	To     string
	Behind int
}

// PlanPinRefresh reads each pin's source tip through mainOf and git, and
// returns the pins to move: those behind the tip by PinRefreshBehind commits or
// more. A pin at the tip or closer than that is not returned. A pin whose tip
// cannot be read, or that is not an ancestor of its tip (ahead or diverged), is
// an error naming the source: the refresh never writes a pin the drift guard
// would refuse, and never guesses at a pin it cannot place.
func PlanPinRefresh(pins []Pin, mainOf MainOf, git bdbuild.Git) ([]PinChange, error) {
	if git == nil {
		return nil, errors.New("pin refresh: no git to read origin/main with")
	}
	var changes []PinChange
	for _, pin := range pins {
		dir, err := mainOf(pin)
		if err != nil {
			return nil, fmt.Errorf("pin refresh: %s pin: %w", pin.Name, err)
		}
		out, err := git(dir, "rev-parse", "--verify", "origin/main^{commit}")
		if err != nil {
			return nil, fmt.Errorf("pin refresh: cannot read origin/main of the %s source in %s (%v): fetch that checkout through its remote, then rerun", pin.Name, dir, err)
		}
		tip := strings.TrimSpace(string(out))
		if pin.Commit == tip {
			continue
		}
		if _, err := git(dir, "merge-base", "--is-ancestor", pin.Commit, tip); err != nil {
			if !isNotAncestor(err) {
				return nil, fmt.Errorf("pin refresh: %s pin %s: cannot test whether it is an ancestor of origin/main %s in %s (%v): fetch or fix that checkout, then rerun", pin.Name, short(pin.Commit), short(tip), dir, err)
			}
			return nil, fmt.Errorf("pin refresh: %s pin %s is not an ancestor of origin/main %s (ahead or diverged): set it by hand", pin.Name, short(pin.Commit), short(tip))
		}
		behind, err := commitsBehind(git, dir, pin.Commit, tip)
		if err != nil {
			return nil, fmt.Errorf("pin refresh: %s pin %s: cannot count the commits to origin/main %s in %s (%v): fetch or fix that checkout, then rerun", pin.Name, short(pin.Commit), short(tip), dir, err)
		}
		if behind < PinRefreshBehind {
			continue
		}
		changes = append(changes, PinChange{Name: pin.Name, From: pin.Commit, To: tip, Behind: behind})
	}
	return changes, nil
}

// SeedCommits maps each seed-pins rig that is the same repository as a source
// the image file pins to that source's commit in imageCommits. A refresh writes
// the result into the seed file, so the two files agree for every shared
// source.
func SeedCommits(imageCommits map[string]string) map[string]string {
	seed := make(map[string]string, len(seedRigOf))
	for name, rig := range seedRigOf {
		if commit, ok := imageCommits[name]; ok {
			seed[rig] = commit
		}
	}
	return seed
}

// RewritePinCommits returns data, the pins file at path, with the commit column
// of each line whose first field is a key of commits replaced by that key's
// value. Every other byte stays as it was: comments, blank lines, URLs, spacing
// and line order. A key with no line is an error naming it and the file, so a
// refresh never drops a source silently. The caller has parsed data already;
// this edits only the commit column.
func RewritePinCommits(data []byte, commits map[string]string, path string) ([]byte, error) {
	seen := make(map[string]bool, len(commits))
	lines := strings.SplitAfter(string(data), "\n")
	for i, raw := range lines {
		body := strings.TrimRight(raw, "\r\n")
		trimmed := strings.TrimSpace(body)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		want, ok := commits[fields[0]]
		if !ok {
			continue
		}
		if len(fields) < 2 {
			return nil, fmt.Errorf("%s line %d names %s with no commit", path, i+1, fields[0])
		}
		old := fields[len(fields)-1]
		at := strings.LastIndex(body, old)
		lines[i] = body[:at] + want + body[at+len(old):] + raw[len(body):]
		seen[fields[0]] = true
	}
	var missing []string
	for name := range commits {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("%s names no line for %s", path, strings.Join(missing, ", "))
	}
	return []byte(strings.Join(lines, "")), nil
}

// RefreshPinFiles returns the image pins file and the seed-pins file with the
// moved pins' commits and the seed's shared-source commits written in. Both are
// computed before either is returned, so a refusal for one file leaves the
// caller with neither to write.
func RefreshPinFiles(image, seed []byte, imagePath, seedPath string, moved, seedCommits map[string]string) (newImage, newSeed []byte, err error) {
	newImage, err = RewritePinCommits(image, moved, imagePath)
	if err != nil {
		return nil, nil, err
	}
	newSeed, err = RefreshSeedPins(seed, seedCommits, seedPath)
	if err != nil {
		return nil, nil, err
	}
	return newImage, newSeed, nil
}

// RefreshSeedPins validates the seed-pins file at path, whose bytes are data,
// and returns it with each rig in commits set to its commit. A file that does
// not parse is an error naming the file, and nothing is returned for it.
func RefreshSeedPins(data []byte, commits map[string]string, path string) ([]byte, error) {
	if _, err := polecatseed.Pins(data); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return RewritePinCommits(data, commits, path)
}
