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
	"time"

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

	// imagePinsRefreshGateBD and imagePinsRefreshAcceptanceBD replace the Gate
	// and the acceptance list once bd is among the pins: a bd move also sets the
	// bdPin constant the ctxlabels test holds (gt-h51lnb).
	imagePinsRefreshGateBD = "go test ./internal/daemonimage -count=1 passes, and go test ./internal/ci/ctxlabels -count=1 passes."

	imagePinsRefreshAcceptanceBD = "- [ ] deploy/daemon/image-pins and deploy/polecat/seed-pins name each source tip named in this bead's Goal, and git diff shows only those commit lines and the bdPin constant changed\n" +
		"- [ ] go test ./internal/daemonimage -count=1 passes"

	// imagePinsRefreshOverseerHeading labels the apply command. It is the
	// overseer's route, not the seat's: it needs the four source checkouts,
	// which a sandbox seat does not have, so the goal names it an alternative
	// rather than the instruction (gt-xuntgv).
	imagePinsRefreshOverseerHeading = "Overseer alternative: needs the four source checkouts, which a seat does not have, and moves only pins 20 or more commits behind"

	// imagePinsRefreshCommand is the overseer's command. Every flag is
	// required by the command and every path must be absolute, so the block
	// names all six and leaves the four checkouts for the reader to fill in.
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

