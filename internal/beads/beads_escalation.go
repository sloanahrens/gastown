// Package beads provides escalation bead management.
package beads

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The escalation helpers are free functions over Client (gt-7iwy0.4.5): every
// caller passes the Client it already opens, so a *Beads and any other Client
// (internal/beads/beadsfake in unit tests) run the same logic on the Client
// surface. Where a *Beads has a bd capability Client deliberately does not
// expose — the stdin-bodied wisp create below, and bd's cross-database prefix
// routing — the *Beads-only helper sits next to the free function that reaches
// for it.

// EscalationFields holds structured fields for escalation beads.
// These are stored as "key: value" lines in the description.
type EscalationFields struct {
	Severity          string // critical, high, medium, low
	Reason            string // Why this was escalated
	Source            string // Source identifier (e.g., plugin:rebuild-gt, patrol:deacon)
	EscalatedBy       string // Agent address that escalated (e.g., "gastown/Toast")
	EscalatedAt       string // ISO 8601 timestamp
	AckedBy           string // Agent that acknowledged (empty if not acked)
	AckedAt           string // When acknowledged (empty if not acked)
	ClosedBy          string // Agent that closed (empty if not closed)
	ClosedReason      string // Resolution reason (empty if not closed)
	RelatedBead       string // Optional: related bead ID (task, bug, etc.)
	OriginalSeverity  string // Original severity before any re-escalation
	ReescalationCount int    // Number of times this has been re-escalated
	LastReescalatedAt string // When last re-escalated (empty if never)
	LastReescalatedBy string // Who last re-escalated (empty if never)
	Fingerprint       string // Stable duplicate-suppression label
	// Occurrences counts how many times this alert has fired. A recurring
	// alert (jsonl spike, main-branch test failure) bumps this instead of
	// minting a new bead per firing (gt-vwry).
	Occurrences int
	// LastSeenAt is when the alert most recently fired, i.e. when
	// Occurrences was last bumped (empty if never re-fired).
	LastSeenAt string
	// LastNotifiedAt is when a notification (mail/email/sms/slack/log) was
	// last actually sent for this alert. Set at creation, then again each
	// time a repeat firing re-notifies after the renotify window elapses
	// (empty only for a bead created before this field existed).
	LastNotifiedAt string
}

// FormatEscalationDescription creates a description string from escalation fields.
func FormatEscalationDescription(title string, fields *EscalationFields) string {
	if fields == nil {
		return title
	}

	var lines []string
	lines = append(lines, title)
	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("severity: %s", fields.Severity))
	lines = append(lines, fmt.Sprintf("reason: %s", fields.Reason))
	if fields.Source != "" {
		lines = append(lines, fmt.Sprintf("source: %s", fields.Source))
	} else {
		lines = append(lines, "source: null")
	}
	lines = append(lines, fmt.Sprintf("escalated_by: %s", fields.EscalatedBy))
	lines = append(lines, fmt.Sprintf("escalated_at: %s", fields.EscalatedAt))

	if fields.AckedBy != "" {
		lines = append(lines, fmt.Sprintf("acked_by: %s", fields.AckedBy))
	} else {
		lines = append(lines, "acked_by: null")
	}

	if fields.AckedAt != "" {
		lines = append(lines, fmt.Sprintf("acked_at: %s", fields.AckedAt))
	} else {
		lines = append(lines, "acked_at: null")
	}

	if fields.ClosedBy != "" {
		lines = append(lines, fmt.Sprintf("closed_by: %s", fields.ClosedBy))
	} else {
		lines = append(lines, "closed_by: null")
	}

	if fields.ClosedReason != "" {
		lines = append(lines, fmt.Sprintf("closed_reason: %s", fields.ClosedReason))
	} else {
		lines = append(lines, "closed_reason: null")
	}

	if fields.RelatedBead != "" {
		lines = append(lines, fmt.Sprintf("related_bead: %s", fields.RelatedBead))
	} else {
		lines = append(lines, "related_bead: null")
	}

	// Reescalation fields
	if fields.OriginalSeverity != "" {
		lines = append(lines, fmt.Sprintf("original_severity: %s", fields.OriginalSeverity))
	} else {
		lines = append(lines, "original_severity: null")
	}
	lines = append(lines, fmt.Sprintf("reescalation_count: %d", fields.ReescalationCount))
	if fields.LastReescalatedAt != "" {
		lines = append(lines, fmt.Sprintf("last_reescalated_at: %s", fields.LastReescalatedAt))
	} else {
		lines = append(lines, "last_reescalated_at: null")
	}
	if fields.LastReescalatedBy != "" {
		lines = append(lines, fmt.Sprintf("last_reescalated_by: %s", fields.LastReescalatedBy))
	} else {
		lines = append(lines, "last_reescalated_by: null")
	}
	if fields.Fingerprint != "" {
		lines = append(lines, fmt.Sprintf("fingerprint: %s", fields.Fingerprint))
	} else {
		lines = append(lines, "fingerprint: null")
	}

	lines = append(lines, fmt.Sprintf("occurrences: %d", fields.Occurrences))
	if fields.LastSeenAt != "" {
		lines = append(lines, fmt.Sprintf("last_seen_at: %s", fields.LastSeenAt))
	} else {
		lines = append(lines, "last_seen_at: null")
	}
	if fields.LastNotifiedAt != "" {
		lines = append(lines, fmt.Sprintf("last_notified_at: %s", fields.LastNotifiedAt))
	} else {
		lines = append(lines, "last_notified_at: null")
	}

	return strings.Join(lines, "\n")
}

