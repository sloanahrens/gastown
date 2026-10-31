package drivertoken

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// ctx is the context every call takes. Nothing cancels it: no store here has
// work to abandon.
var ctx = context.Background()

// fixedTime is the instant every clock in these tests reads, set away from the
// epoch so a Record stamped with the zero time is a visible bug.
var fixedTime = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// fixedClock returns fixedTime, whatever the moment.
func fixedClock() time.Time { return fixedTime }

// newDeps returns Deps over store, reading reproducible bytes.
func newDeps(store Store) Deps {
	return Deps{Store: store, Rand: rand.New(rand.NewSource(1)), Clock: fixedClock}
}

// allow is a membership check that lets every issue through.
func allow(context.Context, string) error { return nil }

// TestTokenVerifiesOnlyForItsWorkspace is the acceptance test for the scope: a
// token issued for one workspace names that workspace, and the token issued for
// the other names only the other.
func TestTokenVerifiesOnlyForItsWorkspace(t *testing.T) {
	t.Parallel()
	d := newDeps(NewMemory())

	first, _, err := Issue(ctx, d, "ws-1", allow)
	if err != nil {
		t.Fatalf("Issue(ws-1): %v", err)
	}
	second, _, err := Issue(ctx, d, "ws-2", allow)
	if err != nil {
		t.Fatalf("Issue(ws-2): %v", err)
	}
	if first == second {
		t.Fatalf("two issues returned one token, so one workspace's token would open the other")
	}

	cases := []struct {
		name  string
		token string
		want  string
	}{
		{"issued for ws-1", first, "ws-1"},
		{"issued for ws-2", second, "ws-2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := Verify(ctx, d, c.token)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if got != c.want {
				t.Errorf("Verify = %q, want %q", got, c.want)
			}
		})
	}
}

// TestRevokedTokenRefused is the acceptance test for revocation: the revoked
// token names no workspace, and a token issued to the same workspace still does.
func TestRevokedTokenRefused(t *testing.T) {
	t.Parallel()
	d := newDeps(NewMemory())

	token, rec, err := Issue(ctx, d, "ws-1", allow)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	neighbour, _, err := Issue(ctx, d, "ws-1", allow)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if err := Revoke(ctx, d, "ws-1", rec.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if ws, err := Verify(ctx, d, token); !errors.Is(err, ErrRevokedToken) {
		t.Errorf("Verify(revoked) = %q, %v, want %v", ws, err, ErrRevokedToken)
	}
	if ws, err := Verify(ctx, d, neighbour); err != nil || ws != "ws-1" {
		t.Errorf("Verify(neighbour) = %q, %v, want ws-1 and no error", ws, err)
	}
}

// TestStoreHoldsNoPlainToken is the acceptance test for the store's contents:
// neither a stored record nor an error names the token it was issued as.
func TestStoreHoldsNoPlainToken(t *testing.T) {
	t.Parallel()
	store := NewMemory()
	d := newDeps(store)

	token, rec, err := Issue(ctx, d, "ws-1", allow)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if len(rec.Hash) == 0 {
		t.Fatalf("the returned record carries no hash")
	}
	if len(store.records) != 1 {
		t.Fatalf("stored %d records, want 1", len(store.records))
	}
	// %q renders a []byte as the string its bytes spell, so a hash that held the
	// token's own bytes would show up in plainText.
	plainText := func(r Record) string {
		return fmt.Sprintf("%q %q %q", r.ID, r.Workspace, r.Hash)
	}
	for _, stored := range store.records {
		if stored.ID == token {
			t.Errorf("the stored id is the token itself")
		}
		if got := plainText(stored); strings.Contains(got, token) {
			t.Errorf("the stored record holds the token: %s", got)
		}
	}
	if got := plainText(rec); strings.Contains(got, token) {
		t.Errorf("the returned record holds the token: %s", got)
	}

	_, err = Verify(ctx, d, "not-a-token")
	if err == nil {
		t.Fatalf("Verify of an unknown token returned no error")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("the refusal names the token: %v", err)
	}
}

// TestIssueRefusesAnUnwiredDeps drives every Deps field Issue cannot work
// without, so a missing one refuses instead of issuing an unscoped token.
func TestIssueRefusesAnUnwiredDeps(t *testing.T) {
	t.Parallel()
	good := newDeps(NewMemory())
	cases := []struct {
		name     string
		deps     Deps
		mayIssue func(context.Context, string) error
		want     error
	}{
		{"no store", Deps{Rand: good.Rand, Clock: fixedClock}, allow, ErrNoStore},
		{"no reader", Deps{Store: NewMemory(), Clock: fixedClock}, allow, ErrNoRandom},
		{"no clock", Deps{Store: NewMemory(), Rand: good.Rand}, allow, ErrNoClock},
		{"no membership check", good, nil, ErrNoMayIssue},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			token, rec, err := Issue(ctx, c.deps, "ws-1", c.mayIssue)
			if !errors.Is(err, c.want) {
				t.Fatalf("Issue error = %v, want %v", err, c.want)
			}
			if token != "" || rec.ID != "" {
				t.Errorf("Issue returned token %q and record %+v despite the refusal", token, rec)
			}
		})
	}
}

