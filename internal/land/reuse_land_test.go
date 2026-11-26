package land

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

// reuseAskRecord is the review-ahead record a stored review of key leaves, made
// at reviewedAt on base.
func reuseAskRecord(key ReviewKey, base string, reviewedAt time.Time) AheadRecord {
	return AheadRecord{
		Bead: key.Bead, Head: key.Head, PatchID: key.PatchID,
		OMVersion: key.OMVersion, OMConfigHash: key.OMConfigHash, InputDigest: key.InputDigest,
		Base: base, ReviewedAt: reviewedAt, Outcome: aheadOutcomeVerdict,
		Verdict: Verdict{Verdict: VerdictApprove, Score: 0.9},
	}
}

// reuseAskSetup gives the fixture's lander the review-ahead store its landings
// read, dated by the lander's clock, and returns the store and the key the
// fixture's work has now.
func reuseAskSetup(t *testing.T, f *landFixture, l *Lander, wt Repo) (*AheadStore, ReviewKey) {
	t.Helper()
	store := TownAheadStore(f.town, "gastown")
	store.Now = l.Now
	l.Ahead = store
	l.ReuseExpiry = time.Hour
	key, _, err := l.reviewKey(context.Background(), wt, wt, f.work, ReviewInput{}, f.base, f.work.Head)
	if err != nil {
		t.Fatalf("reviewKey error = %v; want nil", err)
	}
	return store, key
}

// TestVerdictReuseAskHit: a stored approve whose record matches the work's key
// is reused, and the verdict comes back as it was stored (gt-5cfuf2).
func TestVerdictReuseAskHit(t *testing.T) {
	t.Parallel()
	f, l, wt := keyFixture(t)
	store, key := reuseAskSetup(t, f, l, wt)
	stored := reuseAskRecord(key, f.base, l.now().Add(-time.Minute))
	if err := store.Put(stored); err != nil {
		t.Fatalf("Put error = %v; want nil", err)
	}
	rec, got, hit, err := l.reuseAhead(context.Background(), wt, wt, f.work, ReviewInput{}, f.base, f.work.Head)
	if err != nil {
		t.Fatalf("reuseAhead error = %v; want nil", err)
	}
	if !hit {
		t.Fatal("reuseAhead hit = false; want true for a stored review matching the work's key")
	}
	if got != key {
		t.Errorf("reuseAhead key = %+v; want the work's key %+v", got, key)
	}
	if rec.Verdict.Verdict != VerdictApprove || rec.Verdict.Score != stored.Verdict.Score {
		t.Errorf("reuseAhead verdict = %+v; want the stored approve at score %.2f", rec.Verdict, stored.Verdict.Score)
	}
}

// reuseTouchedRepo is the repository a miss is read through when main moved:
// every recorded base is an ancestor of the target, and the diff since it names
// the path the fixture's work touches.
type reuseTouchedRepo struct{ Repo }

func (reuseTouchedRepo) IsAncestor(_, _ string) (bool, error) { return true, nil }

func (reuseTouchedRepo) DiffNameOnly(_, _ string) ([]string, error) { return []string{"b.txt"}, nil }

