package cmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/outbox"
	"github.com/steveyegge/gastown/internal/seatship"
)

// seatShipFailingPost is a beads client whose AddComment fails and counts its
// calls, so a test sees both the error and that nothing retried.
type seatShipFailingPost struct {
	beads.Client
	calls int
}

func (c *seatShipFailingPost) AddComment(id, comment string) error {
	c.calls++
	return errors.New("the bead store refused the comment")
}

// seatShipDepsOf builds the deps one test runs with: empty seat directories,
// a one-minute deadline and a buffer that holds what the command printed.
func seatShipDepsOf(t *testing.T, c beads.Client) (seatShipDeps, *bytes.Buffer) {
	t.Helper()
	var stderr bytes.Buffer
	return seatShipDeps{
		Beads:    c,
		Projects: t.TempDir(),
		Outbox:   t.TempDir(),
		Deadline: time.Minute,
		Stderr:   &stderr,
	}, &stderr
}

func seatShipFixture(t *testing.T) *beadsfake.Fake {
	t.Helper()
	db := beadsfake.New()
	if _, err := db.Create(beads.CreateOptions{ID: "gt-seat", Labels: []string{"gt:ready-to-land"}}); err != nil {
		t.Fatal(err)
	}
	return db
}

// TestSeatShipPostsTheRecord: a finished, submitted seat posts exactly one
// comment, in the SEAT-END form, whose decoded payload names the seat and the
// bead and says it was submitted; nothing goes to stderr.
func TestSeatShipPostsTheRecord(t *testing.T) {
	t.Parallel()
	db := seatShipFixture(t)
	deps, stderr := seatShipDepsOf(t, db)

	if code := runSeatShip(context.Background(), deps, "gastown", "nux", "gt-seat"); code != 0 {
		t.Fatalf("runSeatShip = %d (stderr %q), want 0", code, stderr)
	}
	comments, err := db.Comments("gt-seat")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || !strings.HasPrefix(comments[0].Text, seatship.CommentPrefix) {
		t.Fatalf("comments = %+v, want one comment starting %q", comments, seatship.CommentPrefix)
	}
	p, found, err := seatship.Decode(comments[0].Text)
	if err != nil || !found {
		t.Fatalf("Decode = %v, %v, want the payload", found, err)
	}
	if p.Rig != "gastown" || p.Polecat != "nux" || p.Bead != "gt-seat" || !p.Submitted {
		t.Errorf("payload = %+v, want rig gastown, polecat nux, bead gt-seat, submitted", p)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want nothing on success", stderr)
	}
}

// TestSeatShipFailuresExitOneAndPostNothingMore: each step's error is one
// `seat ship: <reason>` line and exit 1, with no retry of the post.
func TestSeatShipFailuresExitOneAndPostNothingMore(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		setup func(t *testing.T) (seatShipDeps, string)
		want  string
		posts int
		// cancelled runs the command with a context that is already done.
		cancelled bool
	}{
		{
			name: "the post is refused",
			setup: func(t *testing.T) (seatShipDeps, string) {
				deps, _ := seatShipDepsOf(t, &seatShipFailingPost{Client: seatShipFixture(t)})
				return deps, "gt-seat"
			},
			want:  "the bead store refused the comment",
			posts: 1,
		},
		{
			name: "the bead cannot be read",
			setup: func(t *testing.T) (seatShipDeps, string) {
				deps, _ := seatShipDepsOf(t, &seatShipFailingPost{Client: seatShipFixture(t)})
				return deps, "gt-missing"
			},
			want:  "gt-missing",
			posts: 0,
		},
		{
			name: "the outbox cannot be collected",
			setup: func(t *testing.T) (seatShipDeps, string) {
				deps, _ := seatShipDepsOf(t, &seatShipFailingPost{Client: seatShipFixture(t)})
				big := bytes.Repeat([]byte("x"), 65<<10)
				if err := os.WriteFile(filepath.Join(deps.Outbox, outbox.ResultFile), big, 0o600); err != nil {
					t.Fatal(err)
				}
				return deps, "gt-seat"
			},
			want:  "outbox",
			posts: 0,
		},
		{
			name: "the context is already done",
			setup: func(t *testing.T) (seatShipDeps, string) {
				deps, _ := seatShipDepsOf(t, &seatShipFailingPost{Client: seatShipFixture(t)})
				return deps, "gt-seat"
			},
			want:      "context canceled",
			posts:     0,
			cancelled: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps, bead := tc.setup(t)
			var stderr bytes.Buffer
			deps.Stderr = &stderr
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			if code := runSeatShip(ctx, deps, "gastown", "nux", bead); code != 1 {
				t.Fatalf("runSeatShip = %d, want 1", code)
			}
			line := stderr.String()
			if !strings.HasPrefix(line, "seat ship: ") || !strings.Contains(line, tc.want) || strings.Count(line, "\n") != 1 {
				t.Errorf("stderr = %q, want one line starting %q and naming %q", line, "seat ship: ", tc.want)
			}
			if got := deps.Beads.(*seatShipFailingPost).calls; got != tc.posts {
				t.Errorf("AddComment calls = %d, want %d (no retry)", got, tc.posts)
			}
		})
	}
}
