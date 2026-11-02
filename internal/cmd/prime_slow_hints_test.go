package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/hmhints"
)

// gt-jamix: the polecat prime shows the town's prior records on slow tests
// when the hooked bead's text mentions one.

// primeSlowHintFake is a specHintSource that records every query and answers
// with hits or err, so no hm process starts.
type primeSlowHintFake struct {
	hits  []hmhints.Hit
	err   error
	calls []string
}

func (f *primeSlowHintFake) source(_ context.Context, text string, limit int) ([]hmhints.Hit, error) {
	f.calls = append(f.calls, fmt.Sprintf("%q limit %d", text, limit))
	return f.hits, f.err
}

// hookedSlowHintsOutput runs the hint for bead with hint as its source and
// returns what it printed.
func hookedSlowHintsOutput(ctx RoleContext, bead *beads.Issue, hint specHintSource) string {
	p, _, out := newFakePrimeTools(&fakePrimeRunner{})
	p.hint = hint
	p.hookedSlowHints(ctx, bead)
	return out.String()
}

// slowBead is a hooked bead whose text mentions a timed-out test, and plainBead
// one that does not.
var (
	slowBead  = &beads.Issue{ID: "gt-slow", Title: "Tests time out at 600s", Description: "the cmd tests hang"}
	plainBead = &beads.Issue{ID: "gt-plain", Title: "Rename a flag", Description: "no timing words here"}
)

// A polecat's triggering bead prints the heading and three hit lines, and the
// query is the bead's title plus the slow-test words, three hits (gt-jamix).
func TestPrimeSlowHintsPrintHeadingAndThreeLines(t *testing.T) {
	t.Parallel()
	fake := &primeSlowHintFake{hits: threeHintHits}
	out := hookedSlowHintsOutput(RoleContext{Role: RolePolecat}, slowBead, fake.source)

	want := hmSlowHintHeading + "\n" +
		"hint: hm #1 doc doc-a: first\n" +
		"hint: hm #2 bead gt-b: second\n" +
		"hint: hm #3 bead gt-c: third\n" +
		"\n"
	if out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("source called %d times, want 1", len(fake.calls))
	}
	wantCall := fmt.Sprintf("%q limit %d", slowBead.Title+" slow tests timeout", specHintLimit)
	if fake.calls[0] != wantCall {
		t.Errorf("query = %s, want %s", fake.calls[0], wantCall)
	}
}

// A bead whose text does not mention a slow or timed-out test never asks the
// source and prints nothing (gt-jamix).
func TestPrimeSlowHintsSilentWithoutSlowText(t *testing.T) {
	t.Parallel()
	fake := &primeSlowHintFake{hits: threeHintHits}
	out := hookedSlowHintsOutput(RoleContext{Role: RolePolecat}, plainBead, fake.source)
	if len(fake.calls) != 0 {
		t.Fatalf("source called %d times for a plain bead: %q", len(fake.calls), fake.calls)
	}
	if out != "" {
		t.Errorf("output = %q, want empty", out)
	}
}

// A source that fails (unavailable, timed out, bad output) prints nothing; the
// hint is advice and never blocks the start (gt-jamix).
func TestPrimeSlowHintsSourceFailureIsSilent(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		err  error
	}{
		{"error", errors.New("hm query: exit status 1")},
		{"unavailable", fmt.Errorf("wrapped: %w", hmhints.ErrUnavailable)},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			// Hits with an error are a source contract violation; they must
			// still print nothing.
			fake := &primeSlowHintFake{hits: threeHintHits, err: row.err}
			out := hookedSlowHintsOutput(RoleContext{Role: RolePolecat}, slowBead, fake.source)
			if out != "" {
				t.Errorf("a failed hint printed %q", out)
			}
			if len(fake.calls) != 1 {
				t.Errorf("source called %d times, want 1", len(fake.calls))
			}
		})
	}
}

