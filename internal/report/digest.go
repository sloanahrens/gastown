package report

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/question"
)

// The overseer digest: the status the overseer used to write by hand every
// fifteen minutes, assembled from the town's own records. Everything in it is
// a read; the text is deterministic for a given set of answers from its
// sources, so a fixture produces the same bytes every time.

// Row caps keep the digest under DigestMaxLines whatever the town holds; a
// section past its cap says how many rows it left out.
const (
	digestDoneRows     = 12
	digestWorkingRows  = 10
	digestNextRows     = 3
	digestHeldRows     = 6
	digestQuestionRows = 6

	// DigestMaxLines is the most lines the digest runs to without the hour's
	// sections.
	DigestMaxLines = 60

	// digestTitleMax caps a bead title in runes. The digest carries a title
	// to say what a bead is, never the bead's description.
	digestTitleMax = 60
)

// SeatWork is a polecat seat that holds a bead. Since is when the bead was
// slung to it, zero when that is not known.
type SeatWork struct {
	Rig, Polecat, Bead string
	Since              time.Time
}

// DispatchLeft is the spec dispatcher's dry run, sorted for the digest: Next
// are the beads it would dispatch in order, as a rig and a sentence, and Held
// are the beads it skips for a reason other than a full seat, with that
// reason. Hold is the reason the whole dispatcher is parked, or empty.
type DispatchLeft struct {
	Hold string
	Next []DispatchRow
	Held []DispatchRow
}

// DispatchRow is one bead the dry run named.
type DispatchRow struct {
	Bead, Rig, Text string
}

// Digest is one overseer digest: the window it covers and the sources it
// reads. A source that errors becomes one unavailable line in its section;
// the digest is still produced.
type Digest struct {
	// Now is when the digest is built, for ages; Slot is the start of the
	// interval it reports on and Since the start of the previous one.
	Now, Slot, Since time.Time

	// Rigs lists the registered rigs; Store opens one store's beads client,
	// "hq" being the town's.
	Rigs  func() ([]string, error)
	Store func(store string) (BeadStore, error)
	// Seats lists the polecat seats that are alive and hold a bead.
	Seats func() ([]SeatWork, error)
	// Left is the dispatcher's dry run.
	Left func() (DispatchLeft, error)
	// Health is the town health line.
	Health func() (string, error)
}

// Render returns the digest text, ending in a newline.
func (d Digest) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "overseer report %s (covers %s to %s)\n",
		d.Slot.UTC().Format(time.RFC3339), d.Since.UTC().Format(time.RFC3339), d.Slot.UTC().Format(time.RFC3339))

	rigs, rigsErr := d.Rigs()
	stores := append([]string{"hq"}, rigs...)

	section(&b, "DONE", d.done(stores, rigsErr))
	section(&b, "IN PROGRESS", d.working())
	section(&b, "LEFT", d.left())
	section(&b, "OPEN QUESTIONS", d.questions())

	b.WriteString("\nhealth: ")
	line, err := d.Health()
	if err != nil {
		line = unavailable(err)
	}
	b.WriteString(Scrub(line))
	b.WriteString("\n")
	return b.String()
}

// section writes a heading and its rows, "(none)" for no rows.
func section(b *strings.Builder, name string, rows []string) {
	fmt.Fprintf(b, "\n%s\n", name)
	if len(rows) == 0 {
		b.WriteString("  (none)\n")
		return
	}
	for _, r := range rows {
		b.WriteString("  " + r + "\n")
	}
}

// unavailable is the one line a failed source costs its section.
func unavailable(err error) string {
	return "unavailable (" + Scrub(err.Error()) + ")"
}

// capRows cuts rows to n and appends the count it left out.
func capRows(rows []string, n int) []string {
	if len(rows) <= n {
		return rows
	}
	return append(rows[:n:n], fmt.Sprintf("+%d more", len(rows)-n))
}

// doneRow is a bead closed in the window.
type doneRow struct {
	closed time.Time
	id     string
	line   string
}

// done lists the beads closed in [Since, Slot) across the town's stores. A
// store that cannot be read is named in one line; the others still count.
func (d Digest) done(stores []string, rigsErr error) []string {
	if rigsErr != nil {
		return []string{unavailable(rigsErr)}
	}
	var rows []doneRow
	var failed []string
	for _, name := range stores {
		s, err := d.Store(name)
		if err == nil {
			var issues []*beads.Issue
			issues, err = s.List(beads.ListOptions{Status: "closed", ClosedAfter: d.Since, Priority: -1})
			for _, is := range issues {
				closed, ok := reportParseBeadTime(is.ClosedAt)
				if !ok || closed.Before(d.Since) || !closed.Before(d.Slot) {
					continue
				}
				rows = append(rows, doneRow{closed: closed, id: is.ID,
					line: fmt.Sprintf("%s  %s  %s", is.ID, Title(is.Title), name)})
			}
		}
		if err != nil {
			failed = append(failed, name+": "+Scrub(err.Error()))
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].closed.Equal(rows[j].closed) {
			return rows[i].closed.Before(rows[j].closed)
		}
		return rows[i].id < rows[j].id
	})
	lines := make([]string, len(rows))
	for i, r := range rows {
		lines[i] = r.line
	}
	lines = capRows(lines, digestDoneRows)
	if len(failed) > 0 {
		lines = append(lines, "unavailable ("+strings.Join(failed, "; ")+")")
	}
	return lines
}

