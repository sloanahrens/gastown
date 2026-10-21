package scorecard

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/jev/conflict"
	"github.com/steveyegge/gastown/internal/land"
)

// TestScorecardConflictQuestionIDsMatchLogKeys fails when the adapter's question
// ids drift from the answer keys the conflict log writes.
func TestScorecardConflictQuestionIDsMatchLogKeys(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(conflict.Line{Bead: "gt-a", Other: "gt-b", Answers: conflict.Answers{}})
	if err != nil {
		t.Fatal(err)
	}
	var line struct {
		Answers map[string]json.RawMessage `json:"answers"`
	}
	if err := json.Unmarshal(raw, &line); err != nil {
		t.Fatal(err)
	}
	for _, id := range []QuestionID{qSameFiles, qSameFunction, qRisk} {
		if _, ok := line.Answers[string(id)]; !ok {
			t.Errorf("the conflict log has no answer key %q", id)
		}
	}
}

func TestConflictAdapterParsesAPair(t *testing.T) {
	t.Parallel()
	a := newConflictAdapter(t.TempDir(), nil)
	raw := conflictLine(t, "gt-b", "gt-a", 0.8, 0.2, 2)

	item, err := a.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if item.ID != "gt-b+gt-a" || item.Model != "jev-test-1" {
		t.Errorf("item = %s model %q, want gt-b+gt-a from jev-test-1", item.ID, item.Model)
	}
	if got := item.Refs[refConflictBead] + "," + item.Refs[refConflictOther]; got != "gt-b,gt-a" {
		t.Errorf("refs = %s, want gt-b,gt-a", got)
	}
	if v := item.Answers[qSameFiles].Noul; v == nil || *v != 0.8 {
		t.Errorf("same_files = %v, want 0.8", v)
	}
	if v := item.Answers[qRisk].Score; v == nil || *v != 2 {
		t.Errorf("risk = %v, want score 2", v)
	}
}

func TestConflictAdapterRejectsALineWithoutTwoBeads(t *testing.T) {
	t.Parallel()
	a := newConflictAdapter(t.TempDir(), nil)
	raw := conflictLine(t, "gt-b", "", 0.8, 0.2, 2)
	if _, err := a.Parse(raw); err == nil {
		t.Error("a line with no second bead must be skipped")
	}
}

