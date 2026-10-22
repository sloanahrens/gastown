package report

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/daemonlog"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/util"
	"github.com/steveyegge/gastown/internal/version"
)

// reportHourWindow is the span gt report --hour summarizes: the window is the
// hour ending at --at (default now), and every source is cut to it.
const reportHourWindow = time.Hour

// reportSpendFreshness bounds the age of .runtime/watch/spend.json. The
// overseer's health watch rewrites it every 10 minutes; a file older than
// this is not a current reading, so the spend section is left out rather
// than reported as if it were live.
const reportSpendFreshness = 15 * time.Minute

// reportGitTimeout bounds each git call the mains section makes. ls-remote
// talks to the remote, so it needs a ceiling the read-only file sources do
// not.
const reportGitTimeout = 30 * time.Second

// reportSectionOrder is the fixed order of the sections gt report --hour
// prints. Every section prints its heading whether or not it has rows, so a
// quiet hour reads the same shape as a busy one; spend is the one section
// that is left out whole (see reportSpendFreshness).
var reportSectionOrder = []string{
	"mains",
	"landings",
	"rejections by kind",
	"queue depth",
	"seats",
	"sweep",
	"escalations",
	"spend",
	"attention",
}

// BeadStore is the read-only beads surface gt report uses for one store:
// the plain client queries, plus the one filtered query beads.ListOptions
// cannot express — the beads whose notes gained a MERGE REJECTION block since
// a time. gt report never calls a write method.
type BeadStore interface {
	beads.Client
	RejectedSince(since time.Time) ([]*beads.Issue, error)
}

// bdReportStore is the production BeadStore: a beads client pinned to one
// store's beads directory, plus bd's note-filtered list.
type bdReportStore struct {
	*beads.Beads
}

// RejectedSince asks bd for the issues carrying a MERGE REJECTION note that
// were updated at or after since. bd filters on the note text, so the reply
// is only the beads a rejection touched; --updated-after is the window, and
// --status=all reaches the ones already closed after their rejection.
func (s bdReportStore) RejectedSince(since time.Time) ([]*beads.Issue, error) {
	out, err := s.Beads.Run("list",
		"--notes-contains", land.MergeRejectionNoteMarker,
		"--updated-after", since.UTC().Format(time.RFC3339),
		"--status=all", "--json", "-n", "0")
	if err != nil {
		return nil, fmt.Errorf("bd list --notes-contains: %w", err)
	}
	var issues []*beads.Issue
	if err := json.Unmarshal(jsonArrayFrom(out), &issues); err != nil {
		return nil, fmt.Errorf("parsing the rejection list: %w", err)
	}
	return issues, nil
}

// OpenStore opens one store's beads client: "hq" is the town store,
// anything else a registered rig.
func OpenStore(townRoot, store string) (BeadStore, error) {
	dir, err := StoreDir(townRoot, store)
	if err != nil {
		return nil, err
	}
	return WrapStore(beads.NewWithBeadsDir(townRoot, dir)), nil
}

// StoreDir is the beads directory of one store, "hq" or a registered rig.
func StoreDir(townRoot, store string) (string, error) {
	dir := doltserver.FindRigBeadsDir(townRoot, store)
	if dir == "" {
		return "", fmt.Errorf("no beads directory for %s", store)
	}
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("beads directory %s: %w", dir, err)
	}
	return dir, nil
}

// WrapStore adds the note-filtered rejection list to a beads client, for a
// caller that builds its own client (the daemon pins bd read-only).
func WrapStore(b *beads.Beads) BeadStore { return bdReportStore{Beads: b} }

// HourRun is one hour report: the window, the collaborators it reads through,
// and where it writes. Unit tests build one against a fixture town and a fake
// store, so no production bd or network is touched.
type HourRun struct {
	TownRoot string
	Loc      *time.Location
	Until    time.Time
	Out      io.Writer

	// Version and Commit identify the installed gt build; the mains section
	// compares the source checkout's origin/main against Commit.
	Version string
	Commit  string

	// Rigs lists the registered rigs.
	Rigs func() ([]string, error)
	// Store opens one store's beads client (OpenStore in production).
	Store func(store string) (BeadStore, error)
	// MainTip reads origin/main's tip without fetching and counts the
	// commits the installed build is behind it (MainTip in production). The
	// reading carries the tip and the count; collectMains adds the installed
	// build's identity.
	MainTip func(installed string) (Mains, error)
}

// NewHourRun returns a run over the town's own sources for the hour ending at
// until: bd stores from OpenStore and origin/main from git ls-remote. Out is
// left for the caller.
func NewHourRun(townRoot string, until time.Time, version, commit string, rigs func() ([]string, error)) HourRun {
	return HourRun{
		TownRoot: townRoot,
		Loc:      time.Local,
		Until:    until,
		Version:  version,
		Commit:   commit,
		Rigs:     rigs,
		Store:    func(store string) (BeadStore, error) { return OpenStore(townRoot, store) },
		MainTip:  func(installed string) (Mains, error) { return reportMainTip(townRoot, installed) },
	}
}

