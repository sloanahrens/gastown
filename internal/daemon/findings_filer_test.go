package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// findingsFilerNow is the clock the fakes and the pass share in these tests.
func findingsFilerNow() time.Time {
	return time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
}

// findingsFilerTestTemplate is the shape the pass lints a filed bead against:
// the built-in sections, with no operator file to read.
func findingsFilerTestTemplate() specdispatch.Template {
	return specdispatch.Template{Sections: specdispatch.DefaultSections, Source: "test"}
}

// findingsFilerTestStore is one rig's database with the two writes the pass
// makes made refusable, so a test can drive the branches a Dolt that is down
// reaches.
type findingsFilerTestStore struct {
	*beadsfake.Fake
	createErr error
	markErr   error
	listErr   error
}

func (s *findingsFilerTestStore) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.Fake.List(opts)
}

func (s *findingsFilerTestStore) Create(opts beads.CreateOptions) (*beads.Issue, error) {
	if s.createErr != nil {
		return nil, s.createErr
	}
	return s.Fake.Create(opts)
}

func (s *findingsFilerTestStore) AddComment(id, comment string) error {
	if s.markErr != nil {
		return s.markErr
	}
	return s.Fake.AddComment(id, comment)
}

// findingsFilerTown is a test town: one fake database per rig, all reading one
// fixed clock.
type findingsFilerTown struct {
	rigs   []string
	stores map[string]*findingsFilerTestStore
	lines  []string
}

func newFindingsFilerTown(t *testing.T, rigs ...string) *findingsFilerTown {
	t.Helper()
	clock := clockwork.NewFakeClockAt(findingsFilerNow())
	town := &findingsFilerTown{stores: map[string]*findingsFilerTestStore{}}
	for i, rig := range rigs {
		town.rigs = append(town.rigs, rig)
		town.stores[rig] = &findingsFilerTestStore{
			Fake: beadsfake.New(beadsfake.WithPrefix(fmt.Sprintf("r%d", i)), beadsfake.WithClock(clock)),
		}
	}
	return town
}

// findingsFilerEntryDaemon is a daemon whose findings_filer block is enabled as
// given and whose rig reads answer from town, so the daemon's own entry can run
// without a registry on disk.
func findingsFilerEntryDaemon(t *testing.T, town *findingsFilerTown, enabled bool) *Daemon {
	t.Helper()
	max := 5
	return &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: discardLogger,
		clock:  clockwork.NewFakeClockAt(findingsFilerNow()),
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			FindingsFiler: &FindingsFilerConfig{Enabled: enabled, MaxPerRun: &max},
		}},
		findingsFilerSeams: &findingsFilerSeams{
			rigs:      town.rigs,
			knownRigs: town.rigs,
			template: func(string) (specdispatch.Template, error) {
				return findingsFilerTestTemplate(), nil
			},
			open: town.open,
		},
	}
}

// open is the pass's store seam: a rig the town does not have is nil.
func (town *findingsFilerTown) open(rig string) findingsFilerStore {
	store, ok := town.stores[rig]
	if !ok {
		return nil
	}
	return store
}

// pass runs one pass at a minute past every write the fakes made, so the
// seeded beads sit inside the day the pass reads.
func (town *findingsFilerTown) pass(maxPerRun int) findingsFilerPassResult {
	return runFindingsFilerPass(findingsFilerOptions{
		Rigs:      town.rigs,
		KnownRigs: town.rigs,
		Now:       findingsFilerNow().Add(time.Minute),
		MaxPerRun: maxPerRun,
		Template:  findingsFilerTestTemplate(),
		Open:      town.open,
		Logf: func(format string, args ...any) {
			town.lines = append(town.lines, fmt.Sprintf(format, args...))
		},
	})
}

// sourceBead creates a bead in rig, records the finding comment on it in the
// shape gt done writes, and moves it to status.
func (town *findingsFilerTown) sourceBead(t *testing.T, rig, status, finding string) string {
	t.Helper()
	bd := town.stores[rig].Fake
	created, err := bd.Create(beads.CreateOptions{Title: "the polecat's task"})
	if err != nil {
		t.Fatalf("%s: create the source bead: %v", rig, err)
	}
	if finding != "" {
		if err := bd.AddComment(created.ID, findingsFilerMarker+"\n"+finding); err != nil {
			t.Fatalf("%s: record the finding on %s: %v", rig, created.ID, err)
		}
	}
	if err := bd.Update(created.ID, beads.UpdateOptions{Status: &status}); err != nil {
		t.Fatalf("%s: move %s to %s: %v", rig, created.ID, status, err)
	}
	return created.ID
}

