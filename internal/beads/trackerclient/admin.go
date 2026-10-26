package trackerclient

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/tracker/api"
)

var _ beads.Admin = (*Client)(nil)

// configKey is where a key lives in the tracker's one config table: under the
// client's rig, like the importer writes it ("rig.gt.types.custom"), so rigs
// that share a tracker do not read each other's settings.
func (c *Client) configKey(key string) string { return "rig." + c.rig + "." + key }

// ConfigGet returns a configuration key's value, "" when it is unset.
// issue_prefix is the rig's prefix, which the tracker knows without a setting.
func (c *Client) ConfigGet(key string) (string, error) {
	if key == "issue_prefix" {
		return c.rig, nil
	}
	v, _, err := c.tc.ConfigGet(ctx(), c.configKey(key))
	return v, mapErr(err)
}

// ConfigSet sets a configuration key.
func (c *Client) ConfigSet(key, value string) error {
	return mapErr(c.tc.ConfigSet(ctx(), c.configKey(key), value))
}

// KVList returns no entries. The tracker has no key-value store yet, and this
// is decision A: an empty store is the contract's answer, and a rig's kv rows
// are imported before its flip, with the tracker-kv-store doctor check
// reporting tracker-routed rigs until then.
func (c *Client) KVList() (map[string]string, error) { return map[string]string{}, nil }

// KVGet reports every key as unset, the same answer KVList gives: the store
// holds nothing, so no key is found.
func (c *Client) KVGet(key string) (string, error) {
	return "", fmt.Errorf("tracker: kv %s: %w", key, beads.ErrNotFound)
}

// CountIssues counts the rig's durable issues, closed ones included and wisps
// not.
func (c *Client) CountIssues() (int, error) {
	st, err := c.tc.Stats(ctx(), c.rig)
	if err != nil {
		return 0, mapErr(err)
	}
	return st.Total, nil
}

// StatsJSON returns bd stats --json's summary for the rig.
func (c *Client) StatsJSON() ([]byte, error) {
	st, err := c.tc.Stats(ctx(), c.rig)
	if err != nil {
		return nil, mapErr(err)
	}
	b, err := json.Marshal(map[string]any{"summary": map[string]int{
		"total_issues": st.Total, "open_issues": st.ByStatus["open"], "in_progress_issues": st.ByStatus["in_progress"],
		"closed_issues": st.ByStatus["closed"], "hooked_issues": st.ByStatus["hooked"], "deferred_issues": st.ByStatus["deferred"],
	}})
	return b, err
}

// MolWispList returns the rig's wisps that are not closed.
func (c *Client) MolWispList() ([]*beads.Issue, error) {
	all, err := c.tc.ListAll(ctx(), api.ListFilter{Rig: c.rig, Ephemeral: true, IncludeInfra: true})
	if err != nil {
		return nil, mapErr(err)
	}
	return issues(all), nil
}

// WispGCCandidates returns the ids of the rig's open wisps last updated at
// least age ago, which `bd mol wisp gc` would delete. Wisps in progress,
// closed wisps and wisps an open issue blocks are never candidates.
func (c *Client) WispGCCandidates(age time.Duration) ([]string, error) {
	open, err := c.tc.ListAll(ctx(), api.ListFilter{Rig: c.rig, Ephemeral: true, IncludeInfra: true, Statuses: []string{"open"}})
	if err != nil {
		return nil, mapErr(err)
	}
	cutoff := time.Now().Add(-age)
	var old []string
	for _, w := range open {
		if !w.UpdatedAt.After(cutoff) {
			old = append(old, w.ID)
		}
	}
	if len(old) == 0 {
		return nil, nil
	}
	// BlockedBy is filled by a read of named issues, not by a list.
	full, err := c.tc.ShowMany(ctx(), old)
	if err != nil {
		return nil, mapErr(err)
	}
	var out []string
	for _, id := range old {
		if w, ok := full[id]; ok && len(w.BlockedBy) == 0 {
			out = append(out, id)
		}
	}
	return out, nil
}

// journalOps maps the tracker's event ops to bd's journal ops; any op not
// listed (a title change, a label, a reopen) is an update.
var journalOps = map[string]string{
	"created": "create", "closed": "close", "deleted": "delete", "commented": "comment",
	"dependency_added": "dep_add", "dependency_removed": "dep_remove",
}

// EventsTail reads journal records with seq above since, at most limit of them
// (0 = all). A since below the oldest retained record is an
// *beads.EventsTruncatedError.
func (c *Client) EventsTail(since int64, limit int) (*beads.EventsPage, error) {
	ask := 0
	if limit > 0 {
		ask = limit + 1 // one more than wanted tells whether records follow
	}
	page, err := c.tc.Events(ctx(), since, ask)
	if err != nil {
		return nil, mapErr(err)
	}
	if page.FirstSeq > 0 && since < page.FirstSeq-1 {
		return nil, &beads.EventsTruncatedError{Since: since, Floor: page.FirstSeq, Head: page.NextAfter}
	}
	out := &beads.EventsPage{NextSince: since}
	events := page.Events
	if limit > 0 && len(events) > limit {
		events, out.More = events[:limit], true
	}
	for _, e := range events {
		op := journalOps[e.Op]
		if op == "" {
			op = "update"
		}
		out.Records = append(out.Records, beads.EventRecord{Seq: e.Seq, TS: ts(e.TS), Op: op, IssueID: e.IssueID, Actor: e.Actor, Status: eventStatus(e)})
		out.NextSince = e.Seq
	}
	return out, nil
}

// eventStatus is the issue's status after the change, which the tracker
// records in the event's payload; "" for a delete.
func eventStatus(e api.Event) string {
	var p struct {
		Status string `json:"status"`
	}
	if len(e.Payload) == 0 || json.Unmarshal(e.Payload, &p) != nil {
		return ""
	}
	return p.Status
}
