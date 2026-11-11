package cloudledger_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/cloudledger"
)

// A DSN file with no content is named empty. A zero-byte file is empty whatever
// its mode: the file a Secret mounts is 0644 by default, and the mode refusal
// would hide that the operator has to fill it. A file with only whitespace is
// empty at a mode that passes, but at 0644 the mode is refused before the
// content is read, so it is not named empty. Errors name the file, never any
// content.
func TestCloudLedgerNamesAnEmptyDSNFile(t *testing.T) {
	t.Parallel()

	const (
		wantDSN = iota
		wantEmpty
		wantModeRefusal
	)
	cases := []struct {
		name string
		body string
		mode os.FileMode
		want int
	}{
		{name: "zero bytes, owner only", body: "", mode: 0o600, want: wantEmpty},
		{name: "zero bytes, readable by others", body: "", mode: 0o644, want: wantEmpty},
		{name: "whitespace only, owner only", body: " \n\t\n", mode: 0o600, want: wantEmpty},
		{name: "whitespace only, readable by others", body: " \n\t\n", mode: 0o644, want: wantModeRefusal},
		{name: "a DSN", body: "postgres://reader:pw@db/ledger\n", mode: 0o600, want: wantDSN},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "ledger-reader.dsn")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}

			err := cloudledger.CheckDSNFile(path)
			switch tc.want {
			case wantDSN:
				if err != nil {
					t.Fatalf("CheckDSNFile = %v, want nil for a DSN", err)
				}
			case wantEmpty:
				if !errors.Is(err, cloudledger.ErrEmptyDSN) {
					t.Fatalf("CheckDSNFile = %v, want ErrEmptyDSN", err)
				}
				if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "empty") {
					t.Errorf("error = %q, want the file named and the word empty", err)
				}
			case wantModeRefusal:
				if err == nil || errors.Is(err, cloudledger.ErrEmptyDSN) || !strings.Contains(err.Error(), "readable by group or others") {
					t.Fatalf("CheckDSNFile = %v, want the mode refusal, not an empty file: the content is never read", err)
				}
				if !strings.Contains(err.Error(), path) {
					t.Errorf("error = %q, want the file named", err)
				}
			}
		})
	}
}

// The read the daemon makes reaches the same refusal, so the source's own call
// site, not only the check, names the empty file.
func TestCloudLedgerEmptyDSNFileIsARefusedRead(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "ledger-reader.dsn")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	src := newPostgres(t, path)
	_, err := src.LatestPatrolReport(context.Background())
	if !errors.Is(err, cloudledger.ErrEmptyDSN) {
		t.Fatalf("LatestPatrolReport = %v, want ErrEmptyDSN", err)
	}
}
