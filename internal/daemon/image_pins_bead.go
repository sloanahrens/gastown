package daemon

// The shape of the refresh bead the image pin patrol files (gt-wmoa0d). The
// spec dispatcher holds a bead it cannot shape (gt-1lzy1), so the whole text is
// built here from the pins the planner named, and one polecat lands the two pins
// files. The bead lookup and the bead creation that carry a draft are injected
// seams (gt-6bav5t).

import (
	"errors"
	"fmt"
	"strings"
)

const (
	// imagePinsRefreshLabel marks a filed refresh bead: the lookup the filer
	// adds finds the one already open by it (gt-6bav5t).
	imagePinsRefreshLabel = "image-pins-refresh"

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
// read, so one of its three fields is blank only when that read did not run.
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