// TestVerdictReuseAskMisses: every answer other than a matching stored verdict
// is a miss. A corrupt record and a key that cannot be computed are errors, so
// the caller knows neither to reuse nor to delete, and each other row is hit
// false with a nil error (gt-5cfuf2). Ignoring ReuseMiss makes the expired row
// a hit.
func TestVerdictReuseAskMisses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// edit changes the lander, the record to store when seed is set, and
		// the repository reuseAhead reads through.
		edit func(l *Lander, rec *AheadRecord, g *Repo)
		seed bool
		// wantErr: the answer is unknown; keyZero: no key could be computed.
		wantErr bool
		keyZero bool
	}{
		{
			name:    "nil Ahead",
			edit:    func(l *Lander, _ *AheadRecord, _ *Repo) { l.Ahead = nil },
			keyZero: true,
		},
		{
			name: "no record",
			edit: func(*Lander, *AheadRecord, *Repo) {},
		},
		{
			name: "another head",
			edit: func(_ *Lander, rec *AheadRecord, _ *Repo) { rec.Head = strings.Repeat("cd", 20) },
			seed: true,
		},
		{
			name: "expired",
			edit: func(_ *Lander, rec *AheadRecord, _ *Repo) {
				rec.ReviewedAt = rec.ReviewedAt.Add(-2 * time.Hour)
			},
			seed: true,
		},
		{
			name: "main changed a touched file",
			edit: func(_ *Lander, rec *AheadRecord, g *Repo) {
				rec.Base = "older-base"
				*g = reuseTouchedRepo{Repo: *g}
			},
			seed: true,
		},
		{
			name: "corrupt record",
			edit: func(l *Lander, _ *AheadRecord, _ *Repo) {
				l.Ahead.readFile = func(string) ([]byte, error) { return []byte("{not a record"), nil }
			},
			wantErr: true,
		},
		{
			name: "key error",
			edit: func(l *Lander, _ *AheadRecord, _ *Repo) {
				l.Reviewer = &fakeReviewer{fn: func(string) (Verdict, error) { return Verdict{}, nil }}
			},
			wantErr: true,
			keyZero: true,
		},
		{
			name: "conflict record",
			edit: func(_ *Lander, rec *AheadRecord, _ *Repo) {
				rec.Outcome, rec.Verdict = aheadOutcomeConflict, Verdict{}
			},
			seed: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, l, wt := keyFixture(t)
			store, key := reuseAskSetup(t, f, l, wt)
			rec := reuseAskRecord(key, f.base, l.now().Add(-time.Minute))
			g := Repo(wt)
			tc.edit(l, &rec, &g)
			if tc.seed {
				if err := store.Put(rec); err != nil {
					t.Fatalf("Put error = %v; want nil", err)
				}
			}
			_, gotKey, hit, err := l.reuseAhead(context.Background(), g, wt, f.work, ReviewInput{}, f.base, f.work.Head)
			if hit {
				t.Fatalf("reuseAhead hit = true; want false for %s", tc.name)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("reuseAhead error = %v; wantErr %v for %s", err, tc.wantErr, tc.name)
			}
			if tc.keyZero {
				if gotKey != (ReviewKey{}) {
					t.Errorf("reuseAhead key = %+v; want the zero key when none could be computed", gotKey)
				}
			} else if gotKey != key {
				t.Errorf("reuseAhead key = %+v; want the work's key %+v", gotKey, key)
			}
		})
	}
}

// barRepo is the repository the rig's .om.json is read through: every read of
// it returns text, and the read numbered failFrom (1-based; 0 for none) and
// every read after it fail. reviewKey reads the file twice, once for the bar and
// once for the config om is given, so failFrom 3 lets the key compute and fails
// the bar reuseAhead re-reads on a hit (gt-dz7a2d).
type barRepo struct {
	Repo
	text     string
	failFrom int
	reads    int
}

func (r *barRepo) ShowFileAtRev(ref, path string) (string, error) {
	if path != omConfigFile {
		return r.Repo.ShowFileAtRev(ref, path)
	}
	r.reads++
	if r.failFrom > 0 && r.reads >= r.failFrom {
		return "", errors.New("the bar is unreadable")
	}
	return r.text, nil
}

// TestVerdictReuseAskAppliesBar: a hit applies today's bar at base to the stored
// verdict, so an approve below a raised bar comes back as request_changes, an
// approve at the bar stays approve, and a bar that cannot be read makes the
// answer unknown: hit false with an error (gt-dz7a2d).
func TestVerdictReuseAskAppliesBar(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// text is the .om.json every read returns; failFrom is the read that fails.
		text        string
		failFrom    int
		wantHit     bool
		wantErr     bool
		wantVerdict string
	}{
		{name: "approve below a raised bar", text: `{"threshold":0.95}`, wantHit: true, wantVerdict: VerdictRequestChanges},
		{name: "approve at the bar stays approve", text: `{"threshold":0.9}`, wantHit: true, wantVerdict: VerdictApprove},
		{name: "unreadable bar", text: keyOMConfig, failFrom: 3, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, l, wt := keyFixture(t)
			store, _ := reuseAskSetup(t, f, l, wt)
			// The record is keyed through the same text the lookup reads, so the
			// key matches and the only thing the row varies is the bar.
			key, _, err := l.reviewKey(context.Background(), &barRepo{Repo: wt, text: tc.text}, wt, f.work, ReviewInput{}, f.base, f.work.Head)
			if err != nil {
				t.Fatalf("reviewKey error = %v; want nil", err)
			}
			if err := store.Put(reuseAskRecord(key, f.base, l.now().Add(-time.Minute))); err != nil {
				t.Fatalf("Put error = %v; want nil", err)
			}
			g := &barRepo{Repo: wt, text: tc.text, failFrom: tc.failFrom}
			rec, _, hit, err := l.reuseAhead(context.Background(), g, wt, f.work, ReviewInput{}, f.base, f.work.Head)
			if (err != nil) != tc.wantErr {
				t.Fatalf("reuseAhead error = %v; wantErr %v", err, tc.wantErr)
			}
			if hit != tc.wantHit {
				t.Fatalf("reuseAhead hit = %v; want %v", hit, tc.wantHit)
			}
			if tc.wantHit && rec.Verdict.Verdict != tc.wantVerdict {
				t.Errorf("reuseAhead verdict = %+v; want %s", rec.Verdict, tc.wantVerdict)
			}
		})
	}
}