// imagePinsRefreshBead builds the refresh bead for the pins the planner named,
// read at filedAt, the patrol's clock. A pin with no name, commit or tip, or a
// zero filing time, is refused, because a blank cell is a read that did not
// happen rather than a value to print.
func imagePinsRefreshBead(pins []ImagePinState, filedAt time.Time) (imagePinsRefreshDraft, error) {
	if len(pins) == 0 {
		return imagePinsRefreshDraft{}, errors.New("image pins refresh bead: the planner named no pin: there is nothing for a polecat to move")
	}
	if filedAt.IsZero() {
		return imagePinsRefreshDraft{}, errors.New("image pins refresh bead: the filing time is zero: the bead says when the tips were read, and a zero time would print as 0001-01-01")
	}
	for _, pin := range pins {
		if err := checkImagePinState(pin); err != nil {
			return imagePinsRefreshDraft{}, err
		}
	}
	return imagePinsRefreshDraft{
		Title:       imagePinsRefreshTitle(pins),
		Description: imagePinsRefreshDescription(pins, filedAt),
		Acceptance:  imagePinsRefreshAcceptanceFor(pins),
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
// recognizable in a list without opening it. A pin with an input list and a
// Total names both counts (gt-a682mo).
func imagePinsRefreshTitle(pins []ImagePinState) string {
	counts := make([]string, len(pins))
	for i, pin := range pins {
		counts[i] = fmt.Sprintf("%s by %d commits", pin.Name, pin.Behind)
		if pinHasInputCount(pin) {
			counts[i] = fmt.Sprintf("%s by %d touching its inputs of %d", pin.Name, pin.Behind, pin.Total)
		}
	}
	return "image-pins refresh: " + joinNaming(counts) + " behind origin/main"
}

// imagePinsRefreshDescription is the six sections the shape lint reads, in
// template order, plus the command the polecat runs.
func imagePinsRefreshDescription(pins []ImagePinState, filedAt time.Time) string {
	return strings.Join([]string{
		"## Goal\n\n" + imagePinsRefreshGoal(pins, filedAt),
		"## Constraints\n\n" + imagePinsRefreshConstraintsFor(pins),
		"## Out of scope\n\n" + imagePinsRefreshOutOfScope,
		"## Gate\n\n" + imagePinsRefreshGateFor(pins),
		"## Size\n\n" + imagePinsRefreshSize,
		"## Acceptance\n\n" + imagePinsRefreshAcceptanceFor(pins),
	}, "\n\n") + "\n"
}

// imagePinsHasBD reports whether the planner named bd, the one pin whose bump
// also moves the bdPin constant (gt-h51lnb).
func imagePinsHasBD(pins []ImagePinState) bool {
	return slices.ContainsFunc(pins, func(pin ImagePinState) bool { return pin.Name == "bd" })
}

// imagePinsRefreshGateFor is the Gate section for pins.
func imagePinsRefreshGateFor(pins []ImagePinState) string {
	if imagePinsHasBD(pins) {
		return imagePinsRefreshGateBD
	}
	return imagePinsRefreshGate
}

// imagePinsRefreshAcceptanceFor is the acceptance list for pins, in the
// description's section and the bead's field alike.
func imagePinsRefreshAcceptanceFor(pins []ImagePinState) string {
	if imagePinsHasBD(pins) {
		return imagePinsRefreshAcceptanceBD
	}
	return imagePinsRefreshAcceptance
}

// imagePinsSeedLines names the line in deploy/polecat/seed-pins that pins the
// same repository a pin names: the seed calls bd "beads". om and hm are seeded
// from no line.
var imagePinsSeedLines = map[string]string{
	"bd":     "beads",
	"devops": "devops",
}

// imagePinsAgeDays rounds a pin's age to whole days, the unit the goal states
// it in: the age is read from committer times, so hours carry no meaning to the
// seat. A half day rounds up, so the text never understates how near the bound
// the pin is.
func imagePinsAgeDays(age time.Duration) int {
	return int((age + 12*time.Hour) / (24 * time.Hour))
}

// imagePinsAgeBoundDays is the drift guard's age bound in whole days, so the
// goal names the bound the guard refuses a pin over.
const imagePinsAgeBoundDays = int(daemonimage.MaxPinAge / (24 * time.Hour))

// imagePinsRefreshGoal names each pin's pinned commit and source tip, the
// instant the tips were read in UTC (a source moves about twenty commits an
// hour, gt-fcsoy8), and the commit columns a seat edits to move it by hand. A
// pin whose commit is at or past imagePinsFileAge says how old it is, because
// its commit count alone does not show how near the guard's age bound it sits
// (gt-yroxx2).
// The apply command, which needs checkouts a seat does not have, follows under
// its own heading and is never the instruction.
func imagePinsRefreshGoal(pins []ImagePinState, filedAt time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "These image pins trail their sources' origin/main, and the pin-drift guard refuses an image roll once a pin is more than %d commits behind its tip, or more than %d days older (gt-wmoa0d). A seat has no source checkout, so set each pin named here by hand.\n\n", daemonimage.MaxPinBehind, imagePinsAgeBoundDays)
	fmt.Fprintf(&b, "These tips were read at %s from each source's origin/main. A source that has moved since is fine: pin the tip named here.\n\n", filedAt.UTC().Format(time.RFC3339))
	for _, pin := range pins {
		line := fmt.Sprintf("- %s: pinned at %s; its source's origin/main is %s, %s", pin.Name, pin.Pin, pin.Tip, pinBehindPhrase(pin))
		if pin.Age >= imagePinsFileAge {
			line += fmt.Sprintf(", and its commit is %d days older than its tip, near the guard's %d-day bound", imagePinsAgeDays(pin.Age), imagePinsAgeBoundDays)
		}
		line += fmt.Sprintf(". Set the %s commit column in deploy/daemon/image-pins", pin.Name)
		if seed, ok := imagePinsSeedLines[pin.Name]; ok {
			line += fmt.Sprintf(" and the %s commit column in deploy/polecat/seed-pins", seed)
		}
		if pin.Name == "bd" {
			line += ", and set the `bdPin` constant in internal/ci/ctxlabels/ctxlabels_test.go to the same commit"
		}
		b.WriteString(line + ".\n")
	}
	b.WriteString("\nSet each commit column named above to its tip, then run `go test ./internal/daemonimage -run TestCommittedPinsAgree -count=1` and the Gate's command.\n\n")
	fmt.Fprintf(&b, "### %s\n\n", imagePinsRefreshOverseerHeading)
	b.WriteString("A machine holding the four source checkouts can move every pin the command accepts, at once; it is not the instruction here.\n\n")
	b.WriteString(imagePinsRefreshCommand)
	b.WriteString("\n\nThe four checkouts are the town's checkouts of those sources; the command reads each one's origin/main and writes neither. Without --apply it only prints the move.")
	return b.String()
}

// The Constraints lines a move must not cross. With bd, the diff line widens and
// one ancestry line is added (gt-5m0vrz).
const (
	imagePinsRefreshConstraintDiff = "- The diff is the commit columns of deploy/daemon/image-pins and deploy/polecat/seed-pins; nothing else in either file changes.\n"

	imagePinsRefreshConstraintDiffBD = "- The diff is the commit columns of deploy/daemon/image-pins and deploy/polecat/seed-pins and, for bd, the one `bdPin` constant; nothing else in either file changes.\n"

	imagePinsRefreshConstraintShared = "- A source the two files share must name the same commit in both, or TestCommittedPinsAgree fails.\n" +
		"- A pin that is not an ancestor of its tip (ahead or diverged) is not a pin to edit: stop and escalate, and leave the guard's 40-commit and 7-day bounds alone.\n" +
		"- Pin a source's tip even when that source's own CI is red: the guard compares commits, not runs."

	imagePinsRefreshConstraintBDTip = "\n- For bd, a tip that lacks the samples build never reaches this bead: the daemon refused it (gt-5m0vrz), so the seat checks no ancestry for bd."
)

// imagePinsRefreshConstraintsFor is the Constraints section for pins.
func imagePinsRefreshConstraintsFor(pins []ImagePinState) string {
	if imagePinsHasBD(pins) {
		return imagePinsRefreshConstraintDiffBD + imagePinsRefreshConstraintShared + imagePinsRefreshConstraintBDTip
	}
	return imagePinsRefreshConstraintDiff + imagePinsRefreshConstraintShared
}

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

// imagePinsFileRefreshBead builds the refresh bead for pins read at filedAt and
// files it through create, returning the new bead's id. A refused set of pins,
// and a zero filing time, never reach create. A failed create returns its error
// and no id, so nothing is recorded for a bead that was not made.
func imagePinsFileRefreshBead(create imagePinsBeadCreate, pins []ImagePinState, filedAt time.Time) (string, error) {
	draft, err := imagePinsRefreshBead(pins, filedAt)
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
