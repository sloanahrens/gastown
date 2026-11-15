package gitfake

import (
	"os"
	"path/filepath"
	"testing"
)

// operationCheckout is a clone the marker tests write into, and its handle.
func operationCheckout(t *testing.T) *handle {
	t.Helper()
	f := New()
	dir := t.TempDir()
	origin, work := filepath.Join(dir, "origin.git"), filepath.Join(dir, "polecat")
	f.InitBare(t, origin)
	f.Commit(t, origin, "main", "initial", map[string]string{"a.txt": "a\n"})
	f.Clone(t, origin, work)
	return f.Open(work).(*handle)
}

// OperationInProgress reads the marker files git keeps in the checkout's git
// directory: a marker holding a commit is the operation, a missing marker is no
// operation, and a marker that cannot be read is an error rather than absent.
func TestOperationInProgress(t *testing.T) {
	t.Parallel()

	const commitID = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name string
		// marker is the file written, relative to the checkout's git directory;
		// file writes it as a regular file where git writes a directory.
		marker  string
		file    bool
		want    string
		wantErr bool
	}{
		{name: "no operation in progress"},
		{name: "a merge in progress", marker: "MERGE_HEAD", want: "merge in progress"},
		{name: "a merge-backed rebase in progress", marker: "rebase-merge/onto", want: "rebase in progress"},
		{name: "an apply-backed rebase in progress", marker: "rebase-apply/onto", want: "rebase in progress"},
		{
			// A marker the filesystem cannot read is an error: the guard refuses
			// a checkpoint it could not check.
			name:    "an unreadable rebase marker",
			marker:  "rebase-merge",
			file:    true,
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := operationCheckout(t)
			if tc.marker != "" {
				gitDir, err := h.GitDir()
				if err != nil {
					t.Fatal(err)
				}
				marker := filepath.Join(gitDir, filepath.FromSlash(tc.marker))
				if !tc.file {
					if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(marker, []byte(commitID+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			got, err := h.OperationInProgress()

			if tc.wantErr && err == nil {
				t.Fatalf("OperationInProgress = %q, nil; want an error for a marker that cannot be read", got)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("OperationInProgress = %v, want no error", err)
			}
			if got != tc.want {
				t.Errorf("OperationInProgress = %q, want %q", got, tc.want)
			}
		})
	}
}

// A handle outside a repository cannot read markers at all, which is an error.
func TestOperationInProgressOutsideARepository(t *testing.T) {
	t.Parallel()
	h := New().Open(t.TempDir()).(*handle)

	got, err := h.OperationInProgress()

	if err == nil {
		t.Fatalf("OperationInProgress = %q, nil; want the not-a-repository error", got)
	}
}