// foundBeads returns the Found beads in rig's database, the beads wearing a
// found-from: label.
func (town *findingsFilerTown) foundBeads(t *testing.T, rig string) []*beads.Issue {
	t.Helper()
	issues, err := town.stores[rig].Fake.List(beads.ListOptions{Status: "all", Priority: -1, Limit: 0})
	if err != nil {
		t.Fatalf("%s: list beads: %v", rig, err)
	}
	var out []*beads.Issue
	for _, issue := range issues {
		for _, label := range issue.Labels {
			if strings.HasPrefix(label, "found-from:") {
				out = append(out, issue)
				break
			}
		}
	}
	return out
}

// marks returns the FINDING-FILED comments on a source bead, in order.
func (town *findingsFilerTown) marks(t *testing.T, rig, id string) []string {
	t.Helper()
	comments, err := town.stores[rig].Fake.Comments(id)
	if err != nil {
		t.Fatalf("%s: read the comments on %s: %v", rig, id, err)
	}
	var out []string
	for _, c := range comments {
		if strings.HasPrefix(findingsFilerFirstLine(c.Text), findingsFilerFiledMarker) {
			out = append(out, c.Text)
		}
	}
	return out
}

func TestFindingsFilerFilesOnce(t *testing.T) {
	t.Parallel()
	town := newFindingsFilerTown(t, "gastown", "gantry")
	source := town.sourceBead(t, "gastown", "closed", "rig: gantry\nthe widget drops a byte on every read")

	if got := town.pass(5).Filed; got != 1 {
		t.Fatalf("filed %d Found beads, want 1", got)
	}
	found := town.foundBeads(t, "gantry")
	if len(found) != 1 {
		t.Fatalf("gantry holds %d Found beads, want the one the finding named", len(found))
	}
	got := found[0]
	if want := "Found: the widget drops a byte on every read"; got.Title != want {
		t.Errorf("title = %q, want %q", got.Title, want)
	}
	if got.Priority != findingsFilerPriority {
		t.Errorf("priority = %d, want %d", got.Priority, findingsFilerPriority)
	}
	if !beads.HasLabel(got, "found-from:"+source) {
		t.Errorf("labels = %v, want found-from:%s", got.Labels, source)
	}
	for _, section := range []string{"## Goal", "## Constraints", "## Out of scope", "## Gate", "## Size", "## Acceptance", "- [ ] the defect described in the finding is fixed or shown not to exist"} {
		if !strings.Contains(got.Description, section) {
			t.Errorf("description does not carry %q:\n%s", section, got.Description)
		}
	}
	if !strings.Contains(got.Description, source) || !strings.Contains(got.Description, "filed from a polecat finding") {
		t.Errorf("Constraints does not name the source bead and the finding:\n%s", got.Description)
	}
	if other := town.foundBeads(t, "gastown"); len(other) != 0 {
		t.Errorf("the source rig holds %d Found beads, want none", len(other))
	}
	marks := town.marks(t, "gastown", source)
	if len(marks) != 1 || marks[0] != "FINDING-FILED: "+got.ID {
		t.Fatalf("marks = %q, want one naming %s", marks, got.ID)
	}

	if got := town.pass(5).Filed; got != 0 {
		t.Fatalf("a second pass filed %d Found beads, want 0", got)
	}
	if n := len(town.foundBeads(t, "gantry")); n != 1 {
		t.Errorf("after the second pass gantry holds %d Found beads, want 1", n)
	}
	if n := len(town.marks(t, "gastown", source)); n != 1 {
		t.Errorf("after the second pass the source bead holds %d marks, want 1", n)
	}
}

