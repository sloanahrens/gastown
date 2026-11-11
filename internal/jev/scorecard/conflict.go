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

	"github.com/steveyegge/gastown/internal/guard"
	"github.com/steveyegge/gastown/internal/jev/conflict"
	"github.com/steveyegge/gastown/internal/jev/submission"
	"github.com/steveyegge/gastown/internal/land"
)

// ConflictBattery is the conflict battery's name, the --battery value that
// scores the log the conflict pass writes.
const ConflictBattery = "conflict"

// The question ids the conflict battery logs, taken from the battery so the
// adapter cannot label a key the log never writes.
const (
	qSameFiles    QuestionID = conflict.QuestionSameFiles
	qSameFunction QuestionID = conflict.QuestionSameFunction
	qRisk         QuestionID = conflict.QuestionRisk
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

// landMergeRe, landRejectRe and landRejectTipRe read the landing worker's
// lines in the daemon log, the record the dashboard's Landings panel reads. A
// merge line names the target tip the merge was built on; a conflict rejection
// line names the kind and, since gt-0nvhe, that rejection's own target tip.
var (
	landMergeRe     = regexp.MustCompile(`landing_worker: \[land\] (\S+): merged (?:[0-9a-f]+ onto \S+ \(([0-9a-f]+)\))?`)
	landRejectRe    = regexp.MustCompile(`landing_worker: \[land\] (\S+): rejected \((\w+)\):`)
	landRejectTipRe = regexp.MustCompile(` \(target tip ([0-9a-f]+)\)$`)
)

// ConflictRepo is the part of the submission battery's bare clone the adapter
// reads through. *submission.Clone satisfies it.
type ConflictRepo interface {
	Sync(ctx context.Context) error
	Run(ctx context.Context, args ...string) (string, error)
}

func init() {
	Register(ConflictBattery, func(townRoot string) Adapter {
		return newConflictAdapter(townRoot, func() (ConflictRepo, error) {
			c, err := submission.OpenClone(townRoot)
			if err != nil {
				return nil, err
			}
			return submission.FetchLocked{Cloner: c, TownRoot: townRoot}, nil
		})
	})
}

// OpenReadOnly opens battery's adapter for a read that must not fetch. The
// conflict battery is the only one with a read-only form; another battery is
// refused rather than opened with a fetch.
func OpenReadOnly(battery, townRoot string) (Adapter, error) {
	if battery != ConflictBattery {
		return nil, fmt.Errorf("battery %q has no read-only form", battery)
	}
	return NewConflictReadOnly(townRoot, func() (ConflictRepo, error) {
		c, err := submission.OpenClone(townRoot)
		if err != nil {
			return nil, err
		}
		return c, nil
	}), nil
}

// NewConflictReadOnly returns the conflict adapter over the clone openClone
// returns, read as it stands. Its Sync is never called, so the clone is never
// fetched. A landing whose commits the clone does not hold is unknown, and the
// pair it belongs to is left unlabeled.
func NewConflictReadOnly(town string, openClone func() (ConflictRepo, error)) Adapter {
	a := newConflictAdapter(town, openClone)
	a.readOnly = true
	return a
}

// conflictAdapter scores the conflict log. Its labels come from two sources:
// the rig's landing records and the bare clone they name, and the daemon log's
// conflict rejections. The clone is opened and synced only when a pair has a
// landing to read, so a scorecard over a log of unlanded pairs never fetches.
type conflictAdapter struct {
	town      string
	openClone func() (ConflictRepo, error)
	// readOnly reads the clone as it stands and never syncs it. See
	// NewConflictReadOnly.
	readOnly bool

	repo    ConflictRepo
	synced  bool
	landing *land.LandingsFile
	// landed is each bead's latest landing record, nil for a bead that never
	// landed. A bead is read once.
	landed map[string]*land.LandingRecord
	// files is each landed bead's changed files, by bead.
	files map[string]map[string]bool
	// rejections is, per bead, the target tip each of its conflict rejections
	// was built on.
	rejections map[string][]string
	// ownTips is the part of rejections each rejection line named itself. A tip
	// borrowed from an earlier merge line is stale, so it counts the rejection
	// but cannot attribute a landing.
	ownTips map[string][]string
	// rejected holds every bead with a conflict rejection in the log, tipped or
	// not. A rejection with no tip at all is in neither map above, yet it still
	// rules out calling the bead's pair disjoint.
	rejected map[string]bool
	// conflicts counts every conflict rejection in the log, and unattributed the
	// ones that named no tip of their own: a borrowed tip or none at all. They
	// are counted as they are read, not derived from the maps above, because a
	// rejection with no tip at all is in neither.
	conflicts, unattributed int
	rejectionsRead          bool
}

func newConflictAdapter(town string, openClone func() (ConflictRepo, error)) *conflictAdapter {
	return &conflictAdapter{
		town:       town,
		openClone:  openClone,
		landed:     map[string]*land.LandingRecord{},
		files:      map[string]map[string]bool{},
		rejections: map[string][]string{},
		ownTips:    map[string][]string{},
		rejected:   map[string]bool{},
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

// Context says how many conflict rejections in the daemon log named no target
// tip of their own, whether they borrowed an earlier merge line's tip or found
// none. Those count against a disjoint label but cannot attribute a landing, so
// a log whose rejection lines lost their tip would leave pairs unlabeled with
// nothing else to show why.
func (a *conflictAdapter) Context() []string {
	if a.unattributed == 0 {
		return nil
	}
	return []string{fmt.Sprintf("conflict: %d of %d conflict rejection(s) in the daemon log name no target tip of their own, so they cannot attribute a landing", a.unattributed, a.conflicts)}
}

// Label says whether the pair touched the same files. It is positive when
// either bead's conflict rejection was built on a target that already held the
// other's landing, or when both landed and their changed files meet. It is
// negative when both landed and share no file, unless one of them was rejected
// for a conflict that cannot be pinned on the other bead: a rejection the
// ancestry test cannot attribute is evidence of a conflict with *something*,
// and calling the pair disjoint on it would be a wrong negative. Otherwise the
// pair is unlabeled, and so is the same_function question, which no landing
// shows. A read-only adapter also leaves the pair unlabeled, rather than
// negative, when a landing's commits are not in the clone.
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
	// A conflict rejection that the ancestry test did not attribute to the
	// other bead still rules out a negative label, because it says the bead did
	// conflict with something. In the negative branch below it is always one
	// the test could not attribute.
	rejectedSomewhere := a.hasRejection(bead) || a.hasRejection(other)

	shared := false
	// known is whether both beads' changed files were read. A read-only adapter
	// may not hold a landing's commits, and then neither shared nor disjoint
	// can be said.
	known := false
	if landedBead != nil && landedOther != nil {
		held, err := a.holdsLandings(ctx, landedBead, landedOther)
		if err != nil {
			return Labeling{}, err
		}
		known = held.IsPass()
	}
	if known {
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
	case known && !rejectedSomewhere:
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

// hasRejection reports whether the daemon log holds any conflict rejection of
// bead. On its own it settles nothing — the rejection may name a third bead —
// but it is enough to stop a pair being labeled disjoint.
func (a *conflictAdapter) hasRejection(bead string) bool {
	return a.rejected[bead]
}

// rejectedAfter reports whether one of bead's conflict rejections was built on
// a target that already held landed's commit. landed is nil when the other bead
// never landed, which settles nothing. Only a rejection's own tip attributes a
// landing: an ancestry test against a stale tip borrowed from an earlier merge
// line says nothing about the attempt that conflicted. A rejection whose target
// the clone cannot trace settles nothing either; the caller then leaves the pair
// unlabeled, because a conflict it cannot attribute is still no sign of a
// disjoint pair.
func (a *conflictAdapter) rejectedAfter(ctx context.Context, bead string, landed *land.LandingRecord) (bool, error) {
	if landed == nil || landed.LandedCommit == "" {
		return false, nil
	}
	for _, tip := range a.ownTips[bead] {
		res, err := a.isAncestor(ctx, landed.LandedCommit, tip)
		if err != nil {
			return false, err
		}
		switch {
		case res.IsPass():
			return true, nil
		case res.IsUnknown():
			// A later rejection of the same bead may still attribute the pair.
			continue
		}
	}
	return false, nil
}

// landedFiles returns the files a bead's landing changed, from its fork point
// to its head in the bare clone: the range is three-dot because the record's
// base is the target tip at land time, which can hold landings made after the
// bead forked (gt-wu2ab). A bead is read once.
func (a *conflictAdapter) landedFiles(ctx context.Context, bead string, rec *land.LandingRecord) (map[string]bool, error) {
	if files, ok := a.files[bead]; ok {
		return files, nil
	}
	if !safeRef(rec.Base) || !safeRef(rec.Head) {
		return nil, fmt.Errorf("landing of %s names refs the clone will not read: %q...%q", bead, rec.Base, rec.Head)
	}
	repo, err := a.clone(ctx)
	if err != nil {
		return nil, err
	}
	out, err := repo.Run(ctx, "diff", "--name-only", "--no-renames", rec.Base+"..."+rec.Head)
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

// holdsLandings says whether the clone holds the base and head commits of every
// landing given, the commits their changed files are read between. A read-only
// adapter asks first. A commit it lacks is Unknown, which leaves the pair
// unlabeled, as isAncestor leaves a commit the clone cannot trace. A fetching
// adapter reports Pass, since its clone fetches on the first read. A ref that
// fails the safety check is skipped, so landedFiles refuses it as before.
func (a *conflictAdapter) holdsLandings(ctx context.Context, recs ...*land.LandingRecord) (guard.Result, error) {
	if !a.readOnly {
		return guard.Pass(), nil
	}
	repo, err := a.clone(ctx)
	if err != nil {
		return guard.Result{}, err
	}
	for _, rec := range recs {
		for _, ref := range []string{rec.Base, rec.Head} {
			if !safeRef(ref) {
				continue
			}
			if _, err := repo.Run(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
				return guard.Unknown(fmt.Errorf("resolving %s: %w", ref, err)), nil
			}
		}
	}
	return guard.Pass(), nil
}

// isAncestor reports whether commit anc is an ancestor of desc. The merge base of
// the two is anc exactly when it is.
//
// The answer is three-way. Pass and Fail are answers the clone reached.
// Unknown says it could not decide: it does not hold a commit the two name (a
// target tip from a landing it never fetched, a rejection older than its
// history) or the two histories have no merge base. The caller leaves the pair
// unlabeled on Unknown, so a commit the clone cannot trace never becomes a
// negative label. An error means the clone itself would not open or sync, and
// fails the pass.
func (a *conflictAdapter) isAncestor(ctx context.Context, anc, desc string) (guard.Result, error) {
	if !safeRef(anc) || !safeRef(desc) {
		return guard.Result{}, fmt.Errorf("refusing ancestry of refs %q and %q", anc, desc)
	}
	repo, err := a.clone(ctx)
	if err != nil {
		return guard.Result{}, err
	}
	full := make([]string, 0, 2)
	for _, ref := range []string{anc, desc} {
		out, err := repo.Run(ctx, "rev-parse", "--verify", ref+"^{commit}")
		if err != nil {
			return guard.Unknown(fmt.Errorf("resolving %s: %w", ref, err)), nil
		}
		full = append(full, strings.TrimSpace(out))
	}
	base, err := repo.Run(ctx, "merge-base", full[0], full[1])
	if err != nil {
		return guard.Unknown(fmt.Errorf("merge base of %s and %s: %w", anc, desc, err)), nil
	}
	if strings.TrimSpace(base) == full[0] {
		return guard.Pass(), nil
	}
	return guard.Fail(fmt.Sprintf("%s is not an ancestor of %s", anc, desc)), nil
}

// clone opens the bare clone on first use and fetches its branches once, so the
// reads after it see the rig's current heads. A read-only adapter opens it and
// never fetches.
func (a *conflictAdapter) clone(ctx context.Context) (ConflictRepo, error) {
	if a.repo == nil {
		r, err := a.openClone()
		if err != nil {
			return nil, err
		}
		a.repo = r
	}
	if !a.synced && !a.readOnly {
		if err := a.repo.Sync(ctx); err != nil {
			return nil, fmt.Errorf("fetching the bare clone: %w", err)
		}
		a.synced = true
	}
	return a.repo, nil
}

// loadRejections reads the conflict rejections from the daemon log once. A
// rejection names the target tip it was built on. A log line written before
// the tip moved onto the rejection line names only the bead, and that
// rejection's tip is the last merge line for it; a conflict writes no merge
// line of its own, so such a tip is the earlier attempt's (gt-0nvhe). A
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
			a.conflicts++
			a.rejected[m[1]] = true
			if tm := landRejectTipRe.FindStringSubmatch(line); tm != nil {
				a.rejections[m[1]] = append(a.rejections[m[1]], tm[1])
				a.ownTips[m[1]] = append(a.ownTips[m[1]], tm[1])
			} else {
				a.unattributed++
				if tip := tips[m[1]]; tip != "" {
					a.rejections[m[1]] = append(a.rejections[m[1]], tip)
				}
			}
		}
	}
	return sc.Err()
}

// safeRef refuses a ref git would read as an option or a range.
func safeRef(ref string) bool {
	return ref != "" && !strings.HasPrefix(ref, "-") && !strings.Contains(ref, "..")
}
