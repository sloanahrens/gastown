package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/steveyegge/gastown/tracker/api"
)

// CreateGraph creates the nodes of req and the links between them in one
// transaction. See Store.CreateGraph.
func (p *Postgres) CreateGraph(ctx context.Context, req api.GraphRequest) (api.GraphResult, error) {
	if err := validateGraph(req); err != nil {
		return api.GraphResult{}, err
	}
	actor := api.ActorFrom(ctx)
	var last error
	for i := 0; i < idAttempts; i++ {
		var res api.GraphResult
		last = p.inTx(ctx, func(tx pgx.Tx) error {
			var err error
			res, err = createGraphInTx(ctx, tx, req, actor)
			return err
		})
		if last == nil {
			return res, nil
		}
		if !isIDTaken(last) {
			return api.GraphResult{}, last
		}
		// A generated id collided; a fresh set of ids is drawn on the retry.
	}
	return api.GraphResult{}, last
}

// isIDTaken is true for the conflict createInTx returns for a taken id, not
// for any other conflict (a cycle will not go away on a retry).
func isIDTaken(err error) bool {
	ae, ok := err.(*api.Error)
	return ok && ae.Kind == api.KindConflict && strings.HasSuffix(ae.Message, "already exists")
}

func validateGraph(req api.GraphRequest) error {
	if req.Rig == "" {
		return api.Invalid("a graph needs a rig")
	}
	if len(req.Nodes) == 0 {
		return api.Invalid("a graph needs at least one node")
	}
	if len(req.Nodes) > api.MaxGraphNodes || len(req.Edges) > api.MaxGraphEdges {
		return api.Invalid("a graph holds at most %d nodes and %d edges, got %d and %d", api.MaxGraphNodes, api.MaxGraphEdges, len(req.Nodes), len(req.Edges))
	}
	keys := map[string]bool{}
	for i, n := range req.Nodes {
		switch {
		case n.Key == "":
			return api.Invalid("node %d has no key", i)
		case keys[n.Key]:
			return api.Invalid("node key %q is used twice", n.Key)
		case strings.TrimSpace(n.Title) == "":
			return api.Invalid("node %q has no title", n.Key)
		case n.ID != "" || n.Parent != "" || n.Rig != "" || n.Actor != "":
			return api.Invalid("node %q sets id, parent, rig or actor: the server mints ids, edges set parents, the request names the rig and the token the actor", n.Key)
		}
		keys[n.Key] = true
	}
	for i, e := range req.Edges {
		if e.From == "" || e.To == "" {
			return api.Invalid("edge %d needs from and to", i)
		}
		if e.From == e.To {
			return api.Invalid("edge %d links %q to itself", i, e.From)
		}
		if !keys[e.From] {
			return api.Invalid("edge %d: from %q is not a node key (only a node can depend on something)", i, e.From)
		}
		if err := validateDepType(e.Type); err != nil {
			return fmt.Errorf("edge %d: %w", i, err)
		}
		if strings.HasPrefix(e.To, "external:") {
			if _, _, ok := parseExternalRef(e.To); !ok {
				return api.Invalid("edge %d: external reference %q must be external:<rig>:<id>", i, e.To)
			}
		}
	}
	return nil
}

func createGraphInTx(ctx context.Context, tx pgx.Tx, req api.GraphRequest, actor string) (api.GraphResult, error) {
	ids := make(map[string]string, len(req.Nodes))
	for _, n := range req.Nodes {
		params := n.CreateParams
		params.Rig, params.Actor = req.Rig, actor
		is, err := createInTx(ctx, tx, params, req.Rig)
		if err != nil {
			return api.GraphResult{}, err
		}
		ids[n.Key] = is.ID
	}
	resolve := func(ref string) string {
		if id, ok := ids[ref]; ok {
			return id
		}
		return ref // an issue that already exists, checked by the link
	}
	for _, e := range req.Edges {
		if err := addDependencyInTx(ctx, tx, ids[e.From], resolve(e.To), e.Type, actor); err != nil {
			return api.GraphResult{}, err
		}
	}
	return api.GraphResult{IDs: ids}, nil
}
