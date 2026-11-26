package cmd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/doctor"
)

type claimListingFailingWriter struct{}

func (claimListingFailingWriter) Write([]byte) (int, error) { return 0, errors.New("pipe closed") }

// claimListingTown returns a new town root holding the mayor's town file.
func claimListingTown(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mayor", "town.json"), []byte(`{"name":"gt"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestTownClaimListingPrintsOnlyTheListing: stdout is exactly the listing PodTownListing returns.
func TestTownClaimListingPrintsOnlyTheListing(t *testing.T) {
	t.Parallel()
	root := claimListingTown(t)
	want, err := doctor.PodTownListing(root)
	if err != nil {
		t.Fatalf("PodTownListing: %v", err)
	}
	var out bytes.Buffer
	if err := runTownClaimListing(&out, func() (string, error) { return root, nil }); err != nil {
		t.Fatalf("runTownClaimListing = %v, want nil", err)
	}
	if out.String() != string(want) {
		t.Fatalf("stdout = %q, want exactly the listing %q", out.String(), want)
	}
}

// TestTownClaimListingError: each refusal returns an error and writes nothing to stdout.
func TestTownClaimListingError(t *testing.T) {
	t.Parallel()
	full := claimListingTown(t)
	empty := t.TempDir()
	cases := []struct {
		name    string
		find    func() (string, error)
		failOut bool
	}{
		{"town not found", func() (string, error) { return "", errors.New("not in a town") }, false},
		{"town holds none of the copy's files", func() (string, error) { return empty, nil }, false},
		{"output cannot be written", func() (string, error) { return full, nil }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			out := io.Writer(&buf)
			if tc.failOut {
				out = claimListingFailingWriter{}
			}
			if err := runTownClaimListing(out, tc.find); err == nil {
				t.Fatal("runTownClaimListing = nil, want an error")
			}
			if buf.Len() != 0 {
				t.Errorf("stdout = %q, want nothing on an error", buf.String())
			}
		})
	}
}
