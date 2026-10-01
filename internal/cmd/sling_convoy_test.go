package cmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	convoyops "github.com/steveyegge/gastown/internal/convoy"
)

// TestConvoyTracksBead: a convoy tracks a bead when the raw tracks-dep rows
// name it, directly or wrapped as external:<rig>:<id>; a failing bd reads as
// not tracked.
func TestConvoyTracksBead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		rows []string // depends_on_id values; nil with fail set fails the query
		fail bool
		want bool
	}{
		{"exact match", []string{"gt-abc123"}, false, true},
		{"external ref", []string{"external:gt-abc:gt-abc123"}, false, true},
		{"other bead", []string{"gt-other456"}, false, false},
		{"no deps", nil, false, false},
		{"among several", []string{"gt-other1", "external:gt-abc:gt-abc123", "gt-other2"}, false, true},
		{"bd fails", nil, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := beadsfake.New(beadsfake.WithPrefix("hq"))
			db.OnSQL(func(query string) ([][]string, error) {
				if tc.fail {
					return nil, errors.New("bd sql failed")
				}
				if !strings.Contains(query, "issue_id = 'hq-cv-test'") {
					return nil, fmt.Errorf("unexpected query %q", query)
				}
				rows := [][]string{{"depends_on_id"}}
				for _, id := range tc.rows {
					rows = append(rows, []string{id})
				}
				return rows, nil
			})
			town := slingConvoyTown{root: t.TempDir(), db: db}
			if got := town.convoyTracksBead("hq-cv-test", "gt-abc123"); got != tc.want {
				t.Fatalf("convoyTracksBead = %v, want %v; sql:\n%q", got, tc.want, db.SQLStatements())
			}
		})
	}
}

// TestBdDepListRawIDsValidation verifies that bdDepListRawIDs rejects
// invalid bead IDs to prevent SQL injection.
func TestBdDepListRawIDsValidation(t *testing.T) {
	t.Parallel()
	_, err := convoyops.DepListRawIDs("/tmp", "'; DROP TABLE deps; --", "down", "tracks")
	if err == nil {
		t.Error("bdDepListRawIDs should reject SQL injection attempts")
	}

	_, err = convoyops.DepListRawIDs("/tmp", "valid-id", "down", "'; DROP TABLE deps; --")
	if err == nil {
		t.Error("bdDepListRawIDs should reject SQL injection in depType")
	}
}
