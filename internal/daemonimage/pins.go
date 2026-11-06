package daemonimage

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
)

// PinsPath is the image-pins file in the gt checkout, relative to the repo
// root: the one place the four external source commits are named.
const PinsPath = "deploy/daemon/image-pins"

// PinNames are the four external sources `gt daemon image` builds from, in the
// order the file is documented to name them.
var PinNames = []string{"bd", "om", "hm", "devops"}

// Pin is one external source: the name the build uses for it, the repository
// URL a later step fetches it from, and the commit it is built at.
type Pin struct {
	Name   string
	URL    string
	Commit string
}

// MaxPinBehind and MaxPinAge bound how far a pin may trail its source's
// origin/main: more commits behind, or an older commit relative to main's tip,
// is a stale pin the guard refuses. Forty commits is a day or two of a live
// rig's main; seven days is long enough that no active rig's pin sits there by
// accident.
const (
	MaxPinBehind = 40
	MaxPinAge    = 7 * 24 * time.Hour
)

// fullCommit is the form an image pin's commit takes: a full sha, so the pin
// names one commit and not an abbreviation that another commit can claim.
var fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ParsePins parses an image-pins file: one "<name> <url> <commit>" per line,
// with blank lines and "#" comments ignored. It refuses a file that does not
// name each of PinNames exactly once, with a full commit and a credential-free
// http(s) repository URL, and every refusal names its line.
func ParsePins(data []byte) ([]Pin, error) {
	var pins []Pin
	seen := make(map[string]bool, len(PinNames))
	for i, line := range strings.Split(string(data), "\n") {
		n := i + 1
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("line %d: want \"<name> <url> <commit>\"", n)
		}
		name, rawURL, commit := fields[0], fields[1], fields[2]
		if !isPinName(name) {
			return nil, fmt.Errorf("line %d: unknown pin name %q: the four names are %s", n, name, strings.Join(PinNames, ", "))
		}
		if seen[name] {
			return nil, fmt.Errorf("line %d: duplicate pin name %q: each name is one line", n, name)
		}
		seen[name] = true
		if err := checkPinURL(rawURL); err != nil {
			return nil, fmt.Errorf("line %d: %s pin: %w", n, name, err)
		}
		if !fullCommit.MatchString(commit) {
			return nil, fmt.Errorf("line %d: %s commit %q is not a full 40-character lowercase hex sha", n, name, commit)
		}
		pins = append(pins, Pin{Name: name, URL: rawURL, Commit: commit})
	}
	var missing []string
	for _, name := range PinNames {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) != 0 {
		return nil, fmt.Errorf("missing pin for %s: the file must name each of %s once", strings.Join(missing, ", "), strings.Join(PinNames, ", "))
	}
	return pins, nil
}

// isPinName reports whether name is one of the four sources the image builds.
func isPinName(name string) bool {
	for _, known := range PinNames {
		if name == known {
			return true
		}
	}
	return false
}

// checkPinURL refuses a URL that is not a credential-free http(s) repository
// URL: a pin is committed to the repository, so a credential in it is a leaked
// secret, and a scheme other than http(s) cannot be fetched by the step that
// later replaces the checkouts by URL. A credential may sit in the user-info, the
// query or the fragment, so a query or fragment is refused too: a repository URL
// names neither. No refusal echoes the URL, which may hold the credential it
// refuses.
func checkPinURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("url does not parse")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("url is not http:// or https://")
	}
	if u.Host == "" {
		return errors.New("url names no host")
	}
	if u.User != nil {
		return errors.New("url carries a credential: name the repository URL without one")
	}
	if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return errors.New("url carries a query or fragment: name the repository URL without one, since a token can sit there")
	}
	if u.Path == "" || u.Path == "/" {
		return errors.New("url names no repository path")
	}
	return nil
}

