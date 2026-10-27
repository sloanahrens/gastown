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
		if cur.assignee != "" && cur.assignee != actor {
			return api.GuardFailed(fmt.Sprintf("%s is held by %q, not by %q", id, cur.assignee, actor))
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
		if _, err := requireHolder(ctx, tx, id, actor); err != nil {
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
	if assignee == "" {
		return api.Invalid("transfer needs the assignee taking the issue; release leaves it unheld")
	}
	return p.inTx(ctx, func(tx pgx.Tx) error {
		cur, err := requireHolder(ctx, tx, id, actor)
		if err != nil {
			return err
		}
		if status == "" {
			status = cur.status
		}
		return updateInTx(ctx, tx, id, api.UpdatePatch{Status: &status, Assignee: &assignee, Actor: actor}, api.Guard{})
	})
}

// requireHolder locks the issue and returns it, or refuses when actor is not
// its assignee.
func requireHolder(ctx context.Context, tx pgx.Tx, id, actor string) (current, error) {
	cur, err := lockIssue(ctx, tx, id)
	if err != nil {
		return cur, err
	}
	if cur.assignee != actor {
		return cur, api.GuardFailed(fmt.Sprintf("%s is assigned to %q, not to %q", id, cur.assignee, actor))
	}
	return cur, nil
}
