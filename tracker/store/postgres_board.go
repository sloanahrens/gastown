package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/steveyegge/gastown/tracker/api"
)

// DispatchBoard reads the dispatch tick's board in one repeatable-read,
// read-only transaction, so an issue is never ready in one part of the answer
// and held in another. Per requested rig it returns the ready, unassigned
// issues (every page, no limit), the held issues and the children of the ready
// ones; a rig the tracker does not know goes to Errors and the rest are still
// answered.
func (p *Postgres) DispatchBoard(ctx context.Context, req api.DispatchBoardRequest) (api.DispatchBoardResponse, error) {
	rigs := uniqueStrings(req.Rigs)
	if len(rigs) == 0 {
		return api.DispatchBoardResponse{}, api.Invalid("at least one rig is required")
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return api.DispatchBoardResponse{}, fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	resp := api.DispatchBoardResponse{Rigs: map[string]api.RigBoard{}, Probes: map[string]api.Probe{}, Errors: map[string]string{}}
	known, err := knownRigs(ctx, tx, rigs)
	if err != nil {
		return api.DispatchBoardResponse{}, err
	}
	for _, rig := range rigs {
		if !known[rig] {
			resp.Errors[rig] = "unknown rig"
			continue
		}
		board, err := rigBoard(ctx, tx, rig, req)
		if err != nil {
			return api.DispatchBoardResponse{}, err
		}
		resp.Rigs[rig] = board
	}
	if err := probe(ctx, tx, uniqueStrings(req.ProbeIDs), resp.Probes); err != nil {
		return api.DispatchBoardResponse{}, err
	}
	return resp, nil
}

func knownRigs(ctx context.Context, q querier, rigs []string) (map[string]bool, error) {
	rows, err := q.Query(ctx, "SELECT prefix FROM tracker.rigs WHERE prefix = ANY($1)", rigs)
	if err != nil {
		return nil, fmt.Errorf("store: board rigs: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, fmt.Errorf("store: board rigs: %w", err)
		}
		out[r] = true
	}
	return out, rows.Err()
}

func rigBoard(ctx context.Context, q querier, rig string, req api.DispatchBoardRequest) (api.RigBoard, error) {
	board := api.RigBoard{Ready: []api.Issue{}, Held: []api.HeldBead{}, Children: map[string][]api.ChildStatus{}}
	f := api.ReadyFilter{Rig: rig, Unassigned: true, ExcludeLabels: req.ExcludeLabels, ExcludeTypes: req.ExcludeTypes, Limit: maxPageSize}
	for {
		page, err := readyPage(ctx, q, f, false)
		if err != nil {
			return api.RigBoard{}, err
		}
		board.Ready = append(board.Ready, page.Issues...)
		if page.NextCursor == "" {
			break
		}
		f.Cursor = page.NextCursor
	}

	if len(req.HeldStatuses) > 0 {
		rows, err := q.Query(ctx, "SELECT id, issue_type, assignee, description FROM tracker.issues WHERE rig = $1 AND status = ANY($2) AND assignee <> '' ORDER BY id", rig, req.HeldStatuses)
		if err != nil {
			return api.RigBoard{}, fmt.Errorf("store: board held: %w", err)
		}
		for rows.Next() {
			var h api.HeldBead
			if err := rows.Scan(&h.ID, &h.Type, &h.Assignee, &h.Description); err != nil {
				rows.Close()
				return api.RigBoard{}, fmt.Errorf("store: board held: %w", err)
			}
			board.Held = append(board.Held, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return api.RigBoard{}, fmt.Errorf("store: board held: %w", err)
		}
	}

	if len(board.Ready) > 0 {
		ids := make([]string, len(board.Ready))
		for i, is := range board.Ready {
			ids[i] = is.ID
		}
		rows, err := q.Query(ctx, "SELECT parent_id, id, status FROM tracker.issues WHERE parent_id = ANY($1) ORDER BY created_at, id", ids)
		if err != nil {
			return api.RigBoard{}, fmt.Errorf("store: board children: %w", err)
		}
		for rows.Next() {
			var parent string
			var c api.ChildStatus
			if err := rows.Scan(&parent, &c.ID, &c.Status); err != nil {
				rows.Close()
				return api.RigBoard{}, fmt.Errorf("store: board children: %w", err)
			}
			board.Children[parent] = append(board.Children[parent], c)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return api.RigBoard{}, fmt.Errorf("store: board children: %w", err)
		}
	}
	return board, nil
}

// probe fills out with the status of each of ids; one that does not exist is
// Found=false.
func probe(ctx context.Context, q querier, ids []string, out map[string]api.Probe) error {
	if len(ids) == 0 {
		return nil
	}
	for _, id := range ids {
		out[id] = api.Probe{}
	}
	rows, err := q.Query(ctx, "SELECT id, status FROM tracker.issues WHERE id = ANY($1)", ids)
	if err != nil {
		return fmt.Errorf("store: board probes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			return fmt.Errorf("store: board probes: %w", err)
		}
		out[id] = api.Probe{Found: true, Status: status}
	}
	return rows.Err()
}
