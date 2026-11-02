package bdshim

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// The third auxiliary handlers: config, events and stats for a tracker rig
// (gt-p85pw.99). Each answers the command its golden row records and refuses
// every other shape by name, so a caller never reads a wider or differently
// shaped answer than bd printed.
//
// The tracker keeps a rig's configuration in its one config table, under the
// rig's name (internal/beads/trackerclient spells the key rig.<rig>.<key>),
// so config reads and writes go through the store's ConfigGet and ConfigSet.
// The seam cannot enumerate that table, so config list reads bd's seeded
// default keys, the rows an imported rig holds; a setting outside that table
// is not listed.
//
// The tracker's events feed is read through the seam's EventsTail. The golden
// records an empty journal, and bd's record line carries the full issue, dep
// and comment objects, which the seam's records do not, so a feed with records
// is refused rather than printed in a shape bd did not record.
//
// stats prints bd's summary computed from the rig's issue and wisp lists plus
// one link read, using the board rules the blocked and ready handlers already
// use.

// configActorKey is bd's configuration key for the audit actor. bd resolves it
// from the environment before the store, so this handler does too.
const configActorKey = "actor"

// bdSeededConfig is bd's default configuration table: the rows bd's 0016
// migration inserts into a new database (cmd/bd's migrations/0016_default_config),
// alphabetically, the order config list prints them. A tracker rig's
// configuration carries the same rows once the rig is imported; a rig whose
// store answers none is answered with these, as a freshly initialized bd
// database would be.
var bdSeededConfig = []struct{ key, value string }{
	{"auto_compact_enabled", "false"},
	{"compact_batch_size", "50"},
	{"compact_parallel_workers", "5"},
	{"compact_tier1_days", "30"},
	{"compact_tier1_dep_levels", "2"},
	{"compact_tier2_commits", "100"},
	{"compact_tier2_days", "90"},
	{"compact_tier2_dep_levels", "5"},
	{"compaction_enabled", "false"},
}

// bdConfigFileDefaults are the defaults bd documents for the keys it keeps in
// config.yaml (`bd config --help`), which a config get answers when the rig's
// store has no value for the key.
var bdConfigFileDefaults = map[string]string{
	"export.auto":     "false",
	"export.path":     "issues.jsonl",
	"export.interval": "60s",
	"export.git-add":  "false",
	"import.path":     "issues.jsonl",
}

// The flag set each verb answers: the flags its golden rows cover.
var (
	configListFlags = readFlagSpec{}
	configGetFlags  = readFlagSpec{}
	configSetFlags  = readFlagSpec{}
	eventsTailFlags = readFlagSpec{}
	statsFlags      = readFlagSpec{"--json": {}}
)

// auxConfigStore is the part of a rig's store the config handlers use. The
// handlers open the full backend.Store; the narrow interface lets a test drive
// each refusal without a tracker server.
type auxConfigStore interface {
	ConfigGet(key string) (string, error)
	ConfigSet(key, value string) error
}

// auxEventsStore is the part of a rig's store the events handler reads.
type auxEventsStore interface {
	EventsTail(since int64, limit int) (*beads.EventsPage, error)
}

// configHandler answers bd config list, get and set for a tracker rig, and
// refuses every other config subcommand by name.
func configHandler(env Env, d Decision) int {
	switch command := subcommand(env.Args); command {
	case "config list":
		return configList(env, d)
	case "config get":
		return configGet(env, d)
	case "config set":
		return configSet(env, d)
	default:
		return shimError(env, fmt.Errorf("%s is not implemented for tracker rig %s: only config list, config get and config set are", command, d.Route.Prefix))
	}
}

// subcommand names the verb and the word after it, through bd's alias table,
// so a command bd aliases is named as the command it means. A word bd does not
// document is still named as the caller's subcommand.
func subcommand(args []string) string {
	ops := operands("", args)
	if len(ops) == 0 {
		return ""
	}
	if len(ops) == 1 {
		return ops[0]
	}
	command := ops[0] + " " + ops[1]
	if canonical, ok := aliases[command]; ok {
		return canonical
	}
	return command
}

