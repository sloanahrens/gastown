package daemon

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/cloudledger"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/plugin"
)

// The cloud-check plugin fails on every run while the cloud patrol's report
// holds an urgent finding, and the operator closes the escalation each time it
// pages: without a key that changes with the finding set the next run minted a
// fresh escalation and the attention queue never drained (gt-ggd75).
//
// The key is the sorted set of urgent finding ids. Before changing it, read
// the cloud-check paragraph of docs/design/plugin-system.md: it states the
// refresh, supersede and reopen rule once, and this file and
// escalatePluginFailure implement it. The exit code and the dashboard pill are
// untouched: this is the escalation path only.
const (
	// cloudCheckPlugin is the script plugin whose nonzero exit reports the
	// cloud patrol's urgent findings.
	cloudCheckPlugin = "cloud-check"

	// cloudReportFile is the patrol's report inside the reports directory.
	cloudReportFile = "report.json"

	// cloudReportMaxBytes refuses an oversized report before reading it, so a
	// file that is not the patrol's own cannot be read into memory to be
	// rejected. The patrol's own reports run a few kilobytes.
	cloudReportMaxBytes = cloudledger.ReportMaxBytes

	// cloudUrgent is the report's worst severity: the findings at it are the
	// ones the check fails over.
	cloudUrgent = "urgent"

	// cloudUnreadableKey is the finding set a run that cannot read the report
	// is keyed by. It gets its own escalation rather than landing on the plain
	// plugin key, so a report this daemon could not read is one item an
	// operator closes once, and it comes back only after the reopen window.
	cloudUnreadableKey = "unreadable-report"

	// cloudUnidentifiedKey is the finding set a report whose urgent findings
	// carry nothing to key them by gets. It is separate from the unreadable
	// report's key: the report was read, and the findings in it are still
	// named in the escalation's kinds and counts.
	cloudUnidentifiedKey = "findings-without-identity"

	// cloudNotConfiguredKey keys a failure while the town's cloud block is
	// present but empty, so an operator reads that the step is off rather than
	// that its report is broken.
	cloudNotConfiguredKey = "cloud-step-not-configured"

	// cloudKindMax caps a finding kind in the escalation text, in runes.
	cloudKindMax = 40

	// pluginEscalationReopenWindow is how long a closed escalation keeps its
	// finding set from raising another one.
	pluginEscalationReopenWindow = 24 * time.Hour
)

// cloudFinding is the part of one report finding the escalation needs. The
// report is another process's output, so every field is read as untrusted text.
type cloudFinding struct {
	ID       string `json:"ID"`
	Kind     string `json:"Kind"`
	Project  string `json:"Project"`
	Resource string `json:"Resource"`
	Severity string `json:"Severity"`
}

// identity is the finding's stable id: its own ID, else the fields that name
// the resource when a report carries no ID. A finding with neither still counts
// in the summary; only the key leaves it out.
func (f cloudFinding) identity() string {
	if id := strings.TrimSpace(f.ID); id != "" {
		return id
	}
	kind, project, resource := strings.TrimSpace(f.Kind), strings.TrimSpace(f.Project), strings.TrimSpace(f.Resource)
	if kind == "" && project == "" && resource == "" {
		return ""
	}
	return strings.Join([]string{kind, project, resource}, "\x00")
}

// cloudReport is the report's finding list. The other fields belong to the
// dashboard and are not this reader's business.
type cloudReport struct {
	Findings []cloudFinding `json:"Findings"`
}

// readCloudReport reads the patrol's report from dir, refusing anything that is
// not a small regular file.
func readCloudReport(dir string) (cloudReport, error) {
	return readCloudReportFrom(cloudledger.NewFileSource(dir, ""))
}

// readCloudReportWith is readCloudReport with the file read injected, so a
// test can drive the unreadable-file branch on a file it cannot make
// unreadable.
func readCloudReportWith(dir string, readFile func(string) ([]byte, error)) (cloudReport, error) {
	return readCloudReportFrom(cloudledger.NewFileSourceWithRead(dir, "", readFile))
}