// reportMainTip reads origin/main's tip for the town's gt source checkout
// with git ls-remote — never a fetch, because the canonical clone at
// <town>/<rig>/mayor/rig is what install-gt builds from and fetching it
// behind the installer's back races the build — and counts the commits the
// installed build commit is behind it. The returned reading carries the tip
// and the count, not the installed build's identity; the caller adds that.
func reportMainTip(townRoot, installed string) (Mains, error) {
	repo, err := version.GetRepoRootForTown(townRoot)
	if err != nil {
		return Mains{}, err
	}
	sha, err := git.NewGit(repo).RemoteBranchTip("origin", "main")
	if err != nil {
		return Mains{}, err
	}
	if sha == "" {
		return Mains{}, errors.New("origin has no main branch")
	}
	m := Mains{SHA: sha}
	behind, ref, ok := reportBehind(
		installed, sha,
		func(from, to string) (int, bool) { return reportCommitsBetween(repo, from, to) },
		func() (string, bool) { return reportGitRev(repo, "origin/main") },
	)
	m.CommitsBehind, m.BehindRef, m.BehindUnknown = behind, ref, !ok
	return m, nil
}

// reportBehind decides how far installed is behind main's remote tip. It
// counts against the tip first; when the tip's objects are not in the local
// store — nobody has fetched since it moved — it counts against this clone's
// own origin/main ref instead and returns that ref, so the answer is the
// fetch-old count with its ref named rather than "unknown" every time main
// moves in a busy town.
//
// count reports how many commits are reachable from to but not from; ok is
// false when git cannot count them. localRef resolves this clone's
// origin/main. An unknown count is ok=false with no ref.
func reportBehind(installed, remoteTip string,
	count func(from, to string) (int, bool),
	localRef func() (string, bool),
) (behind int, ref string, ok bool) {
	if installed == "" {
		return 0, "", false
	}
	if n, counted := count(installed, remoteTip); counted {
		return n, "", true
	}
	local, resolved := localRef()
	if !resolved || local == remoteTip {
		return 0, "", false
	}
	if n, counted := count(installed, local); counted {
		return n, local, true
	}
	return 0, "", false
}