// TestFindingsFilerScanStatuses pins which beads the pass reads: the three
// statuses a finding can be recorded under, and not an open bead.
func TestFindingsFilerScanStatuses(t *testing.T) {
	t.Parallel()
	town := newFindingsFilerTown(t, "gastown")
	for _, status := range []string{"closed", "deferred", "hooked", "open"} {
		town.sourceBead(t, "gastown", status, "the "+status+" defect")
	}

	if got := town.pass(5).Filed; got != 3 {
		t.Fatalf("filed %d Found beads, want 3", got)
	}
	var titles []string
	for _, issue := range town.foundBeads(t, "gastown") {
		titles = append(titles, issue.Title)
	}
	want := map[string]bool{"Found: the closed defect": true, "Found: the deferred defect": true, "Found: the hooked defect": true}
	if len(titles) != len(want) {
		t.Fatalf("titles = %q, want the three findings on changed beads", titles)
	}
	for _, title := range titles {
		if !want[title] {
			t.Errorf("filed %q, want only the findings on closed, deferred and hooked beads", title)
		}
	}
}

func TestFindingsFilerRetries(t *testing.T) {
	t.Parallel()
	town := newFindingsFilerTown(t, "gastown")
	source := town.sourceBead(t, "gastown", "closed", "the parser panics on an empty line")
	town.stores["gastown"].createErr = errors.New("dolt is unreachable")

	if got := town.pass(5).Filed; got != 0 {
		t.Fatalf("filed %d Found beads past a failed create, want 0", got)
	}
	if n := len(town.foundBeads(t, "gastown")); n != 0 {
		t.Errorf("a failed create left %d Found beads", n)
	}
	if marks := town.marks(t, "gastown", source); len(marks) != 0 {
		t.Fatalf("a failed create left the marks %q, want none", marks)
	}
	if !strings.Contains(strings.Join(town.lines, "\n"), "dolt is unreachable") {
		t.Errorf("the failed create was not logged; lines: %q", town.lines)
	}

	town.stores["gastown"].createErr = nil
	if got := town.pass(5).Filed; got != 1 {
		t.Fatalf("the retry filed %d Found beads, want 1", got)
	}
	if n := len(town.foundBeads(t, "gastown")); n != 1 {
		t.Errorf("the retry left %d Found beads, want 1", n)
	}
	if marks := town.marks(t, "gastown", source); len(marks) != 1 {
		t.Errorf("the retry left the marks %q, want one", marks)
	}
}

// TestFindingsFilerUnreadableDestination pins the dedup read's failure: an
// empty answer would file the defect twice, so the pass files nothing and the
// unmarked finding is retried.
func TestFindingsFilerUnreadableDestination(t *testing.T) {
	t.Parallel()
	town := newFindingsFilerTown(t, "gastown", "gantry")
	source := town.sourceBead(t, "gastown", "closed", "rig: gantry\nthe widget drops a byte")
	town.stores["gantry"].listErr = errors.New("the destination is unreachable")

	if got := town.pass(5).Filed; got != 0 {
		t.Fatalf("filed %d Found beads past an unreadable destination, want 0", got)
	}
	if marks := town.marks(t, "gastown", source); len(marks) != 0 {
		t.Fatalf("marks = %q, want none while the destination cannot be read", marks)
	}
	if !strings.Contains(strings.Join(town.lines, "\n"), "the destination is unreachable") {
		t.Errorf("the unreadable destination was not logged; lines: %q", town.lines)
	}

	town.stores["gantry"].listErr = nil
	if got := town.pass(5).Filed; got != 1 {
		t.Fatalf("the retry filed %d Found beads, want 1", got)
	}
	if n := len(town.foundBeads(t, "gantry")); n != 1 {
		t.Errorf("the retry left %d Found beads in gantry, want 1", n)
	}
}

// TestFindingsFilerMissingDatabases pins the two stores a pass cannot open: a
// scanned rig with no database is counted and skipped, and a destination rig
// with none stops that source bead. Neither reads as a pass that found nothing
// to file, and nothing is marked, so the next pass retries the finding.
func TestFindingsFilerMissingDatabases(t *testing.T) {
	t.Parallel()
	town := newFindingsFilerTown(t, "gastown", "atlantis")
	source := town.sourceBead(t, "gastown", "closed", "rig: atlantis\nthe foo module leaks a goroutine")
	delete(town.stores, "atlantis")

	got := town.pass(5)
	if got.Filed != 0 {
		t.Fatalf("filed %d Found beads with no database in atlantis, want 0", got.Filed)
	}
	if got.Failed != 2 {
		t.Errorf("a pass over a missing database reported %d failures, want the scan and the filing", got.Failed)
	}
	lines := strings.Join(town.lines, "\n")
	if !strings.Contains(lines, "no database to scan") || !strings.Contains(lines, "no database for rig atlantis") {
		t.Errorf("the pass did not name both missing databases; log: %q", town.lines)
	}
	if marks := town.marks(t, "gastown", source); len(marks) != 0 {
		t.Errorf("marks = %q, want none while the destination cannot be opened", marks)
	}
}

