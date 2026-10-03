package schedulerrun

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/scheduler/capacity"
	"github.com/steveyegge/gastown/internal/style"
)

// ContextInfo is one scheduled bead as the operator surfaces show it: the
// sling context reconciled with the readiness of the work bead it carries.
type ContextInfo struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Status    string `json:"status"`
	TargetRig string `json:"target_rig"`
	Blocked   bool   `json:"blocked,omitempty"`
	// MergePending is set when the bead is held not because a blocker is open
	// but because a blocker was submitted and its merge request has not landed.
	// Operators need to distinguish the two: the first is normal waiting, the
	// second means "look at the merge queue" (gt-0r0z).
	MergePending bool `json:"merge_pending,omitempty"`
	// BlockerMR is the open merge request holding the bead back, when known.
	BlockerMR string `json:"blocker_mr,omitempty"`
}

// contextRecord is one open sling context bead together with the beads
// database it lives in.
type contextRecord struct {
	issue    *beads.Issue
	workDir  string
	beadsDir string
}

// assessment is one scheduled context judged against its work bead: whether
// the bead still exists, whether it is blocked, and whether it may dispatch.
type assessment struct {
	context        contextRecord
	fields         *capacity.SlingContextFields
	info           beadStatusInfo
	found          bool
	blocked        bool
	blockedUnknown bool
	mergePending   bool   // a blocker is submitted but unmerged (gt-0r0z)
	mergePendingMR string // the open MR holding it back
	ready          bool
}

// beadStatusInfo holds batch-fetched bead status, title, and labels.
type beadStatusInfo struct {
	Status string
	Title  string
	Labels []string
	// Dependencies carries the blocking edges declared by this bead, when the
	// query that filled this struct returned them. Only `bd show --json` does;
	// the lighter query paths leave it nil, and a nil dependency list means
	// "no merge-aware gating" — which is the pre-gt-0r0z behavior.
	Dependencies []beads.IssueDep
}

// warnf writes a dispatch warning to w, defaulting to stderr. Callers on the
// daemon heartbeat pass the daemon's writer so the line lands in its log.
func warnf(w io.Writer, format string, args ...interface{}) {
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, format, args...)
}

func beadsForContextRecord(rec contextRecord) beads.Client {
	return beads.NewWithBeadsDir(rec.workDir, rec.beadsDir)
}

// ListScheduled returns one ContextInfo per open sling context whose work bead
// is still claimable. It reconciles sling context beads with work bead
// readiness to mark blocked status, and uses a batch fetch for work bead info
// to avoid N+1 subprocess spawns.
func ListScheduled(townRoot string) ([]ContextInfo, error) {
	assessments, err := assessScheduledContexts(townRoot, nil)
	if err != nil {
		return nil, err
	}
	return contextInfosFromAssessments(assessments), nil
}

// ListAllSlingContexts returns all open sling context beads across all rig
// beads dirs. Sling contexts are created in the target rig's beads dir
// (GH#3468), so we scan HQ plus all rig dirs.
//
// Deduplicates by context ID: different search dirs can resolve to the same
// underlying beads DB (e.g., when a rig's top-level .beads is a redirect to
// mayor/rig/.beads), and both paths would otherwise return the same contexts.
func ListAllSlingContexts(townRoot string) ([]*beads.Issue, error) {
	records, err := listAllSlingContextRecords(townRoot)
	if err != nil {
		return nil, err
	}
	all := make([]*beads.Issue, 0, len(records))
	for _, rec := range records {
		all = append(all, rec.issue)
	}
	return all, nil
}

func listAllSlingContextRecords(townRoot string) ([]contextRecord, error) {
	recs, err := beads.ListOpenSlingContextRecords(townRoot)
	if err != nil {
		return nil, err
	}
	records := make([]contextRecord, 0, len(recs))
	for _, r := range recs {
		records = append(records, contextRecord{issue: r.Issue, workDir: r.WorkDir, beadsDir: r.BeadsDir})
	}
	return records, nil
}