// A role that is not a polecat prints nothing and never asks the source, even
// for a triggering bead. Printing for every role makes this test red (gt-jamix).
func TestPrimeSlowHintsOnlyForPolecat(t *testing.T) {
	t.Parallel()
	for _, r := range []Role{RoleCrew, RoleUnknown} {
		r := r
		t.Run(string(r), func(t *testing.T) {
			t.Parallel()
			fake := &primeSlowHintFake{hits: threeHintHits}
			out := hookedSlowHintsOutput(RoleContext{Role: r}, slowBead, fake.source)
			if len(fake.calls) != 0 {
				t.Fatalf("%s asked the source %d times: %q", r, len(fake.calls), fake.calls)
			}
			if out != "" {
				t.Errorf("%s printed %q", r, out)
			}
		})
	}
}

// A source that answers with no hits prints nothing, not even the heading.
func TestPrimeSlowHintsEmptyHitsPrintNothing(t *testing.T) {
	t.Parallel()
	fake := &primeSlowHintFake{}
	out := hookedSlowHintsOutput(RoleContext{Role: RolePolecat}, slowBead, fake.source)
	if out != "" {
		t.Errorf("an empty hint printed %q", out)
	}
}

// A prime that may start no subprocess (dry-run, continuation) asks no source
// and prints nothing (gt-jamix).
func TestPrimeSlowHintsSkipped(t *testing.T) {
	t.Parallel()
	fake := &primeSlowHintFake{hits: threeHintHits}
	p, _, out := newFakePrimeTools(&fakePrimeRunner{})
	p.skipHints = true
	p.hint = fake.source
	p.hookedSlowHints(RoleContext{Role: RolePolecat}, slowBead)
	if len(fake.calls) != 0 {
		t.Fatalf("a skipped hint asked the source %d times", len(fake.calls))
	}
	if out.Len() != 0 {
		t.Errorf("a skipped hint printed %q", out.String())
	}
}

// gt-tdfbk: checkSlungWorkIn sets skipHints from continuation, so a fresh
// polecat prime asks the hint source for a triggering bead and a continuation
// prime never does. This drives the call site, not hookedSlowHints directly:
// dropping the skipHints wiring makes the continuation row ask the source,
// and dropping the hint parameter makes the fresh row miss the fake.
func TestSlungWorkWiresTheSlowHint(t *testing.T) {
	t.Parallel()
	ctx := RoleContext{Role: RolePolecat, Rig: "myrig", Polecat: "nux", TownRoot: t.TempDir()}
	rows := []struct {
		name         string
		continuation bool
		wantCalls    int
	}{
		{"fresh prime asks the source", false, 1},
		{"continuation prime skips it", true, 0},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			fake := &primeSlowHintFake{hits: threeHintHits}
			var out bytes.Buffer
			if _, err := checkSlungWorkIn(&out, row.continuation, ctx, slowBead, fake.source); err != nil {
				t.Fatalf("checkSlungWorkWithHint: %v", err)
			}
			if len(fake.calls) != row.wantCalls {
				t.Fatalf("source calls = %d, want %d: %q", len(fake.calls), row.wantCalls, fake.calls)
			}
			printed := strings.Contains(out.String(), hmSlowHintHeading)
			if want := row.wantCalls > 0; printed != want {
				t.Errorf("printed hint heading = %v, want %v", printed, want)
			}
		})
	}
}

// A nil hooked bead prints nothing and asks no source.
func TestPrimeSlowHintsNoBead(t *testing.T) {
	t.Parallel()
	fake := &primeSlowHintFake{hits: threeHintHits}
	p, _, out := newFakePrimeTools(&fakePrimeRunner{})
	p.hint = fake.source
	p.hookedSlowHints(RoleContext{Role: RolePolecat}, nil)
	if len(fake.calls) != 0 {
		t.Errorf("no bead asked the source %d times", len(fake.calls))
	}
	if out.Len() != 0 {
		t.Errorf("no bead printed %q", out.String())
	}
}
