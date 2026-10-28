package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/installpending"
)

// A gt install in flight stops a hand sling before it spawns anything, and a
// marker that has aged out or no marker leaves the sling to run (gt-2rp64).
func TestSlingRefusesOnlyWhileAnInstallIsPending(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		setup       func(h *slingHarness)
		wantRefused bool
	}{
		{"live marker", func(h *slingHarness) { h.setPendingInstall(time.Now()) }, true},
		{"stale marker", func(h *slingHarness) { h.setPendingInstall(time.Now().Add(-installpending.Ceiling)) }, false},
		{"no marker", func(*slingHarness) {}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead("gt-a", beadInfo{Title: "work"})
			tc.setup(h)

			err := h.sling("gt-a", "gastown")
			if tc.wantRefused {
				wantSlingErr(t, err, "an install of gt is pending")
				h.wantNo("spawn")
				h.wantNo("hook")
				return
			}
			if err != nil {
				t.Fatalf("sling: %v", err)
			}
			h.wantCalls("spawn", "spawn gastown")
		})
	}
}

// The refusal names the pending install — when the cycle recorded it and the
// daemon that recorded it — and carries the marker an automatic dispatcher
// reads to defer the bead rather than count a failure (gt-2rp64).
func TestSlingInstallRefusalNamesThePendingInstall(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead("gt-a", beadInfo{Title: "work"})
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	h.setPendingInstall(at)

	err := h.sling("gt-a", "gastown")
	wantSlingErr(t, err, dispatch.SlingRefusalMarker)
	for _, want := range []string{
		"an install of gt is pending",
		at.UTC().Format(time.RFC3339),
		fmt.Sprintf("daemon pid %d", os.Getpid()),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
	if !isSlingRefusal(err) {
		t.Error("isSlingRefusal = false, want a refusal an automatic dispatcher defers the bead on")
	}
}

// A marker the reader cannot parse refuses too: a sling that must not race an
// install never reads broken state as no install (gt-2rp64).
func TestSlingRefusesOnAnUnreadableInstallMarker(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead("gt-a", beadInfo{Title: "work"})
	h.breakPendingInstall()

	wantSlingErr(t, h.sling("gt-a", "gastown"), "cannot be read")
	h.wantNo("spawn")
}

// A tick refuses under a live marker before it reads the board or spends a seat,
// and proceeds on a marker that has aged out or none at all (gt-2rp64).
func TestSpecDispatchRefusesOnlyWhileAnInstallIsPending(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		at          time.Time
		wantRefused bool
	}{
		{"live marker", specTestNow, true},
		{"stale marker", specTestNow.Add(-installpending.Ceiling), false},
		{"no marker", time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeSpecTown(cleanSpec("gt-a", 2, "2026-09-29T12:00:00Z"))
			env := f.env()
			env.PendingInstall = specInstallMarkerAt(t, tc.at)

			r := runSpecDispatchCycle(env)
			if tc.wantRefused {
				if !strings.Contains(r.Hold, "an install of gt is pending") {
					t.Fatalf("hold = %q, want the pending install named", r.Hold)
				}
				if r.Candidates != 0 || len(f.slung) != 0 {
					t.Fatalf("tick read %d candidate(s) and slung %v under a pending install", r.Candidates, f.slung)
				}
				return
			}
			if r.Hold != "" {
				t.Fatalf("hold = %q, want the tick to proceed", r.Hold)
			}
			if got := strings.Join(f.slung, " "); got != "gt-a" {
				t.Fatalf("slung %q, want gt-a", got)
			}
		})
	}
}

// A marker the tick cannot read stops the tick and reaches the daemon's log as
// an error, not as a tick that found nothing to sling (gt-2rp64).
func TestSpecDispatchReportsAnUnreadableInstallMarker(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 2, "2026-09-29T12:00:00Z"))
	env := f.env()
	store := installpending.StoreIn(filepath.Join(t.TempDir(), installpending.FileName))
	if err := os.WriteFile(store.Path(), []byte("{not a marker"), 0o644); err != nil {
		t.Fatal(err)
	}
	env.PendingInstall = func() (*installpending.Marker, error) { return store.Pending(specTestNow) }

	r := runSpecDispatchCycle(env)
	if len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "install-pending marker unreadable") {
		t.Fatalf("errors = %q, want the unreadable marker reported once", r.Errors)
	}
	if len(f.slung) != 0 {
		t.Fatalf("slung %v with an unreadable marker, want nothing", f.slung)
	}
}

// A background job refuses under a marker written after the tick planned it, and
// runs on a marker that has aged out or none at all (gt-2rp64).
func TestSpecSlingJobRefusesOnlyWhileAnInstallIsPending(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		at          time.Time
		wantRefused bool
	}{
		{"live marker", specTestNow, true},
		{"stale marker", specTestNow.Add(-installpending.Ceiling), false},
		{"no marker", time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeSpecTown(cleanSpec("gt-a", 2, "2026-09-29T12:00:00Z"))
			env := f.env()
			env.PendingInstall = specInstallMarkerAt(t, tc.at)
			job := specSlingJob{Bead: "gt-a", Rig: "gastown", Agent: "deepseek-flash"}

			r := runSpecSlingJob(env, job)
			if tc.wantRefused {
				if !strings.Contains(r.Hold, "an install of gt is pending") {
					t.Fatalf("hold = %q, want the pending install named", r.Hold)
				}
				if len(f.slung) != 0 {
					t.Fatalf("slung %v under a pending install, want nothing", f.slung)
				}
				return
			}
			if r.Hold != "" {
				t.Fatalf("hold = %q, want the job to run", r.Hold)
			}
			if got := strings.Join(f.slung, " "); got != "gt-a" {
				t.Fatalf("slung %q, want gt-a", got)
			}
		})
	}
}

// specInstallMarkerAt writes the marker a tick or a sling job reads, recorded by
// this test process at at, and returns the reader over it at the tick's clock. A
// zero at writes no marker.
func specInstallMarkerAt(t *testing.T, at time.Time) func() (*installpending.Marker, error) {
	t.Helper()
	store := installpending.StoreIn(filepath.Join(t.TempDir(), installpending.FileName))
	if !at.IsZero() {
		if err := store.Write(at, os.Getpid()); err != nil {
			t.Fatalf("writing the install-pending marker: %v", err)
		}
	}
	return func() (*installpending.Marker, error) { return store.Pending(specTestNow) }
}