// cleanupStaleContexts closes invalid and stale sling context beads.
// Called explicitly before the dispatch cycle to separate cleanup from querying.
func cleanupStaleContexts(townRoot string) error {
	contexts, err := listAllSlingContextRecords(townRoot)
	if err != nil {
		return err
	}

	// First pass: close invalid and circuit-broken contexts, collect work bead IDs
	// that need status checks for stale detection.
	var staleCheckContexts []contextRecord
	var staleCheckFields []*capacity.SlingContextFields
	for _, ctx := range contexts {
		fields := beads.ParseSlingContextFields(ctx.issue.Description)
		if fields == nil {
			_ = beads.CloseSlingContext(beadsForContextRecord(ctx), ctx.issue.ID, "invalid-context")
			continue
		}
		if fields.DispatchFailures >= MaxDispatchFailures {
			_ = beads.CloseSlingContext(beadsForContextRecord(ctx), ctx.issue.ID, "circuit-broken")
			continue
		}
		staleCheckContexts = append(staleCheckContexts, ctx)
		staleCheckFields = append(staleCheckFields, fields)
	}

	if len(staleCheckContexts) == 0 {
		return nil
	}

	// Collect work bead IDs to fetch
	workBeadIDs := make([]string, 0, len(staleCheckFields))
	for _, fields := range staleCheckFields {
		workBeadIDs = append(workBeadIDs, fields.WorkBeadID)
	}

	// Batch-fetch work bead info for only the specific IDs we need
	workBeadInfo := batchFetchBeadInfoByIDs(townRoot, workBeadIDs)

	// Second pass: close contexts whose work beads are stale.
	// Note: in_progress is intentionally excluded — the work bead is being
	// actively worked, and bd ready won't return it, so the dispatch query
	// already prevents re-dispatch. The context stays open until the polecat
	// finishes and the bead transitions to closed/tombstone.
	for i, ctx := range staleCheckContexts {
		fields := staleCheckFields[i]
		info, found := workBeadInfo[fields.WorkBeadID]
		if found && (info.Status == "hooked" || info.Status == "closed" || info.Status == "tombstone") {
			_ = beads.CloseSlingContext(beadsForContextRecord(ctx), ctx.issue.ID, "stale-work-bead")
		}
	}
	return nil
}

// assessScheduledContexts judges every open sling context against its work
// bead: does the bead still exist, is it blocked, is a blocker's merge
// request still open, and may it dispatch. w takes the warnings.
func assessScheduledContexts(townRoot string, w io.Writer) ([]assessment, error) {
	contexts, err := listAllSlingContextRecords(townRoot)
	if err != nil {
		return nil, err
	}
	if len(contexts) == 0 {
		return nil, nil
	}

	candidates := make([]assessment, 0, len(contexts))
	workBeadIDs := make([]string, 0, len(contexts))
	for _, ctx := range contexts {
		fields := beads.ParseSlingContextFields(ctx.issue.Description)
		if fields == nil || fields.WorkBeadID == "" || fields.TargetRig == "" {
			continue
		}
		if fields.DispatchFailures >= MaxDispatchFailures {
			continue
		}
		candidates = append(candidates, assessment{context: ctx, fields: fields})
		workBeadIDs = append(workBeadIDs, fields.WorkBeadID)
	}
	if len(candidates) == 0 {
		return nil, nil
	}

	sort.Slice(candidates, func(i, j int) bool {
		fi := candidates[i].fields
		fj := candidates[j].fields
		if fi.EnqueuedAt != fj.EnqueuedAt {
			return fi.EnqueuedAt < fj.EnqueuedAt
		}
		return candidates[i].context.issue.ID < candidates[j].context.issue.ID
	})

	workBeadInfo := batchFetchBeadInfoByIDs(townRoot, workBeadIDs)
	blockedWorkIDs, blockedUnknownIDs, blockedErr := listBlockedWorkBeadIDStates(townRoot, workBeadIDs, w)
	mergePendingWorkIDs := listUnmergedBlockedWorkBeadIDs(townRoot, workBeadIDs, workBeadInfo, w)

	seenWork := make(map[string]bool)
	assessments := make([]assessment, 0, len(candidates))
	for _, candidate := range candidates {
		workBeadID := candidate.fields.WorkBeadID
		if seenWork[workBeadID] {
			continue
		}
		seenWork[workBeadID] = true

		info, found := workBeadInfo[workBeadID]
		candidate.info = info
		candidate.found = found
		candidate.blocked = blockedWorkIDs[workBeadID]
		candidate.blockedUnknown = blockedUnknownIDs[workBeadID]
		_, candidate.mergePending = mergePendingWorkIDs[workBeadID]
		candidate.mergePendingMR = mergePendingWorkIDs[workBeadID]
		candidate.ready = isScheduledWorkBeadMergeReady(workBeadID, info, found, blockedWorkIDs, blockedUnknownIDs, mergePendingWorkIDs)
		assessments = append(assessments, candidate)
	}

	return assessments, blockedErr
}

