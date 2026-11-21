package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/forgejo"
)

// ImagePinCompare is the Forgejo read the tip reader makes: the comparison that
// counts the commits head has and base does not.
type ImagePinCompare interface {
	CompareCommits(ctx context.Context, owner, repo, base, head string) (*forgejo.CompareInfo, error)
}

// PinTipGit answers what `git ls-remote --refs <url> refs/heads/main` prints for
// a source repository: the injected seam that keeps the reader from running
// git, so a test supplies a fake and no unit test reaches a network.
type PinTipGit func(url string) ([]byte, error)

// PinPathCount counts the commits a pin trails its source's tip by that touch
// paths, the inputs the image builds the pin's source from: the injected seam
// for the git read in the daemon's clone, which can name the paths a commit
// changed where the compare API cannot.
type PinPathCount func(url, pin, tip string, paths []string) (int, error)

// pinTipRef is the ref an image pin's source tip is read from.
const pinTipRef = "refs/heads/main"

// pinShaForm is the form a full commit sha takes, both the source tip an
// ls-remote answer names and the pin the file pins: 40 lowercase hex, so a
// read names one commit and not an abbreviation another commit can claim.
var pinShaForm = regexp.MustCompile(`^[0-9a-f]{40}$`)

// pinRepoForm is the shape of a Forgejo repository name, the same shape an
// owner takes: one path segment, so it cannot traverse out of the API path.
var pinRepoForm = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ReadImagePinTips reads every pin in an image-pins file: its source's
// origin/main tip through tipGit, and how many commits the pin trails that tip,
// through pathCount limited to the pin's inputs when it has a list (devops) and
// through cmp otherwise, neither of which is called for a pin already at its
// tip. ownerOf names the Forgejo owner a {owner} placeholder expands to and is
// called only when the file holds one; the owner and repository the comparison
// addresses are read from the expanded pin URL.
//
// Each pin is read on its own. A pin's URL that does not name one usable,
// credential-free http(s) repository, a tip read that fails, a path-limited
// count that fails or is missing for a pin that needs one, and a compare that
// fails are that pin's Err, and the pins after it are still read: a failed read
// is never reported with a guessed answer. The error return is the file's
// alone: a missing seam, a {owner} that cannot be expanded, and a file that
// does not name its four pins. A state with Err is not a read, whatever its Tip
// and Behind hold, so no caller may take it for a fresh pin.
//
// A bd tip that does not contain the samples build is that pin's Err, so a
// refresh never moves bd back to a build without `bd sample`. A bd pin behind
// that tip has its own commit held to the floor too: one that lacks it reads
// LacksFloor, which is not an error, so a pin older than the floor is told apart
// from one that cannot be read.
func ReadImagePinTips(ctx context.Context, data []byte, ownerOf func() (string, error), tipGit PinTipGit, pathCount PinPathCount, cmp ImagePinCompare) ([]ImagePinState, error) {
	if tipGit == nil {
		return nil, errors.New("image pins read: no git to read each source's origin/main with")
	}
	if cmp == nil {
		return nil, errors.New("image pins read: no forgejo compare to count a pin's lag with")
	}
	pins, err := parseImagePins(data, ownerOf)
	if err != nil {
		return nil, err
	}
	states := make([]ImagePinState, 0, len(pins))
	for _, pin := range pins {
		states = append(states, readImagePin(ctx, cmp, tipGit, pathCount, pin))
	}
	return states, nil
}

// readImagePin reads one pin: its source's tip, and its lag behind that tip. A
// bd pin's tip is held to the samples build every bd image needs, whether or not
// the pin is at it (imagePinBDFloor), and a bd pin not at that tip has its own
// commit held to the floor after the tip's check passed: one that lacks it reads
// LacksFloor, which is not an error. The first failure is the state's Err, with
// the pin's Name and Pin from the file, so the caller can tell which pin failed
// and no pin is read with a guessed answer.
func readImagePin(ctx context.Context, cmp ImagePinCompare, tipGit PinTipGit, pathCount PinPathCount, pin daemonimage.Pin) ImagePinState {
	state := ImagePinState{Name: pin.Name, Pin: pin.Commit}
	owner, repo, err := pinRepo(pin)
	if err != nil {
		state.Err = err
		return state
	}
	tip, err := imagePinTip(tipGit, pin)
	if err != nil {
		state.Err = err
		return state
	}
	state.Tip = tip
	if pin.Name == "bd" {
		if err := imagePinBDFloor(ctx, cmp, owner, repo, tip); err != nil {
			state.Err = err
			return state
		}
		if pin.Commit != tip {
			lacksFloor, err := imagePinCommitLacksBDFloor(ctx, cmp, owner, repo, pin.Commit)
			if err != nil {
				state.Err = err
				return state
			}
			state.LacksFloor = lacksFloor
		}
	}
	if pin.Commit != tip {
		behind, err := imagePinBehind(ctx, cmp, pathCount, pin, owner, repo, tip)
		if err != nil {
			state.Err = err
			return state
		}
		state.Behind = behind
	}
	return state
}

