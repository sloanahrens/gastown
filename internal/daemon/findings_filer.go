package daemon

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The findings_filer patrol (gt-rws9m): every interval it reads the FINDING:
// comments gt done recorded on the beads that closed, deferred or hooked in
// the last day and files one Found bead per comment in the rig that owns it,
// then marks the finding filed so the next pass files nothing for it. The
// polecat reports the defect through gt done and never runs the create.
//
// A pass files into rig databases, so the patrol is off unless
// patrols.findings_filer.enabled is true, and one pass files at most
// max_per_run. A finding too thin to shape is filed unshaped with a SHAPE
// note; the patrol raises no attention item, because the Found bead is the
// record.

const (
	// findingsFilerMarker is the comment's first line, as gt done writes it.
	findingsFilerMarker = "FINDING:"
	// findingsFilerFiledMarker is the first line of the comment a pass leaves
	// on the source bead once the Found bead exists.
	findingsFilerFiledMarker = "FINDING-FILED:"
	// findingsFilerActor is the author of the mark, so a reader follows the
	// filing back to the patrol.
	findingsFilerActor = "daemon/findings-filer"
	// findingsFilerWindow is how far back a pass looks for changed beads: a
	// day, so a finding recorded just before a daemon restart is still read.
	findingsFilerWindow = 24 * time.Hour
	// findingsFilerScanStatuses are the statuses a source bead can hold when
	// a finding is recorded: closed by the landing, deferred by the polecat,
	// or hooked while the work is still held.
	findingsFilerScanStatuses = "closed,deferred,hooked"
	// findingsFilerTitleLen is where the finding's first line is cut for the
	// new bead's title.
	findingsFilerTitleLen = 120
	// findingsFilerBdTimeout bounds one bd call the pass makes.
	findingsFilerBdTimeout = 30 * time.Second
	// findingsFilerWithoutSubject is the title a finding with no text gets.
	findingsFilerWithoutSubject = "(the finding names no defect)"
	// findingsFilerEmptyGoal is what a finding with no text says under Goal.
	findingsFilerEmptyGoal = "(the finding carried no text beyond its rig line)"
	// findingsFilerEmptyNote is the SHAPE note such a finding is filed with:
	// the dispatcher would hold a bead whose Goal names nothing to fix, and
	// the finding is too thin to shape into one that does not.
	findingsFilerEmptyNote = "SHAPE: ## Goal: the finding carries no text beyond its rig line"
)

// findingsFilerPriority is the priority of a filed finding, raised when the
// finding's subject says P1.
const (
	findingsFilerPriority = 3
	findingsFilerP1       = 1
)

