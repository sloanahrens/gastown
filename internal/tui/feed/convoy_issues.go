package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/util"
)

// trackedIDsCacheTTL bounds how long a convoy's tracked-issue-ID list (from
// `bd dep list <convoy> -t tracks`) is reused before being re-queried.
// Convoy membership is set once at dispatch time and essentially never
// changes afterward, so re-deriving it on every 10s tick for every convoy
// was pure waste — it was the dominant source of the concurrent `bd dep
// list` fan-out that saturated Dolt during the gt-05vk town-wide bd famine.
const trackedIDsCacheTTL = 60 * time.Second

type trackedIDsCacheEntry struct {
	ids       []string
	fetchedAt time.Time
}

var (
	trackedIDsCacheMu sync.Mutex
	trackedIDsCache   = make(map[string]trackedIDsCacheEntry)
)

// trackedIssueIDs returns the issue IDs a convoy tracks, from cache when
// fresh (no bd call at all). On a cache miss or expiry it spawns exactly one
// `bd dep list` child. Callers are expected to invoke this serially across
// convoys (see enrichConvoys) so at most one bd child is in flight at a time.
func trackedIssueIDs(beadsDir, convoyID string) []string {
	if !convoyIDPattern.MatchString(convoyID) {
		return nil
	}

	trackedIDsCacheMu.Lock()
	entry, cached := trackedIDsCache[convoyID]
	trackedIDsCacheMu.Unlock()
	if cached && time.Since(entry.fetchedAt) < trackedIDsCacheTTL {
		return entry.ids
	}

	ctx, cancel := context.WithTimeout(context.Background(), constants.BdSubprocessTimeout)
	defer cancel()

	cmd := beads.CommandContextWithEnv(ctx, beadsDir, os.Environ(), "dep", "list", convoyID, "-t", "tracks", "--json")
	util.SetDetachedProcessGroup(cmd)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		// Transient failure (e.g. Dolt contention) — keep serving the last
		// known membership rather than dropping the convoy to zero tracked
		// issues until the next successful refresh.
		return entry.ids
	}

	var deps []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &deps); err != nil {
		return entry.ids
	}

	ids := make([]string, 0, len(deps))
	for _, dep := range deps {
		ids = append(ids, beads.ExtractIssueID(dep.ID))
	}

	trackedIDsCacheMu.Lock()
	trackedIDsCache[convoyID] = trackedIDsCacheEntry{ids: ids, fetchedAt: time.Now()}
	trackedIDsCacheMu.Unlock()

	return ids
}

// batchIssueStatus runs a single `bd show <ids...> --json` covering every
// distinct tracked issue ID across every convoy in one FetchConvoys pass,
// replacing what used to be one `bd show` call per convoy. bd dep list
// returns status from the dependency record in HQ beads, which is never
// updated when cross-rig issues (e.g., gt-* tracked by hq-* convoys) are
// closed in their rig — this batched bd show is what keeps status current.
func batchIssueStatus(ids []string) map[string]string {
	if len(ids) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), constants.BdSubprocessTimeout)
	defer cancel()

	args := append([]string{"show"}, ids...)
	args = append(args, "--json")
	cmd := beads.CommandContextWithEnv(ctx, "", os.Environ(), args...)
	util.SetDetachedProcessGroup(cmd)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil
	}

	var issues []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &issues); err != nil {
		return nil
	}

	result := make(map[string]string, len(issues))
	for _, issue := range issues {
		result[issue.ID] = issue.Status
	}
	return result
}