// imagePinCommitLacksBDFloor asks the compare whether commit holds the samples
// build, in the direction imagePinBDFloor uses: the commit as base and the floor
// as head, so the count is the commits only the floor has. It reports that count
// as a flag where imagePinBDFloor returns an error for a floor the commit lacks,
// since this read records that answer on the pin's state. A compare that fails,
// one that answers no comparison, and a negative count are errors worded as
// imagePinBDFloor words them, naming the commit and the floor.
func imagePinCommitLacksBDFloor(ctx context.Context, cmp ImagePinCompare, owner, repo, commit string) (bool, error) {
	info, err := cmp.CompareCommits(ctx, owner, repo, commit, daemonimage.BDSamplesFloor)
	if err != nil {
		return false, daemonimage.CheckBDFloor(commit, 0, err)
	}
	if info == nil {
		return false, daemonimage.CheckBDFloor(commit, 0, errors.New("the compare answered no comparison"))
	}
	if info.TotalCommits < 0 {
		return false, daemonimage.CheckBDFloor(commit, info.TotalCommits, nil)
	}
	return info.TotalCommits > 0, nil
}

// parseImagePins expands the file's {owner} placeholder, as `gt daemon image
// pins` reads the file, and splits it into its pins. A file with no placeholder
// never calls ownerOf, so a reader that has no town to resolve the owner from
// still reads it.
//
// The shape checks are the pin-drift guard's parser's (daemonimage.ParsePins),
// restated here because that parser judges the file as a whole: it refuses the
// file, so a pin's URL failure cannot be that pin's error while the other pins
// read. The URL itself is not checked here: pinRepo checks and validates the one
// each pin names.
func parseImagePins(data []byte, ownerOf func() (string, error)) ([]daemonimage.Pin, error) {
	text := string(data)
	if strings.Contains(text, config.ForgeOwnerPlaceholder) {
		if ownerOf == nil {
			return nil, fmt.Errorf("image pins file holds %s, and nothing was given to expand it: name the town's forge.owner", config.ForgeOwnerPlaceholder)
		}
		owner, err := ownerOf()
		if err != nil {
			return nil, fmt.Errorf("image pins file: %w", err)
		}
		if text, err = config.ExpandForgeOwner(text, owner); err != nil {
			return nil, fmt.Errorf("image pins file: %w", err)
		}
	}
	var pins []daemonimage.Pin
	seen := make(map[string]bool, len(daemonimage.PinNames))
	for i, line := range strings.Split(text, "\n") {
		n := i + 1
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("image pins file: line %d: want \"<name> <url> <commit>\"", n)
		}
		name, rawURL, commit := fields[0], fields[1], fields[2]
		if !isImagePinName(name) {
			return nil, fmt.Errorf("image pins file: line %d: unknown pin name %q: the four names are %s", n, name, strings.Join(daemonimage.PinNames, ", "))
		}
		if seen[name] {
			return nil, fmt.Errorf("image pins file: line %d: duplicate pin name %q: each name is one line", n, name)
		}
		seen[name] = true
		if !pinShaForm.MatchString(commit) {
			return nil, fmt.Errorf("image pins file: line %d: %s commit %q is not a full 40-character lowercase hex sha", n, name, commit)
		}
		pins = append(pins, daemonimage.Pin{Name: name, URL: rawURL, Commit: commit})
	}
	var missing []string
	for _, name := range daemonimage.PinNames {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		return nil, fmt.Errorf("image pins file: missing pin for %s: the file must name each of %s once", strings.Join(missing, ", "), strings.Join(daemonimage.PinNames, ", "))
	}
	return pins, nil
}

// isImagePinName reports whether name is one of the four sources the image
// builds, the names daemonimage parses a pins file for.
func isImagePinName(name string) bool {
	for _, known := range daemonimage.PinNames {
		if name == known {
			return true
		}
	}
	return false
}

