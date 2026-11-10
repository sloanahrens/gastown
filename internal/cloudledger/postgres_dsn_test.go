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

// A DSN file with no content is named empty, whatever its mode: the file a
// Secret mounts is 0644 by default, and the mode refusal would hide that the
// operator has to fill it. The error names the file and never any content.
func TestCloudLedgerNamesAnEmptyDSNFile(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		body      string
		mode      os.FileMode
		wantEmpty bool
	}{
		{name: "zero bytes, owner only", body: "", mode: 0o600, wantEmpty: true},
		{name: "zero bytes, readable by others", body: "", mode: 0o644, wantEmpty: true},
		{name: "whitespace only", body: " \n\t\n", mode: 0o600, wantEmpty: true},
		{name: "a DSN", body: "postgres://reader:pw@db/ledger\n", mode: 0o600},
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
			if !tc.wantEmpty {
				if err != nil {
					t.Fatalf("CheckDSNFile = %v, want nil for a DSN", err)
				}
				return
			}
			if !errors.Is(err, cloudledger.ErrEmptyDSN) {
				t.Fatalf("CheckDSNFile = %v, want ErrEmptyDSN", err)
			}
			if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "empty") {
				t.Errorf("error = %q, want the file named and the word empty", err)
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
