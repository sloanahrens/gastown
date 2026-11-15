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

// ImagePinRead is one image pin's read: the pin as the file names it, its
// source's origin/main tip, and how many commits the pin trails that tip.
type ImagePinRead struct {
	Name   string
	Pin    string
	Tip    string
	Behind int
}

// ImagePinCompare is the Forgejo read the tip reader makes: the comparison that
// counts the commits head has and base does not.
type ImagePinCompare interface {
	CompareCommits(ctx context.Context, owner, repo, base, head string) (*forgejo.CompareInfo, error)
}

// PinTipGit answers what `git ls-remote --refs <url> refs/heads/main` prints for
// a source repository: the injected seam that keeps the reader from running
// git, so a test supplies a fake and no unit test reaches a network.
type PinTipGit func(url string) ([]byte, error)

// pinTipRef is the ref an image pin's source tip is read from.
const pinTipRef = "refs/heads/main"

// pinTipForm is the form an ls-remote tip takes: a full sha, so a pin compared
// with it names one commit.
var pinTipForm = regexp.MustCompile(`^[0-9a-f]{40}$`)

// pinRepoForm is the shape of a Forgejo repository name, the same shape an
// owner takes: one path segment, so it cannot traverse out of the API path.
var pinRepoForm = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ReadImagePinTips reads every pin in an image-pins file: its source's
// origin/main tip through tipGit, and how many commits the pin trails that tip
// through cmp, which is not called for a pin already at its tip. ownerOf names
// the Forgejo owner a {owner} placeholder expands to and is called only when
// the file holds one; the owner and repository the comparison addresses are
// read from the expanded pin URL. Any read that fails is an error naming the
// pin, never a pin reported with a guessed answer.
func ReadImagePinTips(ctx context.Context, data []byte, ownerOf func() (string, error), tipGit PinTipGit, cmp ImagePinCompare) ([]ImagePinRead, error) {
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
	reads := make([]ImagePinRead, 0, len(pins))
	for _, pin := range pins {
		owner, repo, err := pinRepo(pin)
		if err != nil {
			return nil, err
		}
		tip, err := imagePinTip(tipGit, pin)
		if err != nil {
			return nil, err
		}
		behind := 0
		if pin.Commit != tip {
			if behind, err = imagePinBehind(ctx, cmp, pin, owner, repo, tip); err != nil {
				return nil, err
			}
		}
		reads = append(reads, ImagePinRead{Name: pin.Name, Pin: pin.Commit, Tip: tip, Behind: behind})
	}
	return reads, nil
}

// parseImagePins expands the file's {owner} placeholder, as `gt daemon image
// pins` reads the file, and parses the result with the pin-drift guard's parser.
// A file with no placeholder never calls ownerOf, so a reader that has no town
// to resolve the owner from still reads it.
func parseImagePins(data []byte, ownerOf func() (string, error)) ([]daemonimage.Pin, error) {
	text := string(data)
	expanded := text
	if strings.Contains(text, config.ForgeOwnerPlaceholder) {
		if ownerOf == nil {
			return nil, fmt.Errorf("image pins file holds %s, and nothing was given to expand it: name the town's forge.owner", config.ForgeOwnerPlaceholder)
		}
		owner, err := ownerOf()
		if err != nil {
			return nil, fmt.Errorf("image pins file: %w", err)
		}
		if expanded, err = config.ExpandForgeOwner(text, owner); err != nil {
			return nil, fmt.Errorf("image pins file: %w", err)
		}
	}
	pins, err := daemonimage.ParsePins([]byte(expanded))
	if err != nil {
		return nil, fmt.Errorf("image pins file: %w", err)
	}
	return pins, nil
}

// pinRepo is the Forgejo owner and repository a pin's URL names, validated
// before either reaches the compare request's path. ParsePins has already
// refused a URL that is not http(s) with a path; the path must still be exactly
// one owner and one repository, each one usable path segment.
func pinRepo(pin daemonimage.Pin) (owner, repo string, err error) {
	var parts []string
	if u, err := url.Parse(pin.URL); err == nil {
		parts = strings.Split(strings.Trim(u.Path, "/"), "/")
	}
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
// prints it. A call that fails, an answer with no such ref, and an answer whose
// sha is not full are all errors: a tip is never guessed.
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
	case !pinTipForm.MatchString(tips[0]):
		return "", fmt.Errorf("image pins read: %s pin: %s of %s answered %q, not a full 40-character lowercase hex sha", pin.Name, pinTipRef, pin.URL, tips[0])
	}
	return tips[0], nil
}

// imagePinBehind counts the commits the tip has and the pin does not, the
// compare API's answer with the pin as base and the tip as head. A compare that
// fails, an answer with no body, and a negative count are errors: the reader
// reports a count it was given, never the zero a healthy pin reports.
func imagePinBehind(ctx context.Context, cmp ImagePinCompare, pin daemonimage.Pin, owner, repo, tip string) (int, error) {
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
