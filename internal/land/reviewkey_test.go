package land

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// helloSHA is the hex sha256 of the bytes "hello".
const helloSHA = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"

// TestVerdictReuseFileSHA: a file's digest is its sha256, a missing file is
// absentSHA, and any other read failure is an error.
func TestVerdictReuseFileSHA(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "content")
	if err := os.WriteFile(file, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	readRefused := errors.New("read refused")
	tests := []struct {
		name    string
		path    string
		read    func(string) ([]byte, error)
		want    string
		wantErr bool
	}{
		{name: "a file is its sha256", path: file, want: helloSHA},
		{name: "no file is absent", path: filepath.Join(dir, "missing"), want: absentSHA},
		{name: "a directory at the path is an error", path: dir, wantErr: true},
		{
			name:    "a failing read is an error",
			path:    file,
			read:    func(string) ([]byte, error) { return nil, readRefused },
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got string
			var err error
			if tc.read == nil {
				got, err = fileSHA(tc.path)
			} else {
				got, err = fileSHAFrom(tc.read, tc.path)
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("fileSHA(%s) = %q, nil; want an error", tc.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("fileSHA(%s) error = %v; want nil", tc.path, err)
			}
			if got != tc.want {
				t.Errorf("fileSHA(%s) = %q; want %q", tc.path, got, tc.want)
			}
		})
	}
}

// omConfigRepo is the landing's repository for an om config read: files holds
// each path's text at base, and a path it lacks answers git.ErrNotAtRef. The
// path failPath answers failErr instead.
type omConfigRepo struct {
	Repo
	files    map[string]string
	failPath string
	failErr  error
}

func (r omConfigRepo) ShowFileAtRev(_, path string) (string, error) {
	if r.failErr != nil && path == r.failPath {
		return "", r.failErr
	}
	text, ok := r.files[path]
	if !ok {
		return "", git.ErrNotAtRef
	}
	return text, nil
}

// TestVerdictReuseOMRepoConfigAt: both texts come back when the key names a
// file; a missing key names none; a missing or unreadable .om.json, invalid
// JSON, an empty or non-string context_file, a named file that is missing or
// unreadable, and a nil repository are errors with no text.
func TestVerdictReuseOMRepoConfigAt(t *testing.T) {
	t.Parallel()
	const (
		omText   = `{"threshold":0.8,"context_file":"docs/rubric.md"}`
		rubric   = "the rubric"
		namedDoc = "docs/rubric.md"
	)
	readRefused := errors.New("read refused")
	tests := []struct {
		name        string
		repo        Repo
		wantJSON    string
		wantContext string
		wantErr     bool
		// wantIs is the cause the error must wrap, when the row names one.
		wantIs error
	}{
		{
			name:        "both texts are returned",
			repo:        omConfigRepo{files: map[string]string{omConfigFile: omText, namedDoc: rubric}},
			wantJSON:    omText,
			wantContext: rubric,
		},
		{
			name:     "no context_file key gives an empty context text",
			repo:     omConfigRepo{files: map[string]string{omConfigFile: `{"threshold":0.8}`}},
			wantJSON: `{"threshold":0.8}`,
		},
		{
			name:    "no .om.json is an error",
			repo:    omConfigRepo{files: map[string]string{namedDoc: rubric}},
			wantErr: true,
			wantIs:  git.ErrNotAtRef,
		},
		{
			name:    "invalid JSON is an error",
			repo:    omConfigRepo{files: map[string]string{omConfigFile: `{"threshold":`}},
			wantErr: true,
		},
		{
			name:    "a named file that is missing is an error",
			repo:    omConfigRepo{files: map[string]string{omConfigFile: omText}},
			wantErr: true,
			wantIs:  git.ErrNotAtRef,
		},
		{
			// The empty path is in the fake: git show base: lists the tree and
			// succeeds, so only the guard refuses it.
			name: "an empty context_file value is an error",
			repo: omConfigRepo{files: map[string]string{
				omConfigFile: `{"context_file":""}`,
				"":           "tree listing",
			}},
			wantErr: true,
		},
		{
			name:    "a context_file that is not a string is an error",
			repo:    omConfigRepo{files: map[string]string{omConfigFile: `{"context_file":3}`}},
			wantErr: true,
		},
		{
			name:    "an unreadable .om.json is an error",
			repo:    omConfigRepo{failPath: omConfigFile, failErr: readRefused},
			wantErr: true,
			wantIs:  readRefused,
		},
		{
			name: "an unreadable named file is an error",
			repo: omConfigRepo{
				files:    map[string]string{omConfigFile: omText},
				failPath: namedDoc,
				failErr:  readRefused,
			},
			wantErr: true,
			wantIs:  readRefused,
		},
		{
			name:    "a nil repository is an error",
			repo:    nil,
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotJSON, gotContext, err := omRepoConfigAt(tc.repo, "base")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("omRepoConfigAt = %q, %q, nil; want an error", gotJSON, gotContext)
				}
				if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
					t.Errorf("omRepoConfigAt error = %v; want it to wrap %v", err, tc.wantIs)
				}
				if gotJSON != "" || gotContext != "" {
					t.Errorf("omRepoConfigAt on error = %q, %q; want both empty", gotJSON, gotContext)
				}
				return
			}
			if err != nil {
				t.Fatalf("omRepoConfigAt error = %v; want nil", err)
			}
			if gotJSON != tc.wantJSON || gotContext != tc.wantContext {
				t.Errorf("omRepoConfigAt = %q, %q; want %q, %q", gotJSON, gotContext, tc.wantJSON, tc.wantContext)
			}
		})
	}
}

