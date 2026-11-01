// Package storetest is the contract suite for keystore.Store: the behavior every
// implementation promises, written once and run against each of them. The
// in-memory Fake and the Secret Manager adapter both run it, so a difference
// between them is a failing test.
//
// Nothing calls it outside tests.
package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/keystore"
)

// Run runs the contract against stores built by newStore. Each subtest gets a
// store of its own, so no subtest sees another's keys. workspace is the one
// workspace the store serves; an implementation bound to a single workspace
// passes its own.
func Run(t *testing.T, workspace string, newStore func(t *testing.T) keystore.Store) {
	t.Helper()
	ctx := context.Background()
	const name = "api-key"

	put := func(t *testing.T, s keystore.Store, value string) keystore.Revision {
		t.Helper()
		rev, err := s.Put(ctx, workspace, name, []byte(value))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		return rev
	}
	get := func(t *testing.T, s keystore.Store) string {
		t.Helper()
		secret, err := s.Get(ctx, workspace, name)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		return string(secret.Bytes())
	}

	t.Run("GetReturnsTheLatestRevision", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		first := put(t, s, "one")
		second := put(t, s, "two")
		if second <= first {
			t.Fatalf("second Put = revision %d, want above %d", second, first)
		}
		if got := get(t, s); got != "two" {
			t.Errorf("Get = %q, want the latest value", got)
		}
	})

	t.Run("DisabledRevisionFallsBack", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		put(t, s, "one")
		second := put(t, s, "two")
		if err := s.Disable(ctx, workspace, name, second); err != nil {
			t.Fatalf("Disable: %v", err)
		}
		if got := get(t, s); got != "one" {
			t.Errorf("Get after disabling the newest = %q, want the previous value", got)
		}
	})

	t.Run("UnknownKeyIsNotFound", func(t *testing.T) {
		t.Parallel()
		if _, err := newStore(t).Get(ctx, workspace, "never-written"); !errors.Is(err, keystore.ErrNotFound) {
			t.Errorf("Get of an unknown key = %v, want ErrNotFound", err)
		}
	})

	t.Run("AllRevisionsDisabledIsNotFound", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		only := put(t, s, "one")
		if err := s.Disable(ctx, workspace, name, only); err != nil {
			t.Fatalf("Disable: %v", err)
		}
		if _, err := s.Get(ctx, workspace, name); !errors.Is(err, keystore.ErrNotFound) {
			t.Errorf("Get with every revision disabled = %v, want ErrNotFound", err)
		}
	})

	t.Run("EmptyNameAndValueAreRefused", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		if _, err := s.Put(ctx, workspace, "", []byte("v")); !errors.Is(err, keystore.ErrInvalidKey) {
			t.Errorf("Put with an empty name = %v, want ErrInvalidKey", err)
		}
		if _, err := s.Put(ctx, "", name, []byte("v")); !errors.Is(err, keystore.ErrInvalidKey) {
			t.Errorf("Put with an empty workspace = %v, want ErrInvalidKey", err)
		}
		if _, err := s.Put(ctx, workspace, name, nil); !errors.Is(err, keystore.ErrEmptyValue) {
			t.Errorf("Put with an empty value = %v, want ErrEmptyValue", err)
		}
	})

	t.Run("UnknownRevisionIsRefused", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		rev := put(t, s, "one")
		if err := s.Disable(ctx, workspace, name, rev+5); !errors.Is(err, keystore.ErrRevisionNotFound) {
			t.Errorf("Disable of an unknown revision = %v, want ErrRevisionNotFound", err)
		}
		if err := s.Destroy(ctx, workspace, name, rev+5); !errors.Is(err, keystore.ErrRevisionNotFound) {
			t.Errorf("Destroy of an unknown revision = %v, want ErrRevisionNotFound", err)
		}
		if err := s.Destroy(ctx, workspace, name, rev); err != nil {
			t.Fatalf("Destroy: %v", err)
		}
		if err := s.Disable(ctx, workspace, name, rev); !errors.Is(err, keystore.ErrRevisionNotFound) {
			t.Errorf("Disable of a destroyed revision = %v, want ErrRevisionNotFound", err)
		}
	})

	t.Run("DestroyedNumberIsNotReused", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		first := put(t, s, "one")
		if err := s.Destroy(ctx, workspace, name, first); err != nil {
			t.Fatalf("Destroy: %v", err)
		}
		if second := put(t, s, "two"); second <= first {
			t.Errorf("Put after destroying %d = %d, want a higher number", first, second)
		}
	})

	t.Run("ValuesAreCopiedAtTheBoundary", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		value := []byte("original")
		if _, err := s.Put(ctx, workspace, name, value); err != nil {
			t.Fatalf("Put: %v", err)
		}
		value[0] = 'X'
		secret, err := s.Get(ctx, workspace, name)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		read := secret.Bytes()
		read[0] = 'Y'
		if got := get(t, s); got != "original" {
			t.Errorf("Get = %q after the caller rewrote its buffers, want %q", got, "original")
		}
	})

	t.Run("RevisionsListsSurvivorsAscending", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		first := put(t, s, "v1")
		second := put(t, s, "v2")
		third := put(t, s, "v3")
		if err := s.Disable(ctx, workspace, name, first); err != nil {
			t.Fatalf("Disable: %v", err)
		}
		if err := s.Destroy(ctx, workspace, name, second); err != nil {
			t.Fatalf("Destroy: %v", err)
		}
		got, err := s.Revisions(ctx, workspace, name)
		if err != nil {
			t.Fatalf("Revisions: %v", err)
		}
		want := []keystore.RevisionInfo{{Revision: first, Enabled: false}, {Revision: third, Enabled: true}}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("Revisions = %v, want %v", got, want)
		}
	})

	t.Run("RevisionsOfAnUnknownKeyIsEmpty", func(t *testing.T) {
		t.Parallel()
		got, err := newStore(t).Revisions(ctx, workspace, "never-written")
		if err != nil || len(got) != 0 {
			t.Errorf("Revisions of an unknown key = %v, %v; want empty and no error", got, err)
		}
	})

	t.Run("CancelledContextStopsEveryMethod", func(t *testing.T) {
		t.Parallel()
		s := newStore(t)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.Put(cctx, workspace, name, []byte("v")); !errors.Is(err, context.Canceled) {
			t.Errorf("Put = %v, want context.Canceled", err)
		}
		if _, err := s.Get(cctx, workspace, name); !errors.Is(err, context.Canceled) {
			t.Errorf("Get = %v, want context.Canceled", err)
		}
		if err := s.Disable(cctx, workspace, name, 1); !errors.Is(err, context.Canceled) {
			t.Errorf("Disable = %v, want context.Canceled", err)
		}
		if err := s.Destroy(cctx, workspace, name, 1); !errors.Is(err, context.Canceled) {
			t.Errorf("Destroy = %v, want context.Canceled", err)
		}
		if _, err := s.Revisions(cctx, workspace, name); !errors.Is(err, context.Canceled) {
			t.Errorf("Revisions = %v, want context.Canceled", err)
		}
	})
}