func TestFindingsFilerUnknownRig(t *testing.T) {
	t.Parallel()
	town := newFindingsFilerTown(t, "gastown", "gantry")
	source := town.sourceBead(t, "gastown", "closed", "rig: atlantis\nthe foo module leaks a goroutine")

	if got := town.pass(5).Filed; got != 1 {
		t.Fatalf("filed %d Found beads, want 1", got)
	}
	found := town.foundBeads(t, "gastown")
	if len(found) != 1 {
		t.Fatalf("the source rig holds %d Found beads, want the fallback filing", len(found))
	}
	if other := town.foundBeads(t, "gantry"); len(other) != 0 {
		t.Errorf("gantry holds %d Found beads, want none", len(other))
	}
	body := found[0].Description
	if !strings.Contains(body, `"atlantis"`) || !strings.Contains(body, "not a rig in this town") || !strings.Contains(body, "filed in gastown instead") {
		t.Errorf("the fallback is not recorded in Constraints:\n%s", body)
	}
	if !beads.HasLabel(found[0], "found-from:"+source) {
		t.Errorf("labels = %v, want found-from:%s", found[0].Labels, source)
	}
}

func TestFindingsFilerOptIn(t *testing.T) {
	t.Parallel()
	if IsPatrolEnabled(nil, "findings_filer") {
		t.Error("the patrol is on with no config; want off")
	}
	absent := &DaemonPatrolConfig{Patrols: &PatrolsConfig{}}
	if IsPatrolEnabled(absent, "findings_filer") {
		t.Error("the patrol is on with no block; want off")
	}
	off := &DaemonPatrolConfig{Patrols: &PatrolsConfig{FindingsFiler: &FindingsFilerConfig{}}}
	if IsPatrolEnabled(off, "findings_filer") {
		t.Error("the patrol is on with enabled unset; want off")
	}
	on := &DaemonPatrolConfig{Patrols: &PatrolsConfig{FindingsFiler: &FindingsFilerConfig{Enabled: true}}}
	if !IsPatrolEnabled(on, "findings_filer") {
		t.Error("the patrol is off with enabled true; want on")
	}

	// The daemon's own entry runs the pass only when the block says so: the
	// disabled daemon files nothing, the enabled one files the finding.
	entryTown := newFindingsFilerTown(t, "gastown")
	entryTown.sourceBead(t, "gastown", "closed", "the daemon entry defect")
	findingsFilerEntryDaemon(t, entryTown, false).runFindingsFiler()
	if n := len(entryTown.foundBeads(t, "gastown")); n != 0 {
		t.Fatalf("the disabled daemon filed %d Found beads, want 0", n)
	}
	findingsFilerEntryDaemon(t, entryTown, true).runFindingsFiler()
	if n := len(entryTown.foundBeads(t, "gastown")); n != 1 {
		t.Fatalf("the enabled daemon filed %d Found beads, want 1", n)
	}

	town := newFindingsFilerTown(t, "gastown")
	town.sourceBead(t, "gastown", "closed", "the first defect")
	town.sourceBead(t, "gastown", "closed", "the second defect")
	if got := town.pass(1).Filed; got != 1 {
		t.Fatalf("filed %d Found beads with max_per_run 1, want 1", got)
	}
	first := town.foundBeads(t, "gastown")
	if len(first) != 1 {
		t.Fatalf("one pass left %d Found beads, want 1", len(first))
	}
	// The next interval files the finding the cap held back, not the one
	// already filed.
	if got := town.pass(1).Filed; got != 1 {
		t.Fatalf("the next pass filed %d Found beads, want 1", got)
	}
	found := town.foundBeads(t, "gastown")
	if len(found) != 2 {
		t.Fatalf("two passes left %d Found beads, want 2", len(found))
	}
	byTitle := map[string]bool{}
	for _, issue := range found {
		byTitle[issue.Title] = true
	}
	if len(byTitle) != 2 {
		t.Errorf("the two passes filed %v, want the two distinct findings", byTitle)
	}
	for _, want := range []string{"Found: the first defect", "Found: the second defect"} {
		if !byTitle[want] {
			t.Errorf("the two passes filed %v, want both %q among them", byTitle, want)
		}
	}

	// The cap holds within one source bead too: two findings on it file one
	// per pass, in comment order.
	both := town.sourceBead(t, "gastown", "closed", "the first of a pair")
	if err := town.stores["gastown"].Fake.AddComment(both, findingsFilerMarker+"\nthe second of a pair"); err != nil {
		t.Fatalf("record the second finding: %v", err)
	}
	if got := town.pass(1).Filed; got != 1 {
		t.Fatalf("a pass over two findings of one bead filed %d, want 1", got)
	}
	if got := town.pass(1).Filed; got != 1 {
		t.Fatalf("the next pass over two findings of one bead filed %d, want 1", got)
	}
	if marks := town.marks(t, "gastown", both); len(marks) != 2 {
		t.Errorf("marks on the paired bead = %q, want both findings filed", marks)
	}
}

