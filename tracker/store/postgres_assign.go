package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/steveyegge/gastown/tracker/api"
)

// The assignment verbs guard on the actor the request's token names, not on an
// assignee the caller states: an actor can only give up an issue it holds, and
// a claim loses to whoever holds it. Each reads the row FOR UPDATE inside its
// transaction, so the guard and the write are one step (gt-p85pw.68).

// assignActor is the caller an assignment guard compares against. A request
// with no identity has nothing to guard on, so it is refused.
func assignActor(ctx context.Context, verb string) (string, error) {
	actor := api.ActorFrom(ctx)
	if actor == "" {
		return "", api.Invalid("%s needs an actor; the request carries no identity", verb)
	}
	return actor, nil
}

// refuseClosed keeps a closed issue out of the assignment verbs: release and
// transfer would put it back in the live set, and a claim would reopen it.
func refuseClosed(id, status, verb string) error {
	if status == "closed" {
		return api.GuardFailed(fmt.Sprintf("%s is closed, so it cannot be %s", id, verb))
	}
	return nil
}

// Claim takes an unheld issue for the caller in one transaction.
func (p *Postgres) Claim(ctx context.Context, id string) error {
	actor, err := assignActor(ctx, "claim")
	if err != nil {
		return err
	}
	inProgress := "in_progress"
	return p.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := lockIssue(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := refuseClosed(id, cur.status, "claimed"); err != nil {
			return err
		}
		if cur.assignee != "" && cur.assignee != actor {
			return api.GuardFailed(fmt.Sprintf("%s is held by another actor", id))
		}
		if cur.assignee == actor && cur.status == inProgress {
			return nil // the row already says this, so the re-claim writes nothing
		}
		return updateInTx(ctx, tx, id, api.UpdatePatch{Status: &inProgress, Assignee: &actor, Actor: actor}, api.Guard{})
	})
}

// Release returns the caller's issue to open and unassigned.
func (p *Postgres) Release(ctx context.Context, id string) error {
	actor, err := assignActor(ctx, "release")
	if err != nil {
		return err
	}
	open, unassigned := "open", ""
	return p.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := requireHolder(ctx, tx, id, actor, "released"); err != nil {
			return err
		}
		return updateInTx(ctx, tx, id, api.UpdatePatch{Status: &open, Assignee: &unassigned, Actor: actor}, api.Guard{})
	})
}

// Transfer hands the caller's issue to another actor.
func (p *Postgres) Transfer(ctx context.Context, id, status, assignee string) error {
	actor, err := assignActor(ctx, "transfer")
	if err != nil {
		return err
	}
	if err := api.ValidateTransferAssignee(assignee); err != nil {
		return err
	}
	if err := api.ValidateTransferStatus(status); err != nil {
		return err
	}
	return p.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := requireHolder(ctx, tx, id, actor, "transferred")
		if err != nil {
			return err
		}
		if status == "" {
			status = cur.status
		}
		if status == cur.status && assignee == cur.assignee {
			return nil // the row already says this, so the no-op transfer writes nothing
		}
		return updateInTx(ctx, tx, id, api.UpdatePatch{Status: &status, Assignee: &assignee, Actor: actor}, api.Guard{})
	})
}

// requireHolder locks the issue and returns it, or refuses when it is closed or
// actor is not its assignee.
func requireHolder(ctx context.Context, tx pgx.Tx, id, actor, verb string) (current, error) {
	cur, err := lockIssue(ctx, tx, id)
	if err != nil {
		return cur, err
	}
	if err := refuseClosed(id, cur.status, verb); err != nil {
		return cur, err
	}
	if cur.assignee != actor {
		return cur, api.GuardFailed(fmt.Sprintf("%s is assigned to another actor", id))
	}
	return cur, nil
}