// ParseEscalationFields extracts escalation fields from an issue's description.
func ParseEscalationFields(description string) *EscalationFields {
	fields := &EscalationFields{}

	for _, line := range strings.Split(description, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		colonIdx := strings.Index(line, ":")
		if colonIdx == -1 {
			continue
		}

		key := strings.TrimSpace(line[:colonIdx])
		value := strings.TrimSpace(line[colonIdx+1:])
		if value == "null" || value == "" {
			value = ""
		}

		switch strings.ToLower(key) {
		case "severity":
			fields.Severity = value
		case "reason":
			fields.Reason = value
		case "source":
			fields.Source = value
		case "escalated_by":
			fields.EscalatedBy = value
		case "escalated_at":
			fields.EscalatedAt = value
		case "acked_by":
			fields.AckedBy = value
		case "acked_at":
			fields.AckedAt = value
		case "closed_by":
			fields.ClosedBy = value
		case "closed_reason":
			fields.ClosedReason = value
		case "related_bead":
			fields.RelatedBead = value
		case "original_severity":
			fields.OriginalSeverity = value
		case "reescalation_count":
			if n, err := strconv.Atoi(value); err == nil {
				fields.ReescalationCount = n
			}
		case "last_reescalated_at":
			fields.LastReescalatedAt = value
		case "last_reescalated_by":
			fields.LastReescalatedBy = value
		case "fingerprint":
			fields.Fingerprint = value
		case "occurrences":
			if n, err := strconv.Atoi(value); err == nil {
				fields.Occurrences = n
			}
		case "last_seen_at":
			fields.LastSeenAt = value
		case "last_notified_at":
			fields.LastNotifiedAt = value
		}
	}

	return fields
}

// escalationPriority is bd's priority for a bead created without --priority,
// which is what the escalation create has always passed.
const escalationPriority = 2

// escalationLabels are the labels an escalation bead carries: the family
// label, plus its severity and its fingerprint when it has one.
func escalationLabels(fields *EscalationFields) []string {
	labels := []string{"gt:escalation"}
	if fields != nil && fields.Severity != "" {
		labels = append(labels, "severity:"+fields.Severity)
	}
	if fields != nil && fields.Fingerprint != "" {
		labels = append(labels, fields.Fingerprint)
	}
	return labels
}

// CreateEscalationBead creates an escalation bead for tracking escalations.
// The created_by field is populated from the creating client's actor for
// provenance tracking.
//
// A *Beads files it as an ephemeral wisp through bd's stdin (see
// createEscalationWisp): bd 1.0.3+ rejects a newline inside --description, and
// every escalation description is multi-line. Another Client creates through
// its own Create, which is the seam it has.
func CreateEscalationBead(c Client, title string, fields *EscalationFields) (*Issue, error) {
	// Guard against flag-like titles (gt-e0kx5: --help garbage beads)
	if IsFlagLikeTitle(title) {
		return nil, fmt.Errorf("refusing to create escalation bead: %w (got %q)", ErrFlagTitle, title)
	}

	labelList := escalationLabels(fields)
	description := FormatEscalationDescription(title, fields)

	if b, ok := c.(*Beads); ok {
		return createEscalationWisp(b, title, description, labelList)
	}

	return c.Create(CreateOptions{
		Title:       title,
		Description: description,
		Labels:      labelList,
		Priority:    escalationPriority,
		Ephemeral:   true,
	})
}