// TestVerdictReuseAskLogsTheReason: every miss that had a record and every error
// logs exactly one line through l.Out naming the bead and the reason. A missing
// record and a nil Ahead log nothing (gt-dz7a2d).
func TestVerdictReuseAskLogsTheReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		edit func(l *Lander, rec *AheadRecord, g *Repo)
		seed bool
		// want is the line's text after the bead id; "" means no line at all.
		want string
	}{
		{
			name: "nil Ahead",
			edit: func(l *Lander, _ *AheadRecord, _ *Repo) { l.Ahead = nil },
		},
		{
			name: "no record",
			edit: func(*Lander, *AheadRecord, *Repo) {},
		},
		{
			name: "another head",
			edit: func(_ *Lander, rec *AheadRecord, _ *Repo) { rec.Head = strings.Repeat("cd", 20) },
			seed: true,
			want: ": the stored review is not reused: key:Head",
		},
		{
			name: "expired",
			edit: func(_ *Lander, rec *AheadRecord, _ *Repo) {
				rec.ReviewedAt = rec.ReviewedAt.Add(-2 * time.Hour)
			},
			seed: true,
			want: ": the stored review is not reused: age",
		},
		{
			name: "main changed a touched file",
			edit: func(_ *Lander, rec *AheadRecord, g *Repo) {
				rec.Base = "older-base"
				*g = reuseTouchedRepo{Repo: *g}
			},
			seed: true,
			want: ": the stored review is not reused: overlap:b.txt",
		},
		{
			name: "conflict record",
			edit: func(_ *Lander, rec *AheadRecord, _ *Repo) {
				rec.Outcome, rec.Verdict = aheadOutcomeConflict, Verdict{}
			},
			seed: true,
			want: ": the stored review is not reused: the record is a merge conflict, not a verdict",
		},
		{
			name: "corrupt record",
			edit: func(l *Lander, _ *AheadRecord, _ *Repo) {
				l.Ahead.readFile = func(string) ([]byte, error) { return []byte("{not a record"), nil }
			},
			want: ": the stored review is not reused: the record could not be read (",
		},
		{
			name: "key error",
			edit: func(l *Lander, _ *AheadRecord, _ *Repo) {
				l.Reviewer = &fakeReviewer{fn: func(string) (Verdict, error) { return Verdict{}, nil }}
			},
			want: ": the stored review is not reused: the review key could not be computed (",
		},
		{
			name: "unreadable bar",
			edit: func(_ *Lander, _ *AheadRecord, g *Repo) {
				*g = &barRepo{Repo: *g, text: keyOMConfig, failFrom: 3}
			},
			seed: true,
			want: ": the stored review is not reused: the rig's bar at ",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, l, wt := keyFixture(t)
			store, key := reuseAskSetup(t, f, l, wt)
			var out bytes.Buffer
			l.Out = &out
			rec := reuseAskRecord(key, f.base, l.now().Add(-time.Minute))
			g := Repo(wt)
			tc.edit(l, &rec, &g)
			if tc.seed {
				if err := store.Put(rec); err != nil {
					t.Fatalf("Put error = %v; want nil", err)
				}
			}
			_, _, hit, _ := l.reuseAhead(context.Background(), g, wt, f.work, ReviewInput{}, f.base, f.work.Head)
			if hit {
				t.Fatalf("reuseAhead hit = true; want a miss for %s", tc.name)
			}
			got := out.String()
			if tc.want == "" {
				if got != "" {
					t.Errorf("log = %q; want no line for %s", got, tc.name)
				}
				return
			}
			prefix := "[land] " + f.work.BeadID + tc.want
			if strings.Count(got, "\n") != 1 || !strings.HasPrefix(got, prefix) {
				t.Errorf("log = %q; want one line starting %q", got, prefix)
			}
		})
	}
}

// reuseLandReviewer answers om's version probe and runs the fixture's fake
// review, so a test counts the reviews a landing runs (gt-er929x).
type reuseLandReviewer struct {
	*fakeReviewer
	version string
}

func (r reuseLandReviewer) OMVersion(context.Context, ReviewInput) string { return r.version }

// reuseLandSetup is a landing whose om stage can reuse a stored review: the
// keyFixture's base and work, a review-ahead store the landing reads, and a
// reviewer that counts the reviews it runs. It also returns the review key the
// landing computes for its work.
func reuseLandSetup(t *testing.T) (*landFixture, *Lander, *AheadStore, ReviewKey) {
	t.Helper()
	f, l, wt := keyFixture(t)
	l.Reviewer = reuseLandReviewer{fakeReviewer: f.review, version: stubOMVersion}
	store := TownAheadStore(f.town, "gastown")
	store.Now = l.Now
	l.Ahead = store
	l.ReuseExpiry = time.Hour
	return f, l, store, reuseLandKey(t, f, l, wt)
}