// findingsFilerP1Re matches the subject of a finding that labels itself P1, a
// whole token so a "P10" is not read as one.
var findingsFilerP1Re = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9])P1([^0-9]|$)`)

// findingsFilerStore is the beads surface one pass needs: the two reads of a
// source bead, and the create and mark it writes. beads.Client satisfies it.
type findingsFilerStore interface {
	List(opts beads.ListOptions) ([]*beads.Issue, error)
	Comments(id string) ([]beads.Comment, error)
	Create(opts beads.CreateOptions) (*beads.Issue, error)
	AddComment(id, comment string) error
}

// findingsFilerOptions is one pass's inputs.
type findingsFilerOptions struct {
	// Rigs are the rigs the pass scans: every unparked rig of the town.
	Rigs []string
	// KnownRigs are every rig the town's registry lists, parked or not: the
	// set an owning rig named by a finding is resolved against.
	KnownRigs []string
	Now       time.Time
	// MaxPerRun caps the Found beads one pass files. A value below 1 files
	// nothing.
	MaxPerRun int
	// Template is the shape the dispatcher lints a filed bead against.
	Template specdispatch.Template
	// Open returns a rig's own database; a nil Open runs nothing.
	Open func(rig string) findingsFilerStore
	// Logf writes the pass's lines; nil discards them.
	Logf func(format string, args ...any)
}

// findingsFilerFinding is one FINDING: comment parsed.
type findingsFilerFinding struct {
	// rig is the owning rig its first line names, "" when it names none.
	rig string
	// text is the finding with the owner line removed.
	text string
	// subject is text's first non-empty line, "" when text is empty: the
	// line the title comes from and the line P1 is read in.
	subject  string
	priority int
}

// runFindingsFilerPass runs one pass and returns how many Found beads it
// filed. A read that fails is logged and skipped, never read as a bead with no
// findings: the finding it hides is retried next pass, since nothing marks it.
func runFindingsFilerPass(opts findingsFilerOptions) int {
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if opts.Open == nil || opts.MaxPerRun < 1 {
		logf("findings_filer: nothing to file: no database reader or a per-pass cap of %d", opts.MaxPerRun)
		return 0
	}
	known := make(map[string]bool, len(opts.KnownRigs))
	for _, rig := range opts.KnownRigs {
		known[rig] = true
	}
	since := opts.Now.Add(-findingsFilerWindow)
	filed := 0
	for _, sourceRig := range opts.Rigs {
		remaining := opts.MaxPerRun - filed
		if remaining < 1 {
			break
		}
		store := opts.Open(sourceRig)
		if store == nil {
			logf("findings_filer: %s: no database to scan", sourceRig)
			continue
		}
		issues, err := store.List(beads.ListOptions{
			Status:       findingsFilerScanStatuses,
			Priority:     -1,
			UpdatedAfter: since,
			Limit:        0,
		})
		if err != nil {
			logf("findings_filer: %s: listing beads changed in the last day: %v", sourceRig, err)
			continue
		}
		for _, issue := range issues {
			if remaining < 1 {
				break
			}
			n := findingsFilerBead(opts, logf, store, sourceRig, issue, known, remaining)
			filed += n
			remaining -= n
		}
	}
	return filed
}

// findingsFilerBead files the pending findings of one source bead and returns
// how many it filed, at most remaining. A create that fails stops the bead:
// the later findings must not be marked ahead of the one that did not file, or
// the mark count the next pass reads would skip it.
func findingsFilerBead(opts findingsFilerOptions, logf func(string, ...any), store findingsFilerStore, sourceRig string, issue *beads.Issue, known map[string]bool, remaining int) int {
	comments, err := store.Comments(issue.ID)
	if err != nil {
		logf("findings_filer: %s: reading the comments: %v", issue.ID, err)
		return 0
	}
	findings, marks := findingsFilerComments(comments)
	if marks >= len(findings) {
		return 0
	}
	pending := findings[marks:]
	// One destination read per rig: the Found beads this source bead already
	// produced, by title. A mark the pass could not write leaves one, and the
	// next pass must not file the finding again.
	existing := map[string]map[string]string{}
	filed := 0
	for _, finding := range pending {
		if filed >= remaining {
			break
		}
		targetRig, fallback := findingsFilerOwnerRig(finding, sourceRig, known)
		target := opts.Open(targetRig)
		if target == nil {
			logf("findings_filer: %s: no database for rig %s", issue.ID, targetRig)
			break
		}
		titles, seen := existing[targetRig]
		if !seen {
			var err error
			if titles, err = findingsFilerFoundTitles(target, issue.ID); err != nil {
				logf("findings_filer: %s: reading the Found beads already filed for it in %s: %v", issue.ID, targetRig, err)
				break
			}
			existing[targetRig] = titles
		}
		title := findingsFilerTitle(finding)
		if id, ok := titles[title]; ok {
			// Already filed by an earlier pass whose mark did not land. The
			// mark is the only thing missing, so write it rather than file
			// the same defect twice.
			if err := store.AddComment(issue.ID, findingsFilerFiledComment(id)); err != nil {
				logf("findings_filer: %s: marking the finding filed as %s: %v", issue.ID, id, err)
			}
			continue
		}
		body := findingsFilerBody(finding, title, issue.ID, fallback, opts.Template)
		created, err := target.Create(beads.CreateOptions{
			Title:       title,
			Description: body,
			Priority:    finding.priority,
			Labels:      []string{"found-from:" + issue.ID},
			Actor:       findingsFilerActor,
		})
		if err != nil {
			logf("findings_filer: %s: filing the finding in %s: %v", issue.ID, targetRig, err)
			break
		}
		filed++
		titles[title] = created.ID
		if err := store.AddComment(issue.ID, findingsFilerFiledComment(created.ID)); err != nil {
			logf("findings_filer: %s: marking the finding filed as %s: %v", issue.ID, created.ID, err)
		}
	}
	return filed
}

// findingsFilerComments splits one bead's comments into the findings, in
// comment order, and the number of marks a pass left for the ones already
// filed. The pass files in that order, so the front marks are the filed ones.
func findingsFilerComments(comments []beads.Comment) ([]findingsFilerFinding, int) {
	var findings []findingsFilerFinding
	marks := 0
	for _, c := range comments {
		if f, ok := findingsFilerParse(c.Text); ok {
			findings = append(findings, f)
			continue
		}
		if strings.HasPrefix(findingsFilerFirstLine(c.Text), findingsFilerFiledMarker) {
			marks++
		}
	}
	return findings, marks
}

// findingsFilerFirstLine returns the trimmed first line of text.
func findingsFilerFirstLine(text string) string {
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	return strings.TrimSpace(text)
}

// findingsFilerFiledComment is the comment that marks a finding filed with the
// bead it became.
func findingsFilerFiledComment(id string) string {
	return findingsFilerFiledMarker + " " + id
}

// findingsFilerParse reads one comment as a finding: the marker line, then an
// optional owner line ("rig: <name>") and the finding's own text. A comment
// whose marker is not its first line is not a finding, and neither is one that
// carries the filed marker.
func findingsFilerParse(text string) (findingsFilerFinding, bool) {
	if findingsFilerFirstLine(text) != findingsFilerMarker {
		return findingsFilerFinding{}, false
	}
	rest := ""
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		rest = strings.TrimSpace(text[i+1:])
	}
	f := findingsFilerFinding{priority: findingsFilerPriority}
	head, after, more := strings.Cut(rest, "\n")
	if rig, named := findingsFilerRigLine(head); named {
		f.rig = rig
		rest = ""
		if more {
			rest = strings.TrimSpace(after)
		}
	}
	f.text = rest
	for _, line := range strings.Split(rest, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			f.subject = line
			break
		}
	}
	if findingsFilerP1Re.MatchString(f.subject) {
		f.priority = findingsFilerP1
	}
	return f, true
}

// findingsFilerRigLine reads a first line that is exactly "rig: <name>". A
// line that only starts that way is finding text, not an owner: a reader who
// wrote a sentence must not lose it to the owner parse.
func findingsFilerRigLine(line string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), "rig:")
	if !ok {
		return "", false
	}
	fields := strings.Fields(rest)
	if len(fields) != 1 {
		return "", false
	}
	return fields[0], true
}

// findingsFilerOwnerRig returns the rig the finding is filed in and, when the
// finding named a rig the town does not have, the sentence that says so in the
// new bead. The source bead's own rig is the fallback.
func findingsFilerOwnerRig(f findingsFilerFinding, sourceRig string, known map[string]bool) (string, string) {
	if f.rig == "" {
		return sourceRig, ""
	}
	if known[f.rig] {
		return f.rig, ""
	}
	return sourceRig, fmt.Sprintf("The finding named rig %q, which is not a rig in this town; filed in %s instead.\n", f.rig, sourceRig)
}

// findingsFilerTitle is the Found bead's title: the finding's first line, cut
// to findingsFilerTitleLen runes.
func findingsFilerTitle(f findingsFilerFinding) string {
	subject := f.subject
	if subject == "" {
		subject = findingsFilerWithoutSubject
	}
	if runes := []rune(subject); len(runes) > findingsFilerTitleLen {
		subject = string(runes[:findingsFilerTitleLen])
	}
	return "Found: " + subject
}

// findingsFilerBody is the Found bead's description: the shaped body, with the
// SHAPE note in front when the dispatcher's lint would hold it. A finding with
// no text is unshaped by definition, so it carries the note without a lint
// pass over a Goal that only says it has nothing.
func findingsFilerBody(f findingsFilerFinding, title, sourceID, fallback string, tmpl specdispatch.Template) string {
	body := findingsFilerDescription(f, sourceID, fallback)
	note := findingsFilerEmptyNote
	if f.text != "" {
		note = findingsFilerShapeNote(body, title, tmpl)
	}
	if note != "" {
		return note + "\n\n" + body
	}
	return body
}

// findingsFilerDescription is the Found bead's body in the shape the spec
// dispatcher lints: the finding under Goal and the sections a work bead needs,
// with the source bead and the fallback, when there is one, in Constraints.
func findingsFilerDescription(f findingsFilerFinding, sourceID, fallback string) string {
	var b strings.Builder
	b.WriteString("## Goal\n")
	if f.text == "" {
		b.WriteString(findingsFilerEmptyGoal + "\n")
	} else {
		b.WriteString(f.text + "\n")
	}
	b.WriteString("\n## Constraints\n")
	fmt.Fprintf(&b, "Source bead %s; filed from a polecat finding.\n", sourceID)
	if fallback != "" {
		b.WriteString(fallback)
	}
	b.WriteString("\n## Out of scope\nanything the finding does not name\n")
	b.WriteString("\n## Gate\ngt done presubmit; the landing runs make gate\n")
	b.WriteString("\n## Size\none worker, one landing\n")
	b.WriteString("\n## Acceptance\n- [ ] the defect described in the finding is fixed or shown not to exist\n")
	return b.String()
}

// findingsFilerShapeNote is the SHAPE note the body earns when the dispatcher's
// lint would hold it, "" when the lint clears it. Only a missing section or an
// empty acceptance list is this file's to fix, as the tier sweep's own filing
// decides it: a Size the patrol cannot satisfy is left for the dispatcher to
// report rather than rewritten.
func findingsFilerShapeNote(body, title string, tmpl specdispatch.Template) string {
	spec := specdispatch.Spec{Title: title, Type: "task", Description: body}
	verdict := specdispatch.LintShape(spec, tmpl)
	for _, r := range verdict.Refusals {
		if r.Field == "acceptance" || strings.HasPrefix(r.Field, "## ") {
			return verdict.ShapeNote()
		}
	}
	return ""
}

// findingsFilerFoundTitles returns the Found beads a source bead already
// produced in one rig, by title. A read that fails is an error rather than an
// empty set: filing past it would file a defect twice, and the finding is
// retried next pass because nothing marked it.
func findingsFilerFoundTitles(store findingsFilerStore, sourceID string) (map[string]string, error) {
	issues, err := store.List(beads.ListOptions{Status: "all", Label: "found-from:" + sourceID, Priority: -1, Limit: 0})
	if err != nil {
		return nil, err
	}
	titles := make(map[string]string, len(issues))
	for _, issue := range issues {
		titles[issue.Title] = issue.ID
	}
	return titles, nil
}

// findingsFilerInterval returns the configured interval, or the default.
func findingsFilerInterval(cfg *DaemonPatrolConfig) time.Duration {
	var c *FindingsFilerConfig
	if cfg != nil && cfg.Patrols != nil {
		c = cfg.Patrols.FindingsFiler
	}
	d, err := c.IntervalValue()
	if err != nil {
		return config.FindingsFilerDefaultInterval
	}
	return d
}

// findingsFilerMaxPerRun returns the configured per-pass cap, or the default.
func findingsFilerMaxPerRun(cfg *DaemonPatrolConfig) int {
	var c *FindingsFilerConfig
	if cfg != nil && cfg.Patrols != nil {
		c = cfg.Patrols.FindingsFiler
	}
	return c.MaxPerRunValue()
}

// findingsFilerTemplate is the shape a filed bead is linted against: the
// operator's spec template, resolved the way the dispatcher's lint resolves
// it, with the built-in sections as the last fallback (specdispatch.LoadTemplate).
func findingsFilerTemplate(townRoot string) specdispatch.Template {
	configured := ""
	if pc, err := ReadPatrolConfig(townRoot); err == nil && pc != nil && pc.Patrols != nil && pc.Patrols.SpecDispatch != nil {
		configured = pc.Patrols.SpecDispatch.Template
	}
	return specdispatch.LoadTemplate(specdispatch.ResolveTemplatePath(configured))
}

// findingsFilerStoreFor opens a rig's own database: the Found bead is created
// there and never routed elsewhere.
func (d *Daemon) findingsFilerStoreFor(rig string) findingsFilerStore {
	return beads.NewPinned(filepath.Join(d.config.TownRoot, rig, ".beads"),
		beads.WithBin(d.bdPathOrDefault())).WithTimeout(findingsFilerBdTimeout).WithProcessGroup()
}

// triggerFindingsFiler runs one pass on its own goroutine, so the bd calls it
// makes cannot hold the daemon's loop; a pass still running skips the next
// tick rather than filing the same findings twice at once.
func (d *Daemon) triggerFindingsFiler() bool {
	if !d.findingsFilerRunning.CompareAndSwap(false, true) {
		d.logger.Printf("findings_filer: previous pass still running, skipping")
		return false
	}
	go func() {
		defer d.findingsFilerRunning.Store(false)
		d.runFindingsFiler()
	}()
	return true
}

// findingsFilerSeams replaces the pass's rig reads in tests: the rigs to scan,
// the rigs a named owner resolves against, the shape to lint against and the
// store each rig opens. Nil reads the town.
type findingsFilerSeams struct {
	rigs      []string
	knownRigs []string
	template  specdispatch.Template
	open      func(rig string) findingsFilerStore
}

// runFindingsFiler is one pass over the town's unparked rigs.
func (d *Daemon) runFindingsFiler() {
	if !d.isPatrolActive("findings_filer") || d.config == nil {
		return
	}
	rigs, known := d.getPatrolRigs("findings_filer"), d.getKnownRigs()
	tmpl, open := findingsFilerTemplate(d.config.TownRoot), d.findingsFilerStoreFor
	if s := d.findingsFilerSeams; s != nil {
		rigs, known, tmpl, open = s.rigs, s.knownRigs, s.template, s.open
	}
	filed := runFindingsFilerPass(findingsFilerOptions{
		Rigs:      rigs,
		KnownRigs: known,
		Now:       d.clk().Now(),
		MaxPerRun: findingsFilerMaxPerRun(d.patrolConfig),
		Template:  tmpl,
		Open:      open,
		Logf:      d.logger.Printf,
	})
	if filed > 0 {
		d.logger.Printf("findings_filer: filed %d Found bead(s)", filed)
	}
}