// createEscalationWisp is the *Beads create for an escalation: an ephemeral
// wisp whose description goes to bd through stdin.
//
// The description travels via stdin (--body-file=-) rather than as a
// --description= value, because bd rejects a newline inside a flag value and
// every escalation description is multi-line (dc-1bxe). --wisp-type=escalation
// keeps the bead out of bd list and bd ready while it is open and gives it the
// escalation TTL under gt compact. Client.Create exposes neither the stdin body
// nor the wisp type, which is why this stays a *Beads-only helper.
func createEscalationWisp(b *Beads, title, description string, labels []string) (*Issue, error) {
	args := []string{"create", "--json",
		"--title=" + title,
		"--body-file=-",
		"--type=task",
		"--ephemeral",
		"--wisp-type=escalation",
	}
	for _, label := range labels {
		args = append(args, "--labels="+label)
	}

	// Default actor from BD_ACTOR env var for provenance tracking
	// Uses getActor() to respect isolated mode (tests)
	if actor := b.getActor(); actor != "" {
		args = append(args, "--actor="+actor)
	}

	out, err := b.runWithStdin([]byte(description), args...)
	if err != nil {
		return nil, err
	}

	var issue Issue
	if err := json.Unmarshal(out, &issue); err != nil {
		return nil, fmt.Errorf("parsing bd create output: %w", err)
	}

	return &issue, nil
}

// AckEscalation acknowledges an escalation bead.
// Sets acked_by and acked_at fields, adds "acked" label.
func AckEscalation(c Client, id, ackedBy string) error {
	// First get current issue to preserve other fields
	issue, err := c.Show(id)
	if err != nil {
		return err
	}

	// Verify it's an escalation
	if !HasLabel(issue, "gt:escalation") {
		return fmt.Errorf("issue %s is not an escalation bead (missing gt:escalation label)", id)
	}

	// Parse existing fields
	fields := ParseEscalationFields(issue.Description)
	fields.AckedBy = ackedBy
	fields.AckedAt = time.Now().Format(time.RFC3339)

	// Format new description
	description := FormatEscalationDescription(issue.Title, fields)

	return c.Update(id, UpdateOptions{
		Description: &description,
		AddLabels:   []string{"acked"},
	})
}

// CloseEscalation closes an escalation bead with a resolution reason.
// Sets closed_by and closed_reason fields, closes the issue.
func CloseEscalation(c Client, id, closedBy, reason string) error {
	// First get current issue to preserve other fields
	issue, err := c.Show(id)
	if err != nil {
		return err
	}

	// Verify it's an escalation
	if !HasLabel(issue, "gt:escalation") {
		return fmt.Errorf("issue %s is not an escalation bead (missing gt:escalation label)", id)
	}

	// Parse existing fields
	fields := ParseEscalationFields(issue.Description)
	fields.ClosedBy = closedBy
	fields.ClosedReason = reason

	// Format new description
	description := FormatEscalationDescription(issue.Title, fields)

	// Update description first
	if err := c.Update(id, UpdateOptions{
		Description: &description,
		AddLabels:   []string{"resolved"},
	}); err != nil {
		return err
	}

	// Close the issue
	return c.CloseWithReason(reason, id)
}

