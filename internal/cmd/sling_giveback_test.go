package cmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
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
