package scorecard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/jev/dedupe"
)

// FindingBattery is the finding adapter's name. It scores the finding battery
// that gt jev dedupe logs beside the duplicate Choice.
const FindingBattery = "finding"

// The rules that label a finding, by the name --json reports. The rule is the
// close reason's class; a landed reason also sets severity from the diff, and
// a reason naming an offered candidate also sets any_duplicate.
const (
	ruleLanded         = "close:landed"
	ruleNoChanges      = "close:no-changes"
	ruleNotFixWorthy   = "close:superseded-duplicate-wontfix"
	ruleNamesCandidate = "close:names-candidate"
)

// A fix whose landed diff is under smallDiffLines changed lines is small, and
// one over largeDiffLines is large. A diff of smallDiffLines through
// largeDiffLines lines, both ends included, is neither, so it leaves the
// severity Score unlabeled.
const (
	smallDiffLines = 50
	largeDiffLines = 300
)

// refOffered is the Item.Refs key for the candidates a line offered, joined by
// spaces. A bead id holds no space, so the join splits back exactly.
const refOffered = "offered"

// gitTimeout bounds one git call for a landed diff.
const gitTimeout = time.Minute

func init() {
	Register(FindingBattery, func(townRoot string) Adapter {
		return NewFinding(townRoot, rigBeads{town: townRoot}, rigDiffs{town: townRoot, sizes: &diffSizes{}})
	})
}

// BeadReader reads one bead. The adapter only reads.
type BeadReader interface {
	// Show returns one bead, or an error wrapping beads.ErrNotFound when the
	// bead does not exist.
	Show(id string) (*beads.Issue, error)
}

// DiffSizer measures a landed commit.
type DiffSizer interface {
	// ChangedLines is the lines added plus removed by commit against its first
	// parent. A commit the repo does not hold returns ErrNoCommit.
	ChangedLines(ctx context.Context, commit string) (int, error)
}

// ErrNoCommit means the repo does not hold the commit asked about.
var ErrNoCommit = errors.New("commit not in the repo")

// NewFinding builds the finding adapter over a bead reader and a diff sizer.
// The registered adapter uses the rig's beads and git repo; a test passes fakes.
func NewFinding(townRoot string, b BeadReader, d DiffSizer) Adapter {
	return &finding{town: townRoot, beads: b, diffs: d}
}

// diffPrefetcher is a DiffSizer that can size many commits in one pass, so the
// ChangedLines that follow answer from what it read.
type diffPrefetcher interface {
	PrefetchLines(ctx context.Context, commits []string)
}

// finding labels the dedupe log's lines from the finding bead's close reason.
type finding struct {
	town  string
	beads BeadReader
	diffs DiffSizer

	// issues holds the beads Prefetch read in one batch, by id. Label reads a
	// bead missing from it the one-at-a-time way.
	issues map[string]*beads.Issue
}

// Prefetch reads every finding bead in one batch, then sizes every landed
// commit their close reasons name in one pass, so Label makes no call per item.
func (f *finding) Prefetch(ctx context.Context, items []Item) {
	f.issues = showAll(f.beads, itemIDs(items))
	dp, ok := f.diffs.(diffPrefetcher)
	if !ok {
		return
	}
	var commits []string
	for _, issue := range f.issues {
		if issue.Status != "closed" {
			continue
		}
		if r := parseCloseReason(issue.CloseReason); r.landed && r.commit != "" {
			commits = append(commits, r.commit)
		}
	}
	dp.PrefetchLines(ctx, commits)
}

// show reads one finding bead, from the batch when Prefetch read it.
func (f *finding) show(id string) (*beads.Issue, error) {
	if issue, ok := f.issues[id]; ok {
		return issue, nil
	}
	return f.beads.Show(id)
}

func (finding) Battery() string   { return FindingBattery }
func (f finding) LogPath() string { return dedupe.LogPath(f.town) }

