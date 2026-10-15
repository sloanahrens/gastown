package report

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

func TestScrubRemovesCredentialShapes(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"plain title", "plain title"},
		{"api_key=abc123 leaked", "[redacted] leaked"},
		{"TOKEN: ghp_abcdefghijklmnop", "[redacted]"},
		{"call with Bearer eyJhbGciOi.payload.sig now", "call with [redacted] now"},
		{"key sk-abcdefghijklmnopqrstu in a title", "key [redacted] in a title"},
		{"two\nlines\there", "two lines here"},
		{"password = hunter2", "[redacted]"},
	}
	for _, tc := range cases {
		if got := Scrub(tc.in); got != tc.want {
			t.Errorf("Scrub(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTitleIsOneShortScrubbedLine(t *testing.T) {
	t.Parallel()
	got := Title("  Fix the\tthing\x00 with secret=s3cr3t " + strings.Repeat("x", 100))
	if strings.ContainsAny(got, "\n\t\x00") || strings.Contains(got, "s3cr3t") {
		t.Errorf("Title kept control characters or a secret: %q", got)
	}
	if n := len([]rune(got)); n > digestTitleMax {
		t.Errorf("Title is %d runes, want at most %d", n, digestTitleMax)
	}
}

func TestShortAge(t *testing.T) {
	t.Parallel()
	cases := map[time.Duration]string{
		-time.Minute:                    "0m",
		0:                               "0m",
		59*time.Minute + 59*time.Second: "59m",
		time.Hour:                       "1h0m",
		3*time.Hour + 12*time.Minute:    "3h12m",
		50*time.Hour + 30*time.Minute:   "2d2h",
	}
	for d, want := range cases {
		if got := shortAge(d); got != want {
			t.Errorf("shortAge(%v) = %q, want %q", d, got, want)
		}
	}
}

// A bead body never reaches the digest: DONE and IN PROGRESS draw a title, and
// a question draws its id, age and held ids only.
func TestDigestCarriesNoBeadBody(t *testing.T) {
	t.Parallel()
	f := beadsfake.New()
	f.Seed(
		beads.Issue{ID: "gt-1", Title: "Short title", Description: "BODY-SENTINEL token=abc", Notes: "NOTES-SENTINEL",
			Status: "closed", ClosedAt: "2026-10-14T20:05:00Z"},
		beads.Issue{ID: "gt-2", Title: "Working", Description: "BODY-SENTINEL", Status: "hooked"},
	)
	store := fakeDigestStore{f}
	d := Digest{
		Now:   time.Date(2026, 10, 14, 20, 17, 0, 0, time.UTC),
		Slot:  time.Date(2026, 10, 14, 20, 15, 0, 0, time.UTC),
		Since: time.Date(2026, 10, 14, 20, 0, 0, 0, time.UTC),
		Rigs:  func() ([]string, error) { return []string{"gastown"}, nil },
		Store: func(name string) (BeadStore, error) {
			if name == "hq" {
				return fakeDigestStore{beadsfake.New()}, nil
			}
			return store, nil
		},
		Seats:  func() ([]SeatWork, error) { return []SeatWork{{Rig: "gastown", Polecat: "nux", Bead: "gt-2"}}, nil },
		Left:   func() (DispatchLeft, error) { return DispatchLeft{}, errors.New("boom token=zzz") },
		Health: func() (string, error) { return "GREEN", nil },
	}
	text := d.Render()
	for _, banned := range []string{"BODY-SENTINEL", "NOTES-SENTINEL", "token=abc", "token=zzz"} {
		if strings.Contains(text, banned) {
			t.Errorf("digest contains %q:\n%s", banned, text)
		}
	}
	if !strings.Contains(text, "gt-1  Short title") || !strings.Contains(text, "unavailable (boom [redacted])") {
		t.Errorf("digest lost its rows or did not scrub the error:\n%s", text)
	}
}

type fakeDigestStore struct{ *beadsfake.Fake }

func (fakeDigestStore) RejectedSince(time.Time) ([]*beads.Issue, error) { return nil, nil }