// reuseLandKey is the review key the landing's om stage computes for f's work.
// The input is built the way the stage builds it, from the bead as it stands
// before the landing writes anything. The digest covers neither the merge
// commit nor the landing's directory. The fixture's work already sits on base,
// so the head's patch-id and changed paths are the merge's, and the head
// stands in for the merge.
func reuseLandKey(t *testing.T, f *landFixture, l *Lander, wt Repo) ReviewKey {
	t.Helper()
	issue := f.bead()
	op := l.operatorConfig(f.work.BeadID)
	accepted, _ := l.acceptedRisks(f.work.BeadID, op)
	in := BuildReviewInput(issue, l.beadComments(issue), op, accepted, l.roundsText(f.work.BeadID), f.base, f.work.Head, f.work.Branch, "", wt)
	key, _, err := l.reviewKey(context.Background(), wt, wt, f.work, in, f.base, f.work.Head)
	if err != nil {
		t.Fatalf("reviewKey error = %v; want nil", err)
	}
	return key
}

// TestVerdictReuseLandApproveSkipsOM: a landing whose work has a matching stored
// approve runs no om review, does not call UsageSink, and lands on the stored
// verdict stamped with the om its key names (gt-er929x). Ignoring the hit runs
// one review, which turns the review count red.
func TestVerdictReuseLandApproveSkipsOM(t *testing.T) {
	t.Parallel()
	f, l, store, key := reuseLandSetup(t)
	if err := store.Put(reuseAskRecord(key, f.base, l.now().Add(-time.Minute))); err != nil {
		t.Fatalf("Put error = %v; want nil", err)
	}
	usages := 0
	l.UsageSink = func(context.Context, ReviewUsage) { usages++ }
	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land error = %v; want nil", err)
	}
	if n := len(f.review.calls); n != 0 {
		t.Errorf("om ran %d time(s) with a matching stored approve; want 0", n)
	}
	if usages != 0 {
		t.Errorf("UsageSink was called %d time(s) on a reused verdict; want 0", usages)
	}
	if res.Verdict.Verdict != VerdictApprove || res.Verdict.Version != stubOMVersion {
		t.Errorf("Land verdict = %+v; want the stored approve stamped with %s", res.Verdict, stubOMVersion)
	}
	if got := f.originMain(); got != res.LandedCommit {
		t.Errorf("origin/main = %s; want the landed commit %s", got, res.LandedCommit)
	}
}

// TestVerdictReuseLandMissRunsInline: a stored review that does not answer for
// this landing (another head, an expired record) and an answer reuseAhead could
// not give (an unreadable record) each leave the om stage to run om once, and
// the landing still lands (gt-er929x).
func TestVerdictReuseLandMissRunsInline(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// seed stores a record for the work, edited by edit; without it the
		// store holds nothing and edit changes the landing's own store.
		seed bool
		edit func(l *Lander, rec *AheadRecord)
	}{
		{
			name: "a different head",
			seed: true,
			edit: func(_ *Lander, rec *AheadRecord) { rec.Head = strings.Repeat("cd", 20) },
		},
		{
			name: "an expired record",
			seed: true,
			edit: func(_ *Lander, rec *AheadRecord) { rec.ReviewedAt = rec.ReviewedAt.Add(-2 * time.Hour) },
		},
		{
			name: "a reuseAhead error",
			edit: func(l *Lander, _ *AheadRecord) {
				l.Ahead.readFile = func(string) ([]byte, error) { return []byte("{not a record"), nil }
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, l, store, key := reuseLandSetup(t)
			rec := reuseAskRecord(key, f.base, l.now().Add(-time.Minute))
			tc.edit(l, &rec)
			if tc.seed {
				if err := store.Put(rec); err != nil {
					t.Fatalf("Put error = %v; want nil", err)
				}
			}
			res, err := l.Land(context.Background(), f.work)
			if err != nil {
				t.Fatalf("Land error = %v; want nil", err)
			}
			if n := len(f.review.calls); n != 1 {
				t.Errorf("om ran %d time(s) for %s; want 1, run inline", n, tc.name)
			}
			if got := f.originMain(); got != res.LandedCommit {
				t.Errorf("origin/main = %s; want the landed commit %s", got, res.LandedCommit)
			}
		})
	}
}