// Parse reads one dedupe log line. A choice answer sets its Positive to the
// option it chose and its probability to the line's confidence, so the
// Choice's precision is how often what it chose was right.
func (finding) Parse(raw []byte) (Item, error) {
	var line dedupe.Line
	if err := json.Unmarshal(raw, &line); err != nil {
		return Item{}, err
	}
	if line.Finding == "" || line.TS.IsZero() {
		return Item{}, errors.New("dedupe line needs a finding and a ts")
	}
	item := Item{
		ID: line.Finding, Time: line.TS, Model: line.Model, Answers: map[QuestionID]Answer{},
		Refs: map[string]string{refOffered: strings.Join(line.CandidatesOffered, " ")},
	}
	for id, a := range line.Answers {
		switch {
		case a.Noul != nil:
			item.Answers[QuestionID(id)] = Answer{Noul: a.Noul}
		case a.Score != nil:
			item.Answers[QuestionID(id)] = Answer{Score: a.Score}
		case a.Choice != nil:
			ans := Answer{Choice: *a.Choice, Positive: *a.Choice}
			if a.Confidence != nil {
				ans.Probabilities = map[string]float64{*a.Choice: *a.Confidence}
			}
			item.Answers[QuestionID(id)] = ans
		}
	}
	return item, nil
}

// Label reads the finding bead and labels the line from its close reason. A
// bead still open, or one that no longer exists, leaves the line unlabeled.
func (f *finding) Label(ctx context.Context, item Item) (Labeling, error) {
	issue, err := f.show(item.ID)
	if errors.Is(err, beads.ErrNotFound) {
		return Labeling{}, nil
	}
	if err != nil {
		return Labeling{}, fmt.Errorf("reading bead %s: %w", item.ID, err)
	}
	if issue.Status != "closed" {
		return Labeling{}, nil
	}

	r := parseCloseReason(issue.CloseReason)
	named := namedCandidates(issue.CloseReason, strings.Fields(item.Refs[refOffered]))
	labels := map[QuestionID]bool{}
	rule := ""
	switch {
	case r.landed:
		rule = ruleLanded
		labels[dedupe.QFixWorthy] = true
		labels[dedupe.QReportOnly] = false
	case r.noChanges:
		rule = ruleNoChanges
		labels[dedupe.QFixWorthy] = false
		labels[dedupe.QReportOnly] = true
	case r.notFixWorthy:
		rule = ruleNotFixWorthy
		labels[dedupe.QFixWorthy] = false
	}

	switch {
	case len(named) > 0:
		if rule == "" {
			rule = ruleNamesCandidate
		}
		labels[dedupe.QAnyDuplicate] = true
	case (r.landed || r.noChanges) && !r.notFixWorthy:
		labels[dedupe.QAnyDuplicate] = false
	}
	if dup, ok := labels[dedupe.QAnyDuplicate]; ok {
		if ans, asked := item.Answers[dedupe.QDuplicate]; asked {
			labels[dedupe.QDuplicate] = choiceCorrect(ans.Choice, named, dup)
		}
	}

	if r.landed && r.commit != "" {
		n, err := f.diffs.ChangedLines(ctx, r.commit)
		switch {
		case errors.Is(err, ErrNoCommit):
		case err != nil:
			return Labeling{}, fmt.Errorf("sizing the diff of %s for bead %s: %w", r.commit, item.ID, err)
		case n < smallDiffLines:
			labels[dedupe.QSeverity] = false
		case n > largeDiffLines:
			labels[dedupe.QSeverity] = true
		}
	}
	if len(labels) == 0 {
		return Labeling{}, nil
	}
	return Labeling{Labeled: true, Labels: labels, Rule: rule}, nil
}

// choiceCorrect says whether the duplicate Choice's answer matched the close
// reason: one of the candidates the reason names, or none when it names none.
func choiceCorrect(chose string, named []string, anyDuplicate bool) bool {
	if !anyDuplicate {
		return chose == "none"
	}
	for _, id := range named {
		if strings.EqualFold(chose, id) {
			return true
		}
	}
	return false
}

// closeReason is what a bead's close reason says about its fix.
type closeReason struct {
	// landed: the landing worker closed the bead on a landing.
	landed bool
	// noChanges: a worker closed it with no change to land.
	noChanges bool
	// notFixWorthy: the wording says superseded, duplicate or wontfix.
	notFixWorthy bool
	// commit is the landed commit the reason names, empty when it names none.
	commit string
}

var (
	commitLine = regexp.MustCompile(`(?im)^\s*(?:landed_commit|commit_sha):\s*([0-9a-f]{7,40})\s*$`)
	commitName = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

	notFixWorthyWords = []string{"superseded", "duplicate", "dup of", "dupe", "wontfix", "won't fix", "wont fix", "will not fix"}
)