// readySlingContextsFromAssessments projects the ready assessments onto the
// beads a dispatch cycle can take.
func readySlingContextsFromAssessments(w io.Writer, assessments []assessment) []capacity.PendingBead {
	var result []capacity.PendingBead
	for _, a := range assessments {
		if !a.ready {
			continue
		}
		workLabels := a.info.Labels
		if capacity.IsMessagingBead(workLabels) {
			warnf(w, "%s dispatch_skip reason=messaging_label bead=%s labels=%v\n",
				style.Dim.Render("○"), a.fields.WorkBeadID, workLabels)
			continue
		}

		result = append(result, capacity.PendingBead{
			ID:              a.context.issue.ID,
			WorkBeadID:      a.fields.WorkBeadID,
			Title:           a.info.Title,
			TargetRig:       a.fields.TargetRig,
			Description:     a.context.issue.Description,
			Labels:          workLabels,
			Context:         a.fields,
			ContextWorkDir:  a.context.workDir,
			ContextBeadsDir: a.context.beadsDir,
		})
	}

	return result
}

func contextInfosFromAssessments(assessments []assessment) []ContextInfo {
	var result []ContextInfo
	for _, a := range assessments {
		bead, ok := contextInfoFromWork(a.context.issue.Title, a.fields, a.info, a.found, a.ready)
		if !ok {
			continue
		}
		bead.MergePending = a.mergePending
		bead.BlockerMR = a.mergePendingMR
		result = append(result, bead)
	}

	return result
}

func contextInfoFromWork(ctxTitle string, fields *capacity.SlingContextFields, info beadStatusInfo, found, ready bool) (ContextInfo, bool) {
	if fields == nil {
		return ContextInfo{}, false
	}
	title := ctxTitle
	status := "open"
	if found {
		title = info.Title
		status = info.Status
		if status == string(beads.IssueStatusHooked) || status == "closed" || status == "tombstone" {
			return ContextInfo{}, false
		}
	}
	return ContextInfo{
		ID:        fields.WorkBeadID,
		Title:     title,
		Status:    status,
		TargetRig: fields.TargetRig,
		Blocked:   !ready,
	}, true
}

// batchFetchBeadInfoByIDs returns a map of bead ID → status+title+labels for
// specific beads. Uses `bd show` with multiple IDs per rig directory instead of
// fetching all beads. This avoids the O(minutes) latency of
// `bd list --all --json --limit=0` on large repos.
func batchFetchBeadInfoByIDs(townRoot string, ids []string) map[string]beadStatusInfo {
	result := make(map[string]beadStatusInfo)
	if len(ids) == 0 {
		return result
	}

	requestedIDs := uniqueNonEmptyIDs(ids)
	idsByBeadsDir := groupBeadIDsByResolvedBeadsDir(townRoot, requestedIDs)
	for beadsDir, groupedIDs := range idsByBeadsDir {
		// Use Beads wrapper to get proper BEADS_DIR resolution, --allow-stale,
		// and BEADS_DOLT_PORT translation (matching how all other bd-invoking
		// functions work). Route IDs directly instead of trying every beads dir;
		// scheduler status/list/run sit on operator hot paths, and repeated bd show
		// fanout dominates latency in large towns.
		infos, err := fetchBeadStatuses(beadsDir, groupedIDs)
		if err != nil {
			continue
		}
		for id, info := range infos {
			result[id] = info
		}
	}

	for _, id := range requestedIDs {
		if _, found := result[id]; found {
			continue
		}
		info, err := fetchBeadStatusOne(beads.ResolveBeadsDirForID(filepath.Join(townRoot, ".beads"), id), id)
		if err != nil {
			continue
		}
		result[id] = info
	}
	return result
}

// fetchBeadStatuses reads the status, title, labels and declared dependencies
// of the given beads from one beads database.
func fetchBeadStatuses(beadsDir string, ids []string) (map[string]beadStatusInfo, error) {
	c := beads.NewWithBeadsDir(filepath.Dir(beadsDir), beadsDir)
	issues, err := c.ShowMultiple(ids)
	if err != nil {
		return nil, err
	}
	result := make(map[string]beadStatusInfo, len(issues))
	for id, issue := range issues {
		result[id] = beadStatusInfo{
			Status:       issue.Status,
			Title:        issue.Title,
			Labels:       issue.Labels,
			Dependencies: issue.Dependencies,
		}
	}
	return result, nil
}

// fetchBeadStatusOne reads one bead, reporting a missing bead as an error
// rather than an empty answer: the callers of this path are guards that must
// refuse a bead they cannot see.
func fetchBeadStatusOne(beadsDir, id string) (beadStatusInfo, error) {
	infos, err := fetchBeadStatuses(beadsDir, []string{id})
	if err != nil {
		return beadStatusInfo{}, err
	}
	info, ok := infos[id]
	if !ok {
		return beadStatusInfo{}, fmt.Errorf("bead %s not found in %s", id, beadsDir)
	}
	return info, nil
}

