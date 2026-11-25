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

// keyFixture is a lander whose reviewer answers the version probe and whose om
// config is a readable file holding "hello", with the origin repository a key's
// git facts are read through. The commits are made on origin, so the handle
// must be opened there rather than on the fixture's stale clone.
func keyFixture(t *testing.T) (*landFixture, *Lander, Repo) {
	t.Helper()
	f := newLandFixture(t)
	l := f.lander()
	l.Reviewer = &keyReviewer{t: t, version: stubOMVersion}
	config := filepath.Join(t.TempDir(), "om.json")
	if err := os.WriteFile(config, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	l.OMConfigPath = config
	return f, l, f.git.Open(f.origin)
}

// TestVerdictReuseKeyGitFacts: the key carries w's bead and head, the patch-id
// of the merge and the paths it touches, the reviewer's om version and the om
// config's digest, with the input digest left to the caller. A base that moves
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
		OMVersion: stubOMVersion, OMConfigHash: helloSHA}
	if first != want {
		t.Errorf("reviewKey = %+v; want %+v, with InputDigest left empty", first, want)
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