// TestVerdictReuseLandFindingsStillReject: a stored approve whose findings
// refuse the landing is refused as a review rejection with no gate run, exactly
// as an inline approve is (gt-vu18r, gt-oi4je). Ignoring findingsRejection on
// the reuse path lands the approve and turns the rejection red.
func TestVerdictReuseLandFindingsStillReject(t *testing.T) {
	t.Parallel()
	minors := make([]Finding, maxMinorFindings+1)
	for i := range minors {
		minors[i] = Finding{Severity: "minor", Path: "b.txt", Line: 1, Title: "a minor"}
	}
	tests := []struct {
		name     string
		findings []Finding
	}{
		{name: "a major finding", findings: []Finding{{ID: "f-1", Severity: "major", Path: "b.txt", Line: 1, Title: "wrong"}}},
		{name: "more than five minors", findings: minors},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, l, store, key := reuseLandSetup(t)
			rec := reuseAskRecord(key, f.base, l.now().Add(-time.Minute))
			rec.Verdict.Findings = tc.findings
			if err := store.Put(rec); err != nil {
				t.Fatalf("Put error = %v; want nil", err)
			}
			_, err := l.Land(context.Background(), f.work)
			f.assertRejected(t, err, RejectReview, LabelRework)
			if n := len(f.review.calls); n != 0 {
				t.Errorf("om ran %d time(s) on a reused approve; want 0", n)
			}
			if n := len(f.gate.dirs); n != 0 {
				t.Errorf("gate ran %d time(s) on a refused reused approve; want 0", n)
			}
		})
	}
}

// TestVerdictReuseLandRejectingRecordRejects: a stored request_changes is
// refused as a review rejection carrying its score and findings, and its
// findings are written to the MERGE REJECTION note, with no om run and no gate
// run (gt-er929x).
func TestVerdictReuseLandRejectingRecordRejects(t *testing.T) {
	t.Parallel()
	f, l, store, key := reuseLandSetup(t)
	rec := reuseAskRecord(key, f.base, l.now().Add(-time.Minute))
	rec.Verdict = Verdict{Verdict: VerdictRequestChanges, Score: 0.3, Summary: "b.txt is wrong.",
		Findings: []Finding{{ID: "abc123", Severity: "major", Path: "b.txt", Line: 1, Title: "wrong"}}}
	if err := store.Put(rec); err != nil {
		t.Fatalf("Put error = %v; want nil", err)
	}
	_, err := l.Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectReview, LabelRework)
	if !rej.Rework || rej.ReviewScore != 0.3 || len(rej.Findings) != 1 {
		t.Errorf("rejection = %+v; want the stored score 0.3 and its one finding", rej)
	}
	if !strings.Contains(f.bead().Notes, "abc123") {
		t.Errorf("MERGE REJECTION note does not carry the stored finding:\n%s", f.bead().Notes)
	}
	if n := len(f.review.calls); n != 0 {
		t.Errorf("om ran %d time(s) on a reused request_changes; want 0", n)
	}
	if n := len(f.gate.dirs); n != 0 {
		t.Errorf("gate ran %d time(s) on a refused reused request_changes; want 0", n)
	}
}

// TestVerdictReuseLandLogsTheReviewOnce: a landing that reuses a stored approve
// or a stored request_changes writes one reviews-log line and one rounds entry
// for it, runs no om review, and numbers the entry as the line's attempt. The
// line names the landing's merged commit as the judged head; the rounds entry
// keeps the submitted head an inline landing writes (gt-jdkzo2). A hit that
// skips the log leaves both empty and turns this red.
func TestVerdictReuseLandLogsTheReviewOnce(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		verdict  string
		score    float64
		findings []Finding
	}{
		{name: "a reused approve", verdict: VerdictApprove, score: 0.9},
		{name: "a reused rejection", verdict: VerdictRequestChanges, score: 0.3,
			findings: []Finding{{ID: "abc123", Severity: "major", Path: "b.txt", Line: 1, Title: "wrong"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, l, store, _ := reuseLandSetup(t)
			reviews := reviewsFileIn(t)
			l.Reviews = reviews
			l.RoundsDir = t.TempDir()
			// The key digests the rounds ledger text, so it is computed once the
			// rounds dir is set, as the landing will compute it.
			key := reuseLandKey(t, f, l, f.git.Open(f.origin))
			rec := reuseAskRecord(key, f.base, l.now().Add(-time.Minute))
			rec.Verdict = Verdict{Verdict: tc.verdict, Score: tc.score, Findings: tc.findings}
			if err := store.Put(rec); err != nil {
				t.Fatalf("Put error = %v; want nil", err)
			}
			res, err := l.Land(context.Background(), f.work)
			if tc.verdict == VerdictRequestChanges {
				f.assertRejected(t, err, RejectReview, LabelRework)
			} else if err != nil {
				t.Fatalf("Land error = %v; want nil", err)
			}
			if n := len(f.review.calls); n != 0 {
				t.Errorf("om ran %d time(s) on a reused verdict; want 0", n)
			}
			recs, err := reviews.Since(time.Time{})
			if err != nil || len(recs) != 1 {
				t.Fatalf("reviews log = %+v, %v; want exactly one line for the reused review", recs, err)
			}
			if recs[0].Verdict != tc.verdict || recs[0].Score != tc.score {
				t.Errorf("reviews line verdict = %s %.2f; want the stored %s %.2f", recs[0].Verdict, recs[0].Score, tc.verdict, tc.score)
			}
			if tc.verdict == VerdictApprove && recs[0].Head != res.LandedCommit {
				t.Errorf("reviews line head = %s; want the landing's merged commit %s", recs[0].Head, res.LandedCommit)
			}
			rounds := renderedOf(t, &RoundsLedger{OutDir: l.RoundsDir, Bead: f.work.BeadID}, 0)
			if len(rounds) != 1 {
				t.Fatalf("rounds ledger holds %d round(s); want exactly one for the reused review", len(rounds))
			}
			if rounds[0].Round != recs[0].Attempt {
				t.Errorf("rounds entry is round %d; want the reviews line's attempt %d", rounds[0].Round, recs[0].Attempt)
			}
			if rounds[0].Head != f.work.Head {
				t.Errorf("rounds entry head = %q; want the submitted head %q as an inline landing writes it", rounds[0].Head, f.work.Head)
			}
		})
	}
}