// configList answers bd config list for a tracker rig: the configuration
// block, the environment overrides bd reports, and bd's tip.
func configList(env Env, d Decision) int {
	fl, err := parseReadArgs("config list", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, configListFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	if len(fl.operands) > 0 {
		return shimError(env, fmt.Errorf("config list takes no operands; %q is not implemented for tracker rig %s", fl.operands[0], d.Route.Prefix))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	return listConfig(env, st, d.Route.Prefix)
}

// listConfig prints bd's configuration block, then the overrides bd reports
// and its tip. The keys are bd's seeded configuration and issue_prefix, sorted
// as bd sorts them; a key the store answers is printed as stored, and one it
// leaves empty as bd's default.
func listConfig(env Env, st auxConfigStore, prefix string) int {
	keys := make([]string, 0, len(bdSeededConfig)+1)
	values := make(map[string]string, len(bdSeededConfig)+1)
	for _, row := range bdSeededConfig {
		v, err := st.ConfigGet(row.key)
		if err != nil {
			return shimError(env, fmt.Errorf("config list: reading %s: %w", row.key, err))
		}
		if v == "" {
			v = row.value
		}
		keys = append(keys, row.key)
		values[row.key] = v
	}
	// issue_prefix is the rig's prefix, which the tracker answers without a
	// stored setting; bd prints it without the dash its ids carry.
	issuePrefix, err := st.ConfigGet("issue_prefix")
	if err != nil {
		return shimError(env, fmt.Errorf("config list: reading issue_prefix: %w", err))
	}
	if issuePrefix == "" {
		return shimError(env, fmt.Errorf("config list: the store answered no issue_prefix for tracker rig %s", prefix))
	}
	keys = append(keys, "issue_prefix")
	values["issue_prefix"] = strings.TrimSuffix(issuePrefix, "-")
	sort.Strings(keys)

	actor, err := envActor(env, "config list", prefix)
	if err != nil {
		return shimError(env, err)
	}
	var answer strings.Builder
	answer.WriteString("\nConfiguration:\n")
	for _, key := range keys {
		fmt.Fprintf(&answer, "  %s = %s\n", key, values[key])
	}
	answer.WriteString("\n")
	if actor != "" {
		answer.WriteString("⚠ Environment variable overrides detected:\n")
		fmt.Fprintf(&answer, "  actor: env BD_ACTOR=%q overrides config\n\n", actor)
	}
	answer.WriteString("Tip: Run 'bd config show' for all effective config with provenance.\n")
	if _, err := fmt.Fprint(env.Stdout, answer.String()); err != nil {
		return shimError(env, fmt.Errorf("config list: writing bd's answer: %w", err))
	}
	return 0
}

// envActor reads the actor bd resolves from BD_ACTOR, the name the recorded
// golden reports. bd also resolves BEADS_ACTOR, git user.name and $USER; the
// shim resolves none of those, so a command that would answer from them is
// refused rather than answered with a different actor.
func envActor(env Env, command, prefix string) (string, error) {
	if actor := env.Getenv("BD_ACTOR"); actor != "" {
		return actor, nil
	}
	if env.Getenv("BEADS_ACTOR") != "" {
		return "", fmt.Errorf("%s with only BEADS_ACTOR set is not implemented for tracker rig %s: bd resolves the actor from it, and the shim's answer is not recorded", command, prefix)
	}
	return "", nil
}

// configGet answers bd config get for a tracker rig.
func configGet(env Env, d Decision) int {
	fl, err := parseReadArgs("config get", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, configGetFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	switch len(fl.operands) {
	case 0:
		return shimError(env, errors.New("config get needs a key"))
	case 1:
	default:
		return shimError(env, fmt.Errorf("config get takes one key; %q is not implemented for tracker rig %s", fl.operands[1], d.Route.Prefix))
	}
	key := fl.operands[0]
	if key == "" {
		return shimError(env, errors.New("config get needs a non-empty key"))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	return getConfig(env, st, key, d.Route.Prefix)
}

// getConfig prints the key's value as bd does: the environment's actor for
// actor, then the rig's stored value, then bd's documented default, and bd's
// "not set" line when it has none.
func getConfig(env Env, st auxConfigStore, key, prefix string) int {
	value := ""
	if key == configActorKey {
		actor, err := envActor(env, fmt.Sprintf("config get %s", key), prefix)
		if err != nil {
			return shimError(env, err)
		}
		if actor == "" {
			return shimError(env, fmt.Errorf("config get %s without BD_ACTOR is not implemented for tracker rig %s: bd falls back to the git identity, which the shim does not resolve", key, prefix))
		}
		value = actor
	} else {
		stored, err := st.ConfigGet(key)
		if err != nil {
			return shimError(env, fmt.Errorf("config get %s: %w", key, err))
		}
		value = stored
	}
	if value == "" {
		value = bdConfigFileDefaults[key]
	}
	if value == "" {
		value = fmt.Sprintf("%s (not set)", key)
	}
	if _, err := fmt.Fprintln(env.Stdout, value); err != nil {
		return shimError(env, fmt.Errorf("config get %s: writing bd's answer: %w", key, err))
	}
	return 0
}

// configSet answers bd config set for a tracker rig: the write through the
// store, then a line naming where it went. bd names config.yaml, the file a Dolt
// rig keeps this setting in; the shim writes the tracker's config table, so the
// line names that rather than a file it never touches (gt-ec6vd).
func configSet(env Env, d Decision) int {
	fl, err := parseReadArgs("config set", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, configSetFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	switch len(fl.operands) {
	case 0:
		return shimError(env, errors.New("config set needs a key and a value"))
	case 1:
		return shimError(env, errors.New("config set needs a value"))
	case 2:
	default:
		return shimError(env, fmt.Errorf("config set takes a key and a value; %q is not implemented for tracker rig %s", fl.operands[2], d.Route.Prefix))
	}
	key, value := fl.operands[0], fl.operands[1]
	if key == "" {
		return shimError(env, errors.New("config set needs a non-empty key"))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	return setConfig(env, st, key, value)
}

// setConfig stores the value and prints the line naming where it went. The line
// is printed only after the write, so a failed print says the value was stored
// rather than reading as a set the caller should retry.
func setConfig(env Env, st auxConfigStore, key, value string) int {
	if err := st.ConfigSet(key, value); err != nil {
		return shimError(env, fmt.Errorf("config set %s: %w", key, err))
	}
	if _, err := fmt.Fprintf(env.Stdout, "Set %s = %s (in the tracker config table)\n", key, value); err != nil {
		return shimError(env, fmt.Errorf("config set %s: the value was stored but the line could not be printed: %w", key, err))
	}
	return 0
}

// eventsHandler answers bd events tail for a tracker rig, and refuses every
// other events subcommand by name.
func eventsHandler(env Env, d Decision) int {
	switch command := subcommand(env.Args); command {
	case "events tail":
		return eventsTail(env, d)
	default:
		return shimError(env, fmt.Errorf("%s is not implemented for tracker rig %s: only events tail is", command, d.Route.Prefix))
	}
}

// eventsTail answers bd events tail for a tracker rig. The golden records an
// empty journal, so an empty read prints nothing; a feed with records is
// refused, because bd's line carries the full issue, dep and comment objects
// and no golden records that shape.
func eventsTail(env Env, d Decision) int {
	fl, err := parseReadArgs("events tail", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, eventsTailFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	switch len(fl.operands) {
	case 0, 1:
	default:
		return shimError(env, fmt.Errorf("events tail takes at most one seq; %q is not implemented for tracker rig %s", fl.operands[1], d.Route.Prefix))
	}
	since := int64(0)
	if len(fl.operands) == 1 {
		n, err := strconv.ParseInt(fl.operands[0], 10, 64)
		if err != nil || n < 0 {
			return shimError(env, fmt.Errorf("events tail since %q is not a seq", fl.operands[0]))
		}
		since = n
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	return tailEvents(env, st, since, d.Route.Prefix)
}

// tailEvents prints the records the feed holds after since, and nothing when
// it holds none. A read below the retained window is refused rather than
// reported as an empty journal, which would read as a complete history.
func tailEvents(env Env, st auxEventsStore, since int64, prefix string) int {
	page, err := st.EventsTail(since, 0)
	if err != nil {
		return shimError(env, fmt.Errorf("events tail from seq %d: %w", since, err))
	}
	if page == nil {
		return shimError(env, fmt.Errorf("events tail from seq %d: the store answered no page", since))
	}
	if len(page.Records) > 0 {
		return shimError(env, fmt.Errorf("events tail of %d record(s) is not implemented for tracker rig %s: the golden records only an empty journal", len(page.Records), prefix))
	}
	if page.More {
		return shimError(env, fmt.Errorf("events tail with more records than the page is not implemented for tracker rig %s", prefix))
	}
	return 0
}

// bdStatsAnswer is bd's stats --json answer: the summary, then the schema
// version.
type bdStatsAnswer struct {
	Data          bdStatsData `json:"data"`
	SchemaVersion int         `json:"schema_version"`
}

// bdStatsData is bd's data member: the counts.
type bdStatsData struct {
	Summary bdStatsSummary `json:"summary"`
}

// bdStatsSummary is bd's summary, in the order bd prints its members.
type bdStatsSummary struct {
	TotalIssues             int `json:"total_issues"`
	OpenIssues              int `json:"open_issues"`
	InProgressIssues        int `json:"in_progress_issues"`
	ClosedIssues            int `json:"closed_issues"`
	BlockedIssues           int `json:"blocked_issues"`
	DeferredIssues          int `json:"deferred_issues"`
	ReadyIssues             int `json:"ready_issues"`
	PinnedIssues            int `json:"pinned_issues"`
	EpicsEligibleForClosure int `json:"epics_eligible_for_closure"`
	AverageLeadTimeHours    int `json:"average_lead_time_hours"`
}

// statsHandler answers bd stats --json for a tracker rig.
func statsHandler(env Env, d Decision) int {
	fl, err := parseReadArgs("stats", env.Args)
	if err != nil {
		return shimError(env, err)
	}
	if err := refuseUnknownFlags(fl, statsFlags, d.Route.Prefix); err != nil {
		return shimError(env, err)
	}
	if len(fl.operands) > 0 {
		return shimError(env, fmt.Errorf("stats takes no operands; %q is not implemented for tracker rig %s", fl.operands[0], d.Route.Prefix))
	}
	// The goldens record stats only as JSON; bd's human summary is not
	// recorded, so a command without --json is refused rather than printed in
	// a shape the shim cannot compare with bd.
	if !fl.has("--json") {
		return shimError(env, fmt.Errorf("stats without --json is not implemented for tracker rig %s", d.Route.Prefix))
	}
	st, err := openTrackerStore(env, d)
	if err != nil {
		return shimError(env, err)
	}
	summary, err := readStats(st, clockOf(env))
	if err != nil {
		return shimError(env, err)
	}
	if err := printBDAnswer(env.Stdout, bdStatsAnswer{Data: bdStatsData{Summary: summary}, SchemaVersion: bdSchemaVersion}); err != nil {
		return shimError(env, err)
	}
	return 0
}

// readStats counts the rig's board as bd's stats does: the durable issues of
// every status, the ones bd's blocked flag reports, the ready board the
// ready_issues view keeps (every type, unlike `bd ready`), and the epics whose
// children are all closed.
func readStats(st readStore, now time.Time) (bdStatsSummary, error) {
	var summary bdStatsSummary
	listed, err := st.List(beads.ListOptions{Status: "all", IncludeInfra: true})
	if err != nil {
		return summary, fmt.Errorf("stats: %w", err)
	}
	full, err := rowDependencies(st, listed)
	if err != nil {
		return summary, fmt.Errorf("stats: %w", err)
	}
	// A wisp child of an epic still counts as an open child, so the epics'
	// children are read from the wisp table as well.
	wisps, err := st.List(beads.ListOptions{Status: "all", IncludeInfra: true, Ephemeral: true})
	if err != nil {
		return summary, fmt.Errorf("stats: reading the rig's wisps: %w", err)
	}
	blocked := blockedIssues(listed, full)
	summary.TotalIssues = len(listed)
	for _, is := range listed {
		switch is.Status {
		case "open":
			summary.OpenIssues++
		case "in_progress":
			summary.InProgressIssues++
		case "closed":
			summary.ClosedIssues++
		case "deferred":
			summary.DeferredIssues++
		case "pinned":
			summary.PinnedIssues++
		}
		// bd's blocked flag is set on issues that are neither closed nor
		// pinned, so those are the ones a stats read counts.
		if is.Status != "closed" && is.Status != "pinned" && blocked[is.ID] {
			summary.BlockedIssues++
		}
		if ready, err := readyForStats(is, full, blocked, now); err != nil {
			return summary, fmt.Errorf("stats: %w", err)
		} else if ready {
			summary.ReadyIssues++
		}
	}
	summary.EpicsEligibleForClosure = countClosableEpics(listed, wisps)
	// AverageLeadTimeHours stays 0: the store carries no started_at, so there
	// is no lead time to average. bd prints the field regardless, as 0 when it
	// has no measurement.
	return summary, nil
}

// readyForStats reports whether bd's ready_issues view keeps is: open, not
// blocked, not deferred, and not a child of a parent that is deferred.
func readyForStats(is *beads.Issue, full map[string]*beads.Issue, blocked map[string]bool, now time.Time) (bool, error) {
	if is.Status != "open" || blocked[is.ID] {
		return false, nil
	}
	deferred, err := defersAfter(is.DeferUntil, now)
	if err != nil || deferred {
		return false, err
	}
	parent := parentOf(full[is.ID])
	if parent == "" {
		return true, nil
	}
	p, ok := full[parent]
	if !ok {
		return false, fmt.Errorf("the bead %s names the parent %s, which the store did not answer", is.ID, parent)
	}
	deferred, err = defersAfter(p.DeferUntil, now)
	if err != nil {
		return false, err
	}
	return !deferred, nil
}

// defersAfter reports whether a defer_until value keeps an issue off the ready
// board at now. An unreadable value is an error, never read as no deferral.
func defersAfter(deferUntil string, now time.Time) (bool, error) {
	if deferUntil == "" {
		return false, nil
	}
	at, err := time.Parse(time.RFC3339, deferUntil)
	if err != nil {
		return false, fmt.Errorf("defer_until %q is not a time: %w", deferUntil, err)
	}
	return at.After(now), nil
}

// countClosableEpics counts the epics of listed whose children, wisps included,
// are all closed. An epic with no child is not eligible, as bd's count of a
// zero-child epic is.
func countClosableEpics(listed []*beads.Issue, wisps []*beads.Issue) int {
	epics := make(map[string]bool)
	for _, is := range listed {
		if is.Type == "epic" && is.Status != "closed" {
			epics[is.ID] = true
		}
	}
	total := make(map[string]int, len(epics))
	closed := make(map[string]int, len(epics))
	// Children are read from the parent-child link the epic's children carry.
	// Only the epics themselves are counted; a child that is itself an epic is
	// a child of this one for this count.
	seen := make(map[string]bool, len(listed)+len(wisps))
	for _, group := range [][]*beads.Issue{listed, wisps} {
		for _, is := range group {
			parent := parentOf(is)
			if parent == "" {
				continue
			}
			if !epics[parent] {
				continue
			}
			key := is.ID + "\x00" + parent
			if seen[key] {
				continue
			}
			seen[key] = true
			total[parent]++
			if is.Status == "closed" {
				closed[parent]++
			}
		}
	}
	count := 0
	for id := range epics {
		if total[id] > 0 && closed[id] == total[id] {
			count++
		}
	}
	return count
}
