package cmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/sling"
)

// TestSlingGiveBackReason pins which sling failures hand back the respawn
// count their spawn charged (gt-hsrr45). The rows are the failures the town
// actually produced, so the text matches are tested against the text a sling
// prints rather than a paraphrase of it. A polecat's death keeps its count:
// that is the failure the respawn limit exists to bound.
func TestSlingGiveBackReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		// reason is the kind the classifier must name; empty for a keep.
		reason string
	}{
		{
			name:   "a Dolt serialization failure",
			err:    errors.New(`bd mol bond mol-polecat-work gt-x: Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction.`),
			reason: "serialization",
		},
		{
			name:   "an assignee sling lock that stayed held",
			err:    errors.New(`serializing hook write for gastown/polecats/toast: timed out acquiring assignee sling lock after 10s (held by pid 10297 started 00:07:09Z; another sling may be stuck)`),
			reason: "assignee-lock",
		},
		{
			name:   "a session that was already running",
			err:    errors.New("starting polecat session: session already running: gt-cheedo"),
			reason: "session",
		},
		{
			name:   "a bd gateway that did not answer",
			err:    errors.New(`gtwrap: the bd gateway at http://bd-gateway.gt.svc:7440 did not answer, so the call did not run: Post "http://bd-gateway.gt.svc:7440/v1/exec": dial tcp 10.43.155.32:7440: connect: connection refused`),
			reason: "gateway",
		},
		{
			name:   "a bond lock that did not free",
			err:    fmt.Errorf("bonding: %w", ErrBondLockTimeout),
			reason: "bond-lock",
		},
		{
			name:   "an assignee lock sentinel",
			err:    fmt.Errorf("serializing hook write: %w", ErrAssigneeLockTimeout),
			reason: "assignee-lock",
		},
		{
			name:   "an image that drifts from the installed gt",
			err:    fmt.Errorf("starting session: %w", session.ErrImageDrift),
			reason: "image-drift",
		},
		{
			name:   "a session sentinel",
			err:    fmt.Errorf("starting polecat session: %w", polecat.ErrSessionRunning),
			reason: "session",
		},
		// The two seat-branch refusals match by type; the marker they share
		// does not buy the held-bead or ready-to-land refusal a give-back
		// (gt-dzqkdw).
		{
			name: "a seat whose worktree branch could not be read",
			err: fmt.Errorf("%s gt-x: the worktree branch of seat gastown/polecats/toast could not be read after 3 attempts, so the hook cannot be checked against another bead's branch; the dispatch is deferred until it can be read: %w",
				dispatch.SlingRefusalMarker, sling.ErrSeatBranchRefused),
			reason: "seat-branch",
		},
		{
			name: "a seat that already holds another bead's branch",
			err: fmt.Errorf("%s gt-x: the seat gastown/polecats/toast already has polecat/toast/gt-other checked out, which carries gt-other's work, so the hook would be written onto another bead's branch: %w",
				dispatch.SlingRefusalMarker, sling.ErrSeatBranchRefused),
			reason: "seat-branch",
		},
		{
			name: "a held bead written after the dispatch read it",
			err: fmt.Errorf("%s gt-x is held (deferred): the hold was written after this dispatch read the bead, so the work is not this dispatch's to take\nRelease the hold (clear the label, or set its status back) to dispatch it, or pass --force to take it anyway",
				dispatch.SlingRefusalMarker),
		},
		{
			name: "a bead submitted for landing",
			err: fmt.Errorf("%s gt-x is submitted for landing (ready-to-land); the landing worker owns it.\nRemove the label first to take it back",
				dispatch.SlingRefusalMarker),
		},
		{
			name: "an error with no refusal",
			err:  errors.New("instantiating the formula: unknown var"),
		},
		{
			name: "a polecat that died at startup",
			err:  errors.New("starting polecat session: agent exited with status 1"),
		},
		{
			name: "an unrelated failure",
			err:  errors.New("bead 'gt-x' not found"),
		},
		{
			name: "no failure",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reason, giveBack := slingGiveBackReason(tc.err)
			if want := tc.reason != ""; giveBack != want {
				t.Fatalf("giveBack = %v, want %v (reason %q)", giveBack, want, reason)
			}
			if !giveBack {
				if reason != "" {
					t.Errorf("a kept count names reason %q, want none", reason)
				}
				return
			}
			if !strings.Contains(reason, tc.reason) {
				t.Errorf("reason = %q, want it to name %q", reason, tc.reason)
			}
		})
	}
}