// BumpEscalation records another firing of an already-open escalation instead
// of minting a new bead for it (gt-vwry).
//
// A recurring alert — a jsonl export spike, a main-branch test failure, a
// state-collapse condition — fires on every patrol cycle it still holds. Before
// this, each firing ran the full create path, so a condition that persisted for
// a night left a dozen identical P0/P1 records behind. Occurrences and
// LastSeenAt make the recurrence legible on the one bead that represents the
// alert, which is the same bead `gt escalate clear` later closes when the
// condition clears.
//
// Severity, reason and source are refreshed to the latest firing when
// non-empty: an alert that escalates in severity, or whose latest evidence
// differs from the first firing's, should say so.
//
// Before this only the bead was ever touched: a repeat firing never re-sent
// its routed notifications, so a condition that recurred for days notified a
// human exactly once, at creation, and then went silent forever no matter how
// long it persisted (gt-9qg1). BumpEscalation now also decides whether this
// firing should re-notify: renotifyWindow is the minimum time between two
// notifications, and the second return value tells the caller to actually
// re-send mail/email/sms/slack/log this time. The decision and the
// last_notified_at write happen together with the occurrence bump so the two
// can never drift apart into "marked notified but nothing was sent" or vice
// versa.
//
// Returns the new occurrence count and whether this firing should re-notify.
func BumpEscalation(c Client, id, severity, reason, source string, renotifyWindow time.Duration) (occurrences int, renotify bool, err error) {
	issue, err := c.Show(id)
	if err != nil {
		return 0, false, err
	}
	if !HasLabel(issue, "gt:escalation") {
		return 0, false, fmt.Errorf("issue %s is not an escalation bead (missing gt:escalation label)", id)
	}

	fields := ParseEscalationFields(issue.Description)
	renotify = ShouldRenotify(fields, renotifyWindow)

	// An escalation created before gt-vwry has no occurrences line at all;
	// its first bump is its second firing, so seed from the fields rather
	// than assuming the bead's own creation was counted.
	if fields.Occurrences == 0 {
		fields.Occurrences = 1
	}
	fields.Occurrences++
	fields.LastSeenAt = time.Now().Format(time.RFC3339)
	if severity != "" {
		fields.Severity = severity
	}
	if reason != "" {
		fields.Reason = reason
	}
	if source != "" {
		fields.Source = source
	}
	if renotify {
		fields.LastNotifiedAt = time.Now().Format(time.RFC3339)
	}

	description := FormatEscalationDescription(issue.Title, fields)
	if err := c.Update(id, UpdateOptions{Description: &description}); err != nil {
		return 0, false, err
	}
	return fields.Occurrences, renotify, nil
}

// ShouldRenotify reports whether a firing of an existing escalation should
// re-send its routed notifications rather than staying silent. A firing
// re-notifies once at least window has elapsed since the alert's last
// notification; a bead with no recorded LastNotifiedAt (created before this
// field existed) falls back to its original EscalatedAt. A window of zero
// re-notifies on every firing, and a bead with no timestamp to measure from
// at all always re-notifies rather than risk staying silent.
func ShouldRenotify(fields *EscalationFields, window time.Duration) bool {
	if fields == nil {
		return true
	}
	last := fields.LastNotifiedAt
	if last == "" {
		last = fields.EscalatedAt
	}
	if last == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, last)
	if err != nil {
		return true
	}
	return time.Since(t) >= window
}

// CloseEscalationsByFingerprints closes every open escalation carrying the given
// fingerprint label and reports the IDs it closed.
//
// This is the "auto-close on clear" half of gt-vwry: the producer that raised a
// keyed alert re-checks its condition on the next cycle and clears the key when
// the condition no longer holds (branch merged, main green, spike gone). A key
// that matches nothing is not an error — that is the ordinary case for a
// producer that calls clear on every healthy cycle.
func CloseEscalationsByFingerprints(c Client, fingerprintLabels []string, closedBy, reason string) ([]string, error) {
	wanted := make(map[string]bool, len(fingerprintLabels))
	for _, label := range fingerprintLabels {
		if label != "" {
			wanted[label] = true
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}

	// One open-escalation listing serves every key: a producer clearing its
	// whole owned set on a healthy cycle must not cost one query per key.
	open, err := ListEscalations(c)
	if err != nil {
		return nil, err
	}

	var closed []string
	var errs []error
	for _, issue := range open {
		if !matchesAnyLabel(issue, wanted) {
			continue
		}
		if err := CloseEscalation(c, issue.ID, closedBy, reason); err != nil {
			errs = append(errs, fmt.Errorf("closing %s: %w", issue.ID, err))
			continue
		}
		closed = append(closed, issue.ID)
	}
	if len(errs) > 0 {
		return closed, errors.Join(errs...)
	}
	return closed, nil
}

// CloseEscalationsByFingerprint is the single-key form of
// CloseEscalationsByFingerprints.
func CloseEscalationsByFingerprint(c Client, fingerprintLabel, closedBy, reason string) ([]string, error) {
	return CloseEscalationsByFingerprints(c, []string{fingerprintLabel}, closedBy, reason)
}

func matchesAnyLabel(issue *Issue, wanted map[string]bool) bool {
	for _, label := range issue.Labels {
		if wanted[label] {
			return true
		}
	}
	return false
}

// GetEscalationBead retrieves an escalation bead by ID.
// Returns nil if not found.
func GetEscalationBead(c Client, id string) (*Issue, *EscalationFields, error) {
	issue, err := c.Show(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil, nil
		}
		return nil, nil, err
	}

	if !HasLabel(issue, "gt:escalation") {
		return nil, nil, fmt.Errorf("issue %s is not an escalation bead (missing gt:escalation label)", id)
	}

	fields := ParseEscalationFields(issue.Description)
	return issue, fields, nil
}