// TestIssueRefusesAnEmptyWorkspace keeps a token from being scoped to nothing.
func TestIssueRefusesAnEmptyWorkspace(t *testing.T) {
	t.Parallel()
	token, _, err := Issue(ctx, newDeps(NewMemory()), "", allow)
	if !errors.Is(err, ErrEmptyWorkspace) {
		t.Fatalf("Issue error = %v, want %v", err, ErrEmptyWorkspace)
	}
	if token != "" {
		t.Errorf("Issue returned a token %q for no workspace", token)
	}
}

// TestIssueReturnsTheMembershipRefusalUnchanged keeps the caller's typed refusal
// intact, and stores nothing.
func TestIssueReturnsTheMembershipRefusalUnchanged(t *testing.T) {
	t.Parallel()
	store := NewMemory()
	refusal := errors.New("membership: not an owner")

	token, _, err := Issue(ctx, newDeps(store), "ws-1", func(context.Context, string) error {
		return refusal
	})
	if err != refusal {
		t.Errorf("Issue error = %v, want the refusal unchanged", err)
	}
	if token != "" {
		t.Errorf("Issue returned a token %q despite the refusal", token)
	}
	if len(store.records) != 0 {
		t.Errorf("stored %d records despite the refusal", len(store.records))
	}
}

// TestIssueRefusesAShortRandomRead keeps a reader that runs dry from producing a
// token built from padding.
func TestIssueRefusesAShortRandomRead(t *testing.T) {
	t.Parallel()
	d := Deps{Store: NewMemory(), Rand: strings.NewReader(""), Clock: fixedClock}
	token, _, err := Issue(ctx, d, "ws-1", allow)
	if err == nil {
		t.Fatalf("Issue succeeded from an empty random reader")
	}
	if token != "" {
		t.Errorf("Issue returned a token %q from a short read", token)
	}
}

// TestIssueRefusesAShortIDRead drives the second read: a reader holding exactly
// one token's bytes cannot also name the record.
func TestIssueRefusesAShortIDRead(t *testing.T) {
	t.Parallel()
	d := Deps{Store: NewMemory(), Rand: bytes.NewReader(make([]byte, tokenBytes+idBytes-1)), Clock: fixedClock}
	token, _, err := Issue(ctx, d, "ws-1", allow)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Issue error = %v, want %v", err, io.ErrUnexpectedEOF)
	}
	if token != "" {
		t.Errorf("Issue returned a token %q from a short read", token)
	}
}

// errStoreFull is what refusingStore returns, so the test names the failure it
// drives rather than only that one happened.
var errStoreFull = errors.New("drivertoken test: store is full")

// refusingStore refuses every Put and answers the rest as an empty Memory does.
type refusingStore struct{ *Memory }

// Put refuses the record.
func (refusingStore) Put(context.Context, Record) error { return errStoreFull }

