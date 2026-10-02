package cmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
)

// TestNamedSlingRefusal_MapsTheVerdict: the dry-run peek turns the read-only
// reuse verdict into the refusal a live sling would raise — the same text,
// from the same builders (gt-yxc7m).
func TestNamedSlingRefusal_MapsTheVerdict(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		held    string
		peekErr error
		create  bool
		want    []string
		notWant []string
	}{
		{
			name: "eligible",
			// Nothing to print: the caller prints the reuse line.
		},
		{
			name:    "parked",
			peekErr: fmt.Errorf("%w: %w: parked (operator parked)", polecat.ErrPolecatNeedsRecovery, polecat.ErrPolecatParked),
			want:    []string{"gastown/garnet cannot take this sling", "gt agent resume gastown/garnet"},
		},
		{
			name:    "still holds the slung bead",
			held:    "gt-new",
			peekErr: fmt.Errorf("%w: not-idle", polecat.ErrPolecatNeedsRecovery),
			want:    []string{"It already holds gt-new", "gt session start gastown/garnet --issue gt-new"},
		},
		{
			name:    "absent without create",
			peekErr: polecat.ErrPolecatNotFound,
			want:    []string{"gastown/flint does not exist", "add --create"},
		},
		{
			name:    "absent with create defers",
			peekErr: polecat.ErrPolecatNotFound,
			create:  true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			name := "garnet"
			if strings.HasPrefix(tt.name, "absent") {
				name = "flint"
			}

			err := namedSlingRefusal("gastown", SlingSpawnOptions{
				Name: name, HookBead: "gt-new", Create: tt.create,
			}, tt.held, tt.peekErr)
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("verdict mapped to %v; want no refusal", err)
				}
				return
			}
			if err == nil {
				t.Fatal("verdict mapped to no refusal")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q lacks %q", err, want)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(err.Error(), notWant) {
					t.Errorf("refusal %q must not contain %q", err, notWant)
				}
			}
		})
	}
}

// TestResolveTarget_DryRunNamedPolecatRefusal: a dry run of a named sling
// prints the refusal a live sling would raise instead of the reuse line, and
// still resolves the target so the rest of the preview is printed (gt-yxc7m).
func TestResolveTarget_DryRunNamedPolecatRefusal(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	var peeked SlingSpawnOptions
	h.run.peekNamed = func(townRoot, rigName string, opts SlingSpawnOptions) error {
		peeked = opts
		if townRoot != slingTestTown || rigName != "gastown" {
			t.Fatalf("peek for %s/%s, want gastown in %s", townRoot, rigName, slingTestTown)
		}
		return namedPolecatRefusal(rigName, opts.Name, opts.HookBead, "",
			fmt.Errorf("%w: not-idle", polecat.ErrPolecatNeedsRecovery))
	}

	res, err := h.run.resolveSlingTarget("gastown/polecats/garnet", ResolveTargetOptions{
		DryRun: true, NoBoot: true, TownRoot: slingTestTown, HookBead: "gt-new",
	})
	if err != nil {
		t.Fatalf("dry run returned an error; it reports a refusal in the preview: %v", err)
	}
	h.wantNo("spawn")
	if peeked.Name != "garnet" || peeked.HookBead != "gt-new" {
		t.Fatalf("peek got %+v; want the named polecat and the slung bead", peeked)
	}
	out := h.out.String()
	for _, want := range []string{"would be refused", "gastown/garnet cannot take this sling", "not-idle"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Would reuse/create") {
		t.Errorf("dry run printed the reuse line for a refused polecat:\n%s", out)
	}
	if res.Agent != "gastown/polecats/garnet" {
		t.Errorf("Agent = %q; want the named polecat", res.Agent)
	}
	if res.Pane == "" {
		t.Error("dry run left the pane empty; runSling prints it as the start prompt target")
	}
}

// TestResolveTarget_DryRunNamedPolecatEligible: when the peek has nothing to
// report, the reuse line is still the preview — the refusal is additive, not a
// replacement for the route (gt-yxc7m).
func TestResolveTarget_DryRunNamedPolecatEligible(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.run.peekNamed = func(string, string, SlingSpawnOptions) error { return nil }

	if _, err := h.run.resolveSlingTarget("gastown/polecats/garnet", ResolveTargetOptions{
		DryRun: true, NoBoot: true, TownRoot: slingTestTown, HookBead: "gt-new",
	}); err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	out := h.out.String()
	if !strings.Contains(out, "Would reuse/create named polecat gastown/garnet") {
		t.Errorf("dry-run output lacks the reuse line:\n%s", out)
	}
	if strings.Contains(out, "would be refused") {
		t.Errorf("dry run reported a refusal for an eligible polecat:\n%s", out)
	}
}