// MainOf returns the checkout whose origin/main a pin is compared with: the
// --<name>-src checkout the build takes as that source. An error is a pin with
// no checkout, never a guess.
type MainOf func(pin Pin) (dir string, err error)

// CheckPins is the pin-drift check: each pin is compared with its source
// checkout's origin/main, read through git and never fetched. A pin at main is
// fine; a pin that is not an ancestor of main, more than MaxPinBehind commits
// behind it, or whose commit is more than MaxPinAge older than main's tip is a
// "pin-drift guard:" error naming the rig, the pin, main and the line to edit.
// A pin inside both bounds passes with one note.
func CheckPins(pins []Pin, mainOf MainOf, pinsPath string, git bdbuild.Git) (notes []string, err error) {
	if git == nil {
		return nil, errors.New("pin-drift guard: no git to read origin/main with")
	}
	for _, pin := range pins {
		dir, err := mainOf(pin)
		if err != nil {
			return nil, fmt.Errorf("pin-drift guard: rig %s pin %s: %w", pin.Name, short(pin.Commit), err)
		}
		out, err := git(dir, "rev-parse", "--verify", "origin/main^{commit}")
		if err != nil {
			return nil, fmt.Errorf("pin-drift guard: cannot read origin/main of rig %s in %s (%v): fetch that checkout through its remote, then rerun", pin.Name, dir, err)
		}
		pinNotes, err := checkPinAgainstMain(git, dir, pin, strings.TrimSpace(string(out)), pinsPath)
		if err != nil {
			return nil, err
		}
		notes = append(notes, pinNotes...)
	}
	return notes, nil
}

// fetchTipDepth is the depth the pin-drift guard fetches a fetched source's main
// at: MaxPinBehind commits behind its tip, plus the tip itself, so a pin exactly
// at the bound is in the fetched history and one beyond it is not.
const fetchTipDepth = MaxPinBehind + 1

// checkFetchedPin is the pin-drift check for a source the build fetches by URL
// rather than reads from a checkout. dir was fetched at pin.Commit and holds no
// origin/main, so the guard first fetches that source's main tip from pin.URL
// into origin/main, through git, and then applies the rules CheckPins applies to
// a checkout. A tip fetch that fails and a tip that cannot be read are errors: a
// fetched pin with no main to compare against is refused, never passed. Its
// caller has refused a nil git, as FetchSource does before the fetch runs.
func checkFetchedPin(git bdbuild.Git, dir string, pin Pin, pinsPath string) error {
	if _, err := git(dir, "fetch", "--depth", strconv.Itoa(fetchTipDepth), pin.URL, "main:refs/remotes/origin/main"); err != nil {
		return fmt.Errorf("pin-drift guard: cannot fetch main of the %s pin from its pin URL: %w", pin.Name, err)
	}
	out, err := git(dir, "rev-parse", "--verify", "origin/main^{commit}")
	if err != nil {
		return fmt.Errorf("pin-drift guard: cannot read the main tip fetched for the %s pin: %w", pin.Name, err)
	}
	_, err = checkPinAgainstMain(git, dir, pin, strings.TrimSpace(string(out)), pinsPath)
	return err
}

