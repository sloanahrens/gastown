package daemon

// The shape of the refresh bead the image pin patrol files (gt-wmoa0d). The
// spec dispatcher holds a bead it cannot shape (gt-1lzy1), so the whole text is
// built here from the pins the planner named, and one polecat lands the two pins
// files.

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/daemonimage"
)

// pinSHAForm is the form a pin's commit and its source tip take: a full
// lowercase sha, as git prints it and daemonimage.ParsePins accepts it.
var pinSHAForm = regexp.MustCompile(`^[0-9a-f]{40}$`)

const (
	// imagePinsRefreshLabel marks a filed refresh bead: the lookup the filer
	// adds finds the one already open by it (gt-6bav5t).
	imagePinsRefreshLabel = "image-pins-refresh"

	// imagePinsRefreshPriority files a refresh bead at P0: once a pin crosses
	// its bound the refresh blocks every image roll, so it goes ahead of the
	// backlog. gt-xuntgv was filed at P2, waited behind that backlog while its
	// source moved, crossed the bound, and a roll was lost (gt-hc7fk3).
	imagePinsRefreshPriority = 0

	// imagePinsRefreshSize is the Size section: a bead that moved the pins is a
	// diff of a few commit columns, which one worker takes in one landing.
	imagePinsRefreshSize = "One worker, one landing."

	// imagePinsRefreshGate is the Gate section. The two files are data, so the
	// pin test, which also holds the two files to the same commit for a source
	// they share, is the whole gate.
	imagePinsRefreshGate = "go test ./internal/daemonimage -count=1 passes."

	// imagePinsRefreshAcceptance is the acceptance list, in the description's
	// section and in the bead's field alike: gt done reads the field while
	// `gt spec lint` reads the section, and the lint refuses two lists that
	// disagree (gt-hgt9j.2).
	imagePinsRefreshAcceptance = "- [ ] deploy/daemon/image-pins and deploy/polecat/seed-pins name each source tip named in this bead's Goal, and git diff shows only those commit lines changed\n" +
		"- [ ] go test ./internal/daemonimage -count=1 passes"

	// imagePinsRefreshCommand is the command the polecat runs. Every flag is
	// required by the command and every path must be absolute, so the block
	// names all six and leaves the four checkouts for the polecat to fill in.
	imagePinsRefreshCommand = "```\n" +
		"gt daemon image pins --apply \\\n" +
		"  --pins \"$PWD/deploy/daemon/image-pins\" \\\n" +
		"  --seed-pins \"$PWD/deploy/polecat/seed-pins\" \\\n" +
		"  --bd-src <absolute path of the beads checkout> \\\n" +
		"  --om-src <absolute path of the om checkout> \\\n" +
		"  --hm-src <absolute path of the hm checkout> \\\n" +
		"  --devops-src <absolute path of the devops checkout>\n" +
		"```"
)

// imagePinsRefreshDraft is the refresh bead a create call files: the title, the
// shaped description, the acceptance field and the labels.
type imagePinsRefreshDraft struct {
	Title       string
	Description string
	// Acceptance is the description's Acceptance section again, in the form
	// the bead's field takes.
	Acceptance string
	Labels     []string
}

// imagePinsRefreshBead builds the refresh bead for the pins the planner named.
// The description names each pin, the commit it is pinned at and its source's
// tip, and carries the refresh command with its four checkouts; a pin with no
// name, commit or tip is refused, because a blank cell is a read that did not
// happen rather than a pin to print.
func imagePinsRefreshBead(pins []ImagePinState) (imagePinsRefreshDraft, error) {
	if len(pins) == 0 {
		return imagePinsRefreshDraft{}, errors.New("image pins refresh bead: the planner named no pin: there is nothing for a polecat to move")
	}
	for _, pin := range pins {
		if err := checkImagePinState(pin); err != nil {
			return imagePinsRefreshDraft{}, err
		}
	}
	return imagePinsRefreshDraft{
		Title:       imagePinsRefreshTitle(pins),
		Description: imagePinsRefreshDescription(pins),
		Acceptance:  imagePinsRefreshAcceptance,
		Labels:      []string{imagePinsRefreshLabel},
	}, nil
}

