package land

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// verdictCommentRE takes a work bead's verdict comment apart: every field of
// the line Land writes, in the order it writes them. The re-score clause is the
// one optional field: only a verdict a borderline rejection was re-scored into
// carries it (gt-cz07b).
var verdictCommentRE = regexp.MustCompile(`^om (.+) (\d+\.\d\d) \(om (\S+), (\S+)–(\S+)\) \[head ([0-9a-f]{8})\]( \(re-scored: .+\))?$`)

// parsedVerdictComment is a verdict comment's fields.
type parsedVerdictComment struct {
	word    string
	score   string
	version string
	start   time.Time
	end     time.Time
	head    string
	rescore string // "" on a verdict om reached in one run
}

// parseVerdictComment fails the test when text is not a verdict comment in the
// shape the landing writes, and returns its fields.
func parseVerdictComment(t *testing.T, text string) parsedVerdictComment {
	t.Helper()
	m := verdictCommentRE.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		t.Fatalf("comment = %q, want the shape of a verdict comment", text)
	}
	parse := func(name, value string) time.Time {
		at, err := time.Parse(time.RFC3339, value)
		if err != nil {
			t.Fatalf("comment %s = %q, want RFC3339: %v", name, value, err)
		}
		return at
	}
	parsed := parsedVerdictComment{word: m[1], score: m[2], version: m[3],
		start: parse("start", m[4]), end: parse("end", m[5]), head: m[6], rescore: strings.TrimSpace(m[7])}
	if parsed.end.Before(parsed.start) {
		t.Errorf("comment window ends before it starts: %s–%s", m[4], m[5])
	}
	return parsed
}

// beadComments is the work bead's comments.
func (f *landFixture) beadComments() []beads.Comment {
	f.t.Helper()
	comments, err := f.bd.Comments(f.work.BeadID)
	if err != nil {
		f.t.Fatal(err)
	}
	return comments
}