func parseCloseReason(reason string) closeReason {
	lower := strings.ToLower(strings.TrimSpace(reason))
	var r closeReason
	switch {
	case strings.HasPrefix(lower, "landed"):
		r.landed = true
		if m := commitLine.FindStringSubmatch(reason); m != nil {
			r.commit = strings.ToLower(m[1])
		}
		return r
	case strings.HasPrefix(lower, "no-changes"):
		r.noChanges = true
	}
	for _, w := range notFixWorthyWords {
		if strings.Contains(lower, w) {
			r.notFixWorthy = true
			break
		}
	}
	return r
}

// namedCandidates returns the offered candidates the close reason mentions as
// a whole bead id: gt-ab is not named by a mention of gt-abc or gt-ab.1.
func namedCandidates(reason string, offered []string) []string {
	lower := strings.ToLower(reason)
	var out []string
	for _, id := range offered {
		if id == "" || id == "none" {
			continue
		}
		if mentions(lower, strings.ToLower(id)) {
			out = append(out, id)
		}
	}
	return out
}

func mentions(text, id string) bool {
	for from := 0; from < len(text); {
		i := strings.Index(text[from:], id)
		if i < 0 {
			return false
		}
		start := from + i
		end := start + len(id)
		if (start == 0 || !isIDChar(text[start-1])) && !continuesID(text, end) {
			return true
		}
		from = start + 1
	}
	return false
}

func isIDChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

// continuesID reports whether the id ends before text[end]: a letter, digit,
// dash or underscore continues it, and so does a dot followed by one, as in a
// child id gt-ab.1.
func continuesID(text string, end int) bool {
	if end >= len(text) {
		return false
	}
	if isIDChar(text[end]) {
		return true
	}
	return text[end] == '.' && end+1 < len(text) && isIDChar(text[end+1])
}

// rigBeads reads the finding rig's beads. It never writes.
type rigBeads struct{ town string }

func (r rigBeads) Show(id string) (*beads.Issue, error) {
	dir := doltserver.FindRigBeadsDir(r.town, dedupe.RigName)
	if dir == "" {
		return nil, fmt.Errorf("no beads directory for rig %s", dedupe.RigName)
	}
	return beads.New(dir).Show(id)
}

// ShowMultiple reads several finding beads in one call. A bead the rig does not
// hold is left out of the result.
func (r rigBeads) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	dir := doltserver.FindRigBeadsDir(r.town, dedupe.RigName)
	if dir == "" {
		return nil, fmt.Errorf("no beads directory for rig %s", dedupe.RigName)
	}
	return beads.New(dir).ShowMultiple(ids)
}

// ListByIDs reads several finding beads in one query. A bead the rig does not
// hold is left out of the result.
func (r rigBeads) ListByIDs(ids []string) (map[string]*beads.Issue, error) {
	dir := doltserver.FindRigBeadsDir(r.town, dedupe.RigName)
	if dir == "" {
		return nil, fmt.Errorf("no beads directory for rig %s", dedupe.RigName)
	}
	return beads.ListByIDs(beads.New(dir), ids)
}

// rigDiffs sizes a landed commit in the finding rig's git repo. Its sizes, when
// set, hold what PrefetchLines read; without them every commit costs two git
// calls.
type rigDiffs struct {
	town  string
	sizes *diffSizes
}

// diffSizes is the commits PrefetchLines sized, by the name the close reason
// gave. A commit in absent is one the repo does not hold. A commit in neither
// map was not prefetched, or git could not answer for it, and ChangedLines
// asks git about it directly.
type diffSizes struct {
	lines  map[string]int
	absent map[string]bool
}

// prefetchChunk bounds how many commits go on one git command line.
const prefetchChunk = 200

// PrefetchLines sizes commits with two git calls for the whole set: one
// cat-file --batch-check that finds which the repo holds, and one log --numstat
// over those. It records nothing for a commit it cannot answer for, which
// ChangedLines then asks git about by itself and reports as it always did.
func (r rigDiffs) PrefetchLines(ctx context.Context, commits []string) {
	repo := rigRepo(filepath.Join(r.town, dedupe.RigName))
	if repo == "" || r.sizes == nil {
		return
	}
	var names []string
	seen := map[string]bool{}
	for _, c := range commits {
		if commitName.MatchString(c) && !seen[c] {
			seen[c] = true
			names = append(names, c)
		}
	}
	r.sizes.lines, r.sizes.absent = map[string]int{}, map[string]bool{}
	for len(names) > 0 {
		n := min(prefetchChunk, len(names))
		r.prefetchChunk(ctx, repo, names[:n])
		names = names[n:]
	}
}