// keyReviewer is a Reviewer that answers om's version probe, so a review key
// can be built from it. It fails the test if a review is run: a key is
// computed without one.
type keyReviewer struct {
	t       *testing.T
	version string
	probes  int
}

func (r *keyReviewer) Review(context.Context, string, string, string) (Verdict, error) {
	r.t.Error("reviewKey ran a review")
	return Verdict{}, errors.New("unreachable")
}

func (r *keyReviewer) OMVersion(context.Context, ReviewInput) string {
	r.probes++
	return r.version
}

// keyOMConfig is the .om.json the fixture's base carries: a bar above zero, so
// the review's flags carry --threshold and the digest covers both.
const keyOMConfig = `{"threshold":0.8}`

// keyFixture is a lander whose reviewer answers the version probe and whose om
// config is a readable file holding "hello", with the origin repository a key's
// git facts and om texts are read through. The base carries an .om.json and the
// work branch sits on it, so base..head is still b.txt only. The commits are
// made on origin, so the handle must be opened there rather than on the
// fixture's stale clone.
func keyFixture(t *testing.T) (*landFixture, *Lander, Repo) {
	t.Helper()
	f := newLandFixture(t)
	base := f.git.Commit(t, f.origin, "main", "main: add the om config", map[string]string{omConfigFile: keyOMConfig})
	f.git.SetRef(t, f.origin, "refs/heads/"+fixtureBranch, base)
	head := f.git.Commit(t, f.origin, fixtureBranch, "feat: add b on the om config", map[string]string{"b.txt": "work\n"})
	f.base = base
	f.ready(head)
	l := f.lander()
	l.Reviewer = &keyReviewer{t: t, version: stubOMVersion}
	config := filepath.Join(t.TempDir(), "om.json")
	if err := os.WriteFile(config, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	l.OMConfigPath = config
	return f, l, f.git.Open(f.origin)
}

// keyDigest is the input digest a key carries for in at the fixture's base: the
// digest of the inputs om is given, with the fixture's .om.json text, no
// context_file and w's target.
func keyDigest(t *testing.T, in ReviewInput) string {
	t.Helper()
	digest, err := ReviewInputDigest(in, ExpectedReviewFlags(in, 0.8), keyOMConfig, "", "main")
	if err != nil {
		t.Fatalf("ReviewInputDigest(%+v): %v", in, err)
	}
	return digest
}

// TestVerdictReuseKeyGitFacts: the key carries w's bead and head, the patch-id
// of the merge and the paths it touches, the reviewer's om version, the om
// config's digest and the digest of the input om is given. A base that moves
// to a later commit touching none of the diff's paths leaves the patch-id
// alone: that equality is why a stored review is matched by the patch-id rather
// than the head (gt-pdusl8).
func TestVerdictReuseKeyGitFacts(t *testing.T) {
	t.Parallel()
	f, l, wt := keyFixture(t)
	in := ReviewInput{Dir: f.workRoot}
	first, touched, err := l.reviewKey(context.Background(), wt, wt, f.work, in, f.base, f.work.Head)
	if err != nil {
		t.Fatalf("reviewKey(base, head) error = %v; want nil", err)
	}
	wantPatchID, err := wt.PatchIDZeroContext(f.base, f.work.Head)
	if err != nil {
		t.Fatalf("PatchIDZeroContext(%s, %s): %v", f.base, f.work.Head, err)
	}
	if first.PatchID != wantPatchID {
		t.Errorf("PatchID = %q; want %q, the zero-context patch-id of the merge", first.PatchID, wantPatchID)
	}
	if !slices.Equal(touched, []string{"b.txt"}) {
		t.Errorf("touched = %v; want [b.txt], the paths the diff changes", touched)
	}
	want := ReviewKey{Bead: f.work.BeadID, Head: f.work.Head, PatchID: first.PatchID,
		OMVersion: stubOMVersion, OMConfigHash: helloSHA, InputDigest: keyDigest(t, in)}
	if first != want {
		t.Errorf("reviewKey = %+v; want %+v", first, want)
	}
	// The later base changes only a.txt, which the branch does not touch, so
	// the merge's zero-context patch-id is the same change.
	later := f.git.Commit(t, f.origin, "main", "main: extend a", map[string]string{"a.txt": "one\ntwo\nthree\nfour\n"})
	f.git.SetRef(t, f.origin, "refs/heads/merge-onto-later", later)
	merged := f.git.Commit(t, f.origin, "merge-onto-later", "merge: add b", map[string]string{"b.txt": "work\n"})
	second, laterTouched, err := l.reviewKey(context.Background(), wt, wt, f.work, in, later, merged)
	if err != nil {
		t.Fatalf("reviewKey(later, merged) error = %v; want nil", err)
	}
	if second.PatchID != first.PatchID {
		t.Errorf("patch-id moved from %q to %q when base moved to a later commit that touches no path of the diff", first.PatchID, second.PatchID)
	}
	if !slices.Equal(laterTouched, []string{"b.txt"}) {
		t.Errorf("touched = %v; want [b.txt], the paths the diff changes", laterTouched)
	}
}

// patchIDFailsRepo is wt for a key whose patch-id read fails; every other call
// answers through Repo.
type patchIDFailsRepo struct{ Repo }

func (patchIDFailsRepo) PatchIDZeroContext(string, string) (string, error) {
	return "", errors.New("patch-id refused")
}

// TestVerdictReuseKeyPatchIDFails: a patch-id that cannot be read is an error,
// which the caller treats as a miss.
func TestVerdictReuseKeyPatchIDFails(t *testing.T) {
	t.Parallel()
	f, l, wt := keyFixture(t)
	failing := patchIDFailsRepo{Repo: wt}
	key, touched, err := l.reviewKey(context.Background(), wt, failing, f.work, ReviewInput{}, f.base, f.work.Head)
	if err == nil {
		t.Fatalf("reviewKey = %+v, %v, nil; want an error when the patch-id read fails", key, touched)
	}
	if key != (ReviewKey{}) || touched != nil {
		t.Errorf("reviewKey on error = %+v, %v; want the zero key and no paths", key, touched)
	}
}

// diffNamesFailsRepo is wt for a key whose changed-paths read fails.
type diffNamesFailsRepo struct{ Repo }

func (diffNamesFailsRepo) DiffNameOnly(string, string) ([]string, error) {
	return nil, errors.New("diff refused")
}

// TestVerdictReuseKeyDiffFails: a changed-paths read that fails is an error,
// which the caller treats as a miss.
func TestVerdictReuseKeyDiffFails(t *testing.T) {
	t.Parallel()
	f, l, wt := keyFixture(t)
	failing := diffNamesFailsRepo{Repo: wt}
	key, touched, err := l.reviewKey(context.Background(), wt, failing, f.work, ReviewInput{}, f.base, f.work.Head)
	if err == nil {
		t.Fatalf("reviewKey = %+v, %v, nil; want an error when the changed-paths read fails", key, touched)
	}
	if key != (ReviewKey{}) || touched != nil {
		t.Errorf("reviewKey on error = %+v, %v; want the zero key and no paths", key, touched)
	}
}

// TestVerdictReuseKeyConfigUnreadable: an om config that cannot be read is an
// error, not an absent file, which the caller treats as a miss.
func TestVerdictReuseKeyConfigUnreadable(t *testing.T) {
	t.Parallel()
	f, l, wt := keyFixture(t)
	// A directory opens but cannot be read as a file: the read fails with a
	// reason other than not-exist, which fileSHA must refuse rather than call
	// absent.
	l.OMConfigPath = t.TempDir()
	key, touched, err := l.reviewKey(context.Background(), wt, wt, f.work, ReviewInput{}, f.base, f.work.Head)
	if err == nil {
		t.Fatalf("reviewKey = %+v, %v, nil; want an error when the om config cannot be read", key, touched)
	}
	if key != (ReviewKey{}) || touched != nil {
		t.Errorf("reviewKey on error = %+v, %v; want the zero key and no paths", key, touched)
	}
}

// TestVerdictReuseKeyNoProbe: a reviewer that cannot be asked om's version
// makes the key an error, which the caller treats as a miss, rather than a key
// with an empty version that the reuse predicate would refuse anyway.
func TestVerdictReuseKeyNoProbe(t *testing.T) {
	t.Parallel()
	f, l, wt := keyFixture(t)
	l.Reviewer = &fakeReviewer{fn: func(string) (Verdict, error) {
		t.Error("reviewKey ran a review")
		return Verdict{}, errors.New("unreachable")
	}}
	key, touched, err := l.reviewKey(context.Background(), wt, wt, f.work, ReviewInput{}, f.base, f.work.Head)
	if err == nil {
		t.Fatalf("reviewKey = %+v, %v, nil; want an error for a reviewer that is not a VersionProber", key, touched)
	}
	if key != (ReviewKey{}) || touched != nil {
		t.Errorf("reviewKey on error = %+v, %v; want the zero key and no paths", key, touched)
	}
}

// TestVerdictReuseKeyStable: two keys built from the same inputs are the same
// key, with a non-empty input digest. A key that changes between calls could
// never match a stored review, and a key with no digest would match one whose
// input om was given has changed (gt-0lys4u).
func TestVerdictReuseKeyStable(t *testing.T) {
	t.Parallel()
	f, l, wt := keyFixture(t)
	in := ReviewInput{
		Dir:               f.workRoot,
		BeadID:            f.work.BeadID,
		CriteriaText:      "- [ ] b.txt is added\n",
		AcceptedRisksText: "the risk was accepted\n",
		PriorFindings:     []Finding{{Severity: omSeverityMinor, Title: "a minor"}},
		BriefText:         "the work item\n",
		PolicyText:        "the policy\n",
		Round:             2,
		RoundsText:        "[]",
	}
	first, _, err := l.reviewKey(context.Background(), wt, wt, f.work, in, f.base, f.work.Head)
	if err != nil {
		t.Fatalf("reviewKey error = %v; want nil", err)
	}
	again, _, err := l.reviewKey(context.Background(), wt, wt, f.work, in, f.base, f.work.Head)
	if err != nil {
		t.Fatalf("second reviewKey error = %v; want nil", err)
	}
	if first != again {
		t.Errorf("reviewKey = %+v on the second call; want the first key %+v", again, first)
	}
	if first.InputDigest == "" {
		t.Error("InputDigest is empty; want the digest of the input om is given")
	}
}

// TestVerdictReuseKeyDigestFollowsInputs: the key's input digest changes when
// an input om is given changes, and nothing else in the key does: the review
// round, the .om.json text with the bar it sets, and the context_file text. A
// digest that ignored one of these would reuse a verdict for an input om never
// judged (gt-0lys4u).
func TestVerdictReuseKeyDigestFollowsInputs(t *testing.T) {
	t.Parallel()
	const (
		withContext = `{"threshold":0.8,"context_file":"docs/rubric.md"}`
		contextPath = "docs/rubric.md"
	)
	tests := []struct {
		name string
		// repo is the key's g for the variant, built from the fixture's wt.
		repo   func(wt Repo) Repo
		mutate func(*ReviewInput)
	}{
		{
			name:   "another round",
			repo:   func(wt Repo) Repo { return wt },
			mutate: func(in *ReviewInput) { in.Round = 1 },
		},
		{
			name: "another .om.json text and bar",
			repo: func(wt Repo) Repo {
				return omConfigRepo{Repo: wt, files: map[string]string{omConfigFile: `{"threshold":0.9}`}}
			},
			mutate: func(*ReviewInput) {},
		},
		{
			name: "another context_file text",
			repo: func(wt Repo) Repo {
				return omConfigRepo{Repo: wt, files: map[string]string{omConfigFile: withContext, contextPath: "another rubric"}}
			},
			mutate: func(*ReviewInput) {},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, l, wt := keyFixture(t)
			in := ReviewInput{Dir: f.workRoot, BeadID: f.work.BeadID}
			base, _, err := l.reviewKey(context.Background(), wt, wt, f.work, in, f.base, f.work.Head)
			if err != nil {
				t.Fatalf("reviewKey error = %v; want nil", err)
			}
			changed := in
			tc.mutate(&changed)
			variant, _, err := l.reviewKey(context.Background(), tc.repo(wt), wt, f.work, changed, f.base, f.work.Head)
			if err != nil {
				t.Fatalf("reviewKey error = %v; want nil", err)
			}
			if variant.InputDigest == base.InputDigest {
				t.Errorf("InputDigest = %q; want it to change when %s changes", variant.InputDigest, tc.name)
			}
			// g feeds nothing but the digest, so the rest of the key must stand.
			variant.InputDigest = base.InputDigest
			if variant != base {
				t.Errorf("reviewKey = %+v; want it to differ from %+v in InputDigest alone", variant, base)
			}
		})
	}
}

