package land

import (
	"errors"
	"os"
	"path/filepath"
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