// ListEscalations returns all open escalation beads in the client's database.
//
// Escalations are created as ephemeral wisps (gt-fcsf), which `bd list`
// hides by default. Without --include-infra this silently returned zero
// results while escalations sat open and unseen — the same bug class as
// gt-4mnd.
//
// This is deliberately single-database: ListEscalations is not only a display
// path: ListStaleEscalations consumes it to reescalate and send mail, and
// ListEscalationsByFingerprint consumes the same query shape for the duplicate
// suppression in runEscalate. Widening those to every rig would change what
// "stale" and "duplicate" mean for two mutating flows. For the display path see
// ListEscalationsAcrossRigs, which is cross-rig by design.
func ListEscalations(c Client) ([]*Issue, error) {
	issues, err := listEscalationsWhere(c, ListOptions{
		Label:        "gt:escalation",
		Status:       string(StatusOpen),
		IncludeInfra: true,
		Priority:     -1,
	})
	if err != nil {
		return nil, err
	}
	return filterEscalationRecords(issues), nil
}

// listEscalationsWhere is the read behind every escalation listing: the issues
// and the wisps matching opts, in one slice.
//
// A *Beads answers it in one bd call, because bd's --include-infra returns the
// persistent issues and the wisps together. Any other Client (beadsfake) keeps
// one plane per List, so the wisp read is a second call merged here — the same
// shape ListMergeRequests uses. A failed wisp read degrades to the issues
// alone.
func listEscalationsWhere(c Client, opts ListOptions) ([]*Issue, error) {
	issues, err := c.List(opts)
	if err != nil {
		return nil, err
	}
	if _, ok := c.(*Beads); ok {
		return issues, nil
	}

	wispOpts := opts
	wispOpts.Ephemeral = true
	wisps, _ := c.List(wispOpts)

	seen := make(map[string]bool, len(issues))
	for _, issue := range issues {
		seen[issue.ID] = true
	}
	for _, wisp := range wisps {
		if !seen[wisp.ID] {
			issues = append(issues, wisp)
		}
	}
	return issues, nil
}

// ListEscalationsAcrossRigs returns all open escalation beads visible to bd's
// cross-rig prefix routing, rather than only those in the current database.
//
// This is the query behind `gt escalate list`. Escalations created inside a rig
// (whose beads carry that rig's prefix, e.g. gt-*) live in that rig's database,
// so a query pinned to a single BEADS_DIR silently hides them and the command
// answers "No escalations found" while escalations sit open — the symptom of
// gt-wbxb (refiled from hq-f1f2y).
//
// Kept separate from ListEscalations, which the mutating escalation flows rely
// on being local; see the note there. Message carriers (gt:message) are
// excluded, matching the open-only display path.
func ListEscalationsAcrossRigs(c Client) ([]*Issue, error) {
	return listEscalationsAcrossRigs(c, string(StatusOpen))
}

// ListAllEscalationsAcrossRigs is ListEscalationsAcrossRigs without the
// --status=open filter, for `gt escalate list --all`. It keeps gt:message
// carriers, matching that flag's long-standing output.
func ListAllEscalationsAcrossRigs(c Client) ([]*Issue, error) {
	return listEscalationsAcrossRigs(c, "all")
}

