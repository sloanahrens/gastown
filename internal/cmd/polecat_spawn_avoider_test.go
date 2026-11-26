package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
)

const avoiderTestPrefix = "gt"

// avoiderTestManager is a rig's polecat manager over a rig directory of its own,
// so each draw starts from a fresh name pool.
func avoiderTestManager(t *testing.T) *polecat.Manager {
	t.Helper()
	return polecat.NewManager(&rig.Rig{Name: "gastown", Path: t.TempDir()}, nil, nil, nil)
}

// poolDraws returns the first n names a fresh rig's pool draws with no avoider.
func poolDraws(t *testing.T, n int) []string {
	t.Helper()
	pool := avoiderTestManager(t).GetNamePool()
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name, err := pool.Allocate()
		if err != nil {
			t.Fatalf("Allocate: %v", err)
		}
		out = append(out, name)
	}
	return out
}

// TestSlingAvoidsStuckSandboxNames: a hand sling that draws a pool name passes
// over a name whose gateway sandbox is stale, and when it cannot tell (an
// unreadable listing, or no sandbox profile) it draws the first name and says
// the check was skipped in one line (gt-y113l8).
func TestSlingAvoidsStuckSandboxNames(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	draws := poolDraws(t, 2)
	first, second := draws[0], draws[1]
	stuckFirst := []sandbox.Entry{{Name: session.PolecatSessionName(avoiderTestPrefix, first), Phase: "Error"}}
	noListing := func(context.Context) ([]sandbox.Entry, error) {
		t.Errorf("the gateway was listed for a town with no sandbox profile")
		return nil, nil
	}

	tests := []struct {
		name      string
		list      func(context.Context) ([]sandbox.Entry, error)
		sandboxed bool
		want      string
		skipNote  string // when set, the output is exactly one line containing it
	}{
		{
			name:      "a stuck sandbox of the first name sends the draw to the next name",
			list:      func(context.Context) ([]sandbox.Entry, error) { return stuckFirst, nil },
			sandboxed: true,
			want:      second,
		},
		{
			name: "a stuck sandbox of another prefix leaves the first name",
			list: func(context.Context) ([]sandbox.Entry, error) {
				return []sandbox.Entry{{Name: session.PolecatSessionName("bd", first), Phase: "Error"}}, nil
			},
			sandboxed: true,
			want:      first,
		},
		{
			name:      "a listing with no stale sandbox leaves the first name",
			list:      func(context.Context) ([]sandbox.Entry, error) { return nil, nil },
			sandboxed: true,
			want:      first,
		},
		{
			name:      "an unreadable listing draws the first name and says the check was skipped",
			list:      func(context.Context) ([]sandbox.Entry, error) { return nil, errors.New("the gateway did not answer") },
			sandboxed: true,
			want:      first,
			skipNote:  "stale seat sandbox check skipped",
		},
		{
			name:      "a town with no sandbox profile draws the first name and says the check was skipped",
			list:      noListing,
			sandboxed: false,
			want:      first,
			skipNote:  "no sandbox profile",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mgr := avoiderTestManager(t)
			stale := newSpecStaleSeats(&specSlingTraces{}, func() time.Time { return now }, tc.sandboxed)
			stale.list = tc.list
			var out bytes.Buffer

			setSlingNameAvoider(&out, mgr, stale, avoiderTestPrefix)
			got, err := mgr.GetNamePool().Allocate()
			if err != nil {
				t.Fatalf("Allocate: %v", err)
			}
			if got != tc.want {
				t.Errorf("drew %q, want %q", got, tc.want)
			}

			text := out.String()
			if tc.skipNote == "" {
				if strings.Contains(text, "skipped") {
					t.Errorf("output %q says the check was skipped, want no skip line", text)
				}
				return
			}
			lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
			if len(lines) != 1 || !strings.Contains(lines[0], tc.skipNote) {
				t.Errorf("output = %q, want one line containing %q", text, tc.skipNote)
			}
		})
	}
}