// TestVerdictReuseKeyChangesWithTheRounds: appending a round to the bead's
// rounds ledger changes the key's input digest, and nothing else in the key.
// An ahead review writes nothing to the ledger, so the text it read is the one
// the landing reads back; a round added between them is a real change to what
// om is given and must force a fresh review (gt-kalg0s).
func TestVerdictReuseKeyChangesWithTheRounds(t *testing.T) {
	t.Parallel()
	f, l, wt := keyFixture(t)
	l.RoundsDir = t.TempDir()
	ledger := RoundsLedger{OutDir: l.RoundsDir, Bead: f.work.BeadID, Now: l.now}
	must(t, ledger.Append(1, "first", []Finding{{ID: "F1", Severity: omSeverityMinor, Path: "b.txt", Title: "a minor"}}))
	in := ReviewInput{Dir: f.workRoot, BeadID: f.work.BeadID, RoundsText: l.roundsText(f.work.BeadID)}
	before, _, err := l.reviewKey(context.Background(), wt, wt, f.work, in, f.base, f.work.Head)
	if err != nil {
		t.Fatalf("reviewKey error = %v; want nil", err)
	}
	firstText := in.RoundsText
	must(t, ledger.Append(2, "second", nil))
	in.RoundsText = l.roundsText(f.work.BeadID)
	if in.RoundsText == firstText {
		t.Fatalf("roundsText = %q before and after a round was appended; want the appended round in the text", firstText)
	}
	after, _, err := l.reviewKey(context.Background(), wt, wt, f.work, in, f.base, f.work.Head)
	if err != nil {
		t.Fatalf("reviewKey error = %v; want nil", err)
	}
	if after.InputDigest == before.InputDigest {
		t.Errorf("InputDigest = %q before and after a round was appended; want it to change with the rounds text", after.InputDigest)
	}
	// The rounds are the only input that moved, so the rest of the key must stand.
	after.InputDigest = before.InputDigest
	if after != before {
		t.Errorf("reviewKey = %+v; want it to differ from %+v in InputDigest alone", after, before)
	}
}