// listEscalationsAcrossRigs reads the escalations at status across databases.
//
// A *Beads runs the query through bd's own prefix routing; another Client has
// only its List, which is one database. The two are not equivalent — Client.List
// pins BEADS_DIR to the client's database, so it answers with that one
// database's beads while the routed call lets bd resolve from the working
// directory — which is why this keeps the routed path for a *Beads rather than
// quietly narrowing the display view (gt-wbxb).
func listEscalationsAcrossRigs(c Client, status string) ([]*Issue, error) {
	if b, ok := c.(*Beads); ok {
		return listEscalationsRouted(b, status)
	}
	return listEscalationsAtStatus(c, status)
}

// listEscalationsRouted is the *Beads cross-database read: bd's native prefix
// routing via routes.jsonl, without the BEADS_DIR pin run/runWithStdin apply.
func listEscalationsRouted(b *Beads, status string) ([]*Issue, error) {
	out, err := b.runWithRouting("list", "--label=gt:escalation", "--status="+status, "--include-infra", "--json")
	if err != nil {
		return nil, err
	}

	var issues []*Issue
	if err := json.Unmarshal(out, &issues); err != nil {
		return nil, fmt.Errorf("parsing bd list output: %w", err)
	}

	if status == string(StatusOpen) {
		return filterEscalationRecords(issues), nil
	}
	return issues, nil
}

// listEscalationsAtStatus is the single-database read any Client can run.
func listEscalationsAtStatus(c Client, status string) ([]*Issue, error) {
	issues, err := listEscalationsWhere(c, ListOptions{
		Label:        "gt:escalation",
		Status:       status,
		IncludeInfra: true,
		Priority:     -1,
	})
	if err != nil {
		return nil, err
	}

	if status == string(StatusOpen) {
		return filterEscalationRecords(issues), nil
	}
	return issues, nil
}

// ListEscalationsByFingerprint returns open escalation beads matching a stable fingerprint label.
//
// Single-database by design: this backs the duplicate suppression in
// runEscalate, which must agree with where the escalation would be created.
func ListEscalationsByFingerprint(c Client, fingerprintLabel string) ([]*Issue, error) {
	if fingerprintLabel == "" {
		return nil, nil
	}
	issues, err := listEscalationsWhere(c, ListOptions{
		Label:        "gt:escalation",
		Labels:       []string{fingerprintLabel},
		Status:       string(StatusOpen),
		IncludeInfra: true,
		Priority:     -1,
	})
	if err != nil {
		return nil, err
	}

	return filterEscalationRecords(issues), nil
}

// ListEscalationsBySeverity returns open escalation beads filtered by severity.
//
// Single-database: no caller needs a cross-rig severity view. A display path
// that does should query ListEscalationsAcrossRigs and filter locally.
func ListEscalationsBySeverity(c Client, severity string) ([]*Issue, error) {
	issues, err := listEscalationsWhere(c, ListOptions{
		Label:        "gt:escalation",
		Labels:       []string{"severity:" + severity},
		Status:       string(StatusOpen),
		IncludeInfra: true,
		Priority:     -1,
	})
	if err != nil {
		return nil, err
	}

	return filterEscalationRecords(issues), nil
}

func filterEscalationRecords(issues []*Issue) []*Issue {
	filtered := issues[:0]
	for _, issue := range issues {
		if IsEscalationRecord(issue) {
			filtered = append(filtered, issue)
		}
	}
	return filtered
}

// IsEscalationRecord reports whether issue is an escalation rather than one of
// the mail carriers routed for it.
//
// A carrier carries gt:escalation so ack and close can find it (see
// mail.Router.buildLabels), and outlives the escalation it delivered.
// filterEscalationRecords and the town-health escalation field share this
// predicate so `gt status --line` and `gt escalate list` agree on the age of
// the oldest open escalation (gt-9k2bx).
func IsEscalationRecord(issue *Issue) bool {
	return issue != nil && HasLabel(issue, "gt:escalation") && !HasLabel(issue, "gt:message")
}