func TestFindingsFilerUnshapedThinFinding(t *testing.T) {
	t.Parallel()
	town := newFindingsFilerTown(t, "gastown")
	town.sourceBead(t, "gastown", "closed", "rig: gastown")

	if got := town.pass(5).Filed; got != 1 {
		t.Fatalf("filed %d Found beads, want 1", got)
	}
	found := town.foundBeads(t, "gastown")
	if len(found) != 1 {
		t.Fatalf("found %d Found beads, want 1", len(found))
	}
	if want := "Found: " + findingsFilerWithoutSubject; found[0].Title != want {
		t.Errorf("title = %q, want %q", found[0].Title, want)
	}
	if !strings.HasPrefix(found[0].Description, "SHAPE: ## Goal:") {
		t.Errorf("the thin finding was not filed unshaped:\n%s", found[0].Description)
	}
}

// TestFindingsFilerShapeNoteForTemplateSection pins the other way a filing is
// unshaped: a template that asks for a section the patrol does not write. The
// note is the lint's own, so the dispatcher and the patrol name the same gap.
func TestFindingsFilerShapeNoteForTemplateSection(t *testing.T) {
	t.Parallel()
	sections := append(append([]string(nil), specdispatch.DefaultSections...), "Rollback")
	got := findingsFilerBody(findingsFilerFinding{text: "the cache never expires"}, "gt-src", "", specdispatch.Template{Sections: sections})
	if !strings.HasPrefix(got, "SHAPE: ## Rollback: section missing") {
		t.Errorf("body = %q, want the lint's SHAPE note", got)
	}
}

func TestFindingsFilerMarkFailureDoesNotFileTwice(t *testing.T) {
	t.Parallel()
	town := newFindingsFilerTown(t, "gastown")
	source := town.sourceBead(t, "gastown", "closed", "the retry counter never resets")
	town.stores["gastown"].markErr = errors.New("the comment write failed")

	if got := town.pass(5).Filed; got != 1 {
		t.Fatalf("filed %d Found beads, want 1", got)
	}
	if n := len(town.foundBeads(t, "gastown")); n != 1 {
		t.Fatalf("found %d Found beads, want 1", n)
	}
	if marks := town.marks(t, "gastown", source); len(marks) != 0 {
		t.Fatalf("marks = %q, want none while the write fails", marks)
	}

	// The Found bead exists; the mark is all that failed. A later pass must
	// write the mark, not the bead again.
	town.stores["gastown"].markErr = nil
	if got := town.pass(5).Filed; got != 0 {
		t.Fatalf("the later pass filed %d Found beads, want 0", got)
	}
	found := town.foundBeads(t, "gastown")
	if len(found) != 1 {
		t.Fatalf("after the later pass there are %d Found beads, want 1", len(found))
	}
	marks := town.marks(t, "gastown", source)
	if len(marks) != 1 || marks[0] != "FINDING-FILED: "+found[0].ID {
		t.Errorf("marks = %q, want one naming %s", marks, found[0].ID)
	}
}