// checkPinAgainstMain applies the three pin-drift rules to one pin: a pin at
// main passes silently; one that is not an ancestor of main, more than
// MaxPinBehind commits behind it, or whose commit is more than MaxPinAge older
// than main's tip is refused; one inside both bounds passes with one note.
func checkPinAgainstMain(git bdbuild.Git, dir string, pin Pin, main, pinsPath string) (notes []string, err error) {
	if pin.Commit == main {
		return nil, nil
	}
	if _, err := git(dir, "merge-base", "--is-ancestor", pin.Commit, main); err != nil {
		if !isNotAncestor(err) {
			return nil, fmt.Errorf("pin-drift guard: rig %s pin %s: cannot test whether it is an ancestor of main %s in %s (git merge-base --is-ancestor gave no not-ancestor answer): fetch or fix that checkout, then rerun", pin.Name, short(pin.Commit), short(main), dir)
		}
		return nil, fmt.Errorf("pin-drift guard: rig %s pin %s is not an ancestor of main %s (ahead or diverged): %s", pin.Name, short(pin.Commit), short(main), pinEdit(pin, pinsPath, main))
	}
	behind, err := commitsBehind(git, dir, pin.Commit, main)
	if err != nil {
		return nil, fmt.Errorf("pin-drift guard: rig %s pin %s: cannot count the commits to main %s in %s (%v): fetch or fix that checkout, then rerun", pin.Name, short(pin.Commit), short(main), dir, err)
	}
	if behind > MaxPinBehind {
		return nil, fmt.Errorf("pin-drift guard: rig %s pin %s is %d commits behind main %s, over the %d-commit bound: %s", pin.Name, short(pin.Commit), behind, short(main), MaxPinBehind, pinEdit(pin, pinsPath, main))
	}
	age, err := commitAge(git, dir, pin.Commit, main)
	if err != nil {
		return nil, fmt.Errorf("pin-drift guard: rig %s pin %s: cannot read the commit times against main %s in %s (%v): fetch or fix that checkout, then rerun", pin.Name, short(pin.Commit), short(main), dir, err)
	}
	if age > MaxPinAge {
		return nil, fmt.Errorf("pin-drift guard: rig %s pin %s is %d commits behind main %s, and its commit is %s older than main's tip, over the %s bound: %s", pin.Name, short(pin.Commit), behind, short(main), age.Round(time.Hour), MaxPinAge, pinEdit(pin, pinsPath, main))
	}
	return []string{fmt.Sprintf("pin-drift guard: rig %s pin %s is %d commits behind main %s, within the %d-commit and %s bounds", pin.Name, short(pin.Commit), behind, short(main), MaxPinBehind, MaxPinAge)}, nil
}

// pinEdit is the exact edit a refused pin needs, for the reader to paste from
// the failure.
func pinEdit(pin Pin, pinsPath, main string) string {
	return fmt.Sprintf("set the %s line in %s to %s, the commit origin/main names", pin.Name, pinsPath, main)
}

// isNotAncestor reports whether err is git's answer that a commit is not an
// ancestor of another. merge-base --is-ancestor exits 1 for that answer and with
// a different status when it cannot answer, such as an unknown commit, so only
// an exit status of 1 is the answer; an error with no status is a failure.
func isNotAncestor(err error) bool {
	var coded interface{ ExitCode() int }
	return errors.As(err, &coded) && coded.ExitCode() == 1
}

// commitsBehind counts the commits from pin to main; the caller has checked
// that pin is an ancestor of main.
func commitsBehind(git bdbuild.Git, dir, pin, main string) (int, error) {
	out, err := git(dir, "rev-list", "--count", pin+".."+main)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, errors.New("git answered the commit count with a non-number")
	}
	return n, nil
}

// commitAge is how much older pin's commit is than main's tip, from the two
// commits' times, so the check does not depend on when it runs.
func commitAge(git bdbuild.Git, dir, pin, main string) (time.Duration, error) {
	pinTime, err := commitTime(git, dir, pin)
	if err != nil {
		return 0, err
	}
	mainTime, err := commitTime(git, dir, main)
	if err != nil {
		return 0, err
	}
	return mainTime.Sub(pinTime), nil
}

// commitTime reads commit's committer time from git.
func commitTime(git bdbuild.Git, dir, commit string) (time.Time, error) {
	out, err := git(dir, "show", "-s", "--format=%ct", commit)
	if err != nil {
		return time.Time{}, err
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return time.Time{}, errors.New("git answered the commit time with a non-number")
	}
	return time.Unix(secs, 0), nil
}

// short is the abbreviation a refusal prints for a commit, so the full sha
// stays in the edit the reader pastes.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