// reportCommitsBetween returns how many commits are reachable from to but not
// from from. ok is false when git cannot count them, which is what happens
// when to names an object this repository does not have.
func reportCommitsBetween(repo, from, to string) (int, bool) {
	out, ok := reportGit(repo, "rev-list", "--count", from+".."+to)
	if !ok {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(out, "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}

// reportGitRev resolves ref to the commit it names.
func reportGitRev(repo, ref string) (string, bool) {
	return reportGit(repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
}

// reportGit runs one read-only git command in repo. ok is false when git
// could not run or exited non-zero.
func reportGit(repo string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), reportGitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repo
	// The same detached group internal/version gives its git calls: the
	// context is the only thing that ends this one, and it cannot end a
	// process the terminal's own signal took first.
	util.SetDetachedProcessGroup(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// --- the collected summary ---

// HourDoc is one gt report --hour: the window it covers and the ten
// sections, in reportSectionOrder. A section whose source could not be read
// names the reason in Unavailable instead of carrying data; a section that is
// absent from both is one whose sources were readable and empty.
type HourDoc struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`

	Unavailable map[string]string `json:"unavailable,omitempty"`

	Mains       *Mains             `json:"mains,omitempty"`
	Landings    []reportLanding    `json:"landings,omitempty"`
	Rejections  []reportKindCount  `json:"rejections,omitempty"`
	QueueDepth  []reportQueueDepth `json:"queue_depth,omitempty"`
	Seats       *reportSeats       `json:"seats,omitempty"`
	Sweep       []reportSweepRig   `json:"sweep,omitempty"`
	Escalations *reportEscalations `json:"escalations,omitempty"`
	Spend       *reportSpend       `json:"spend,omitempty"`
	Attention   *reportAttention   `json:"attention,omitempty"`
}

type Mains struct {
	SHA             string `json:"origin_main"`
	InstalledCommit string `json:"installed_commit,omitempty"`
	InstalledVer    string `json:"installed_version,omitempty"`
	CommitsBehind   int    `json:"commits_behind"`
	BehindUnknown   bool   `json:"behind_unknown,omitempty"`
	// BehindRef is the ref the count was made against when it is not the
	// remote tip: nobody had fetched the tip's objects, so the count came
	// from this clone's own origin/main ref instead.
	BehindRef string `json:"behind_ref,omitempty"`
}

type reportLanding struct {
	Bead      string     `json:"bead"`
	Rig       string     `json:"rig,omitempty"`
	Merged    *time.Time `json:"merged,omitempty"`
	Landed    *time.Time `json:"landed,omitempty"`
	Stages    string     `json:"stages,omitempty"`
	Gate      string     `json:"gate,omitempty"`
	OMVerdict string     `json:"om_verdict,omitempty"`
	OMScore   float64    `json:"om_score,omitempty"`
	Route     string     `json:"route,omitempty"`
	Rejected  string     `json:"rejected_kind,omitempty"`
}

type reportKindCount struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}

type reportQueueDepth struct {
	Rig         string `json:"rig"`
	Count       int    `json:"count"`
	Unavailable string `json:"unavailable,omitempty"`
}

type reportSeats struct {
	IntentRecords  int             `json:"intent_records"`
	IntentRun      int             `json:"intent_run"`
	RefillEpisodes int             `json:"refill_episodes"`
	Episodes       []reportEpisode `json:"episodes,omitempty"`
}

type reportEpisode struct {
	Seat       string    `json:"seat"`
	EmptySince time.Time `json:"empty_since"`
}

type reportSweepRig struct {
	Rig         string            `json:"rig"`
	NoRecord    bool              `json:"no_sweep_record,omitempty"`
	Unavailable string            `json:"unavailable,omitempty"`
	Tiers       []reportSweepTier `json:"tiers,omitempty"`
	LastSHA     string            `json:"last_sha,omitempty"`
	LastRun     *time.Time        `json:"last_run,omitempty"`
}

type reportSweepTier struct {
	Tier        string   `json:"tier"`
	Verdict     string   `json:"verdict"`
	Passed      int      `json:"passed"`
	Failed      int      `json:"failed"`
	FailedNames []string `json:"failed_names,omitempty"`
	// SHA is the tip this tier's verdict was taken at. It is the record's own
	// last_sha for a tier the last cycle ran, and an earlier tip for a tier
	// that cycle never reached (gt-euxf1).
	SHA string `json:"sha,omitempty"`
}

type reportEscalations struct {
	Opened int `json:"opened"`
	Closed int `json:"closed"`
}

type reportSpend struct {
	PerHour float64   `json:"per_hour"`
	Balance float64   `json:"balance"`
	At      time.Time `json:"at"`
}

type reportAttention struct {
	Open    int                   `json:"open"`
	High    int                   `json:"high"`
	Stale   bool                  `json:"stale,omitempty"`
	Updated time.Time             `json:"updated,omitempty"`
	Items   []reportAttentionItem `json:"items,omitempty"`
}

type reportAttentionItem struct {
	Key       string    `json:"key"`
	Kind      string    `json:"kind"`
	Severity  string    `json:"severity"`
	Rig       string    `json:"rig,omitempty"`
	Bead      string    `json:"bead,omitempty"`
	SHA       string    `json:"sha,omitempty"`
	Summary   string    `json:"summary"`
	FirstSeen time.Time `json:"first_seen"`
}

// Collect reads every source and assembles the summary. No source failure
// escapes: each one becomes that section's Unavailable reason, so the command
// always prints a full report and always exits 0.
func (r HourRun) Collect() *HourDoc {
	doc := &HourDoc{
		Since: r.Until.Add(-reportHourWindow),
		Until: r.Until,
		// Non-nil only when a section is missing, so the JSON omits the key
		// entirely on a clean run.
		Unavailable: map[string]string{},
	}
	rigs, rigsErr := r.Rigs()
	if rigsErr != nil {
		rigs = nil
	}
	r.collectMains(doc)
	r.collectLandings(doc, rigs, rigsErr)
	r.collectStores(doc, rigs, rigsErr)
	r.collectSeats(doc)
	r.collectSweep(doc, rigs, rigsErr)
	r.collectSpend(doc)
	r.collectAttention(doc)
	if len(doc.Unavailable) == 0 {
		doc.Unavailable = nil
	}
	return doc
}

// collectMains reads origin/main's tip and the installed build's distance
// from it, and adds the installed build's identity.
func (r HourRun) collectMains(doc *HourDoc) {
	m, err := r.MainTip(r.Commit)
	if err != nil {
		doc.setUnavailable("mains", err)
		return
	}
	m.InstalledCommit = r.Commit
	m.InstalledVer = r.Version
	doc.Mains = &m
}

// reportLandLineRE finds a "[land] <bead>: <rest>" line in a daemon.log
// line's text, which the landing worker prefixes with "landing_worker: ".
var reportLandLineRE = regexp.MustCompile(`\[land\] (\S+): (.+)$`)

// collectLandings builds one row per landing from the daemon's [land] lines
// for the window, then fills each row's verdict fields from the rigs'
// landings files. The daemon.log is read through internal/daemonlog, the
// parser gt tail reads it with, so rotation, backups and the local-time stamp
// are handled the same way. An unreadable log makes the section unavailable
// rather than a quiet hour with no landings.
func (r HourRun) collectLandings(doc *HourDoc, rigs []string, rigsErr error) {
	lines, err := daemonlog.Read(filepath.Join(r.TownRoot, "daemon"), doc.Since, r.Loc)
	if err != nil {
		doc.setUnavailable("landings", err)
		return
	}
	rows := map[string]*reportLanding{}
	order := []string{}
	for _, line := range lines {
		if line.At.Before(doc.Since) || line.At.After(doc.Until) {
			continue
		}
		m := reportLandLineRE.FindStringSubmatch(line.Text)
		if m == nil {
			continue
		}
		bead, rest, at := m[1], m[2], line.At
		if strings.HasPrefix(rest, "rejected (") {
			// A rejection is not a landing; it only annotates the row of a
			// bead that was merged in this window. The kinds are tallied
			// from the beads' notes, in the rejections section.
			if row, ok := rows[bead]; ok {
				row.Rejected = reportRejectionKind(rest)
			}
			continue
		}
		row, ok := rows[bead]
		if !ok {
			row = &reportLanding{Bead: bead}
			rows[bead] = row
			order = append(order, bead)
		}
		switch {
		case strings.HasPrefix(rest, "merged "):
			if row.Merged == nil {
				t := at
				row.Merged = &t
			}
		case strings.HasPrefix(rest, "landed "), strings.HasPrefix(rest, "already landed as "):
			if row.Landed == nil {
				t := at
				row.Landed = &t
			}
		case strings.HasPrefix(rest, "stages: "):
			row.Stages = rest
		}
	}
	order = r.fillLandingRecords(rows, order, doc, rigs, rigsErr)

	var out []reportLanding
	for _, bead := range order {
		row := rows[bead]
		if row.Merged == nil && row.Landed == nil {
			// Only a rejection mentioned this bead in the window: it is a
			// rejection, not a landing, and the rejection section counts it.
			continue
		}
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := reportLandingTime(out[i]), reportLandingTime(out[j])
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return out[i].Bead < out[j].Bead
	})
	doc.Landings = out
}

// reportRejectionKind reads the failure class out of a
// "[land] <bead>: rejected (<kind>): <reason>" line.
func reportRejectionKind(rest string) string {
	inner := strings.TrimPrefix(rest, "rejected (")
	if i := strings.Index(inner, ")"); i >= 0 {
		if kind := inner[:i]; kind != "" {
			return kind
		}
	}
	// An unclassified rejection (land's empty failure class).
	return "unclassified"
}

// fillLandingRecords joins each rig's landings file into the rows the daemon
// log produced, and adds rows for records the log no longer holds (a rotated
// backup the poll did not reach, or a daemon.log that lost the line). It
// returns the row order with those additions appended.
//
// A rig registry that cannot be read costs the join only: the landings the
// daemon log holds are still reported, without their om verdicts.
func (r HourRun) fillLandingRecords(rows map[string]*reportLanding, order []string, doc *HourDoc, rigs []string, rigsErr error) []string {
	if rigsErr != nil {
		return order
	}
	for _, rig := range rigs {
		path, err := landings.Path(r.TownRoot, rig)
		if err != nil {
			continue
		}
		recs, _, err := (&landings.Reader{Path: path}).ReadNew()
		if err != nil {
			continue
		}
		for _, rec := range recs {
			if rec.LandedAt.Before(doc.Since) || rec.LandedAt.After(doc.Until) {
				continue
			}
			row, ok := rows[rec.Bead]
			if !ok {
				t := rec.LandedAt
				row = &reportLanding{Bead: rec.Bead, Landed: &t}
				rows[rec.Bead] = row
				order = append(order, rec.Bead)
			}
			row.Rig = rig
			if rec.OMVerdict != "" {
				row.OMVerdict = rec.OMVerdict
				row.OMScore = rec.OMScore
			}
			row.Gate = rec.GateResult
			row.Route = rec.Route
		}
	}
	return order
}

// reportLandingTime is a row's display time: the land when it has one, the
// merge otherwise.
func reportLandingTime(l reportLanding) time.Time {
	if l.Landed != nil {
		return *l.Landed
	}
	if l.Merged != nil {
		return *l.Merged
	}
	return time.Time{}
}

// collectStores fills the three sections that read beads: rejections by kind,
// queue depth per rig, and escalations opened and closed.
func (r HourRun) collectStores(doc *HourDoc, rigs []string, rigsErr error) {
	stores := append([]string{"hq"}, rigs...)
	if rigsErr != nil {
		doc.setUnavailable("rejections by kind", rigsErr)
		doc.setUnavailable("queue depth", rigsErr)
		doc.setUnavailable("escalations", rigsErr)
		return
	}

	rejections := map[string]int{}
	rejErr := error(nil)
	opened, closed := 0, 0
	escErr := error(nil)
	for _, store := range stores {
		s, err := r.Store(store)
		if err != nil {
			if rejErr == nil {
				rejErr = err
			}
			if escErr == nil {
				escErr = err
			}
			continue
		}
		issues, err := s.RejectedSince(doc.Since)
		if err != nil {
			if rejErr == nil {
				rejErr = err
			}
		} else {
			for _, is := range issues {
				note, ok := land.ParseRejectionNote(is.Notes)
				if !ok {
					continue
				}
				kind := note.Kind
				if kind == "" {
					// The note's unclassified class (land's empty kind).
					kind = "unclassified"
				}
				rejections[kind]++
			}
		}
		escs, err := s.List(beads.ListOptions{Label: "gt:escalation", Status: "all", IncludeInfra: true, Priority: -1})
		if err != nil {
			if escErr == nil {
				escErr = err
			}
			continue
		}
		for _, is := range escs {
			if !beads.IsEscalationRecord(is) {
				continue
			}
			if t, ok := reportParseBeadTime(is.CreatedAt); ok && !t.Before(doc.Since) && !t.After(doc.Until) {
				opened++
			}
			if t, ok := reportParseBeadTime(is.ClosedAt); ok && !t.Before(doc.Since) && !t.After(doc.Until) {
				closed++
			}
		}
	}
	if rejErr != nil {
		doc.setUnavailable("rejections by kind", rejErr)
	} else {
		doc.Rejections = reportKindCounts(rejections)
	}
	if escErr != nil {
		doc.setUnavailable("escalations", escErr)
	} else {
		doc.Escalations = &reportEscalations{Opened: opened, Closed: closed}
	}

	for _, rig := range rigs {
		row := reportQueueDepth{Rig: rig}
		s, err := r.Store(rig)
		switch {
		case err != nil:
			row.Unavailable = err.Error()
		default:
			issues, err := s.List(beads.ListOptions{Label: land.LabelReadyToLand, Priority: -1})
			if err != nil {
				row.Unavailable = err.Error()
				break
			}
			for _, is := range issues {
				if !beads.HasLabel(is, land.LabelReadyToLand) {
					continue
				}
				if beads.IssueStatus(strings.TrimSpace(is.Status)).IsActionable() {
					row.Count++
				}
			}
		}
		doc.QueueDepth = append(doc.QueueDepth, row)
	}
}

// reportKindCounts sorts a kind tally by kind.
func reportKindCounts(counts map[string]int) []reportKindCount {
	out := make([]reportKindCount, 0, len(counts))
	for kind, n := range counts {
		out = append(out, reportKindCount{Kind: kind, Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// reportParseBeadTime reads a bd timestamp, which is RFC3339 in UTC.
func reportParseBeadTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// collectSeats counts the live seat records under .runtime/agents/ and the
// refill episodes the seat-refill plugin records.
func (r HourRun) collectSeats(doc *HourDoc) {
	seats := &reportSeats{}
	dir := filepath.Join(constants.TownRuntimePath(r.TownRoot), "agents")
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		rec, _ := intent.ReadPath(path)
		seats.IntentRecords++
		if rec.Desired == intent.DesiredRun {
			seats.IntentRun++
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		doc.setUnavailable("seats", fmt.Errorf("reading %s: %w", dir, err))
		return
	}

	episodes, err := r.refillEpisodes()
	switch {
	case err != nil:
		doc.setUnavailable("seats", err)
		return
	default:
		seats.Episodes = episodes
		seats.RefillEpisodes = len(episodes)
	}
	doc.Seats = seats
}

// reportSeatRefillPath is the seat-refill plugin's state file, at the path the
// plugin writes when nothing overrides it.
func reportSeatRefillPath(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "seat-refill.json")
}

// reportRefillState is .runtime/seat-refill.json as plugins/seat-refill/run.sh
// writes it: one episode per seat that is empty, keyed by seat name.
type reportRefillState struct {
	Version  int                          `json:"version"`
	Episodes map[string]reportRefillEntry `json:"episodes"`
}

type reportRefillEntry struct {
	EmptySince int64 `json:"empty_since"` // unix seconds
	LastNudge  int64 `json:"last_nudge"`  // unix seconds
}

// refillEpisodes reads the refill plugin's episodes, oldest empty first. A
// missing file is no episodes, not an error: the plugin may never have run.
func (r HourRun) refillEpisodes() ([]reportEpisode, error) {
	data, err := os.ReadFile(reportSeatRefillPath(r.TownRoot))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading seat refill state: %w", err)
	}
	var st reportRefillState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parsing seat refill state: %w", err)
	}
	out := make([]reportEpisode, 0, len(st.Episodes))
	for seat, ep := range st.Episodes {
		at := time.Time{}
		if ep.EmptySince > 0 {
			at = time.Unix(ep.EmptySince, 0)
		}
		out = append(out, reportEpisode{Seat: seat, EmptySince: at})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].EmptySince.Equal(out[j].EmptySince) {
			return out[i].EmptySince.Before(out[j].EmptySince)
		}
		return out[i].Seat < out[j].Seat
	})
	return out, nil
}

// reportTierSweepPath is one rig's tier-sweep record, which the daemon's
// hourly sweep job writes (gt-vsct7.5).
func reportTierSweepPath(townRoot, rig string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "tier-sweep", rig+".json")
}

// reportTierSweepFile is the sweep record's shape as gt-vsct7.5 specifies it.
// gt report reads it as a whole record; fields the sweep does not write yet
// stay zero.
type reportTierSweepFile struct {
	LastRun      time.Time                   `json:"last_run"`
	LastSHA      string                      `json:"last_sha"`
	LastGreenSHA string                      `json:"last_green_sha"`
	Tiers        map[string]reportTierResult `json:"tiers"`
}

type reportTierResult struct {
	Verdict     string   `json:"verdict"`
	Passed      int      `json:"passed"`
	Failed      int      `json:"failed"`
	FailedNames []string `json:"failed_names"`
	// SHA is the tip the verdict was taken at. A record written before the
	// field carries none, and the record's own last_sha answers for it.
	SHA string `json:"sha,omitempty"`
}

// collectSweep reads each rig's tier-sweep record. A rig with no record says
// so: until the daemon's sweep job has run there is nothing to report, which
// is not a failure.
func (r HourRun) collectSweep(doc *HourDoc, rigs []string, rigsErr error) {
	if rigsErr != nil {
		doc.setUnavailable("sweep", rigsErr)
		return
	}
	for _, rig := range rigs {
		row := reportSweepRig{Rig: rig}
		data, err := os.ReadFile(reportTierSweepPath(r.TownRoot, rig))
		switch {
		case errors.Is(err, os.ErrNotExist):
			row.NoRecord = true
		case err != nil:
			row.Unavailable = err.Error()
		default:
			var f reportTierSweepFile
			if err := json.Unmarshal(data, &f); err != nil {
				row.Unavailable = err.Error()
				break
			}
			row.LastSHA = f.LastSHA
			if !f.LastRun.IsZero() {
				t := f.LastRun
				row.LastRun = &t
			}
			for tier, res := range f.Tiers {
				// A tier with no sha of its own is one an older daemon wrote;
				// the record's tip is the only date such a verdict has.
				sha := res.SHA
				if sha == "" {
					sha = f.LastSHA
				}
				row.Tiers = append(row.Tiers, reportSweepTier{
					Tier:        tier,
					Verdict:     res.Verdict,
					Passed:      res.Passed,
					Failed:      res.Failed,
					FailedNames: res.FailedNames,
					SHA:         sha,
				})
			}
			sort.Slice(row.Tiers, func(i, j int) bool { return row.Tiers[i].Tier < row.Tiers[j].Tier })
		}
		doc.Sweep = append(doc.Sweep, row)
	}
}

// reportSpendPath is the spend reading the overseer's health watch rewrites
// every 10 minutes.
func reportSpendPath(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "watch", "spend.json")
}

// reportSpendFile is .runtime/watch/spend.json as the health watch's emit.sh
// writes it.
type reportSpendFile struct {
	TS      time.Time `json:"ts"`
	PerHour float64   `json:"per_hour"`
	Balance float64   `json:"balance"`
}

// collectSpend reads the DeepSeek spend reading, and leaves the section out
// when the file is missing or older than reportSpendFreshness: a stale
// reading reported without its age is worse than no reading. A file that is
// present but unreadable or unparseable is reported as unavailable, so a
// broken writer is visible.
func (r HourRun) collectSpend(doc *HourDoc) {
	path := reportSpendPath(r.TownRoot)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		doc.setUnavailable("spend", fmt.Errorf("reading %s: %w", path, err))
		return
	}
	var f reportSpendFile
	if err := json.Unmarshal(data, &f); err != nil {
		doc.setUnavailable("spend", fmt.Errorf("parsing %s: %w", path, err))
		return
	}
	at := f.TS
	if at.IsZero() {
		info, statErr := os.Stat(path)
		if statErr != nil {
			doc.setUnavailable("spend", fmt.Errorf("stat %s: %w", path, statErr))
			return
		}
		at = info.ModTime()
	}
	if r.Until.Sub(at) >= reportSpendFreshness {
		return
	}
	doc.Spend = &reportSpend{PerHour: f.PerHour, Balance: f.Balance, At: at}
}

// collectAttention reads the daemon's attention state. A state.json that is
// not there is unavailable, not empty: until the daemon's attention job runs
// (gt-vsct7.2), no queue exists, and an empty queue and an unwritten one must
// not read the same.
func (r HourRun) collectAttention(doc *HourDoc) {
	path := attention.StatePath(r.TownRoot)
	if _, err := os.Stat(path); err != nil {
		doc.setUnavailable("attention", err)
		return
	}
	state, err := attention.ReadState(r.TownRoot)
	if err != nil {
		doc.setUnavailable("attention", err)
		return
	}
	out := &reportAttention{
		Updated: state.Updated,
		Stale:   attention.Stale(state, r.Until, attention.StaleAfter),
	}
	for _, item := range state.Items {
		if item.AckedAt != nil {
			continue
		}
		out.Open++
		if item.Severity == attention.SeverityHigh {
			out.High++
		}
		out.Items = append(out.Items, reportAttentionItem{
			Key:       item.Key,
			Kind:      string(item.Kind),
			Severity:  string(item.Severity),
			Rig:       item.Rig,
			Bead:      item.Bead,
			SHA:       item.SHA,
			Summary:   item.Summary,
			FirstSeen: item.FirstSeen,
		})
	}
	sort.Slice(out.Items, func(i, j int) bool {
		if !out.Items[i].FirstSeen.Equal(out.Items[j].FirstSeen) {
			return out.Items[i].FirstSeen.Before(out.Items[j].FirstSeen)
		}
		return out.Items[i].Key < out.Items[j].Key
	})
	doc.Attention = out
}

// setUnavailable records why a section is missing. The first reason wins:
// later failures in the same section are consequences of the first.
func (d *HourDoc) setUnavailable(section string, err error) {
	if d.Unavailable == nil {
		d.Unavailable = map[string]string{}
	}
	if _, ok := d.Unavailable[section]; ok {
		return
	}
	d.Unavailable[section] = err.Error()
}

// --- rendering ---

// Render prints the plain-text report: the window, then each section in
// reportSectionOrder. It writes no color, so a pipe and a golden file read the
// same.
func (r HourRun) Render(doc *HourDoc) {
	fmt.Fprintf(r.Out, "hour ending %s (%s)\n",
		doc.Until.In(r.Loc).Format("2006-01-02 15:04:05 -0700"), reportHourWindow)
	for _, name := range reportSectionOrder {
		// spend is the one section that is absent rather than empty when its
		// source is not fresh.
		if name == "spend" && doc.Spend == nil {
			if _, unavailable := doc.Unavailable[name]; !unavailable {
				continue
			}
		}
		fmt.Fprintf(r.Out, "\n%s\n", name)
		if reason, ok := doc.Unavailable[name]; ok {
			r.row("unavailable (%s)", reason)
			continue
		}
		r.renderSection(name, doc)
	}
}

// renderSection writes one section's rows, or "(none)" when it is readable
// and empty.
func (r HourRun) renderSection(name string, doc *HourDoc) {
	switch name {
	case "mains":
		m := doc.Mains
		behind := "up to date"
		switch {
		case m.BehindUnknown:
			behind = "commits behind unknown (the remote tip is not in the local object store)"
		case m.CommitsBehind == 1:
			behind = "1 commit behind"
		case m.CommitsBehind > 1:
			behind = fmt.Sprintf("%d commits behind", m.CommitsBehind)
		}
		if m.BehindRef != "" {
			behind += fmt.Sprintf(" (counted against origin/main at %s: the remote tip is not fetched)", shortHash(m.BehindRef))
		}
		r.row("origin/main %s  installed gt %s (%s)  %s",
			shortHash(m.SHA), m.InstalledVer, reportOrDash(shortHash(m.InstalledCommit)), behind)
	case "landings":
		if len(doc.Landings) == 0 {
			r.row("(none)")
			return
		}
		for _, l := range doc.Landings {
			r.row("%s  merged %s  landed %s%s%s%s",
				l.Bead,
				r.stamp(l.Merged),
				r.stamp(l.Landed),
				reportOMField(l),
				reportStagesField(l.Stages),
				reportRejectedField(l.Rejected))
		}
	case "rejections by kind":
		if len(doc.Rejections) == 0 {
			r.row("(none)")
			return
		}
		for _, k := range doc.Rejections {
			r.row("%s  %d", k.Kind, k.Count)
		}
	case "queue depth":
		if len(doc.QueueDepth) == 0 {
			r.row("(none)")
			return
		}
		for _, q := range doc.QueueDepth {
			if q.Unavailable != "" {
				r.row("%s  unavailable (%s)", q.Rig, q.Unavailable)
				continue
			}
			r.row("%s  %d ready-to-land", q.Rig, q.Count)
		}
	case "seats":
		r.row("intent records %d (%d run)", doc.Seats.IntentRecords, doc.Seats.IntentRun)
		if len(doc.Seats.Episodes) == 0 {
			r.row("refill episodes 0")
			return
		}
		for _, ep := range doc.Seats.Episodes {
			r.row("refill episode %s  empty %s", ep.Seat, Age(r.Until.Sub(ep.EmptySince)))
		}
	case "sweep":
		for _, s := range doc.Sweep {
			switch {
			case s.NoRecord:
				r.row("%s  no sweep record", s.Rig)
			case s.Unavailable != "":
				r.row("%s  unavailable (%s)", s.Rig, s.Unavailable)
			case len(s.Tiers) == 0:
				r.row("%s  sweep record has no tiers  sha %s", s.Rig, reportOrDash(shortHash(s.LastSHA)))
			default:
				for _, tier := range s.Tiers {
					// The tier's own tip: a cycle that did not reach a tier
					// leaves the earlier verdict, and dating it by the last
					// cycle's tip would name a commit nothing judged it at
					// (gt-euxf1).
					r.row("%s  %s %s  %d passed, %d failed%s  sha %s  %s",
						s.Rig, tier.Tier, reportOrDash(tier.Verdict), tier.Passed, tier.Failed,
						reportFailedNames(tier.FailedNames), reportOrDash(shortHash(tier.SHA)), r.tierAge(s, tier))
				}
			}
		}
	case "escalations":
		r.row("opened %d  closed %d", doc.Escalations.Opened, doc.Escalations.Closed)
	case "spend":
		r.row("DeepSeek $%.2f/h  balance $%.2f  (%s old)",
			doc.Spend.PerHour, doc.Spend.Balance, Age(r.Until.Sub(doc.Spend.At)))
	case "attention":
		r.row("%d open (%d high)  state %s old", doc.Attention.Open, doc.Attention.High,
			Age(r.Until.Sub(doc.Attention.Updated)))
		if doc.Attention.Stale {
			r.row("state is stale (older than %s): the daemon has stopped writing it", attention.StaleAfter)
		}
		if doc.Attention.Open == 0 {
			r.row("(none)")
			return
		}
		for _, item := range doc.Attention.Items {
			r.row("%s  %s  %s  %s  %s",
				item.Severity, item.Kind, reportAttentionRef(item),
				Age(r.Until.Sub(item.FirstSeen)), item.Summary)
		}
	}
}

// row writes one indented row.
func (r HourRun) row(format string, args ...any) {
	fmt.Fprintf(r.Out, "  ")
	fmt.Fprintf(r.Out, format, args...)
	fmt.Fprintln(r.Out)
}

// stamp renders an optional event time in the operator's zone.
func (r HourRun) stamp(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.In(r.Loc).Format("15:04:05")
}

// sweptAge renders when a sweep ran, relative to the window end.
func (r HourRun) sweptAge(t *time.Time) string {
	if t == nil {
		return "run time unknown"
	}
	return Age(r.Until.Sub(*t)) + " ago"
}

// tierAge dates a tier's verdict only when the last cycle ran it: an empty
// last_sha dates nothing, so two missing SHAs must not match (gt-euxf1).
func (r HourRun) tierAge(s reportSweepRig, tier reportSweepTier) string {
	if s.LastSHA == "" || tier.SHA != s.LastSHA {
		return "run time unknown, not in last cycle"
	}
	return r.sweptAge(s.LastRun)
}

func reportOMField(l reportLanding) string {
	if l.OMVerdict == "" {
		return ""
	}
	return fmt.Sprintf("  om %s/%.2f", l.OMVerdict, l.OMScore)
}

func reportStagesField(stages string) string {
	if stages == "" {
		return ""
	}
	return "  " + stages
}

func reportRejectedField(kind string) string {
	if kind == "" {
		return ""
	}
	return "  rejected (" + kind + ")"
}

func reportFailedNames(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return " (" + strings.Join(names, ", ") + ")"
}

func reportAttentionRef(item reportAttentionItem) string {
	for _, ref := range []string{item.Bead, item.SHA, item.Rig, item.Key} {
		if ref != "" {
			return ref
		}
	}
	return "-"
}

// Age renders a duration at second resolution; a negative age (a
// source stamped after the window's end) reads as 0s rather than a negative
// duration.
func Age(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}

func reportOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// shortHash returns at most 8 characters of a hash for display, so an
// abbreviated hash shorter than that does not panic.
func shortHash(hash string) string {
	if len(hash) > 8 {
		return hash[:8]
	}
	return hash
}

// jsonArrayFrom drops the non-JSON prefix (warnings, notices) a bd subprocess
// may print before its payload, returning data unchanged when it holds no '['.
func jsonArrayFrom(data []byte) []byte {
	if idx := bytes.IndexByte(data, '['); idx >= 0 {
		return data[idx:]
	}
	return data
}