func (r rigDiffs) prefetchChunk(ctx context.Context, repo string, names []string) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	var in strings.Builder
	for _, name := range names {
		in.WriteString(name + "^{commit}\n")
	}
	check := exec.CommandContext(ctx, "git", "-C", repo, "cat-file", "--batch-check=%(objectname) %(objecttype)")
	check.Stdin = strings.NewReader(in.String())
	out, err := check.Output()
	if err != nil {
		return
	}
	answers := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(answers) != len(names) {
		return
	}

	var held []string
	byFull := map[string][]string{}
	for i, answer := range answers {
		switch fields := strings.Fields(answer); {
		case len(fields) == 2 && fields[1] == "commit":
			if _, dup := byFull[fields[0]]; !dup {
				held = append(held, fields[0])
			}
			byFull[fields[0]] = append(byFull[fields[0]], names[i])
		case strings.HasSuffix(answer, " missing"):
			r.sizes.absent[names[i]] = true
		}
	}
	if len(held) == 0 {
		return
	}

	// --diff-merges=first-parent is the first-parent diff the single call asks
	// for with commit^..commit, and the \x01 header cannot start a numstat line.
	args := append([]string{"-C", repo, "log", "--no-walk=unsorted", "--numstat", "--no-renames",
		"--diff-merges=first-parent", "--format=%x01%H %P"}, held...)
	logOut, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return
	}
	for _, block := range strings.Split(string(logOut), "\x01")[1:] {
		head, body, _ := strings.Cut(block, "\n")
		fields := strings.Fields(head)
		// A root commit has no parent to diff against: the single call fails on
		// it, and so must the report.
		if len(fields) < 2 {
			continue
		}
		for _, name := range byFull[fields[0]] {
			r.sizes.lines[name] = numstatLines(body)
		}
	}
}

func (r rigDiffs) ChangedLines(ctx context.Context, commit string) (int, error) {
	// A name that is not bare hex could read as a git option, so it never
	// reaches the command line.
	if !commitName.MatchString(commit) {
		return 0, fmt.Errorf("commit %q is not a hex object name", commit)
	}
	if r.sizes != nil {
		if n, ok := r.sizes.lines[commit]; ok {
			return n, nil
		}
		if r.sizes.absent[commit] {
			return 0, ErrNoCommit
		}
	}
	repo := rigRepo(filepath.Join(r.town, dedupe.RigName))
	if repo == "" {
		return 0, fmt.Errorf("rig %s has no git repo (neither .repo.git nor mayor/rig)", dedupe.RigName)
	}
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	// rev-parse exits 1 for a commit the repo does not hold, and 128 for a
	// repo it cannot read, which must fail the report. cat-file -e exits 128
	// for both, so it cannot tell them apart.
	err := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "--verify", "--quiet", commit+"^{commit}").Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return 0, ErrNoCommit
	}
	if err != nil {
		return 0, fmt.Errorf("git rev-parse %s: %w", commit, err)
	}
	out, err := exec.CommandContext(ctx, "git", "-C", repo,
		"diff", "--numstat", "--no-renames", commit+"^.."+commit).Output()
	if err != nil {
		return 0, fmt.Errorf("git diff %s: %w", commit, err)
	}
	return numstatLines(string(out)), nil
}

// numstatLines sums the added and removed counts of `git diff --numstat`
// output. A binary file reports "-" and counts 0.
func numstatLines(out string) int {
	total := 0
	for _, l := range strings.Split(out, "\n") {
		parts := strings.SplitN(l, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		added, _ := strconv.Atoi(parts[0])
		removed, _ := strconv.Atoi(parts[1])
		total += added + removed
	}
	return total
}

// rigRepo is the rig's git repo: the shared bare .repo.git, else mayor/rig.
// It is empty when the rig has neither.
func rigRepo(rigRoot string) string {
	for _, rel := range []string{".repo.git", filepath.Join("mayor", "rig")} {
		dir := filepath.Join(rigRoot, rel)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	return ""
}