// TestSlingGiveBackReasonPreSpawn pins the other half of the seat-branch
// give-back: giveBackSlingRespawn reaches a spawn only when the engine returned
// one, so a refusal raised before the spawn has no count to hand back behind
// it. The classifier does name this refusal, so removing the nil-SpawnInfo
// early return panics in cmdSpawn rather than passing quietly.
func TestSlingGiveBackReasonPreSpawn(t *testing.T) {
	t.Parallel()
	refusal := fmt.Errorf("%s gt-x: the worktree branch of seat gastown/polecats/toast could not be read after 3 attempts: %w",
		dispatch.SlingRefusalMarker, sling.ErrSeatBranchRefused)
	if _, giveBack := slingGiveBackReason(refusal); !giveBack {
		t.Fatal("the seat-branch refusal must be classified as a give-back, or this test pins nothing")
	}
	giveBackSlingRespawn(&SlingResult{}, refusal)
}

// TestSlingGiveBackReasonAnchored pins gt-uauwit: a message counts for one of
// the three phrases only where it has the shape the town prints, so an error
// that merely embeds the phrase keeps its respawn count. The sentinel, or the
// anchored shape, gives it back.
func TestSlingGiveBackReasonAnchored(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		err      error
		giveBack bool
	}{
		{
			name: "an assignee-lock phrase embedded in prose",
			err:  errors.New("the audit log says timed out acquiring assignee sling lock for gastown/polecats/toast after 10s (stale) and was not retried"),
		},
		{
			name: "a session phrase embedded in prose",
			err:  errors.New("reconcile: session already running: gt-cheedo was left behind by the last tick"),
		},
		{
			name: "a gateway phrase embedded in prose",
			err:  errors.New("the dispatcher logged gtwrap: the bd gateway at http://bd-gateway.gt.svc:7440 did not answer, so the call did not run: earlier"),
		},
		{
			name:     "an assignee-lock sentinel",
			err:      fmt.Errorf("serializing hook write: %w", assigneeLockTimeout{agent: "gastown/polecats/toast", seconds: 10}),
			giveBack: true,
		},
		{
			name:     "an assignee-lock timeout as the town prints it",
			err:      errors.New("serializing hook write for gastown/polecats/toast: timed out acquiring assignee sling lock after 10s (held by pid 10297)"),
			giveBack: true,
		},
		{
			name:     "a session already running as the town prints it",
			err:      errors.New("starting polecat session: session already running: gt-cheedo"),
			giveBack: true,
		},
		{
			name:     "a session already running with a healthy agent as the tmux layer prints it",
			err:      errors.New("starting polecat session: session already running with healthy agent"),
			giveBack: true,
		},
		{
			name:     "a session already running with no id after it",
			err:      errors.New("starting polecat session: session already running"),
			giveBack: true,
		},
		{
			name: "a session phrase that continues into other words",
			err:  errors.New("starting polecat session: session already running with a stale agent left over by the last tick"),
		},
		{
			name: "a session phrase with a word before it and no separator",
			err:  errors.New("no session already running"),
		},
		{
			name: "a session phrase after a colon with no space",
			err:  errors.New("x:session already running"),
		},
		{
			name: "a healthy agent followed by other words",
			err:  errors.New("starting polecat session: session already running with healthy agent and a dead bead"),
		},
		{
			name:     "a session already running alone as the whole message",
			err:      errors.New("session already running"),
			giveBack: true,
		},
		{
			name:     "a bd gateway that did not answer as the town prints it",
			err:      errors.New("gtwrap: the bd gateway at http://bd-gateway.gt.svc:7440 did not answer, so the call did not run: connection refused"),
			giveBack: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reason, giveBack := slingGiveBackReason(tc.err)
			if giveBack != tc.giveBack {
				t.Fatalf("giveBack = %v, want %v (reason %q)", giveBack, tc.giveBack, reason)
			}
		})
	}
}