// TestFindingsFilerSameSubjectOnOneBead pins what a filed bead is recognised
// by on a later pass: the finding's own text, not the title its first line
// makes. Two findings on one source bead can share that line, and a mark the
// first pass could not write must not cost the second finding its own bead.
func TestFindingsFilerSameSubjectOnOneBead(t *testing.T) {
	t.Parallel()
	town := newFindingsFilerTown(t, "gastown")
	source := town.sourceBead(t, "gastown", "closed", "the widget leaks a byte\non every read")
	if err := town.stores["gastown"].Fake.AddComment(source, findingsFilerMarker+"\nthe widget leaks a byte\non every write"); err != nil {
		t.Fatalf("record the second finding: %v", err)
	}

	// No mark lands: the pass files the first finding and stops at its failed
	// mark. The next pass must recognise that bead by the finding's text, not
	// by the title it shares with the finding it held back.
	town.stores["gastown"].markErr = errors.New("the comment write failed")
	if got := town.pass(5).Filed; got != 1 {
		t.Fatalf("filed %d Found beads while the mark write fails, want 1", got)
	}
	town.stores["gastown"].markErr = nil

	// The retry writes the missing mark, then files the finding it held back.
	if got := town.pass(5).Filed; got != 1 {
		t.Fatalf("the retry filed %d Found beads, want the second finding's one", got)
	}
	if got := town.pass(5).Filed; got != 0 {
		t.Fatalf("the pass after the retry filed %d Found beads, want 0", got)
	}

	found := town.foundBeads(t, "gastown")
	if len(found) != 2 {
		t.Fatalf("three passes left %d Found beads, want one per finding", len(found))
	}
	for _, want := range []string{"on every read", "on every write"} {
		var hit bool
		for _, issue := range found {
			if strings.Contains(issue.Description, want) {
				hit = true
			}
		}
		if !hit {
			t.Errorf("no Found bead carries %q; found %d beads", want, len(found))
		}
	}
	filedIDs := map[string]bool{}
	for _, issue := range found {
		filedIDs[issue.ID] = true
	}
	marks := town.marks(t, "gastown", source)
	if len(marks) != 2 {
		t.Fatalf("marks = %q, want one per finding", marks)
	}
	markedIDs := map[string]bool{}
	for _, mark := range marks {
		markedIDs[strings.TrimPrefix(mark, findingsFilerFiledMarker+" ")] = true
	}
	if len(markedIDs) != 2 {
		t.Errorf("marks = %q, want each naming its own Found bead", marks)
	}
	for id := range markedIDs {
		if !filedIDs[id] {
			t.Errorf("a mark names %s, which is not one of the Found beads %v", id, filedIDs)
		}
	}
}