// checkImagePinState refuses a pin the bead cannot name: the state comes from a
// read, so one of its three fields is blank only when that read did not run. The
// fields are written into the bead text, so each takes its canonical form too: a
// name or sha with a line break or markdown would open a section of the bead.
func checkImagePinState(pin ImagePinState) error {
	for _, field := range []struct{ name, value string }{
		{"name", pin.Name},
		{"pinned commit", pin.Pin},
		{"source tip", pin.Tip},
	} {
		if field.value == "" {
			return fmt.Errorf("image pins refresh bead: a pin the planner named has no %s (name %q, pinned commit %q, source tip %q): the bead names what the read returned", field.name, pin.Name, pin.Pin, pin.Tip)
		}
	}
	if !slices.Contains(daemonimage.PinNames, pin.Name) {
		return fmt.Errorf("image pins refresh bead: a pin the planner named has an unknown name %q: the four sources are %s", pin.Name, strings.Join(daemonimage.PinNames, ", "))
	}
	if !pinSHAForm.MatchString(pin.Pin) {
		return fmt.Errorf("image pins refresh bead: %s pinned commit %q is not a full 40-character lowercase hex sha", pin.Name, pin.Pin)
	}
	if !pinSHAForm.MatchString(pin.Tip) {
		return fmt.Errorf("image pins refresh bead: %s source tip %q is not a full 40-character lowercase hex sha", pin.Name, pin.Tip)
	}
	return nil
}

// imagePinsRefreshTitle names each pin with how far it trails, so the bead is
// recognizable in a list without opening it.
func imagePinsRefreshTitle(pins []ImagePinState) string {
	counts := make([]string, len(pins))
	for i, pin := range pins {
		counts[i] = fmt.Sprintf("%s by %d commits", pin.Name, pin.Behind)
	}
	return "image-pins refresh: " + joinNaming(counts) + " behind origin/main"
}

// imagePinsRefreshDescription is the six sections the shape lint reads, in
// template order, plus the command the polecat runs.
func imagePinsRefreshDescription(pins []ImagePinState) string {
	return strings.Join([]string{
		"## Goal\n\n" + imagePinsRefreshGoal(pins),
		"## Constraints\n\n" + imagePinsRefreshConstraints,
		"## Out of scope\n\n" + imagePinsRefreshOutOfScope,
		"## Gate\n\n" + imagePinsRefreshGate,
		"## Size\n\n" + imagePinsRefreshSize,
		"## Acceptance\n\n" + imagePinsRefreshAcceptance,
	}, "\n\n") + "\n"
}

// imagePinsRefreshGoal names each pin's pinned commit and source tip, then the
// command that moves them.
func imagePinsRefreshGoal(pins []ImagePinState) string {
	var b strings.Builder
	b.WriteString("These image pins trail their sources' origin/main, and the pin-drift guard refuses an image roll once a pin is more than 40 commits behind its tip (gt-wmoa0d). Move each pin named here to its source tip and land the two pins files the refresh writes.\n\n")
	for _, pin := range pins {
		fmt.Fprintf(&b, "- %s: pinned at %s; its source's origin/main is %s, %d commits behind.\n", pin.Name, pin.Pin, pin.Tip, pin.Behind)
	}
	b.WriteString("\nRun this at the root of the gt checkout this bead lands on, naming the four source checkouts by absolute path:\n\n")
	b.WriteString(imagePinsRefreshCommand)
	b.WriteString("\n\nThe four checkouts are the town's checkouts of those sources; the command reads each one's origin/main and writes neither. Without --apply it only prints the move.")
	return b.String()
}