// TestVerdictReuseKeyChangesWithOMJSON: changing the threshold in the .om.json
// at base changes the key's input digest, and the change is the bar's text
// rather than the diff: the merge's patch-id is the same. The bar is part of the
// key only through the text om is given (gt-kalg0s).
func TestVerdictReuseKeyChangesWithOMJSON(t *testing.T) {
	t.Parallel()
	f, l, wt := keyFixture(t)
	in := ReviewInput{Dir: f.workRoot}
	before, _, err := l.reviewKey(context.Background(), wt, wt, f.work, in, f.base, f.work.Head)
	if err != nil {
		t.Fatalf("reviewKey error = %v; want nil", err)
	}
	// The same diff, on a main whose .om.json sets a higher bar.
	raised := f.git.Commit(t, f.origin, "main", "main: raise the bar", map[string]string{omConfigFile: `{"threshold":0.9}`})
	f.git.SetRef(t, f.origin, "refs/heads/raised-bar", raised)
	merged := f.git.Commit(t, f.origin, "raised-bar", "merge: add b", map[string]string{"b.txt": "work\n"})
	w := f.work
	w.Head = merged
	after, touched, err := l.reviewKey(context.Background(), wt, wt, w, in, raised, merged)
	if err != nil {
		t.Fatalf("reviewKey error = %v; want nil", err)
	}
	if !slices.Equal(touched, []string{"b.txt"}) {
		t.Errorf("touched = %v; want [b.txt], the paths the diff changes", touched)
	}
	if after.PatchID != before.PatchID {
		t.Fatalf("PatchID = %q at the raised bar, %q before; want the same diff", after.PatchID, before.PatchID)
	}
	if after.InputDigest == before.InputDigest {
		t.Errorf("InputDigest = %q before and after the .om.json at base changed its threshold; want it to change with the bar's text", after.InputDigest)
	}
}