// TestFindingsFilerTemplateReadFailure pins the template's read: a patrol
// config the pass cannot read leaves the shape it asked for unknown, so the
// pass files nothing rather than filing under the built-in sections and
// calling it shaped.
func TestFindingsFilerTemplateReadFailure(t *testing.T) {
	t.Parallel()
	town := newFindingsFilerTown(t, "gastown")
	town.sourceBead(t, "gastown", "closed", "the widget drops a byte")

	townRoot := t.TempDir()
	settings := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(settings, 0o755); err != nil {
		t.Fatalf("create the settings directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(settings, "config.json"), []byte(`{"daemon": `), 0o644); err != nil {
		t.Fatalf("write an unreadable settings file: %v", err)
	}

	logs := &bytes.Buffer{}
	d := findingsFilerEntryDaemon(t, town, true)
	d.config = &Config{TownRoot: townRoot}
	d.logger = log.New(logs, "", 0)
	d.findingsFilerSeams.template = nil
	d.runFindingsFiler()

	if n := len(town.foundBeads(t, "gastown")); n != 0 {
		t.Fatalf("filed %d Found beads without the template, want 0", n)
	}
	if !strings.Contains(logs.String(), "not filing this pass") {
		t.Errorf("the template read failure was not logged; log: %q", logs.String())
	}
}

// TestFindingsFilerFailureIsNotAnEmptyPass pins the pass's outcome: a pass
// whose reads or writes failed says so, so a caller cannot read it as a pass
// that found nothing to file.
func TestFindingsFilerFailureIsNotAnEmptyPass(t *testing.T) {
	t.Parallel()
	town := newFindingsFilerTown(t, "gastown")
	town.sourceBead(t, "gastown", "closed", "the widget drops a byte")

	if got := town.pass(5); got != (findingsFilerPassResult{Filed: 1}) {
		t.Fatalf("a clean pass over a bead with one finding reported %+v, want one filing and no failures", got)
	}

	failing := newFindingsFilerTown(t, "gastown")
	failing.sourceBead(t, "gastown", "closed", "the widget drops a byte")
	failing.stores["gastown"].createErr = errors.New("dolt is unreachable")
	logs := &bytes.Buffer{}
	d := findingsFilerEntryDaemon(t, failing, true)
	d.logger = log.New(logs, "", 0)
	d.runFindingsFiler()

	if n := len(failing.foundBeads(t, "gastown")); n != 0 {
		t.Fatalf("filed %d Found beads past a failed create, want 0", n)
	}
	if !strings.Contains(logs.String(), "failed") {
		t.Errorf("a pass that filed nothing because its create failed logged %q, want the failure named", logs.String())
	}
}

func TestFindingsFilerPriorityAndTitle(t *testing.T) {
	t.Parallel()
	town := newFindingsFilerTown(t, "gastown")
	subject := "P1: " + strings.Repeat("z", 200)
	town.sourceBead(t, "gastown", "closed", subject)

	if got := town.pass(5).Filed; got != 1 {
		t.Fatalf("filed %d Found beads, want 1", got)
	}
	found := town.foundBeads(t, "gastown")
	if len(found) != 1 {
		t.Fatalf("found %d Found beads, want 1", len(found))
	}
	if found[0].Priority != findingsFilerP1 {
		t.Errorf("priority = %d, want %d for a subject that says P1", found[0].Priority, findingsFilerP1)
	}
	title := strings.TrimPrefix(found[0].Title, "Found: ")
	if n := len([]rune(title)); n != findingsFilerTitleLen {
		t.Errorf("title subject is %d runes, want it cut to %d", n, findingsFilerTitleLen)
	}
}

// TestFindingsFilerParsesFinding pins what one comment is read as: the marker,
// the optional owner line, the subject the title and P1 come from, and the
// lines that are finding text rather than an owner.
func TestFindingsFilerParsesFinding(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		comment string
		want    findingsFilerFinding
		isNone  bool
	}{
		{
			name:    "text only",
			comment: "FINDING:\nthe cache never expires",
			want:    findingsFilerFinding{text: "the cache never expires", subject: "the cache never expires", priority: 3},
		},
		{
			name:    "owner line",
			comment: "FINDING:\nrig: gantry\nthe cache never expires",
			want:    findingsFilerFinding{rig: "gantry", text: "the cache never expires", subject: "the cache never expires", priority: 3},
		},
		{
			name:    "owner line and nothing else",
			comment: "FINDING:\nrig: gantry\n\n",
			want:    findingsFilerFinding{rig: "gantry", priority: 3},
		},
		{
			name:    "rig-prefixed prose is text, not an owner",
			comment: "FINDING:\nrig: the parser drops the last line",
			want:    findingsFilerFinding{text: "rig: the parser drops the last line", subject: "rig: the parser drops the last line", priority: 3},
		},
		{
			name:    "P1 in the subject",
			comment: "FINDING:\nP1: the parser panics",
			want:    findingsFilerFinding{text: "P1: the parser panics", subject: "P1: the parser panics", priority: 1},
		},
		{
			name:    "P10 is not P1",
			comment: "FINDING:\nP10: the parser panics",
			want:    findingsFilerFinding{text: "P10: the parser panics", subject: "P10: the parser panics", priority: 3},
		},
		{
			name:    "marker not on the first line",
			comment: "note first\nFINDING:\nthe cache never expires",
			isNone:  true,
		},
		{
			name:    "the filed marker is not a finding",
			comment: "FINDING-FILED: gt-abc",
			isNone:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := findingsFilerParse(tt.comment)
			if tt.isNone {
				if ok {
					t.Fatalf("parsed %q as a finding: %+v", tt.comment, got)
				}
				return
			}
			if !ok {
				t.Fatalf("%q did not parse as a finding", tt.comment)
			}
			if got != tt.want {
				t.Errorf("parsed %+v, want %+v", got, tt.want)
			}
		})
	}
}
