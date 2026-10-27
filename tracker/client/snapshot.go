package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/steveyegge/gastown/tracker/api"
)

// Snapshot returns one page of the town snapshot: the issues, their blocked
// verdicts and the counts over the whole set, all read in one transaction.
// Pass the page's NextCursor in req.Cursor for the next page.
//
// Each page is its own moment. Following the cursor reads a later moment, so
// a walk over several pages is not one consistent view of the town; there is
// deliberately no SnapshotAll helper, because one would invite callers to
// treat a multi-page walk as a single read. A caller that needs the whole town
// in one moment asks for a Limit big enough to hold it in one page.
func (c *Client) Snapshot(ctx context.Context, req api.SnapshotRequest) (api.TownSnapshot, error) {
	q := url.Values{}
	if req.Cursor != "" {
		q.Set("cursor", req.Cursor)
	}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}
	path := "/v1/snapshot/town"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out api.TownSnapshot
	if err := c.do(ctx, http.MethodGet, path, nil, &out, true); err != nil {
		return api.TownSnapshot{}, err
	}
	// The page and the counts are collections: empty, never nil.
	if out.Issues == nil {
		out.Issues = []api.SnapshotIssue{}
	}
	if out.Counts.ByStatus == nil {
		out.Counts.ByStatus = map[string]int{}
	}
	if out.Counts.ByRig == nil {
		out.Counts.ByRig = map[string]int{}
	}
	if out.Counts.BlockedRigs == nil {
		out.Counts.BlockedRigs = map[string]int{}
	}
	return out, nil
}