// conflictLine is one conflict log line for the pair, with the given noul
// answers and risk score.
func conflictLine(t *testing.T, bead, other string, sameFiles, sameFunction float64, risk float64) []byte {
	t.Helper()
	at := time.Date(2026, 10, 21, 10, 20, 0, 0, time.UTC)
	raw, err := json.Marshal(conflict.Line{
		TS:              at,
		Bead:            bead,
		AttachedAt:      at,
		Other:           other,
		OtherAttachedAt: at.Add(-20 * time.Minute),
		Answers: conflict.Answers{
			SameFiles:    sameFiles,
			SameFunction: sameFunction,
			Risk:         conflict.RiskAnswer{Score: risk, Confidence: 0.5},
		},
		QuestionVersion: conflict.Version,
		Model:           "jev-test-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// conflictCase is one pair to label. commits are the files each commit of the
// main branch writes, in order; the landings and the daemon log name them.
type conflictCase struct {
	name string
	// commits writes each commit of the repo's main branch.
	commits []map[string]string
	// landings names the commit each bead landed, by bead ID. The record's base
	// is the commit before it.
	landings map[string]int
	// log is the daemon log lines the case writes, built from the commit shas.
	log func(sha []string) string
	// want is the label the pair gets.
	want Labeling
}

func TestConflictAdapterLabelsAPair(t *testing.T) {
	t.Parallel()
	cases := []conflictCase{
		{
			name: "shared file",
			commits: []map[string]string{
				{"base.txt": "base"},
				{"b.go": "b", "shared.go": "b"},
				{"a.go": "a", "shared.go": "a"},
			},
			landings: map[string]int{"gt-b": 1, "gt-a": 2},
			want:     Labeling{Labeled: true, Labels: map[QuestionID]bool{qSameFiles: true}, Rule: ruleConflictSharedFile},
		},
		{
			name: "disjoint files",
			commits: []map[string]string{
				{"base.txt": "base"},
				{"b.go": "b"},
				{"a.go": "a"},
			},
			landings: map[string]int{"gt-b": 1, "gt-a": 2},
			want:     Labeling{Labeled: true, Labels: map[QuestionID]bool{qSameFiles: false}, Rule: ruleConflictDisjointFiles},
		},
		{
			name: "one bead never landed",
			commits: []map[string]string{
				{"base.txt": "base"},
				{"b.go": "b"},
			},
			landings: map[string]int{"gt-b": 1},
			want:     Labeling{},
		},
		{
			name: "rejected against a target that held the other's landing",
			commits: []map[string]string{
				{"base.txt": "base"},
				{"b.go": "b"},
				{"a.go": "a"},
			},
			landings: map[string]int{"gt-b": 1, "gt-a": 2},
			log: func(sha []string) string {
				// gt-a was merged onto the tip at commit 2, which holds gt-b's
				// landed commit, and the merge conflicted there.
				return mergeLine("gt-a", sha[2], sha[2], "cand1") + rejectLine("gt-a", "conflict")
			},
			want: Labeling{Labeled: true, Labels: map[QuestionID]bool{qSameFiles: true}, Rule: ruleConflictRejectedAfterLanding},
		},
		{
			name: "rejected against a target from before the other landed",
			commits: []map[string]string{
				{"base.txt": "base"},
				{"b.go": "b"},
				{"a.go": "a"},
			},
			landings: map[string]int{"gt-b": 1, "gt-a": 2},
			log: func(sha []string) string {
				// The conflict was against the base, which does not hold gt-b. The
				// rejection cannot be attributed to this pair, so the pair stays
				// unlabeled: a conflict happened, and calling the beads disjoint
				// would be a wrong negative.
				return mergeLine("gt-a", sha[2], sha[0], "cand1") + rejectLine("gt-a", "conflict")
			},
			want: Labeling{},
		},
		{
			name: "rejected against a tip the clone does not hold",
			commits: []map[string]string{
				{"base.txt": "base"},
				{"b.go": "b"},
				{"a.go": "a"},
			},
			landings: map[string]int{"gt-b": 1, "gt-a": 2},
			log: func(sha []string) string {
				// The tip was never fetched into the clone, so ancestry settles
				// nothing and the pair stays unlabeled.
				return mergeLine("gt-a", sha[2], "deadbeef", "cand1") + rejectLine("gt-a", "conflict")
			},
			want: Labeling{},
		},
		{
			name: "a conflict on the first attempt names its target tip",
			commits: []map[string]string{
				{"base.txt": "base"},
				{"b.go": "b"},
				{"a.go": "a"},
			},
			landings: map[string]int{"gt-b": 1, "gt-a": 2},
			log: func(sha []string) string {
				// The first attempt conflicted, so it wrote no merge line: the
				// rejection line's own tip is all the record holds.
				return rejectLineTip("gt-a", "conflict", sha[2])
			},
			want: Labeling{Labeled: true, Labels: map[QuestionID]bool{qSameFiles: true}, Rule: ruleConflictRejectedAfterLanding},
		},
		{
			name: "a conflict after an earlier merge reads its own tip, not the stale merge tip",
			commits: []map[string]string{
				{"base.txt": "base"},
				{"b.go": "b"},
				{"a.go": "a"},
			},
			landings: map[string]int{"gt-b": 1, "gt-a": 2},
			log: func(sha []string) string {
				// gt-a's earlier attempt merged onto the base, before gt-b
				// landed there. This attempt conflicts on the tip that holds
				// gt-b, and only the rejection line names it.
				return mergeLine("gt-a", sha[2], sha[0], "cand1") + rejectLineTip("gt-a", "conflict", sha[2])
			},
			want: Labeling{Labeled: true, Labels: map[QuestionID]bool{qSameFiles: true}, Rule: ruleConflictRejectedAfterLanding},
		},
		{
			name: "a rejection of another kind is not a conflict",
			commits: []map[string]string{
				{"base.txt": "base"},
				{"b.go": "b"},
				{"a.go": "a"},
			},
			landings: map[string]int{"gt-b": 1, "gt-a": 2},
			log: func(sha []string) string {
				return mergeLine("gt-a", sha[2], sha[2], "cand1") + rejectLine("gt-a", "gate")
			},
			want: Labeling{Labeled: true, Labels: map[QuestionID]bool{qSameFiles: false}, Rule: ruleConflictDisjointFiles},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			repo, sha := linearHistory(c.commits)
			writeLandings(t, town, sha, c.landings)
			if c.log != nil {
				writeDaemonLog(t, town, c.log(sha))
			}
			a := newConflictAdapter(town, func() (conflictRepo, error) { return repo, nil })
			item, err := a.Parse(conflictLine(t, "gt-b", "gt-a", 0.5, 0.5, 1))
			if err != nil {
				t.Fatal(err)
			}

			got, err := a.Label(context.Background(), item)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("label = %+v, want %+v", got, c.want)
			}
		})
	}
}

// A bead that forked before another landed stays disjoint from it when the
// landing record's base has advanced to a tip holding the other's work: the
// file set is the bead's own change from its fork point, and a diff against the
// base reads the target's changes since the fork backwards, as this bead's
// (gt-wu2ab).
func TestConflictAdapterLabelsATargetThatAdvancedAfterFork(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	repo, sha := linearHistory([]map[string]string{
		{"base.txt": "base"},
		{"a.go": "a"}, // gt-o's landing
	})
	// gt-b forked at the root, so gt-o's commit is not in its history.
	b := repo.fork(t, sha[0], map[string]string{"b.go": "b"})
	f, err := land.RigLandingsFile(town, conflict.RigName)
	if err != nil {
		t.Fatal(err)
	}
	appendLanding(t, f, "gt-o", sha[0], sha[1])
	// gt-b merged onto the tip gt-o left, so its base holds gt-o's landing.
	appendLanding(t, f, "gt-b", sha[1], b)

	a := newConflictAdapter(town, func() (conflictRepo, error) { return repo, nil })
	item, err := a.Parse(conflictLine(t, "gt-b", "gt-o", 0.5, 0.5, 1))
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Label(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	want := Labeling{Labeled: true, Labels: map[QuestionID]bool{qSameFiles: false}, Rule: ruleConflictDisjointFiles}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("label = %+v, want %+v", got, want)
	}
}

// A pair where neither bead landed and nothing was rejected labels nothing, and
// never opens the clone, because it has nothing to read there.
func TestConflictAdapterLeavesAnUnlandedPairAloneWithoutTheClone(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	a := newConflictAdapter(town, func() (conflictRepo, error) {
		t.Error("the clone was opened for a pair with no landing and no rejection")
		return nil, fmt.Errorf("not opened")
	})
	item, err := a.Parse(conflictLine(t, "gt-b", "gt-a", 0.5, 0.5, 1))
	if err != nil {
		t.Fatal(err)
	}
	got, err := a.Label(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	if got.Labeled {
		t.Errorf("label = %+v, want none", got)
	}
}

// linearRepo stands in for the bare clone with a main branch that runs in a
// line unless a test forks a commit off an earlier one. Each commit carries the
// full tree it leaves, so a diff reads either two trees as they stand or, for a
// three-dot range, the head against the two commits' merge base.
type linearRepo struct {
	order  []string
	parent map[string]string            // commit -> its parent, "" for a root
	tree   map[string]map[string]string // commit -> path -> content
}

func (r *linearRepo) Sync(context.Context) error { return nil }

func (r *linearRepo) Run(_ context.Context, args ...string) (string, error) {
	switch args[0] {
	case "diff":
		base, head, threeDot, err := r.rangeOf(args[len(args)-1])
		if err != nil {
			return "", err
		}
		if threeDot {
			if base, err = r.mergeBase(base, head); err != nil {
				return "", err
			}
		}
		return r.changed(base, head), nil
	case "merge-base":
		mb, err := r.mergeBase(args[1], args[2])
		if err != nil {
			return "", err
		}
		return mb + "\n", nil
	case "rev-parse":
		sha := strings.TrimSuffix(args[len(args)-1], "^{commit}")
		if _, err := r.index(sha); err != nil {
			return "", err
		}
		return sha + "\n", nil
	}
	return "", fmt.Errorf("unexpected git %s", args[0])
}

func (r *linearRepo) index(sha string) (int, error) {
	for i, c := range r.order {
		if c == sha {
			return i, nil
		}
	}
	return 0, fmt.Errorf("no commit %s", sha)
}

// rangeOf splits a diff range, base..head or base...head, and says which form
// it was.
func (r *linearRepo) rangeOf(spec string) (base, head string, threeDot bool, err error) {
	sep := ".."
	if strings.Contains(spec, "...") {
		sep, threeDot = "...", true
	}
	base, head, _ = strings.Cut(spec, sep)
	for _, c := range []string{base, head} {
		if _, err := r.index(c); err != nil {
			return "", "", false, err
		}
	}
	return base, head, threeDot, nil
}

// mergeBase is the newest commit both can reach, an error when they share none.
func (r *linearRepo) mergeBase(a, b string) (string, error) {
	reach := map[string]bool{}
	for c := a; c != ""; c = r.parent[c] {
		reach[c] = true
	}
	for c := b; c != ""; c = r.parent[c] {
		if reach[c] {
			return c, nil
		}
	}
	return "", fmt.Errorf("no merge base of %s and %s", a, b)
}

// changed lists the paths whose content differs between two commits' trees.
func (r *linearRepo) changed(from, to string) string {
	var names []string
	for path, content := range r.tree[to] {
		if r.tree[from][path] != content {
			names = append(names, path)
		}
	}
	for path := range r.tree[from] {
		if _, ok := r.tree[to][path]; !ok {
			names = append(names, path)
		}
	}
	sort.Strings(names)
	return strings.Join(names, "\n")
}

// fork adds a commit off an earlier one, standing in for a branch that forked
// before the commits after its parent landed.
func (r *linearRepo) fork(t *testing.T, parent string, changed map[string]string) string {
	t.Helper()
	if _, err := r.index(parent); err != nil {
		t.Fatal(err)
	}
	sha := fmt.Sprintf("c%d", len(r.order))
	r.order = append(r.order, sha)
	r.parent[sha] = parent
	r.tree[sha] = writeFiles(r.tree[parent], changed)
	return sha
}

// linearHistory makes a linear repo from the files each commit changed, and
// returns it with the commit names c0, c1, ... in order.
func linearHistory(commits []map[string]string) (*linearRepo, []string) {
	r := &linearRepo{parent: map[string]string{}, tree: map[string]map[string]string{}}
	prev := ""
	for i, changed := range commits {
		sha := fmt.Sprintf("c%d", i)
		r.order = append(r.order, sha)
		r.parent[sha] = prev
		r.tree[sha] = writeFiles(r.tree[prev], changed)
		prev = sha
	}
	return r, r.order
}

// writeFiles copies a tree with the given paths written over it.
func writeFiles(tree, changed map[string]string) map[string]string {
	out := make(map[string]string, len(tree)+len(changed))
	for path, content := range tree {
		out[path] = content
	}
	for path, content := range changed {
		out[path] = content
	}
	return out
}

// writeLandings records each bead's landing: its head is the commit it named,
// and its base is the commit before that.
func writeLandings(t *testing.T, town string, sha []string, landings map[string]int) {
	t.Helper()
	f, err := land.RigLandingsFile(town, conflict.RigName)
	if err != nil {
		t.Fatal(err)
	}
	for bead, i := range landings {
		base := ""
		if i > 0 {
			base = sha[i-1]
		}
		appendLanding(t, f, bead, base, sha[i])
	}
}

// appendLanding records one bead's landing from base to head.
func appendLanding(t *testing.T, f *land.LandingsFile, bead, base, head string) {
	t.Helper()
	if err := f.Append(land.LandingRecord{
		BeadID:       bead,
		Rig:          conflict.RigName,
		Branch:       "polecat/x/" + bead,
		Head:         head,
		Base:         base,
		Target:       "main",
		LandedCommit: head,
		LandedAt:     time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
}

// writeDaemonLog writes the daemon log the rejection reader parses.
func writeDaemonLog(t *testing.T, town, lines string) {
	t.Helper()
	dir := filepath.Join(town, "daemon")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "daemon.log"), []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
}

// mergeLine is the landing worker's merge line for bead: its head, the target
// tip it merged onto, and the candidate.
func mergeLine(bead, head, tip, cand string) string {
	return fmt.Sprintf("2026/10/21 10:00:00 landing_worker: [land] %s: merged %s onto main (%s) as %s\n", bead, head, tip, cand)
}

// rejectLine is the landing worker's rejection line for bead, of kind.
func rejectLine(bead, kind string) string {
	return fmt.Sprintf("2026/10/21 10:01:00 landing_worker: [land] %s: rejected (%s): merging conflicts in 1 file\n", bead, kind)
}

// rejectLineTip is the landing worker's rejection line for a conflict, carrying
// the target tip the merge was built on.
func rejectLineTip(bead, kind, tip string) string {
	return fmt.Sprintf("2026/10/21 10:01:00 landing_worker: [land] %s: rejected (%s): merging conflicts in 1 file (target tip %s)\n", bead, kind, tip)
}