func uniqueNonEmptyIDs(ids []string) []string {
	result := make([]string, 0, len(ids))
	seen := make(map[string]bool)
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		result = append(result, id)
	}
	return result
}

func groupBeadIDsByResolvedBeadsDir(townRoot string, ids []string) map[string][]string {
	townBeadsDir := filepath.Join(townRoot, ".beads")
	idsByBeadsDir := make(map[string][]string)
	seen := make(map[string]bool)
	for _, id := range ids {
		if id == "" {
			continue
		}
		beadsDir := beads.ResolveBeadsDirForID(townBeadsDir, id)
		key := beadsDir + "\x00" + id
		if seen[key] {
			continue
		}
		seen[key] = true
		idsByBeadsDir[beadsDir] = append(idsByBeadsDir[beadsDir], id)
	}
	return idsByBeadsDir
}

// blockedWorkQuery answers which beads one database reports blocked. It is
// the test seam for the read below, whose production value is
// runBlockedWorkQuery.
type blockedWorkQuery func(beadsDir string, groupedIDs []string) ([]*beads.Issue, error)

// blockedReader is the store surface the blocked read needs: bd's blocked
// cache has no Client verb, so this pins the one call to the typed method.
type blockedReader interface {
	Blocked() ([]*beads.Issue, error)
}

func runBlockedWorkQuery(beadsDir string, _ []string) ([]*beads.Issue, error) {
	// Use the Beads wrapper to get proper BEADS_DIR resolution, --allow-stale,
	// and BEADS_DOLT_PORT translation. bd's blocked cache is the authority a
	// dependency walk would only approximate, so this stays the one read the
	// scheduler makes outside Client.
	var store blockedReader = beads.NewWithBeadsDir(filepath.Dir(beadsDir), beadsDir)
	return store.Blocked()
}

func listBlockedWorkBeadIDStates(townRoot string, workBeadIDs []string, w io.Writer) (map[string]bool, map[string]bool, error) {
	return listBlockedWorkBeadIDStatesWithRunner(townRoot, workBeadIDs, runBlockedWorkQuery, w)
}

// listBlockedWorkBeadIDStatesWithRunner reads `bd blocked` once per beads
// database. A query that fails marks its whole group unknown rather than
// ready: an unreadable blocked list must not release a blocked bead.
func listBlockedWorkBeadIDStatesWithRunner(townRoot string, workBeadIDs []string, query blockedWorkQuery, w io.Writer) (map[string]bool, map[string]bool, error) {
	blockedIDs := make(map[string]bool)
	blockedUnknownIDs := make(map[string]bool)
	idsByBeadsDir := groupBeadIDsByResolvedBeadsDir(townRoot, workBeadIDs)
	failCount := 0
	var lastErr error
	for beadsDir, groupedIDs := range idsByBeadsDir {
		blockedBeads, err := query(beadsDir, groupedIDs)
		if err != nil {
			failCount++
			lastErr = err
			markBlockedUnknown(blockedUnknownIDs, groupedIDs)
			warnf(w, "%s Warning: bd blocked failed for %s: %v\n",
				style.Dim.Render("⚠"), filepath.Dir(beadsDir), err)
			continue
		}
		for _, blocked := range blockedBeads {
			if blocked != nil && blocked.ID != "" {
				blockedIDs[blocked.ID] = true
			}
		}
	}
	if failCount == len(idsByBeadsDir) && failCount > 0 {
		return blockedIDs, blockedUnknownIDs, fmt.Errorf("all %d bd blocked queries failed (last: %w)", failCount, lastErr)
	}
	return blockedIDs, blockedUnknownIDs, nil
}

func markBlockedUnknown(blockedUnknownIDs map[string]bool, ids []string) {
	for _, id := range ids {
		if id != "" {
			blockedUnknownIDs[id] = true
		}
	}
}

func isScheduledWorkBeadReady(workBeadID string, info beadStatusInfo, found bool, blockedWorkIDs, blockedUnknownIDs map[string]bool) bool {
	return isScheduledWorkBeadMergeReady(workBeadID, info, found, blockedWorkIDs, blockedUnknownIDs, nil)
}

