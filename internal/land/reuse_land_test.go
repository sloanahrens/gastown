package land

import (
	"bytes"
	"context"
	"errors"
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