// TestVerdictReuseLandStageLineSaysReused: a landing that reuses a stored review
// writes its stage line with ", om reused (<cost>, <age> old)" after the om
// stage on each path that writes one: the landed path, the review-rejection path
// and the gate-rejection path. The cost is the record's ReviewSecs and the age is
// the landing's clock less the record's ReviewedAt. om did not run, so the line
// holds no om duration. A path that drops the suffix leaves its row's line
// without "om reused (" and turns that row red (gt-nf79k4).
func TestVerdictReuseLandStageLineSaysReused(t *testing.T) {
	t.Parallel()
	const want = ", om reused (2m31s, 14m0s old)"
	omDuration := regexp.MustCompile(`\bom [0-9]`)
	tests := []struct {
		name       string
		verdict    Verdict
		gateRed    bool
		wantReject RejectionKind
	}{
		{name: "landed", verdict: Verdict{Verdict: VerdictApprove, Score: 0.9}},
		{name: "review rejection", verdict: Verdict{Verdict: VerdictRequestChanges, Score: 0.3, Summary: "b.txt is wrong."}, wantReject: RejectReview},
		{name: "gate rejection", verdict: Verdict{Verdict: VerdictApprove, Score: 0.9}, gateRed: true, wantReject: RejectGate},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, l, store, key := reuseLandSetup(t)
			rec := reuseAskRecord(key, f.base, l.now().Add(-14*time.Minute))
			rec.ReviewSecs = 151
			rec.Verdict = tc.verdict
			if err := store.Put(rec); err != nil {
				t.Fatalf("Put error = %v; want nil", err)
			}
			if tc.gateRed {
				f.gate.fn = func(string) GateResult {
					return GateResult{Passed: false, Steps: []StepResult{{Name: "test", ExitCode: 1}}}
				}
			}
			var out bytes.Buffer
			l.Out = &out
			_, err := l.Land(context.Background(), f.work)
			if tc.wantReject != "" {
				f.assertRejected(t, err, tc.wantReject, LabelRework)
			} else if err != nil {
				t.Fatalf("Land error = %v; want nil", err)
			}
			var stages []string
			for _, line := range strings.Split(out.String(), "\n") {
				if strings.Contains(line, "stages: ") {
					stages = append(stages, line)
				}
			}
			if len(stages) != 1 {
				t.Fatalf("stage lines = %q; want exactly one for the landing", stages)
			}
			if !strings.Contains(stages[0], want) {
				t.Errorf("stage line = %q; want it to carry %q", stages[0], want)
			}
			if omDuration.MatchString(stages[0]) {
				t.Errorf("stage line = %q; want no om duration on a reused landing", stages[0])
			}
		})
	}
}

// noBaseRepo is the repository a stored record with no reviewed base is read
// through: it reports an empty base as an ancestor of any commit and as touching
// nothing, so such a record reaches the landing's comment routing rather than a
// base-unreadable miss. The reuse predicate's own tests own the miss (gt-bydaep).
type noBaseRepo struct{ Repo }

func (r noBaseRepo) IsAncestor(ancestor, descendant string) (bool, error) {
	if ancestor == "" {
		return true, nil
	}
	return r.Repo.IsAncestor(ancestor, descendant)
}

func (r noBaseRepo) DiffNameOnly(base, head string) ([]string, error) {
	if base == "" {
		return nil, nil
	}
	return r.Repo.DiffNameOnly(base, head)
}

