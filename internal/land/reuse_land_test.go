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
