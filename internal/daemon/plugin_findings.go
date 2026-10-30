package daemon

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/plugin"
)

// The cloud-check plugin fails on every run while the cloud patrol's report
// holds an urgent finding, and the operator closes the escalation each time it
// pages: without a key that changes with the finding set the next run minted a
// fresh escalation and the attention queue never drained (gt-ggd75).
//
// The key below is the sorted set of urgent finding ids, so an unchanged set
// refreshes the open escalation and a changed one raises a new escalation,
// closing the previous set's (see escalatePluginFailure in plugin_script.go).
// A set whose escalation the operator closed stays quiet for
// pluginEscalationReopenWindow, then pages again if it is still there. The
// exit code and the dashboard pill are untouched: this is the escalation path
// only.
const (
	// cloudCheckPlugin is the script plugin whose nonzero exit reports the
	// cloud patrol's urgent findings.
	cloudCheckPlugin = "cloud-check"

	// cloudReportFile is the patrol's report inside config.CloudReportsDir.
	cloudReportFile = "report.json"

	// cloudReportMaxBytes refuses an oversized report before reading it, so a
	// file that is not the patrol's own cannot be read into memory to be
	// rejected. The patrol's own reports run a few kilobytes.
	cloudReportMaxBytes = 256 << 10

	// cloudUrgent is the report's worst severity: the findings at it are the
	// ones the check fails over.
	cloudUrgent = "urgent"

	// cloudUnreadableKey is the finding set a run that cannot read the report
	// is keyed by. It gets its own escalation rather than landing on the plain
	// plugin key, so a report this daemon could not read is one item an
	// operator closes once, and it comes back only after the reopen window.
	cloudUnreadableKey = "unreadable-report"

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
	return readCloudReportWith(dir, os.ReadFile)
}

// readCloudReportWith is readCloudReport with the file read injected, so a
// test can drive the unreadable-file branch on a file it cannot make
// unreadable.
func readCloudReportWith(dir string, readFile func(string) ([]byte, error)) (cloudReport, error) {
	path := filepath.Join(dir, cloudReportFile)
	fi, err := os.Lstat(path)
	if err != nil {
		return cloudReport{}, err
	}
	if !fi.Mode().IsRegular() {
		return cloudReport{}, fmt.Errorf("%s: not a regular file", path)
	}
	if fi.Size() > cloudReportMaxBytes {
		return cloudReport{}, fmt.Errorf("%s: larger than %d bytes", path, cloudReportMaxBytes)
	}
	raw, err := readFile(path)
	if err != nil {
		return cloudReport{}, err
	}
	var rep cloudReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		return cloudReport{}, fmt.Errorf("%s: %w", path, err)
	}
	return rep, nil
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
	rep, err := readCloudReport(d.cloudReportDir())
	if err != nil {
		d.logger.Printf("Handler: cloud-check: %v; keying the escalation by the unreadable report", err)
		return pluginFailureAlertKey(p.Name) + ":" + cloudUnreadableKey, ""
	}
	ids, kinds := urgentFindingIDs(rep)
	if len(ids) == 0 {
		return "", ""
	}
	return findingSetKey(p.Name, ids), findingSummary(kinds)
}

// cloudReportDir is the patrol's reports directory: the test's own when one is
// injected, the directory config.CloudReportsDir names otherwise.
func (d *Daemon) cloudReportDir() string {
	if d.cloudReportsDir != nil {
		return d.cloudReportsDir()
	}
	return config.CloudReportsDir()
}

// pluginEscalationClosedRecently reports whether the escalation for key was
// closed less than pluginEscalationReopenWindow ago, so a failure the operator
// already closed does not page again until the window passes (gt-ggd75).
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
		if t := beads.ParseIssueTime(is.ClosedAt); t.After(newest) {
			newest = t
		}
	}
	return !newest.IsZero() && now.Sub(newest) < pluginEscalationReopenWindow
}