// ListStaleEscalations returns escalations older than the given threshold.
// threshold is a duration string like "1h" or "30m".
func ListStaleEscalations(c Client, threshold time.Duration) ([]*Issue, error) {
	// Get all open escalations
	escalations, err := ListEscalations(c)
	if err != nil {
		return nil, err
	}

	cutoff := time.Now().Add(-threshold)
	var stale []*Issue

	for _, issue := range escalations {
		// Skip acknowledged escalations
		if HasLabel(issue, "acked") {
			continue
		}

		// Check if older than threshold
		createdAt, err := time.Parse(time.RFC3339, issue.CreatedAt)
		if err != nil {
			continue // Skip if can't parse
		}

		if createdAt.Before(cutoff) {
			stale = append(stale, issue)
		}
	}

	return stale, nil
}

// ReescalationResult holds the result of a reescalation operation.
type ReescalationResult struct {
	ID              string
	Title           string
	OldSeverity     string
	NewSeverity     string
	ReescalationNum int
	Skipped         bool
	SkipReason      string
}

// ReescalateEscalation bumps the severity of an escalation and updates tracking fields.
// Returns the new severity if successful, or an error.
// reescalatedBy should be the identity of the agent/process doing the reescalation.
// maxReescalations limits how many times an escalation can be bumped (0 = unlimited).
func ReescalateEscalation(c Client, id, reescalatedBy string, maxReescalations int) (*ReescalationResult, error) {
	// Get the escalation
	issue, fields, err := GetEscalationBead(c, id)
	if err != nil {
		return nil, err
	}
	if issue == nil {
		return nil, fmt.Errorf("escalation not found: %s", id)
	}

	result := &ReescalationResult{
		ID:          id,
		Title:       issue.Title,
		OldSeverity: fields.Severity,
	}

	// Check if already at max reescalations
	if maxReescalations > 0 && fields.ReescalationCount >= maxReescalations {
		result.Skipped = true
		result.SkipReason = fmt.Sprintf("already at max reescalations (%d)", maxReescalations)
		return result, nil
	}

	// Check if already at critical (can't bump further)
	if fields.Severity == "critical" {
		result.Skipped = true
		result.SkipReason = "already at critical severity"
		result.NewSeverity = "critical"
		return result, nil
	}

	// Save original severity on first reescalation
	if fields.OriginalSeverity == "" {
		fields.OriginalSeverity = fields.Severity
	}

	// Bump severity
	newSeverity := bumpSeverity(fields.Severity)
	fields.Severity = newSeverity
	fields.ReescalationCount++
	fields.LastReescalatedAt = time.Now().Format(time.RFC3339)
	fields.LastReescalatedBy = reescalatedBy

	result.NewSeverity = newSeverity
	result.ReescalationNum = fields.ReescalationCount

	// Format new description
	description := FormatEscalationDescription(issue.Title, fields)

	// Update the bead with new description and severity label
	if err := c.Update(id, UpdateOptions{
		Description:  &description,
		AddLabels:    []string{"reescalated", "severity:" + newSeverity},
		RemoveLabels: []string{"severity:" + result.OldSeverity},
	}); err != nil {
		return nil, fmt.Errorf("updating escalation: %w", err)
	}

	return result, nil
}

// FindLatestIssueByTitleAndAssignee finds the newest issue matching the given
// title and assignee, in any Client.
//
// Client.List has no title filter, so this lists the assignee's open issues and
// keeps the exact title matches itself; it is the same set bd's --title would
// have returned for the same assignee, without a second query shape.
func FindLatestIssueByTitleAndAssignee(c Client, title, assignee string) (*Issue, error) {
	issues, err := c.List(ListOptions{Assignee: assignee, Status: string(StatusOpen), Priority: -1})
	if err != nil {
		return nil, fmt.Errorf("bd list: %w", err)
	}

	var newest *Issue
	for _, issue := range issues {
		if issue.Title != title || issue.Assignee != assignee {
			continue
		}
		if newest == nil || issue.CreatedAt > newest.CreatedAt {
			newest = issue
		}
	}
	if newest == nil {
		return nil, ErrNotFound
	}
	return newest, nil
}

// bumpSeverity returns the next higher severity level.
// low -> medium -> high -> critical
func bumpSeverity(severity string) string {
	switch severity {
	case "low":
		return "medium"
	case "medium":
		return "high"
	case "high":
		return "critical"
	default:
		return "critical"
	}
}
