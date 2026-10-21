package scorecard

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/jev/conflict"
	"github.com/steveyegge/gastown/internal/jev/submission"
	"github.com/steveyegge/gastown/internal/land"
)

// ConflictBattery is the conflict battery's name, the --battery value that
// scores the log the conflict pass writes.
const ConflictBattery = "conflict"

// The question ids the conflict battery logs. They must match the ids in
// internal/jev/conflict/questions.go, which does not export them.
// TestScorecardConflictQuestionIDsMatchLogKeys fails when they drift from the
// answer keys the log writes.
const (
	qSameFiles    QuestionID = "same_files"
	qSameFunction QuestionID = "same_function"
	qRisk         QuestionID = "risk"
)

// Rules the conflict adapter names in the --json report's lines.
const (
	ruleConflictRejectedAfterLanding = "rejected:conflict-after-landing"
	ruleConflictSharedFile           = "landed:shared-file"
	ruleConflictDisjointFiles        = "landed:disjoint-files"
)

// The Item.Refs keys the conflict adapter labels from: the two beads of a pair.
const (
	refConflictBead  = "conflict_bead"
	refConflictOther = "conflict_other"
)

// landMergeRe and landRejectRe read the landing worker's lines in the daemon
// log, the record the dashboard's Landings panel reads. A merge line names the
// target tip the merge was built on; a rejection line names the kind.
var (
	landMergeRe  = regexp.MustCompile(`landing_worker: \[land\] (\S+): merged (?:[0-9a-f]+ onto \S+ \(([0-9a-f]+)\))?`)
	landRejectRe = regexp.MustCompile(`landing_worker: \[land\] (\S+): rejected \((\w+)\):`)
)

// conflictRepo is the part of the submission battery's bare clone the adapter
// reads through. *submission.Clone satisfies it.
type conflictRepo interface {
	Sync(ctx context.Context) error
	Run(ctx context.Context, args ...string) (string, error)
}

func init() {
	Register(ConflictBattery, func(townRoot string) Adapter {
		return newConflictAdapter(townRoot, func() (conflictRepo, error) {
			c, err := submission.OpenClone(townRoot)
			if err != nil {
				return nil, err
			}
			return c, nil
		})
	})
}

// conflictAdapter scores the conflict log. Its labels come from two sources:
// the rig's landing records and the bare clone they name, and the daemon log's
// conflict rejections. The clone is opened and synced only when a pair has a
// landing to read, so a scorecard over a log of unlanded pairs never fetches.
type conflictAdapter struct {
	town      string
	openClone func() (conflictRepo, error)

	repo    conflictRepo
	synced  bool
	landing *land.LandingsFile
	// landed is each bead's latest landing record, nil for a bead that never
	// landed. A bead is read once.
	landed map[string]*land.LandingRecord
	// files is each landed bead's changed files, by bead.
	files map[string]map[string]bool
	// rejections is, per bead, the target tip each of its conflict rejections
	// was built on.
	rejections     map[string][]string
	rejectionsRead bool
}

func newConflictAdapter(town string, openClone func() (conflictRepo, error)) *conflictAdapter {
	return &conflictAdapter{
		town:       town,
		openClone:  openClone,
		landed:     map[string]*land.LandingRecord{},
		files:      map[string]map[string]bool{},
		rejections: map[string][]string{},
	}
}

func (a *conflictAdapter) Battery() string { return ConflictBattery }
func (a *conflictAdapter) LogPath() string { return conflict.LogPath(a.town) }