// working lists the seats that hold a bead, with the bead's title and how
// long the seat has had it.
func (d Digest) working() []string {
	seats, err := d.Seats()
	if err != nil {
		return []string{unavailable(err)}
	}
	sort.Slice(seats, func(i, j int) bool {
		if seats[i].Rig != seats[j].Rig {
			return seats[i].Rig < seats[j].Rig
		}
		return seats[i].Polecat < seats[j].Polecat
	})
	lines := make([]string, 0, len(seats))
	for _, s := range seats {
		title := "?"
		if st, err := d.Store(s.Rig); err == nil {
			if is, err := st.Show(s.Bead); err == nil && is != nil {
				title = Title(is.Title)
			}
		}
		lines = append(lines, fmt.Sprintf("%s/%s  %s  %s  %s", s.Rig, s.Polecat, s.Bead, title, minutes(d.Now, s.Since)))
	}
	return capRows(lines, digestWorkingRows)
}

// minutes is how long since the seat got its bead, "?" when unknown.
func minutes(now, since time.Time) string {
	if since.IsZero() {
		return "?m"
	}
	m := int(now.Sub(since) / time.Minute)
	if m < 0 {
		m = 0
	}
	return fmt.Sprintf("%dm", m)
}

// shortAge renders a wait the way a question's age reads best: whole days and
// hours past a day, hours and minutes past an hour, minutes below it. A
// negative wait reads as 0m.
func shortAge(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd%dh", int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour))
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	case d > 0:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return "0m"
}

// left lists the next beads the dispatcher would sling and the ones it holds
// for a reason that is not a full seat.
func (d Digest) left() []string {
	l, err := d.Left()
	if err != nil {
		return []string{unavailable(err)}
	}
	if l.Hold != "" {
		return []string{"dispatch held: " + Scrub(l.Hold)}
	}
	var lines []string
	for i, r := range l.Next {
		if i == digestNextRows {
			break
		}
		lines = append(lines, "next  "+dispatchRow(r))
	}
	var held []string
	for _, r := range l.Held {
		held = append(held, "held  "+dispatchRow(r))
	}
	return append(lines, capRows(held, digestHeldRows)...)
}

func dispatchRow(r DispatchRow) string {
	if r.Rig == "" {
		return fmt.Sprintf("%s  %s", r.Bead, Scrub(r.Text))
	}
	return fmt.Sprintf("%s  %s  %s", r.Bead, r.Rig, Scrub(r.Text))
}

// questions lists the overseer's open questions, oldest first: the id, how
// long it has waited, and the work it holds.
func (d Digest) questions() []string {
	s, err := d.Store("hq")
	if err != nil {
		return []string{unavailable(err)}
	}
	issues, err := s.List(question.ListOptions)
	if err != nil {
		return []string{unavailable(err)}
	}
	type row struct {
		asked time.Time
		id    string
		line  string
	}
	rows := make([]row, 0, len(issues))
	for _, is := range issues {
		asked := question.AskedAt(is)
		age := "?"
		if !asked.IsZero() {
			age = shortAge(d.Now.Sub(asked))
		}
		held := "none"
		if h := question.Parse(is).Held; len(h) > 0 {
			held = strings.Join(h, ",")
		}
		rows = append(rows, row{asked: asked, id: is.ID,
			line: fmt.Sprintf("%s  %s  held: %s", is.ID, age, held)})
	}
	// Oldest first; a bead with no readable time sorts after every one that
	// has one, so a bad stamp cannot read as the longest wait.
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		switch {
		case a.asked.IsZero() != b.asked.IsZero():
			return b.asked.IsZero()
		case !a.asked.Equal(b.asked):
			return a.asked.Before(b.asked)
		}
		return a.id < b.id
	})
	lines := make([]string, len(rows))
	for i, r := range rows {
		lines[i] = r.line
	}
	return capRows(lines, digestQuestionRows)
}

// ReportsDir is where the overseer's reports live inside the town: one file
// per run, named for the instant it covers. The daemon's overseer_report
// patrol writes it and the dashboard's Report pane reads it.
func ReportsDir(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "overseer", "reports")
}

// Title is a bead title as the digest draws it: one line, no control
// characters, at most digestTitleMax runes, and scrubbed of anything that
// reads as a credential.
func Title(s string) string {
	s = strings.Join(strings.FieldsFunc(s, unicode.IsControl), " ")
	s = Scrub(strings.Join(strings.Fields(s), " "))
	if r := []rune(s); len(r) > digestTitleMax {
		return string(r[:digestTitleMax-1]) + "…"
	}
	return s
}

// secretRE finds the shapes a credential takes in text: a key=value or
// key: value pair whose key names one, a bearer header, and the well-known
// token prefixes.
var secretRE = regexp.MustCompile(`(?i)\b(?:api[_-]?key|token|secret|passw(?:or)?d|credential)s?\b\s*[:=]\s*\S+|\bbearer\s+\S+|\b(?:sk|ghp|gho|ghs|glpat|xox[abp])[-_][A-Za-z0-9_-]{12,}`)

// Scrub removes the credential-shaped parts of s and any line break. It is the
// last thing every dynamic string passes through, so an error message or a
// title that quotes a token never reaches the report.
func Scrub(s string) string {
	s = secretRE.ReplaceAllString(s, "[redacted]")
	return strings.Join(strings.Fields(s), " ")
}