// imagePinsRefreshConstraints are the lines a move must not cross: the two
// files stay in step, and a pin the command refuses is not edited into place.
const imagePinsRefreshConstraints = "- The diff is the commit columns of deploy/daemon/image-pins and deploy/polecat/seed-pins; nothing else in either file changes.\n" +
	"- A source the two files share must name the same commit in both, or TestCommittedPinsAgree fails.\n" +
	"- A pin the command refuses (not an ancestor of its tip: ahead or diverged) is not a pin to edit: stop and escalate with the command's message, and leave the guard's 40-commit and 7-day bounds alone.\n" +
	"- Pin a source's tip even when that source's own CI is red: the guard compares commits, not runs."

// imagePinsRefreshOutOfScope keeps a polecat from widening a pins diff.
const imagePinsRefreshOutOfScope = "The pins this bead does not name, the pin-drift guard's bounds, and the daemon that files this bead."

// joinNaming joins parts as "a", "a and b" or "a, b and c", and "" for none.
func joinNaming(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// imagePinsBeadList is the bead lookup the refresh filer reads through: the
// beads opts selects, in whatever status they are. beads.Client.List satisfies
// it.
type imagePinsBeadList func(opts beads.ListOptions) ([]*beads.Issue, error)

// imagePinsBeadCreate files one bead and returns it. beads.Client.Create
// satisfies it.
type imagePinsBeadCreate func(opts beads.CreateOptions) (*beads.Issue, error)

// imagePinsOpenRefreshBead finds the open refresh bead the planner reads, with
// the time it was opened, which is the age the planner measures. It returns nil
// when no refresh bead is open, and skips closed ones. Two open refresh beads
// are refused: the planner holds one at a time, so a second is a state the
// filer cannot name. An open bead whose created time does not parse is refused
// too, rather than handed to the planner with a zero age.
func imagePinsOpenRefreshBead(list imagePinsBeadList) (*ImagePinRefreshBead, error) {
	issues, err := list(beads.ListOptions{Label: imagePinsRefreshLabel, Status: "all"})
	if err != nil {
		return nil, fmt.Errorf("image pins refresh bead: listing the %s beads: %w", imagePinsRefreshLabel, err)
	}
	var open *ImagePinRefreshBead
	for _, is := range issues {
		if beads.IssueStatus(is.Status) == beads.StatusClosed {
			continue
		}
		openedAt := beads.ParseIssueTime(is.CreatedAt)
		if openedAt.IsZero() {
			return nil, fmt.Errorf("image pins refresh bead: %s has a created time %q that does not parse: its age is unknown", is.ID, is.CreatedAt)
		}
		if open != nil {
			return nil, fmt.Errorf("image pins refresh bead: %s and %s are both open: the planner holds one refresh bead at a time", open.ID, is.ID)
		}
		open = &ImagePinRefreshBead{ID: is.ID, OpenedAt: openedAt}
	}
	return open, nil
}

// imagePinsFileRefreshBead builds the refresh bead for pins and files it through
// create, returning the new bead's id. A refused set of pins never reaches
// create. A failed create returns its error and no id, so nothing is recorded
// for a bead that was not made.
func imagePinsFileRefreshBead(create imagePinsBeadCreate, pins []ImagePinState) (string, error) {
	draft, err := imagePinsRefreshBead(pins)
	if err != nil {
		return "", err
	}
	issue, err := create(beads.CreateOptions{
		Title:       draft.Title,
		Description: draft.Description,
		Acceptance:  draft.Acceptance,
		Labels:      draft.Labels,
		Priority:    imagePinsRefreshPriority,
	})
	if err != nil {
		return "", fmt.Errorf("image pins refresh bead: filing %q: %w", draft.Title, err)
	}
	if issue == nil || issue.ID == "" {
		return "", fmt.Errorf("image pins refresh bead: filing %q returned no bead id", draft.Title)
	}
	return issue.ID, nil
}