// TestLandCommentsTheOmVerdictOnEveryOutcome: every review outcome leaves
// exactly one comment on the work bead naming the verdict, the om behind it
// and the head it covered, so a bead can be tied to its review (gt-uebmq).
func TestLandCommentsTheOmVerdictOnEveryOutcome(t *testing.T) {
	t.Parallel()
	const version = "v1.2.3"
	reviewing := func(v Verdict) func(*Lander, *landFixture) error {
		return func(l *Lander, _ *landFixture) error { l.Reviewer = &recordBeadReviewer{verdict: v}; return nil }
	}
	for _, tc := range []struct {
		name     string
		setup    func(l *Lander, f *landFixture) error
		landed   bool
		rejected bool
		word     string
		score    string
		version  string
	}{
		{
			name:    "an approve",
			setup:   reviewing(Verdict{Verdict: VerdictApprove, Score: 0.92, Version: version}),
			landed:  true,
			word:    VerdictApprove,
			score:   "0.92",
			version: version,
		},
		{
			// The findings bounce this approval after the review is over, so
			// the verdict is still commented: a findings check ordered ahead
			// of the comment would leave the rejected head with no line tying
			// it to its review (gt-vu18r).
			name: "an approve the findings bounce",
			setup: reviewing(Verdict{Verdict: VerdictApprove, Score: 0.93, Version: version,
				Findings: []Finding{{ID: "f-1", Severity: "major", Path: "b.txt", Line: 1, Title: "not atomic"}}}),
			rejected: true,
			word:     VerdictApprove,
			score:    "0.93",
			version:  version,
		},
		{
			name:     "a request for changes",
			setup:    reviewing(Verdict{Verdict: VerdictRequestChanges, Score: 0.40, Version: version}),
			rejected: true,
			word:     VerdictRequestChanges,
			score:    "0.40",
			version:  version,
		},
		{
			name: "an om that could not run",
			setup: func(l *Lander, _ *landFixture) error {
				l.ReviewErrorLands = true
				l.Reviewer = &recordBeadReviewer{verdict: Verdict{Version: version},
					err: errors.New("om review did not run: exec: om: not found")}
				return nil
			},
			landed:  true,
			word:    VerdictErrorPrefix + "om review did not run: exec: om: not found",
			score:   "0.00",
			version: version,
		},
		{
			name:    "review disabled",
			setup:   func(l *Lander, _ *landFixture) error { l.Reviewer = SkipReviewer{}; return nil },
			landed:  true,
			word:    VerdictSkipped,
			score:   "0.00",
			version: omVersionUnknown,
		},
		{
			name: "an overseer-reviewed head",
			setup: func(_ *Lander, f *landFixture) error {
				if err := f.bd.AppendNotes(f.work.BeadID, OverseerReviewedMarker+" "+f.work.Head); err != nil {
					return err
				}
				return f.bd.Update(f.work.BeadID, beads.UpdateOptions{AddLabels: []string{LabelOverseerReviewed}})
			},
			landed:  true,
			word:    "skipped (overseer-reviewed)",
			score:   "0.00",
			version: omVersionUnknown,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			l := f.lander()
			if err := tc.setup(l, f); err != nil {
				t.Fatal(err)
			}
			res, landErr := l.Land(context.Background(), f.work)
			if tc.landed && landErr != nil {
				t.Fatalf("Land: %v, want it to land", landErr)
			}
			if tc.rejected {
				var rej *Rejection
				if !errors.As(landErr, &rej) {
					t.Fatalf("Land error = %v, want a rejection", landErr)
				}
				// The comment is an addition to the bead, not a replacement
				// for what a rejection writes there.
				if !strings.Contains(f.bead().Notes, MergeRejectionNoteMarker) {
					t.Errorf("the rejection note is gone: %q", f.bead().Notes)
				}
			}

			comments := f.beadComments()
			if len(comments) != 1 {
				t.Fatalf("comments = %d (%q), want exactly one", len(comments), commentTexts(comments))
			}
			got := parseVerdictComment(t, comments[0].Text)
			if got.word != tc.word {
				t.Errorf("verdict word = %q, want %q (comment %q)", got.word, tc.word, comments[0].Text)
			}
			if got.score != tc.score {
				t.Errorf("score = %q, want %q", got.score, tc.score)
			}
			if got.version != tc.version {
				t.Errorf("version = %q, want %q", got.version, tc.version)
			}
			// The comment is keyed on the head the review covered: the merge
			// candidate, which is the landed commit on every outcome that
			// lands.
			if tc.landed && got.head != shortSHA(res.LandedCommit) {
				t.Errorf("head = %s, want the candidate %s", got.head, shortSHA(res.LandedCommit))
			}
		})
	}
}

// TestLandWritesNoSecondVerdictCommentOnARetry: the comment is keyed on the
// landing attempt — the candidate head and the verdict — so a pass that
// already wrote its own finds it and writes nothing (gt-uebmq).
func TestLandWritesNoSecondVerdictCommentOnARetry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		seedWord string
		seedHead string
		want     int
	}{
		{name: "the same head and verdict", seedWord: VerdictApprove, want: 1},
		{name: "the same head, another verdict", seedWord: VerdictRequestChanges, want: 2},
		{name: "another head, the same verdict", seedWord: VerdictApprove,
			seedHead: "1111111111111111111111111111111111111111", want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			l := f.lander()
			seed := &seedingReviewer{t: t, bd: f.bd, seedWord: tc.seedWord, seedHead: tc.seedHead,
				verdict: Verdict{Verdict: VerdictApprove, Score: 0.9}}
			l.Reviewer = seed
			if _, err := l.Land(context.Background(), f.work); err != nil {
				t.Fatalf("Land: %v", err)
			}
			comments := f.beadComments()
			if len(comments) != tc.want {
				t.Fatalf("comments = %d (%q), want %d", len(comments), commentTexts(comments), tc.want)
			}
			if tc.want == 1 && commentTexts(comments)[0] != seed.seeded {
				t.Errorf("comment = %q, want the record this attempt already had (%q)",
					commentTexts(comments)[0], seed.seeded)
			}
		})
	}
}

// commentTexts is the comments' text, for an assertion a failure can read.
func commentTexts(comments []beads.Comment) []string {
	out := make([]string, len(comments))
	for i, c := range comments {
		out[i] = strings.TrimSpace(c.Text)
	}
	return out
}