// TestVerdictReuseKeyOMConfigRefused: a key that cannot read the bar or the two
// texts om is given is an error, which the caller treats as a miss, and never a
// key built from empty texts: an unreadable, absent or invalid .om.json, a
// context_file the base does not have, and no repository to read from
// (gt-0lys4u).
func TestVerdictReuseKeyOMConfigRefused(t *testing.T) {
	t.Parallel()
	readRefused := errors.New("read refused")
	tests := []struct {
		name string
		repo func(Repo) Repo
	}{
		{
			name: "an unreadable .om.json",
			repo: func(wt Repo) Repo {
				return omConfigRepo{Repo: wt, failPath: omConfigFile, failErr: readRefused}
			},
		},
		{
			name: "no .om.json at the base",
			repo: func(wt Repo) Repo { return omConfigRepo{Repo: wt} },
		},
		{
			name: "an invalid .om.json",
			repo: func(wt Repo) Repo {
				return omConfigRepo{Repo: wt, files: map[string]string{omConfigFile: `{"threshold":`}}
			},
		},
		{
			// The texts read fine, so only the bar's own read refuses this.
			name: "a threshold that is not a number",
			repo: func(wt Repo) Repo {
				return omConfigRepo{Repo: wt, files: map[string]string{omConfigFile: `{"threshold":"high"}`}}
			},
		},
		{
			name: "a context_file the base does not have",
			repo: func(wt Repo) Repo {
				return omConfigRepo{Repo: wt, files: map[string]string{
					omConfigFile: `{"threshold":0.8,"context_file":"docs/rubric.md"}`,
				}}
			},
		},
		{
			name: "no repository to read from",
			repo: func(Repo) Repo { return nil },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, l, wt := keyFixture(t)
			key, touched, err := l.reviewKey(context.Background(), tc.repo(wt), wt, f.work, ReviewInput{}, f.base, f.work.Head)
			if err == nil {
				t.Fatalf("reviewKey = %+v, %v, nil; want an error when %s", key, touched, tc.name)
			}
			if key != (ReviewKey{}) || touched != nil {
				t.Errorf("reviewKey on error = %+v, %v; want the zero key and no paths", key, touched)
			}
		})
	}
}