// TestIssueReportsAStoreThatRefuses keeps a refused write from returning a token
// the store never held.
func TestIssueReportsAStoreThatRefuses(t *testing.T) {
	t.Parallel()
	d := Deps{Store: refusingStore{Memory: NewMemory()}, Rand: rand.New(rand.NewSource(1)), Clock: fixedClock}
	token, rec, err := Issue(ctx, d, "ws-1", allow)
	if !errors.Is(err, errStoreFull) {
		t.Fatalf("Issue error = %v, want %v", err, errStoreFull)
	}
	if token != "" || rec.ID != "" {
		t.Errorf("Issue returned token %q and record %+v despite the refused write", token, rec)
	}
}

// TestVerifyRefusesAnUnknownToken keeps a token the store never issued from
// naming a workspace.
func TestVerifyRefusesAnUnknownToken(t *testing.T) {
	t.Parallel()
	d := newDeps(NewMemory())
	for _, token := range []string{"", "not-a-token"} {
		if ws, err := Verify(ctx, d, token); !errors.Is(err, ErrUnknownToken) {
			t.Errorf("Verify(%q) = %q, %v, want %v", token, ws, err, ErrUnknownToken)
		}
	}
}

// TestVerifyRefusesAnExpiredToken reads ExpiresAt against the injected clock: a
// token expires at its ExpiresAt instant, not after it.
func TestVerifyRefusesAnExpiredToken(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		expiresAt time.Time
		wantErr   error
	}{
		{"before the expiry", fixedTime.Add(time.Hour), nil},
		{"at the expiry instant", fixedTime, ErrExpiredToken},
		{"after the expiry", fixedTime.Add(-time.Hour), ErrExpiredToken},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			store := NewMemory()
			d := newDeps(store)
			token, rec, err := Issue(ctx, d, "ws-1", allow)
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}
			rec.ExpiresAt = c.expiresAt
			store.records = []Record{rec}

			ws, err := Verify(ctx, d, token)
			if !errors.Is(err, c.wantErr) {
				t.Fatalf("Verify error = %v, want %v", err, c.wantErr)
			}
			if c.wantErr == nil && ws != "ws-1" {
				t.Errorf("Verify = %q, want ws-1", ws)
			}
		})
	}
}

// TestVerifyRefusesAnUnwiredDeps drives the Deps fields Verify cannot work
// without.
func TestVerifyRefusesAnUnwiredDeps(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		deps Deps
		want error
	}{
		"no store": {Deps{Clock: fixedClock}, ErrNoStore},
		"no clock": {Deps{Store: NewMemory()}, ErrNoClock},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Verify(ctx, c.deps, "token"); !errors.Is(err, c.want) {
				t.Errorf("Verify error = %v, want %v", err, c.want)
			}
		})
	}
}

// TestRevokeRefusesAnotherWorkspacesToken keeps an id from revoking a token in a
// workspace the caller did not name.
func TestRevokeRefusesAnotherWorkspacesToken(t *testing.T) {
	t.Parallel()
	d := newDeps(NewMemory())
	token, rec, err := Issue(ctx, d, "ws-1", allow)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if err := Revoke(ctx, d, "ws-2", rec.ID); !errors.Is(err, ErrNoRecord) {
		t.Errorf("Revoke under ws-2 error = %v, want %v", err, ErrNoRecord)
	}
	if ws, err := Verify(ctx, d, token); err != nil || ws != "ws-1" {
		t.Errorf("Verify = %q, %v, want ws-1 unchanged", ws, err)
	}
}

// TestRevokeIsIdempotent keeps a retried revoke from failing the second time.
func TestRevokeIsIdempotent(t *testing.T) {
	t.Parallel()
	d := newDeps(NewMemory())
	_, rec, err := Issue(ctx, d, "ws-1", allow)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if err := Revoke(ctx, d, "ws-1", rec.ID); err != nil {
			t.Fatalf("Revoke attempt %d: %v", attempt, err)
		}
	}
}

// TestRevokeRefusesAnUnwiredDeps keeps a revoke with no store from reporting a
// success it did not have.
func TestRevokeRefusesAnUnwiredDeps(t *testing.T) {
	t.Parallel()
	if err := Revoke(ctx, Deps{Clock: fixedClock}, "ws-1", "id-1"); !errors.Is(err, ErrNoStore) {
		t.Errorf("Revoke error = %v, want %v", err, ErrNoStore)
	}
}