// TestVerdictReuseLandCommentSaysReused: a landing that reuses a stored review
// writes its one bead comment through commentReused, which names the reused
// review and its base. A landing that runs om, whether no record matched or the
// store could not be read, writes commentVerdict's comment with no "reused
// from"; a reused record commentReused refuses gets that same comment rather
// than none. Taking the reused form out of the routing turns the hit row red,
// and letting a refused record write nothing turns its row red (gt-bydaep).
func TestVerdictReuseLandCommentSaysReused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// seed stores the record edited by edit; without it the store is empty.
		seed bool
		edit func(l *Lander, rec *AheadRecord)
		// omRuns: the stored review does not answer, so the landing runs om.
		omRuns bool
		// reusedForm: the comment is commentReused's, naming the stored review.
		reusedForm bool
	}{
		{name: "a stored approve is reused", seed: true, reusedForm: true},
		{
			name:   "a different head runs om",
			seed:   true,
			edit:   func(_ *Lander, rec *AheadRecord) { rec.Head = strings.Repeat("cd", 20) },
			omRuns: true,
		},
		{
			name: "an unreadable record runs om",
			edit: func(l *Lander, _ *AheadRecord) {
				l.Ahead.readFile = func(string) ([]byte, error) { return []byte("{not a record"), nil }
			},
			omRuns: true,
		},
		{
			name: "a record with no reviewed base is commented as this landing's own",
			seed: true,
			edit: func(l *Lander, rec *AheadRecord) {
				rec.Base = ""
				open := l.openRepo
				l.openRepo = func(dir string) Repo { return noBaseRepo{Repo: open(dir)} }
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, l, store, key := reuseLandSetup(t)
			rec := reuseAskRecord(key, f.base, l.now().Add(-time.Minute))
			if tc.edit != nil {
				tc.edit(l, &rec)
			}
			if tc.seed {
				if err := store.Put(rec); err != nil {
					t.Fatalf("Put error = %v; want nil", err)
				}
			}
			res, err := l.Land(context.Background(), f.work)
			if err != nil {
				t.Fatalf("Land error = %v; want nil", err)
			}
			comments := f.beadComments()
			if len(comments) != 1 {
				t.Fatalf("%d comment(s) on the bead; want exactly one verdict comment", len(comments))
			}
			text := comments[0].Text
			head := verdictCommentHead(res.LandedCommit)
			if !strings.Contains(text, head) {
				t.Fatalf("comment = %q; want it to name the merged head %s", text, head)
			}
			wantOM := 0
			if tc.omRuns {
				wantOM = 1
			}
			if n := len(f.review.calls); n != wantOM {
				t.Errorf("om ran %d time(s) for %s; want %d", n, tc.name, wantOM)
			}
			if tc.reusedForm {
				want := "om approve 0.90 " + head + " (reused from " + shortSHA(f.base) + ", 1m0s old)"
				if text != want {
					t.Errorf("comment = %q; want the reused comment %q", text, want)
				}
				return
			}
			if strings.Contains(text, "reused from") {
				t.Errorf("comment = %q; want commentVerdict's comment, not the reused one", text)
			}
			parseVerdictComment(t, text)
		})
	}
}

// reuseOffReadyNote is the work bead's note before a landing touches it, as the
// fixture seeds it, with the head reuseOffVolatile blanks.
const reuseOffReadyNote = "READY TO LAND\n" +
	"Branch: polecat/opal/gt-abc+x1\n" +
	"Head: <sha>\n" +
	"Target: main\n" +
	"Worker: opal"