// isScheduledWorkBeadMergeReady is isScheduledWorkBeadReady plus the
// merge-aware dependency gate (gt-0r0z).
//
// `bd blocked` answers "is this bead blocked?" from bead STATUS alone: a
// blocker that a polecat closed at MR-creation time reads as satisfied even
// while its work is still sitting in the merge queue. mergePendingWorkIDs
// carries the missing distinction — it maps a work bead to the open merge
// request of a blocker that has been submitted but has not landed, and any
// entry here holds the bead back. A nil or empty map is the ordinary case and
// behaves exactly like the status-only check.
func isScheduledWorkBeadMergeReady(workBeadID string, info beadStatusInfo, found bool, blockedWorkIDs, blockedUnknownIDs map[string]bool, mergePendingWorkIDs map[string]string) bool {
	if !found || blockedWorkIDs[workBeadID] || blockedUnknownIDs[workBeadID] {
		return false
	}
	if _, pending := mergePendingWorkIDs[workBeadID]; pending {
		return false
	}
	return info.Status == "open"
}

// openMRIndexLookup returns the open merge requests of one beads database,
// indexed by the source issue they were submitted for.
type openMRIndexLookup func(beadsDir string) (map[string]*beads.Issue, error)

func runOpenMRIndexLookup(beadsDir string) (map[string]*beads.Issue, error) {
	c := beads.NewWithBeadsDir(filepath.Dir(beadsDir), beadsDir)
	return beads.OpenMRsBySourceIssue(c)
}

// listUnmergedBlockedWorkBeadIDs returns the work beads that must not dispatch
// yet because a blocking dependency has been submitted but not landed. The map
// value is the open merge-request bead holding the dependency back, which is
// what the worker is told about its starting context.
//
// This is the merge-aware half of dispatch readiness (gt-0r0z). It is
// deliberately cheap in the common case: a work bead with no dependencies, or
// whose blockers are all still open, needs no merge-queue lookup at all, so if
// nothing in the batch has a closed blocking dependency NO query is issued and
// the batch dispatches on exactly the timing it had before this feature
// existed.
func listUnmergedBlockedWorkBeadIDs(townRoot string, workBeadIDs []string, infos map[string]beadStatusInfo, w io.Writer) map[string]string {
	return listUnmergedBlockedWorkBeadIDsWithLookup(townRoot, workBeadIDs, infos, runOpenMRIndexLookup, w)
}

func listUnmergedBlockedWorkBeadIDsWithLookup(townRoot string, workBeadIDs []string, infos map[string]beadStatusInfo, lookup openMRIndexLookup, w io.Writer) map[string]string {
	held := make(map[string]string)
	if len(workBeadIDs) == 0 {
		return held
	}

	// Pass 1: which blockers read as resolved by status? Only those can be
	// "closed but unmerged". A blocker still open is handled by `bd blocked`
	// and never needs a merge-queue lookup.
	blockers := make([]string, 0)
	seenBlocker := make(map[string]bool)
	for _, id := range uniqueNonEmptyIDs(workBeadIDs) {
		info, ok := infos[id]
		if !ok {
			continue
		}
		probe := &beads.Issue{ID: id, Dependencies: info.Dependencies}
		for _, blockerID := range beads.CandidateMergeAwareBlockerIDs(probe) {
			if seenBlocker[blockerID] {
				continue
			}
			seenBlocker[blockerID] = true
			blockers = append(blockers, blockerID)
		}
	}
	if len(blockers) == 0 {
		return held
	}

	// Pass 2: one merge-request index per beads database the blockers live in.
	// A failure here is reported but NOT fatal: the merge-aware gate fails open.
	// Failing closed would turn a transient Dolt hiccup into a town-wide
	// dispatch stall — a new failure class — whereas a missed gate merely
	// reproduces the pre-existing behavior. The warning keeps it visible, and
	// criterion 5 still tells the worker its dependency's true state.
	openMRs := make(map[string]*beads.Issue)
	for beadsDir := range groupBeadIDsByResolvedBeadsDir(townRoot, blockers) {
		index, err := lookup(beadsDir)
		if err != nil {
			warnf(w, "%s Warning: merge-aware dependency check unavailable for %s: %v\n",
				style.Dim.Render("⚠"), filepath.Dir(beadsDir), err)
			continue
		}
		for source, mr := range index {
			if _, ok := openMRs[source]; !ok {
				openMRs[source] = mr
			}
		}
	}

	// Pass 3: classify each candidate work bead against that index.
	for _, id := range uniqueNonEmptyIDs(workBeadIDs) {
		info, ok := infos[id]
		if !ok {
			continue
		}
		probe := &beads.Issue{ID: id, Dependencies: info.Dependencies}
		unmerged := beads.UnmergedBlockerIDs(probe, openMRs)
		if len(unmerged) == 0 {
			continue
		}
		mrID := ""
		if mr := openMRs[unmerged[0]]; mr != nil {
			mrID = mr.ID
		}
		held[id] = mrID
	}
	return held
}