// seedingReviewer writes the comment its landing attempt is keyed on before
// Land writes its own: an earlier pass over the same attempt that left the
// record behind (gt-uebmq). It seeds the head Land told it it was reviewing,
// so the record is keyed on the same attempt.
type seedingReviewer struct {
	t        *testing.T
	bd       *beadsfake.Fake
	seedWord string
	seedHead string // "" seeds the head the review was told
	verdict  Verdict
	seeded   string
}

func (r *seedingReviewer) Review(context.Context, string, string, string) (Verdict, error) {
	return r.verdict, nil
}

func (r *seedingReviewer) ReviewBead(_ context.Context, in ReviewInput) (Verdict, error) {
	if r.seedWord != "" {
		head := r.seedHead
		if head == "" {
			head = in.Head
		}
		start := time.Date(2026, 9, 30, 11, 0, 0, 0, time.UTC)
		r.seeded = verdictCommentText(Verdict{Verdict: r.seedWord, Score: 0.11, Version: "v0.1.0"},
			head, start, start.Add(time.Minute))
		if err := r.bd.AddComment(in.BeadID, r.seeded); err != nil {
			r.t.Fatal(err)
		}
	}
	return r.verdict, nil
}

// TestLandSurvivesAVerdictCommentFailure: the landing is decided by the gate
// and the review, so a bead's comments that cannot be read or written cost a
// log line and leave the result where it was (gt-uebmq).
func TestLandSurvivesAVerdictCommentFailure(t *testing.T) {
	t.Parallel()
	// The landing a working bead write produces, for the failing cases to be
	// compared against: two landings cannot share one fixture, and the fake
	// world makes them identical.
	control := newLandFixture(t)
	controlRes, err := control.lander().Land(context.Background(), control.work)
	if err != nil {
		t.Fatalf("the control landing: %v", err)
	}
	for _, tc := range []struct {
		name  string
		beads func(*beadsfake.Fake) Beads
		want  string
	}{
		{
			name:  "the write fails",
			beads: func(f *beadsfake.Fake) Beads { return &commentBeads{Fake: f, failComment: true} },
			want:  "could not comment the om verdict",
		},
		{
			name:  "the read fails",
			beads: func(f *beadsfake.Fake) Beads { return &commentBeads{Fake: f, failComments: true} },
			want:  "could not read the bead's comments",
		},
		{
			name:  "a bead write that works",
			beads: func(f *beadsfake.Fake) Beads { return &commentBeads{Fake: f} },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			l := f.lander()
			l.Beads = tc.beads(f.bd)
			var log strings.Builder
			l.Out = &log
			res, err := l.Land(context.Background(), f.work)
			if err != nil {
				t.Fatalf("Land: %v, want the landing to be unaffected", err)
			}
			if res.LandedCommit != controlRes.LandedCommit {
				t.Errorf("landed %s, want %s: a bead comment must not change the result",
					res.LandedCommit, controlRes.LandedCommit)
			}
			if res.Verdict.Verdict != VerdictApprove {
				t.Errorf("verdict = %+v, want the reviewer's approve", res.Verdict)
			}
			if tc.want != "" && !strings.Contains(log.String(), tc.want) {
				t.Errorf("log = %q, want the failure named (%q)", log.String(), tc.want)
			}
		})
	}
}

// commentBeads is a beads fake whose comments can fail: the reads a duplicate
// check makes, or the write itself.
type commentBeads struct {
	*beadsfake.Fake
	failComments bool
	failComment  bool
	failShow     bool
}

func (b *commentBeads) Show(id string) (*beads.Issue, error) {
	if b.failShow {
		return nil, errors.New("dolt: connection refused")
	}
	return b.Fake.Show(id)
}

func (b *commentBeads) Comments(id string) ([]beads.Comment, error) {
	if b.failComments {
		return nil, errors.New("dolt: connection refused")
	}
	return b.Fake.Comments(id)
}

func (b *commentBeads) AddComment(id, text string) error {
	if b.failComment {
		return errors.New("database is locked")
	}
	return b.Fake.AddComment(id, text)
}
