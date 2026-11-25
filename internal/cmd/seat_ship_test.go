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
			name: "the record is too large for one comment",
			setup: func(t *testing.T) (seatShipDeps, string) {
				deps, _ := seatShipDepsOf(t, &seatShipFailingPost{Client: seatShipFixture(t)})
				// Each outbox file is under its cap, but the two together are
				// over the comment cap, and with no transcript there is no tail
				// to drop.
				big := bytes.Repeat([]byte("x"), 60<<10)
				for _, name := range []string{outbox.ResultFile, outbox.NotesFile} {
					if err := os.WriteFile(filepath.Join(deps.Outbox, name), big, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return deps, "gt-seat"
			},
			want:  "too large",
			posts: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deps, bead := tc.setup(t)
			var stderr bytes.Buffer
			deps.Stderr = &stderr
			if code := runSeatShip(context.Background(), deps, "gastown", "nux", bead); code != 1 {
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

// seatShipBlockingPost is a beads client whose AddComment holds the post, the
// way a hung bead store holds it: it closes reached when the post is entered,
// waits for release, then delegates to the store so the late post lands, and
// closes exited when it is done. A test that joins on exited has joined the
// goroutine the run left behind.
type seatShipBlockingPost struct {
	beads.Client
	// ctx is the test's own context: the hold also ends when the test does, so a
	// post nothing releases cannot outlive it.
	ctx     context.Context
	reached chan<- struct{}
	release <-chan struct{}
	exited  chan<- struct{}
}

func (c *seatShipBlockingPost) AddComment(id, comment string) error {
	close(c.reached)
	select {
	case <-c.release:
	case <-c.ctx.Done():
		close(c.exited)
		return c.ctx.Err()
	}
	err := c.Client.AddComment(id, comment)
	close(c.exited)
	return err
}

// seatShipFiredDeadline is a deadline context the test fires itself: Done is
// the fired channel and Err is DeadlineExceeded once it is closed, so no row
// waits on real time for a deadline.
type seatShipFiredDeadline struct {
	context.Context
	fired chan struct{}
}

func (c *seatShipFiredDeadline) Done() <-chan struct{} { return c.fired }

func (c *seatShipFiredDeadline) Err() error {
	select {
	case <-c.fired:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// blockedPostFixture is a run whose post is held: the bead store, the deps with
// the injected deadline, and the channels the test drives it with.
type blockedPostFixture struct {
	db      *beadsfake.Fake
	deps    seatShipDeps
	stderr  *bytes.Buffer
	reached chan struct{}
	release chan struct{}
	exited  chan struct{}
	fired   chan struct{}
}

func newBlockedPost(t *testing.T) *blockedPostFixture {
	t.Helper()
	f := &blockedPostFixture{
		db:      seatShipFixture(t),
		reached: make(chan struct{}),
		release: make(chan struct{}),
		exited:  make(chan struct{}),
		fired:   make(chan struct{}),
	}
	t.Cleanup(func() {
		select {
		case <-f.release:
		default:
			close(f.release)
		}
	})
	f.deps, f.stderr = seatShipDepsOf(t, &seatShipBlockingPost{Client: f.db, ctx: t.Context(), reached: f.reached, release: f.release, exited: f.exited})
	f.deps.Expire = func(parent context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
		return &seatShipFiredDeadline{Context: parent, fired: f.fired}, func() {}
	}
	return f
}

// run starts runSeatShip and returns the channel its exit code arrives on.
func (f *blockedPostFixture) run(ctx context.Context) <-chan int {
	got := make(chan int, 1)
	go func() { got <- runSeatShip(ctx, f.deps, "gastown", "nux", "gt-seat") }()
	return got
}

// TestSeatShipDeadlineRefusesANonPositiveBudget: a zero or negative deadline is
// refused before anything is read or posted.
func TestSeatShipDeadlineRefusesANonPositiveBudget(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		deadline time.Duration
	}{
		{"zero", 0},
		{"negative", -time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := &seatShipFailingPost{Client: seatShipFixture(t)}
			deps, stderr := seatShipDepsOf(t, client)
			deps.Deadline = tc.deadline
			if code := runSeatShip(context.Background(), deps, "gastown", "nux", "gt-seat"); code != 1 {
				t.Fatalf("runSeatShip = %d, want 1", code)
			}
			line := stderr.String()
			want := "deadline " + tc.deadline.String()
			if !strings.HasPrefix(line, "seat ship: ") || !strings.Contains(line, want) || strings.Count(line, "\n") != 1 {
				t.Errorf("stderr = %q, want one seat ship line naming %q", line, want)
			}
			if client.calls != 0 {
				t.Errorf("AddComment calls = %d, want 0", client.calls)
			}
		})
	}
}

// TestSeatShipDeadlineStopsABlockedPost: a post the bead store holds is cut by
// the deadline the test fires itself, with no real wait: the run returns 1 with
// the deadline and the may-still-complete note, and once the held post is
// released and joined the bead holds exactly one comment, so a failure line can
// precede a post that lands.
func TestSeatShipDeadlineStopsABlockedPost(t *testing.T) {
	t.Parallel()
	f := newBlockedPost(t)
	got := f.run(context.Background())
	awaitEventWithin(t, f.reached, "the post to be entered")
	close(f.fired)
	if code := awaitEventWithin(t, got, "runSeatShip"); code != 1 {
		t.Fatalf("runSeatShip = %d, want 1", code)
	}
	line := f.stderr.String()
	want := "deadline " + f.deps.Deadline.String()
	if !strings.HasPrefix(line, "seat ship: ") || !strings.Contains(line, want) || !strings.Contains(line, "may still complete") || strings.Count(line, "\n") != 1 {
		t.Errorf("stderr = %q, want one seat ship line naming %q and that the post may still complete", line, want)
	}

	close(f.release)
	awaitEventWithin(t, f.exited, "the held post to return")
	comments, err := f.db.Comments("gt-seat")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 {
		t.Errorf("comments after the late post = %d, want 1: the failure line did not mean nothing was posted", len(comments))
	}
}

// TestSeatShipCancelIsNotADeadline: a parent context that is canceled, before
// the run or while the post is held, is reported as `seat ship: canceled` and
// never as the deadline, and a refused run posts nothing.
func TestSeatShipCancelIsNotADeadline(t *testing.T) {
	t.Parallel()
	t.Run("canceled before the run", func(t *testing.T) {
		t.Parallel()
		client := &seatShipFailingPost{Client: seatShipFixture(t)}
		deps, stderr := seatShipDepsOf(t, client)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if code := runSeatShip(ctx, deps, "gastown", "nux", "gt-seat"); code != 1 {
			t.Fatalf("runSeatShip = %d, want 1", code)
		}
		if got, want := stderr.String(), "seat ship: canceled\n"; got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
		if client.calls != 0 {
			t.Errorf("AddComment calls = %d, want 0", client.calls)
		}
	})
	t.Run("canceled while the post is held", func(t *testing.T) {
		t.Parallel()
		f := newBlockedPost(t)
		f.deps.Expire = nil // the real deadline: only the parent's cancel ends this run
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		got := f.run(ctx)
		awaitEventWithin(t, f.reached, "the post to be entered")
		cancel()
		if code := awaitEventWithin(t, got, "runSeatShip"); code != 1 {
			t.Fatalf("runSeatShip = %d, want 1", code)
		}
		if got, want := f.stderr.String(), "seat ship: canceled\n"; got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
		close(f.release)
		awaitEventWithin(t, f.exited, "the held post to return")
	})
}

// TestSeatShipParentDeadlineIsNotOurs: a parent context whose own deadline has
// passed is reported as the caller's deadline, never as d.Deadline.
func TestSeatShipParentDeadlineIsNotOurs(t *testing.T) {
	t.Parallel()
	client := &seatShipFailingPost{Client: seatShipFixture(t)}
	deps, stderr := seatShipDepsOf(t, client)
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancel()
	if code := runSeatShip(ctx, deps, "gastown", "nux", "gt-seat"); code != 1 {
		t.Fatalf("runSeatShip = %d, want 1", code)
	}
	line := stderr.String()
	if !strings.Contains(line, "the caller's deadline") || strings.Contains(line, "deadline "+deps.Deadline.String()) {
		t.Errorf("stderr = %q, want the caller's deadline named and not d.Deadline (%s)", line, deps.Deadline)
	}
	if client.calls != 0 {
		t.Errorf("AddComment calls = %d, want 0", client.calls)
	}
}