// readCloudReportFrom reads the report from the cloud step's source.
func readCloudReportFrom(s cloudledger.Source) (cloudReport, error) {
	rep, err := s.LatestPatrolReport(context.Background())
	if err != nil {
		return cloudReport{}, err
	}
	out := cloudReport{Findings: make([]cloudFinding, 0, len(rep.Findings))}
	for _, f := range rep.Findings {
		out.Findings = append(out.Findings, cloudFinding{
			ID: f.ID, Kind: f.Kind, Project: f.Project, Resource: f.Resource, Severity: f.Severity,
		})
	}
	return out, nil
}

// urgentFindingIDs returns the sorted, deduplicated identities of the report's
// urgent findings and the count per kind. An empty id set with a non-empty kind
// count means every urgent finding lacked an identity to key on.
func urgentFindingIDs(rep cloudReport) (ids []string, kinds map[string]int) {
	kinds = map[string]int{}
	seen := map[string]bool{}
	for _, f := range rep.Findings {
		if !strings.EqualFold(strings.TrimSpace(f.Severity), cloudUrgent) {
			continue
		}
		id := f.identity()
		if id != "" && seen[id] {
			continue // a repeat names no new finding
		}
		kinds[findingKind(f.Kind)]++
		if id == "" {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, kinds
}

// findingKind is the kind as the escalation shows it: control characters
// dropped (the escalation reason becomes a bead description) and capped.
func findingKind(kind string) string {
	kind = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(kind))
	if kind == "" {
		return "unknown"
	}
	if runes := []rune(kind); len(runes) > cloudKindMax {
		kind = string(runes[:cloudKindMax]) + "…"
	}
	return kind
}

// findingSetKey is the alert key for one plugin failure scoped to a finding
// set: the same set keeps the same key, so a repeat refreshes the open
// escalation, and a different set gets its own.
func findingSetKey(pluginName string, ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return fmt.Sprintf("%s:%x", pluginFailureAlertKey(pluginName), sum[:6])
}

// findingSummary names the findings behind the failure by kind and count, so
// the escalation reports the set rather than only a total.
func findingSummary(kinds map[string]int) string {
	total := 0
	for _, n := range kinds {
		total += n
	}
	names := make([]string, 0, len(kinds))
	for kind := range kinds {
		names = append(names, kind)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, kind := range names {
		parts = append(parts, fmt.Sprintf("%s x%d", kind, kinds[kind]))
	}
	return fmt.Sprintf("cloud patrol findings making the check fail: %d urgent (%s)", total, strings.Join(parts, ", "))
}

// cloudCheckAlert returns the alert key and reason summary for p's failure.
// Both are empty for a plugin that is not the cloud check, whose plain plugin
// key is its own. A report that cannot be read is a finding set of its own
// (cloudUnreadableKey), so a broken report does not hide behind — or collide
// with — the key of a real finding set.
func (d *Daemon) cloudCheckAlert(p *plugin.Plugin) (key, summary string) {
	if p == nil || p.Name != cloudCheckPlugin {
		return "", ""
	}
	source, err := d.cloudSource()
	if err != nil {
		d.logger.Printf("Handler: cloud-check: %v; keying the escalation by the unreadable report", err)
		return pluginFailureAlertKey(p.Name) + ":" + cloudUnreadableKey, cloudUnreadableSummary(err)
	}
	defer d.closeCloudSource(source)
	rep, err := readCloudReportFrom(source)
	if err != nil {
		if errors.Is(err, cloudledger.ErrNotConfigured) {
			d.logger.Printf("Handler: cloud-check: %s", config.CloudNotConfigured)
			return pluginFailureAlertKey(p.Name) + ":" + cloudNotConfiguredKey, config.CloudNotConfigured
		}
		d.logger.Printf("Handler: cloud-check: %v; keying the escalation by the unreadable report", err)
		return pluginFailureAlertKey(p.Name) + ":" + cloudUnreadableKey, cloudUnreadableSummary(err)
	}
	ids, kinds := urgentFindingIDs(rep)
	if len(ids) == 0 {
		if len(kinds) == 0 {
			return "", ""
		}
		// Urgent findings that name nothing to key them by still raise their
		// own escalation, and it names their kinds and counts: a finding must
		// not disappear because the report left its identity blank.
		return pluginFailureAlertKey(p.Name) + ":" + cloudUnidentifiedKey, findingSummary(kinds)
	}
	return findingSetKey(p.Name, ids), findingSummary(kinds)
}

// cloudSource is the cloud step's reader: the test's file directory when one is
// injected, the source the town settings select otherwise. A settings file that
// cannot be read is an error: reading the default directories instead would
// read a step the town may have turned off.
func (d *Daemon) cloudSource() (cloudledger.Source, error) {
	if d.cloudReportsDir != nil {
		return cloudledger.NewFileSource(d.cloudReportsDir(), ""), nil
	}
	settings, err := config.LoadTownSettings(config.TownSettingsPath(d.config.TownRoot))
	if err != nil {
		return nil, fmt.Errorf("reading town settings: %w", err)
	}
	return cloudledger.NewSource(settings.ResolveCloud(os.Getenv)), nil
}

// closeCloudSource releases the source's database pool when it holds one. The
// read it followed has already answered, so a close error is only logged.
func (d *Daemon) closeCloudSource(s cloudledger.Source) {
	c, ok := s.(io.Closer)
	if !ok {
		return
	}
	if err := c.Close(); err != nil {
		d.logger.Printf("Handler: cloud-check: closing the cloud source: %v", err)
	}
}

// cloudUnreadableSummary is the reason an unreadable report is escalated with.
// The error names the file or the source; a Postgres source has already removed
// the DSN from it.
func cloudUnreadableSummary(err error) string {
	return "cloud report unreadable: " + err.Error()
}

// The reasons the daemon closes a plugin escalation with itself. A close that
// carries one of them is the daemon tidying its own escalation, not an operator
// who read the failure and closed it, so it never counts toward the reopen
// window: a set that failed, was superseded, and failed again must page again
// (gt-ggd75).
const (
	pluginCloseSupersededPrefix = "superseded by "
	pluginCloseCleared          = "cleared: the plugin passed"
)

// daemonClosedEscalation reports whether an escalation was closed by the
// daemon's own tidying (a superseded set, or a plugin that passed).
func daemonClosedEscalation(reason string) bool {
	return strings.HasPrefix(reason, pluginCloseSupersededPrefix) || reason == pluginCloseCleared
}

// pluginEscalationClosedRecently reports whether the escalation for key was
// closed by an operator less than pluginEscalationReopenWindow ago, so a failure
// the operator already closed does not page again until the window passes
// (gt-ggd75). A close the daemon made itself (daemonClosedEscalation) is not an
// operator's and does not suppress.
//
// An open escalation under key is a refresh, not a suppression. A read that
// fails raises rather than suppresses: an unread escalation must not hide a
// finding.
func (d *Daemon) pluginEscalationClosedRecently(key string) bool {
	label := notify.FingerprintLabel(key)
	if label == "" {
		return false
	}
	opts := beads.ListOptions{
		Label:        "gt:escalation",
		Labels:       []string{label},
		Status:       "all",
		IncludeInfra: true,
		Priority:     -1,
	}
	bd := d.escalationBeads()
	issues, err := bd.List(opts)
	if err != nil {
		d.logger.Printf("Handler: reading escalation %s: %v; raising anyway", key, err)
		return false
	}
	if len(issues) == 0 {
		// Escalations are wisps. bd's --include-infra reads both planes in one
		// call; a client that keeps one plane per List (the test fake) needs
		// the second (see beads.listEscalationsWhere).
		opts.Ephemeral = true
		issues, err = bd.List(opts)
		if err != nil {
			d.logger.Printf("Handler: reading escalation %s (wisps): %v; raising anyway", key, err)
			return false
		}
	}
	now := d.clk().Now()
	var newest time.Time
	for _, is := range issues {
		if !beads.IsEscalationRecord(is) {
			continue
		}
		if !beads.IssueStatus(is.Status).IsTerminal() {
			return false
		}
		if daemonClosedEscalation(is.CloseReason) {
			continue
		}
		if t := beads.ParseIssueTime(is.ClosedAt); t.After(newest) {
			newest = t
		}
	}
	return !newest.IsZero() && now.Sub(newest) < pluginEscalationReopenWindow
}