// The values two runs of one landing cannot share, blanked by reuseOffVolatile
// so the rest of a landing's log, comment and notes can be pinned to a
// literal: the clock's RFC3339 stamps, the durations real time yields, the
// host's load sample, and the commit and patch ids a repository mints.
var (
	reuseOffTime     = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z`)
	reuseOffDuration = regexp.MustCompile(`\d+(?:\.\d+)?(?:h\d+m\d+s|m\d+s|ms|µs|ns|s)\b`)
	reuseOffLoad     = regexp.MustCompile(` \(load1 [0-9.]+\)`)
	reuseOffID       = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
)

func reuseOffVolatile(s string) string {
	s = reuseOffTime.ReplaceAllString(s, "<time>")
	s = reuseOffDuration.ReplaceAllString(s, "<dur>")
	s = reuseOffLoad.ReplaceAllString(s, "")
	return reuseOffID.ReplaceAllString(s, "<sha>")
}

// TestVerdictReuseLandOffIsToday: a review pool that is switched off (Ahead
// nil) and a pool that holds no record for the bead each leave a landing
// exactly as main left it before the pool. The landing's whole log output, its
// bead comment and its bead notes equal each other byte for byte once the
// clock, the durations, the host load and the ids are blanked, and equal the
// literals below, which are the strings main prints for this fixture that the
// approve-landing tests assert the shape of. A reused verdict's log line,
// comment or "om reused" suffix reaching either path turns that path's row red
// (gt-bmcjpa).
func TestVerdictReuseLandOffIsToday(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		verdict Verdict
		// wantLog is the landing's whole log, one literal line per logf call.
		wantLog string
		// wantComment is the one comment the landing leaves on the bead.
		wantComment string
		// wantNotes is the bead's whole notes: the ready note and what the
		// landing appends to it.
		wantNotes string
	}{
		{
			name:    "an approve",
			verdict: Verdict{Verdict: VerdictApprove, Score: 0.9},
			wantLog: "[land] gt-abc: merged <sha> onto origin/main (<sha>) as <sha>; reviewing the merge candidate, then pushing it for its CI verdict\n" +
				"[land] gt-abc: no town root, so no operator.names is read; no operator decision or accepted risk is entitled\n" +
				"[land] gt-abc: stages: ci <dur>, om <dur>\n" +
				"[land] gt-abc: landed <sha> on origin/main (patch-id <sha>)\n",
			wantComment: "om approve 0.90 (om unknown, <time>–<time>) [head <sha>]",
			wantNotes: reuseOffReadyNote + "\nLANDING RECORD\n" +
				"landed_commit: <sha>\n" +
				"patch_id: <sha>\n" +
				"target: main\n" +
				"base: <sha>\n" +
				"branch: polecat/opal/gt-abc+x1\n" +
				"head: <sha>\n" +
				"gate_result: pass (ci exit 0 <dur>)\n" +
				"om_verdict: approve\n" +
				"om_score: 0.9000\n" +
				"route: daemon\n" +
				"landed_at: <time>",
		},
		{
			name: "a request_changes",
			verdict: Verdict{Verdict: VerdictRequestChanges, Score: 0.3, Summary: "b.txt is wrong.",
				Findings: []Finding{{ID: "abc123", Severity: "major", Path: "b.txt", Line: 1, Title: "wrong"}}},
			wantLog: "[land] gt-abc: merged <sha> onto origin/main (<sha>) as <sha>; reviewing the merge candidate, then pushing it for its CI verdict\n" +
				"[land] gt-abc: no town root, so no operator.names is read; no operator decision or accepted risk is entitled\n" +
				"[land] gt-abc: stages: ci skipped (om rejected), om <dur>\n" +
				"[land] gt-abc: rejected (review): om requested changes (score 0.30, 1 finding(s))\n",
			wantComment: "om request_changes 0.30 (om unknown, <time>–<time>) [head <sha>]",
			wantNotes: reuseOffReadyNote + "\nMERGE REJECTION (attempt 1): review - om requested changes (score 0.30, 1 finding(s))\n" +
				"Branch: polecat/opal/gt-abc+x1\n" +
				"Target: main\n" +
				"MR: gt-abc\n" +
				"- id:abc123 sev:major b.txt:1 — wrong\n" +
				"Score: 0.3000\n" +
				"Head: <sha>",
		},
	}
	// offRun lands the row's verdict on its own fixture and returns the
	// landing's log, the bead's one comment and the bead's notes, blanked.
	// pool false leaves Ahead nil, as a lander built before the pool has it;
	// pool true gives the landing a store that holds no record for the bead.
	offRun := func(t *testing.T, pool bool, verdict Verdict) (log, comment, notes string) {
		t.Helper()
		f, l, _, _ := reuseLandSetup(t)
		f.review.fn = func(string) (Verdict, error) { return verdict, nil }
		if !pool {
			l.Ahead = nil
		}
		var out bytes.Buffer
		l.Out = &out
		_, err := l.Land(context.Background(), f.work)
		if verdict.Verdict == VerdictRequestChanges {
			if err == nil {
				t.Fatalf("Land error = nil; want %s refused", verdict.Verdict)
			}
		} else if err != nil {
			t.Fatalf("Land error = %v; want nil", err)
		}
		comments := f.beadComments()
		if len(comments) != 1 {
			t.Fatalf("%d comment(s) on the bead; want the one verdict comment", len(comments))
		}
		return reuseOffVolatile(out.String()), reuseOffVolatile(comments[0].Text), reuseOffVolatile(f.bead().Notes)
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			offLog, offComment, offNotes := offRun(t, false, tc.verdict)
			poolLog, poolComment, poolNotes := offRun(t, true, tc.verdict)
			if offLog != poolLog {
				t.Errorf("an empty pool changed the landing log:\noff:  %q\npool: %q", offLog, poolLog)
			}
			if offComment != poolComment {
				t.Errorf("an empty pool changed the bead comment:\noff:  %q\npool: %q", offComment, poolComment)
			}
			if offNotes != poolNotes {
				t.Errorf("an empty pool changed the bead notes:\noff:  %q\npool: %q", offNotes, poolNotes)
			}
			if offLog != tc.wantLog {
				t.Errorf("landing log = %q; want main's %q", offLog, tc.wantLog)
			}
			if offComment != tc.wantComment {
				t.Errorf("bead comment = %q; want main's %q", offComment, tc.wantComment)
			}
			if offNotes != tc.wantNotes {
				t.Errorf("bead notes = %q; want main's %q", offNotes, tc.wantNotes)
			}
		})
	}
}
