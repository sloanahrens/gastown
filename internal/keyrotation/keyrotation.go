// Package keyrotation rotates a workspace's key over the key-store interface:
// Rotate writes a new revision, checks it, and either keeps the old key or
// retires every older revision; SeatEnded destroys a retired revision once no
// seat started on it is still running.
//
// Nothing calls this package yet: its callers are the owner console (plan row
// O5) and the seat-end path (a later wiring bead).
//
// All the state that decides what is safe to destroy lives in the store. A
// revision is retired when the store reports it disabled (keystore.Store
// Revisions), so SeatEnded works from any process and after any restart: the
// process that rotated and the process that ends the last seat need share
// nothing.
//
// Two obligations rest on the callers, because the store cannot enforce them:
//
//   - The check window. Put makes the new revision the one Get returns before
//     the credential check has run, so a seat that starts between Put and the
//     end of Rotate can read an unchecked key. The store cannot stage a write;
//     closing the window needs a staged write in the store (a separate bead).
//     Until then nothing in this package keeps an unchecked key from being
//     read, and a seat that starts during a rotation may get it.
//   - Counting before reading. A seat start must register with the SeatCounter
//     before it reads the key, and SeatEnded must be called after the ending
//     seat has left the count. Otherwise SeatEnded can count zero while a seat
//     that read the old revision is not yet counted, and destroy a key that
//     seat still holds.
package keyrotation

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/keystore"
)

// rollbackTimeout bounds the Disable that undoes a rotation whose caller has
// gone away: the new revision must not stay readable because the context ended.
const rollbackTimeout = 10 * time.Second

var (
	// ErrSeatsRunning reports that SeatEnded left a retired revision alone
	// because seats started on it are still running. It is not a failure: the
	// last seat's SeatEnded destroys the revision.
	ErrSeatsRunning = errors.New("keyrotation: seats still running on the revision")

	// ErrRevisionLive reports that SeatEnded refused a revision the store still
	// serves to new seats. Destroying it would delete the key new seats read.
	ErrRevisionLive = errors.New("keyrotation: revision is still enabled")
)

// Checker asks whether a revision's key works, for example by verifying it with
// the model provider. An error means the key is not accepted.
type Checker interface {
	Check(ctx context.Context, workspace, name string, rev keystore.Revision) error
}

// SeatCounter reports how many seats started on a revision are still running.
type SeatCounter interface {
	Running(ctx context.Context, workspace, name string, rev keystore.Revision) (int, error)
}

// Outcome is what Rotate did.
type Outcome struct {
	// Rotated is true when the new revision passed its check and is the key new
	// seats read.
	Rotated bool

	// Revision is the revision Rotate wrote, set whenever a Put succeeded, also
	// when Rotated is false (so the caller can find it).
	Revision keystore.Revision

	// OwnerNotice tells the owner what happened when Rotated is false. It names
	// the type of the checker's error and never its text, because that text may
	// carry the key.
	OwnerNotice string
}

// Rotate puts value as a new revision of workspace/name and checks it. When the
// check fails it disables the new revision, so the old key stays the one new
// seats read, and returns Rotated false with an OwnerNotice and a nil error: a
// key the provider refuses is a normal outcome, not a fault. When the check
// passes it disables every older enabled revision for new seats; the older
// revisions are not destroyed, because seats started on them are still running
// (SeatEnded destroys them later).
//
// A non-nil error means a store call or the context failed. Then Outcome still
// says what is known: after a passed check whose retirement failed, Rotated is
// true and Revision is live, and the caller should retry the retirement by
// calling Rotate again with the same value or retiring by hand; after a failed
// rollback, Rotated is false, Revision is the unchecked revision that may still
// be readable, and the error says so.
func Rotate(ctx context.Context, store keystore.Store, check Checker, workspace, name string, value []byte) (Outcome, error) {
	rev, err := store.Put(ctx, workspace, name, value)
	if err != nil {
		return Outcome{}, fmt.Errorf("keyrotation: write new revision: %w", err)
	}

	if checkErr := check.Check(ctx, workspace, name, rev); checkErr != nil {
		out := Outcome{Revision: rev, OwnerNotice: refusalNotice(rev, checkErr)}
		rb, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()
		if err := store.Disable(rb, workspace, name, rev); err != nil {
			return out, fmt.Errorf("keyrotation: revision %d failed its check and could not be disabled, so new seats may still read it: %w", rev, err)
		}
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		return out, nil
	}

	out := Outcome{Rotated: true, Revision: rev}
	infos, err := store.Revisions(ctx, workspace, name)
	if err != nil {
		return out, fmt.Errorf("keyrotation: revision %d passed its check but the older revisions could not be listed: %w", rev, err)
	}
	var errs []error
	for _, info := range infos {
		if info.Revision < rev && info.Enabled {
			if err := store.Disable(ctx, workspace, name, info.Revision); err != nil {
				errs = append(errs, fmt.Errorf("disable revision %d: %w", info.Revision, err))
			}
		}
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("keyrotation: revision %d passed its check and is live, but older revisions stay enabled: %w", rev, errors.Join(errs...))
	}
	return out, nil
}

// refusalNotice is the owner-facing text for a key that failed its check. It
// carries the error's type, which tells a refusal from a network fault, and
// never the error's text.
func refusalNotice(rev keystore.Revision, err error) string {
	return fmt.Sprintf("the new key (revision %d) did not pass its check (%T), so the previous key stays in use; replace the new key and try again", rev, err)
}

// SeatEnded destroys revision rev of workspace/name when the store reports it
// disabled and no seat started on it is still running. Call it when a seat that
// started on rev ends, after that seat has left the SeatCounter's count.
//
// It returns nil when the revision is destroyed or is already gone (so a second
// call is safe), ErrRevisionLive when the store still serves rev to new seats,
// and ErrSeatsRunning when seats remain, in which case the next seat to end
// calls it again. Other errors are store or counter failures.
func SeatEnded(ctx context.Context, store keystore.Store, seats SeatCounter, workspace, name string, rev keystore.Revision) error {
	infos, err := store.Revisions(ctx, workspace, name)
	if err != nil {
		return fmt.Errorf("keyrotation: list revisions: %w", err)
	}
	var found *keystore.RevisionInfo
	for i := range infos {
		if infos[i].Revision == rev {
			found = &infos[i]
		}
	}
	switch {
	case found == nil:
		return nil
	case found.Enabled:
		return fmt.Errorf("%w: revision %d", ErrRevisionLive, rev)
	}
	running, err := seats.Running(ctx, workspace, name, rev)
	if err != nil {
		return fmt.Errorf("keyrotation: count seats on revision %d: %w", rev, err)
	}
	if running > 0 {
		return fmt.Errorf("%w: %d on revision %d", ErrSeatsRunning, running, rev)
	}
	if err := store.Destroy(ctx, workspace, name, rev); err != nil && !errors.Is(err, keystore.ErrRevisionNotFound) {
		return fmt.Errorf("keyrotation: destroy revision %d: %w", rev, err)
	}
	return nil
}