// Parse reads one conflict line. The item is the pair, stamped with the later
// of its two placements.
func (a *conflictAdapter) Parse(raw []byte) (Item, error) {
	var l conflict.Line
	if err := json.Unmarshal(raw, &l); err != nil {
		return Item{}, err
	}
	if l.Bead == "" || l.Other == "" || strings.ContainsAny(l.Bead+l.Other, `/\`) || l.AttachedAt.IsZero() {
		return Item{}, errors.New("conflict line needs two bead ids and an attached_at stamp")
	}
	noul := func(p float64) Answer { return Answer{Noul: &p} }
	risk := l.Answers.Risk.Score
	return Item{
		ID:    l.Bead + "+" + l.Other,
		Time:  l.AttachedAt,
		Model: l.Model,
		Answers: map[QuestionID]Answer{
			qSameFiles:    noul(l.Answers.SameFiles),
			qSameFunction: noul(l.Answers.SameFunction),
			qRisk:         {Score: &risk},
		},
		Refs: map[string]string{refConflictBead: l.Bead, refConflictOther: l.Other},
	}, nil
}

// Label says whether the pair touched the same files. It is positive when
// either bead's conflict rejection was built on a target that already held the
// other's landing, or when both landed and their changed files meet. It is
// negative when both landed and share no file. Otherwise it is unlabeled, and
// so is the same_function question, which no landing shows.
func (a *conflictAdapter) Label(ctx context.Context, item Item) (Labeling, error) {
	bead, other := item.Refs[refConflictBead], item.Refs[refConflictOther]
	landedBead, err := a.landingOf(bead)
	if err != nil {
		return Labeling{}, err
	}
	landedOther, err := a.landingOf(other)
	if err != nil {
		return Labeling{}, err
	}
	if err := a.loadRejections(); err != nil {
		return Labeling{}, err
	}

	var rules []string
	rejected, err := a.rejectedAfter(ctx, bead, landedOther)
	if err != nil {
		return Labeling{}, err
	}
	if !rejected {
		rejected, err = a.rejectedAfter(ctx, other, landedBead)
		if err != nil {
			return Labeling{}, err
		}
	}
	if rejected {
		rules = append(rules, ruleConflictRejectedAfterLanding)
	}

	shared := false
	if landedBead != nil && landedOther != nil {
		fb, err := a.landedFiles(ctx, bead, landedBead)
		if err != nil {
			return Labeling{}, err
		}
		fo, err := a.landedFiles(ctx, other, landedOther)
		if err != nil {
			return Labeling{}, err
		}
		for f := range fb {
			if fo[f] {
				shared = true
				break
			}
		}
		if shared {
			rules = append(rules, ruleConflictSharedFile)
		}
	}

	switch {
	case rejected || shared:
		return Labeling{Labeled: true, Labels: map[QuestionID]bool{qSameFiles: true}, Rule: strings.Join(rules, "+")}, nil
	case landedBead != nil && landedOther != nil:
		return Labeling{Labeled: true, Labels: map[QuestionID]bool{qSameFiles: false}, Rule: ruleConflictDisjointFiles}, nil
	}
	return Labeling{}, nil
}

// landingOf returns the bead's latest landing record, nil when it never landed.
func (a *conflictAdapter) landingOf(bead string) (*land.LandingRecord, error) {
	if rec, ok := a.landed[bead]; ok {
		return rec, nil
	}
	if a.landing == nil {
		f, err := land.RigLandingsFile(a.town, conflict.RigName)
		if err != nil {
			return nil, err
		}
		a.landing = f
	}
	rec, found, err := a.landing.LatestForBead(bead)
	if err != nil {
		return nil, fmt.Errorf("reading the landings of %s: %w", bead, err)
	}
	if !found {
		a.landed[bead] = nil
		return nil, nil
	}
	a.landed[bead] = &rec
	return &rec, nil
}

// rejectedAfter reports whether one of bead's conflict rejections was built on
// a target that already held landed's commit. landed is nil when the other bead
// never landed, which settles nothing.
func (a *conflictAdapter) rejectedAfter(ctx context.Context, bead string, landed *land.LandingRecord) (bool, error) {
	if landed == nil || landed.LandedCommit == "" {
		return false, nil
	}
	for _, tip := range a.rejections[bead] {
		ok, err := a.isAncestor(ctx, landed.LandedCommit, tip)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// landedFiles returns the files a bead's landing changed, from its base to its
// head in the bare clone. A bead is read once.
func (a *conflictAdapter) landedFiles(ctx context.Context, bead string, rec *land.LandingRecord) (map[string]bool, error) {
	if files, ok := a.files[bead]; ok {
		return files, nil
	}
	if !safeRef(rec.Base) || !safeRef(rec.Head) {
		return nil, fmt.Errorf("landing of %s names refs the clone will not read: %q..%q", bead, rec.Base, rec.Head)
	}
	repo, err := a.clone(ctx)
	if err != nil {
		return nil, err
	}
	out, err := repo.Run(ctx, "diff", "--name-only", "--no-renames", rec.Base+".."+rec.Head)
	if err != nil {
		return nil, fmt.Errorf("listing the files of %s: %w", bead, err)
	}
	files := map[string]bool{}
	for _, f := range strings.Split(strings.TrimSpace(out), "\n") {
		if f != "" {
			files[f] = true
		}
	}
	a.files[bead] = files
	return files, nil
}

// isAncestor reports whether commit anc is an ancestor of desc. The merge base of
// the two is anc exactly when it is.
func (a *conflictAdapter) isAncestor(ctx context.Context, anc, desc string) (bool, error) {
	if !safeRef(anc) || !safeRef(desc) {
		return false, fmt.Errorf("refusing ancestry of refs %q and %q", anc, desc)
	}
	repo, err := a.clone(ctx)
	if err != nil {
		return false, err
	}
	base, err := repo.Run(ctx, "merge-base", anc, desc)
	if err != nil {
		return false, fmt.Errorf("merge base of %s and %s: %w", anc, desc, err)
	}
	full, err := repo.Run(ctx, "rev-parse", "--verify", anc+"^{commit}")
	if err != nil {
		return false, fmt.Errorf("resolving %s: %w", anc, err)
	}
	return strings.TrimSpace(base) == strings.TrimSpace(full), nil
}

// clone opens the bare clone on first use and fetches its branches once, so the
// reads after it see the rig's current heads.
func (a *conflictAdapter) clone(ctx context.Context) (conflictRepo, error) {
	if a.repo == nil {
		r, err := a.openClone()
		if err != nil {
			return nil, err
		}
		a.repo = r
	}
	if !a.synced {
		if err := a.repo.Sync(ctx); err != nil {
			return nil, fmt.Errorf("fetching the bare clone: %w", err)
		}
		a.synced = true
	}
	return a.repo, nil
}

// loadRejections reads the conflict rejections from the daemon log once. A
// rejection names the target tip of the merge before it for the same bead. A
// missing log holds none.
func (a *conflictAdapter) loadRejections() error {
	if a.rejectionsRead {
		return nil
	}
	a.rejectionsRead = true
	f, err := os.Open(filepath.Join(a.town, "daemon", "daemon.log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the daemon log: %w", err)
	}
	defer f.Close()

	tips := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, "[land]") {
			continue
		}
		if m := landMergeRe.FindStringSubmatch(line); m != nil {
			if m[2] == "" {
				delete(tips, m[1])
			} else {
				tips[m[1]] = m[2]
			}
			continue
		}
		if m := landRejectRe.FindStringSubmatch(line); m != nil && m[2] == string(land.RejectConflict) {
			if tip := tips[m[1]]; tip != "" {
				a.rejections[m[1]] = append(a.rejections[m[1]], tip)
			}
		}
	}
	return sc.Err()
}

// safeRef refuses a ref git would read as an option or a range.
func safeRef(ref string) bool {
	return ref != "" && !strings.HasPrefix(ref, "-") && !strings.Contains(ref, "..")
}