// pinRepo is the Forgejo owner and repository a pin's URL names, validated
// before either reaches a git call or the compare request's path. Every URL
// defect is this pin's error: a URL that does not parse, one that is not a
// credential-free http(s) URL, one whose path is not exactly one owner and one
// repository, and an owner or repository that is not one usable path segment.
// No refusal echoes the URL, which may hold the credential it refuses.
func pinRepo(pin daemonimage.Pin) (owner, repo string, err error) {
	u, err := url.Parse(pin.URL)
	if err != nil {
		return "", "", fmt.Errorf("image pins read: %s pin: its URL does not parse", pin.Name)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", fmt.Errorf("image pins read: %s pin: its URL is not http:// or https://", pin.Name)
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("image pins read: %s pin: its URL names no host", pin.Name)
	}
	if u.User != nil {
		return "", "", fmt.Errorf("image pins read: %s pin: its URL carries a credential: name the repository URL without one", pin.Name)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", "", fmt.Errorf("image pins read: %s pin: its URL carries a query or fragment: name the repository URL without one, since a token can sit there", pin.Name)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("image pins read: %s pin: its URL does not name exactly one owner and one repository", pin.Name)
	}
	owner, repo = parts[0], strings.TrimSuffix(parts[1], ".git")
	if err := config.ValidForgeOwner(owner); err != nil {
		return "", "", fmt.Errorf("image pins read: %s pin: its URL names no usable owner: %w", pin.Name, err)
	}
	if !pinRepoForm.MatchString(repo) {
		return "", "", fmt.Errorf("image pins read: %s pin: its URL names no usable repository: want letters, digits, '.', '_' or '-', starting with a letter or digit", pin.Name)
	}
	return owner, repo, nil
}

// imagePinTip reads one pin's source tip: the ref pinTipRef as tipGit's answer
// prints it. A call that fails, a line that is not that ref, an answer with no
// such ref or the same ref twice, and an answer whose sha is not full are all
// errors: a tip is never guessed.
func imagePinTip(tipGit PinTipGit, pin daemonimage.Pin) (string, error) {
	out, err := tipGit(pin.URL)
	if err != nil {
		return "", fmt.Errorf("image pins read: %s pin: cannot read %s of %s: %w", pin.Name, pinTipRef, pin.URL, err)
	}
	var tips []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 || fields[1] != pinTipRef {
			return "", fmt.Errorf("image pins read: %s pin: cannot read %s of %s: git answered a line that is not \"<sha> %s\"", pin.Name, pinTipRef, pin.URL, pinTipRef)
		}
		tips = append(tips, fields[0])
	}
	switch {
	case len(tips) == 0:
		return "", fmt.Errorf("image pins read: %s pin: %s of %s named no commit", pin.Name, pinTipRef, pin.URL)
	case len(tips) > 1:
		return "", fmt.Errorf("image pins read: %s pin: %s of %s was answered %d times: want one commit", pin.Name, pinTipRef, pin.URL, len(tips))
	case !pinShaForm.MatchString(tips[0]):
		return "", fmt.Errorf("image pins read: %s pin: %s of %s answered %q, not a full 40-character lowercase hex sha", pin.Name, pinTipRef, pin.URL, tips[0])
	}
	return tips[0], nil
}

// imagePinBehind counts the commits the tip has and the pin does not: through
// pathCount, limited to the inputs the image builds the pin's source from, for a
// pin with such a list; through the compare API, which cannot name the paths a
// commit changed, for a pin without one. A failed count, a missing pathCount
// where a pin needs one, an answer with no body, and a negative count are
// errors: the reader reports a count it was given, never the zero a healthy pin
// reports.
func imagePinBehind(ctx context.Context, cmp ImagePinCompare, pathCount PinPathCount, pin daemonimage.Pin, owner, repo, tip string) (int, error) {
	if paths := daemonimage.PinInputPaths(pin.Name); len(paths) != 0 {
		if pathCount == nil {
			return 0, fmt.Errorf("image pins read: %s pin %s: no path count to count the commits it trails %s by that touch its inputs", pin.Name, sha12(pin.Commit), pinTipRef)
		}
		n, err := pathCount(pin.URL, pin.Commit, tip, paths)
		if err != nil {
			return 0, fmt.Errorf("image pins read: %s pin %s: cannot count the commits it trails %s of %s that touch its inputs: %w", pin.Name, sha12(pin.Commit), pinTipRef, pin.URL, err)
		}
		if n < 0 {
			return 0, fmt.Errorf("image pins read: %s pin %s: counting the commits it trails %s by that touch its inputs answered %d, which is not a count", pin.Name, sha12(pin.Commit), pinTipRef, n)
		}
		return n, nil
	}
	info, err := cmp.CompareCommits(ctx, owner, repo, pin.Commit, tip)
	if err != nil {
		return 0, fmt.Errorf("image pins read: %s pin %s: cannot compare it with %s of %s/%s: %w", pin.Name, sha12(pin.Commit), pinTipRef, owner, repo, err)
	}
	if info == nil {
		return 0, fmt.Errorf("image pins read: %s pin %s: comparing it with %s of %s/%s answered no comparison", pin.Name, sha12(pin.Commit), pinTipRef, owner, repo)
	}
	if info.TotalCommits < 0 {
		return 0, fmt.Errorf("image pins read: %s pin %s: comparing it with %s of %s/%s answered %d commits, which is not a count", pin.Name, sha12(pin.Commit), pinTipRef, owner, repo, info.TotalCommits)
	}
	return info.TotalCommits, nil
}
