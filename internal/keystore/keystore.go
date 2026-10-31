// Package keystore defines the key-store contract for per-workspace customer
// keys and an in-memory Fake to implement it.
//
// The production Store is the Secret Manager adapter, a separate bead that
// carries the cloud SDK and its review; nothing here makes a cloud call, so the
// custody logic around keys runs in tests against Fake.
//
// No code calls this package yet. Its callers are the pre-dispatch credential
// check (gt-thrtt, plan K3), key rotation (gt-fn9nz, plan K5) and the gateway
// that loads a workspace's key.
package keystore

import (
	"context"
	"errors"
	"fmt"
)

// Revision identifies one version of a key. Numbers rise within a key and are
// never reused, so the number of a destroyed revision stays spent.
type Revision uint64

// Store holds the revisions of each workspace's key under a name. A revision is
// enabled from the Put that wrote it until a Disable or a Destroy touches it, and
// Get reads only enabled revisions.
//
// Every method is safe for concurrent use.
type Store interface {
	// Put stores value as the next revision of workspace/name and returns its
	// number; the new revision is the highest, so it is what Get returns next.
	Put(ctx context.Context, workspace, name string, value []byte) (Revision, error)

	// Get returns the highest enabled revision of workspace/name, so a disabled
	// newest revision falls back to the one below it.
	Get(ctx context.Context, workspace, name string) (Secret, error)

	// Disable stops new reads of revision rev: Get skips it from now on. A seat
	// already holding the value keeps it; nothing can revoke that.
	Disable(ctx context.Context, workspace, name string, rev Revision) error

	// Destroy deletes revision rev, so no read returns it again.
	Destroy(ctx context.Context, workspace, name string, rev Revision) error

	// Revisions lists the revisions of workspace/name that Destroy has not
	// deleted, lowest number first, each with whether Get may still read it. A
	// key with no revision yields an empty list and no error. The state it
	// reports is the store's own, so a process that did not write a revision, or
	// one restarted since, reads the same answer.
	Revisions(ctx context.Context, workspace, name string) ([]RevisionInfo, error)
}

// RevisionInfo is one surviving revision of a key and whether Get may read it.
type RevisionInfo struct {
	Revision Revision
	Enabled  bool
}

var (
	// ErrNotFound reports a workspace/name with no enabled revision to read.
	ErrNotFound = errors.New("keystore: no enabled revision")

	// ErrRevisionNotFound reports a Disable or Destroy for a revision Put never
	// wrote, or one Destroy already deleted.
	ErrRevisionNotFound = errors.New("keystore: no such revision")

	// ErrInvalidKey reports an empty workspace or key name.
	ErrInvalidKey = errors.New("keystore: empty workspace or key name")

	// ErrEmptyValue reports a Put whose value is empty.
	ErrEmptyValue = errors.New("keystore: empty value")
)

// checkKey refuses a canceled context and a key that cannot be addressed. No
// error names a value: a key's value never travels through an error.
func checkKey(ctx context.Context, workspace, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	switch {
	case workspace == "":
		return fmt.Errorf("%w: workspace is empty", ErrInvalidKey)
	case name == "":
		return fmt.Errorf("%w: key name is empty", ErrInvalidKey)
	}
	return nil
}
