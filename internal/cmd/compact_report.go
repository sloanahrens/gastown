package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
)

var (
	compactReportDryRun  bool
	compactReportWeekly  bool
	compactReportVerbose bool
	compactReportDate    string
	compactReportJSON    bool
)

// wispCategory maps individual wisp types to display categories.
// Matches the design doc: Heartbeats, Patrols, Errors, Untyped.
var wispCategoryMap = map[string]string{
	"heartbeat":  "Heartbeats",
	"ping":       "Heartbeats",
	"patrol":     "Patrols",
	"gc_report":  "Patrols",
	"error":      "Errors",
	"recovery":   "Errors",
	"escalation": "Errors",
}

// categoryOrder is the display order for categories in reports.
var categoryOrder = []string{"Heartbeats", "Patrols", "Errors", "Untyped"}

const zeroPatrolReportingGap = "0 eligible patrol wisps in the report query/window (patrol health not assessed)"

// categoryStats tracks per-category compaction statistics.
type categoryStats struct {
	Deleted  int `json:"deleted"`
	Promoted int `json:"promoted"`
	Active   int `json:"active"`
}

// compactReport is the full daily digest data.
type compactReport struct {
	Date       string                    `json:"date"`
	Categories map[string]*categoryStats `json:"categories"`
	Promotions []compactAction           `json:"promotions,omitempty"`
	Anomalies  []string                  `json:"anomalies,omitempty"`
	Errors     []string                  `json:"errors,omitempty"`
}

// weeklyRollup aggregates daily reports for trend data.
type weeklyRollup struct {
	WeekStart  string                    `json:"week_start"`
	WeekEnd    string                    `json:"week_end"`
	Days       int                       `json:"days"`
	Totals     map[string]*categoryStats `json:"totals"`
	Promotions int                       `json:"total_promotions"`
	Anomalies  []string                  `json:"anomalies,omitempty"`
}

var compactReportCmd = &cobra.Command{
	Use:   "report",
	Short: "Generate and send compaction digest report",
	Long: `Generate a compaction digest and send it to deacon/ (cc mayor/).

The daily digest shows per-category breakdown of deleted, promoted, and active
wisps, plus any promotions with reasons and detected anomalies.

The weekly rollup (--weekly) aggregates the past 7 days of compaction event
beads and sends trend data to mayor/.

Examples:
  gt compact report              # Run compaction + send daily digest
  gt compact report --dry-run    # Preview the report without sending
  gt compact report --weekly     # Send weekly rollup to mayor/
  gt compact report --json       # Output report as JSON`,
	RunE: runCompactReport,
}

func init() {
	compactReportCmd.Flags().BoolVar(&compactReportDryRun, "dry-run", false, "Preview report without sending")
	compactReportCmd.Flags().BoolVar(&compactReportWeekly, "weekly", false, "Generate weekly rollup instead of daily digest")
	compactReportCmd.Flags().BoolVarP(&compactReportVerbose, "verbose", "v", false, "Verbose output")
	compactReportCmd.Flags().StringVar(&compactReportDate, "date", "", "Report for specific date (YYYY-MM-DD); default: today")
	compactReportCmd.Flags().BoolVar(&compactReportJSON, "json", false, "Output report as JSON")

	compactCmd.AddCommand(compactReportCmd)
}

func runCompactReport(cmd *cobra.Command, args []string) error {
	workDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working dir: %w", err)
	}
	r := compactReportRun{
		dryRun:  compactReportDryRun,
		verbose: compactReportVerbose,
		date:    compactReportDate,
		json:    compactReportJSON,
		now:     time.Now().UTC(),
		workDir: workDir,
		compact: func() ([]byte, error) { return exec.Command("gt", "compact", "--json").Output() },
		mail:    sendMayorMail,
		out:     os.Stdout,
	}
	if compactReportWeekly {
		return r.weeklyRollup()
	}
	return r.dailyDigest()
}

// compactReportRun is one gt compact report: its flags, the clock reading it
// reports for, and the collaborators it reads and writes through. Unit tests
// build one with an in-process bd and recording compact and mail funcs.
type compactReportRun struct {
	dryRun, verbose, json bool
	date                  string // --date; "" means today
	now                   time.Time
	workDir               string
	// db records and finds the audit event beads; nil is bd in the working
	// directory with the process environment (compactReportDB).
	db beads.Client
	// wisps lists the active wisps; nil asks bd in workDir (listReportWisps).
	wisps func() ([]*compactIssue, error)
	// compact runs `gt compact --json` and returns its output.
	compact func() ([]byte, error)
	// mail sends subject and body to mayor/.
	mail func(subject, body string) error
	out  io.Writer
}

// sendMayorMail sends a report to mayor/ through gt mail.
func sendMayorMail(subject, body string) error {
	mailCmd := exec.Command("gt", "mail", "send", "mayor/",
		"-s", subject,
		"-m", body,
	)
	mailCmd.Stdout = os.Stdout
	mailCmd.Stderr = os.Stderr
	return mailCmd.Run()
}

// events returns the audit-bead database: r.db, or bd run plain in the
// working directory.
func (r compactReportRun) events() beads.Client {
	if r.db != nil {
		return r.db
	}
	return beads.NewPlain("", nil)
}

func (r compactReportRun) dailyDigest() error {
	dateStr := r.now.Format("2006-01-02")
	if r.date != "" {
		if _, err := time.Parse("2006-01-02", r.date); err != nil {
			return fmt.Errorf("invalid date format (use YYYY-MM-DD): %w", err)
		}
		dateStr = r.date
	}

	// Idempotency check: see if digest already exists for this date
	existingID, err := findExistingCompactReport(r.events(), dateStr)
	if err != nil {
		// Non-fatal: continue with creation attempt
		if r.verbose {
			fmt.Fprintf(os.Stderr, "warning: idempotency check failed: %v\n", err)
		}
	} else if existingID != "" {
		fmt.Fprintf(r.out, "%s Compaction digest already sent for %s (bead: %s)\n",
			style.Dim.Render("○"), dateStr, existingID)
		return nil
	}

	// Run compaction with --json to get results
	compactOut, err := r.compact()
	if err != nil {
		return fmt.Errorf("running compaction: %w", err)
	}

	var result compactResult
	if err := json.Unmarshal(extractJSONObject(compactOut), &result); err != nil {
		return fmt.Errorf("parsing compaction output: %w", err)
	}

	// Query active wisps for the "Active" column
	wisps := r.wisps
	if wisps == nil {
		wisps = func() ([]*compactIssue, error) { return listReportWisps(beads.New(r.workDir)) }
	}
	activeWisps, err := wisps()
	if err != nil {
		return fmt.Errorf("listing active wisps: %w", err)
	}

	// Build report
	report := buildReport(dateStr, &result, activeWisps)

	// Detect anomalies
	report.Anomalies = detectAnomalies(report)

	if r.json {
		enc := json.NewEncoder(r.out)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}

	// Format as markdown
	markdown := formatDailyDigest(report)

	if r.dryRun {
		fmt.Fprintf(r.out, "%s [DRY RUN] Daily compaction digest for %s:\n\n", style.Dim.Render("[dry-run]"), dateStr)
		fmt.Fprintln(r.out, markdown)
		return nil
	}

	// Create permanent event bead for audit trail
	beadID, err := createCompactReportBead(r.events(), report, markdown)
	if err != nil {
		return fmt.Errorf("recording compact report audit bead: %w", err)
	}

	// Send to mayor/ only — deacon/ is not a valid mail address (audit bead
	// serves as the deacon-side record).
	if err := r.mail(fmt.Sprintf("Wisp Compaction: %s", dateStr), markdown); err != nil {
		return fmt.Errorf("sending digest: %w", err)
	}

	fmt.Fprintf(r.out, "%s Compaction digest sent for %s\n", style.Success.Render("✓"), dateStr)
	if beadID != "" {
		fmt.Fprintf(r.out, "  Audit bead: %s\n", beadID)
	}

	return nil
}

// listReportWisps includes infrastructure wisps that the default bd list view
// hides. This is intentionally separate from listWisps, whose result drives
// mutating compaction decisions and must retain its existing scope.
func listReportWisps(bd *beads.Beads) ([]*compactIssue, error) {
	out, err := bd.Run("list", "--include-infra", "--json", "--all", "-n", "0")
	if err != nil {
		return nil, err
	}

	var allIssues []*compactIssue
	if err := json.Unmarshal(extractJSONArray(out), &allIssues); err != nil {
		return nil, fmt.Errorf("parsing report issue list: %w", err)
	}

	var wisps []*compactIssue
	for _, issue := range allIssues {
		if issue.Ephemeral {
			wisps = append(wisps, issue)
		}
	}
	return wisps, nil
}

// buildReport aggregates compaction results by category.
func buildReport(dateStr string, result *compactResult, activeWisps []*compactIssue) *compactReport {
	report := &compactReport{
		Date:       dateStr,
		Categories: make(map[string]*categoryStats),
		Errors:     result.Errors,
	}

	// Initialize all categories
	for _, cat := range categoryOrder {
		report.Categories[cat] = &categoryStats{}
	}

	// Tally deleted by category
	for _, d := range result.Deleted {
		cat := wispTypeToCategory(d.WispType, d.Title)
		report.Categories[cat].Deleted++
	}

	// Tally promoted by category
	for _, p := range result.Promoted {
		cat := wispTypeToCategory(p.WispType, p.Title)
		report.Categories[cat].Promoted++
		report.Promotions = append(report.Promotions, p)
	}

	// Tally active wisps by category
	for _, w := range activeWisps {
		cat := wispTypeToCategory(w.WispType, w.Title)
		report.Categories[cat].Active++
	}

	return report
}

// wispTypeToCategory maps a wisp_type string to its display category.
func wispTypeToCategory(wispType, title string) string {
	if cat, ok := wispCategoryMap[wispType]; ok {
		return cat
	}
	if wispType == "" && strings.Contains(strings.ToLower(title), "patrol") {
		return "Patrols"
	}
	return "Untyped"
}

// detectAnomalies checks for unusual patterns in the compaction data.
func detectAnomalies(report *compactReport) []string {
	var anomalies []string

	for _, cat := range categoryOrder {
		stats := report.Categories[cat]

		// High deletion volume (> 1000 in a single day for heartbeats suggests restart loop)
		if cat == "Heartbeats" && stats.Deleted > 1000 {
			anomalies = append(anomalies, fmt.Sprintf(
				"%dx normal heartbeat volume (possible restart loop)",
				stats.Deleted/300)) // ~300/day is baseline for a rig
		}

		// A zero query result is a reporting observation, not agent-health proof.
		if cat == "Patrols" && stats.Active == 0 && stats.Deleted == 0 && stats.Promoted == 0 {
			anomalies = append(anomalies, zeroPatrolReportingGap)
		}

		// High promotion rate (> 50% of non-skipped suggests miscategorized wisps)
		total := stats.Deleted + stats.Promoted
		if total > 10 && stats.Promoted > total/2 {
			anomalies = append(anomalies,
				fmt.Sprintf("%s: high promotion rate (%d/%d) — review wisp classification",
					cat, stats.Promoted, total))
		}
	}

	return anomalies
}

// formatDailyDigest renders the markdown daily digest per the design doc format.
func formatDailyDigest(report *compactReport) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## Wisp Compaction: %s\n\n", report.Date))

	// Summary table
	sb.WriteString("### Summary\n")
	sb.WriteString("| Category | Deleted | Promoted | Active |\n")
	sb.WriteString("|----------|---------|----------|--------|\n")

	for _, cat := range categoryOrder {
		stats := report.Categories[cat]
		// Skip empty categories
		if stats.Deleted == 0 && stats.Promoted == 0 && stats.Active == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("| %s | %d | %d | %d |\n",
			cat, stats.Deleted, stats.Promoted, stats.Active))
	}

	// Promotions
	if len(report.Promotions) > 0 {
		sb.WriteString("\n### Promotions\n")
		for _, p := range report.Promotions {
			sb.WriteString(fmt.Sprintf("- %s: %q (reason: %s)\n",
				p.ID, compactTruncate(p.Title, 60), p.Reason))
		}
	}

	// Anomalies
	if len(report.Anomalies) > 0 {
		sb.WriteString("\n### Anomalies\n")
		for _, a := range report.Anomalies {
			sb.WriteString(fmt.Sprintf("- %s\n", a))
		}
	}

	// Errors
	if len(report.Errors) > 0 {
		sb.WriteString("\n### Errors\n")
		for _, e := range report.Errors {
			sb.WriteString(fmt.Sprintf("- %s\n", e))
		}
	}

	return sb.String()
}

// createCompactReportBead creates a permanent audit bead for the daily digest.
func createCompactReportBead(db beads.Client, report *compactReport, markdown string) (string, error) {
	payloadJSON, err := json.Marshal(report)
	if err != nil {
		return "", fmt.Errorf("marshaling report payload: %w", err)
	}
	return createAuditEvent(db, fmt.Sprintf("Compaction Report %s", report.Date), "wisp.compaction",
		string(payloadJSON), markdown, "daily compaction report")
}

// createAuditEvent creates an event bead and closes it at once: an audit
// record, not work. Close failures surface: an open audit bead never matches
// the idempotency checks (they read closed events only), so the report would
// re-fire every patrol cycle.
func createAuditEvent(db beads.Client, title, kind, payload, markdown, closeReason string) (string, error) {
	issue, err := db.Create(beads.CreateOptions{
		Title:        title,
		Priority:     -1,
		Description:  markdown,
		EventKind:    kind,
		EventPayload: payload,
	})
	if err != nil {
		return "", fmt.Errorf("creating audit bead: %w", err)
	}
	if err := db.CloseWithReason(closeReason, issue.ID); err != nil {
		return "", fmt.Errorf("auto-closing audit bead %s: %w", issue.ID, err)
	}
	return issue.ID, nil
}

// --- Weekly Rollup ---

func (r compactReportRun) weeklyRollup() error {
	weekEnd := r.now.Format("2006-01-02")
	weekStart := r.now.AddDate(0, 0, -7).Format("2006-01-02")

	// Idempotency check: see if weekly rollup already exists for this week
	existingID, err := findExistingWeeklyRollup(r.events(), weekStart, weekEnd)
	if err != nil {
		if r.verbose {
			fmt.Fprintf(os.Stderr, "warning: weekly idempotency check failed: %v\n", err)
		}
	} else if existingID != "" {
		fmt.Fprintf(r.out, "%s Weekly rollup already sent for %s to %s (bead: %s)\n",
			style.Dim.Render("○"), weekStart, weekEnd, existingID)
		return nil
	}

	// Query compaction report event beads from the past week
	reports, err := queryCompactionReports(r.events(), weekStart, weekEnd)
	if err != nil {
		return fmt.Errorf("querying compaction reports: %w", err)
	}

	rollup := &weeklyRollup{
		WeekStart: weekStart,
		WeekEnd:   weekEnd,
		Days:      len(reports),
		Totals:    make(map[string]*categoryStats),
	}

	// Initialize totals
	for _, cat := range categoryOrder {
		rollup.Totals[cat] = &categoryStats{}
	}

	// Aggregate
	for _, report := range reports {
		for cat, stats := range report.Categories {
			if _, ok := rollup.Totals[cat]; !ok {
				rollup.Totals[cat] = &categoryStats{}
			}
			rollup.Totals[cat].Deleted += stats.Deleted
			rollup.Totals[cat].Promoted += stats.Promoted
			rollup.Totals[cat].Active = stats.Active // Use latest active count
		}
		rollup.Promotions += len(report.Promotions)
		for _, anomaly := range report.Anomalies {
			rollup.Anomalies = append(rollup.Anomalies, normalizeCompactionAnomaly(anomaly))
		}
	}

	if r.json {
		enc := json.NewEncoder(r.out)
		enc.SetIndent("", "  ")
		return enc.Encode(rollup)
	}

	markdown := formatWeeklyRollup(rollup)

	if r.dryRun {
		fmt.Fprintf(r.out, "%s [DRY RUN] Weekly compaction rollup (%s to %s):\n\n",
			style.Dim.Render("[dry-run]"), weekStart, weekEnd)
		fmt.Fprintln(r.out, markdown)
		return nil
	}

	// Create audit event bead for the weekly rollup (for future idempotency checks)
	beadID, beadErr := createWeeklyRollupBead(r.events(), rollup, markdown)
	if beadErr != nil {
		return fmt.Errorf("recording weekly rollup audit bead: %w", beadErr)
	}

	// Send to mayor/
	subject := fmt.Sprintf("Weekly Wisp Compaction: %s to %s", weekStart, weekEnd)
	if err := r.mail(subject, markdown); err != nil {
		return fmt.Errorf("sending weekly rollup: %w", err)
	}

	fmt.Fprintf(r.out, "%s Weekly compaction rollup sent to mayor/ (%s to %s)\n",
		style.Success.Render("✓"), weekStart, weekEnd)
	if beadID != "" {
		fmt.Fprintf(r.out, "  Audit bead: %s\n", beadID)
	}

	return nil
}

// queryCompactionReports queries compaction report event beads in a date
// range.
func queryCompactionReports(db beads.Client, startDate, endDate string) ([]*compactReport, error) {
	events, err := db.List(beads.ListOptions{IssueType: "event", Status: "all", Priority: -1})
	if err != nil {
		return nil, fmt.Errorf("listing event beads: %w", err)
	}

	var reports []*compactReport
	reportIndexByDate := make(map[string]int)
	reportCreatedAtByDate := make(map[string]string)
	matchingEvents := 0
	for _, evt := range events {
		if !strings.HasPrefix(evt.Title, "Compaction Report ") {
			continue
		}
		// Extract date from title
		evtDate := strings.TrimPrefix(evt.Title, "Compaction Report ")
		if evtDate < startDate || evtDate > endDate {
			continue
		}
		matchingEvents++

		// Parse the event payload back into a compactReport
		if evt.Payload == "" {
			continue
		}
		var report compactReport
		if err := json.Unmarshal([]byte(evt.Payload), &report); err != nil {
			continue
		}
		if idx, exists := reportIndexByDate[report.Date]; exists {
			// A failed historical idempotency check can leave duplicate daily
			// audit beads. Count each calendar day once and keep the newest copy.
			if evt.CreatedAt > reportCreatedAtByDate[report.Date] {
				reports[idx] = &report
				reportCreatedAtByDate[report.Date] = evt.CreatedAt
			}
			continue
		}
		reportIndexByDate[report.Date] = len(reports)
		reportCreatedAtByDate[report.Date] = evt.CreatedAt
		reports = append(reports, &report)
	}
	if matchingEvents > 0 && len(reports) == 0 {
		return nil, fmt.Errorf("found %d matching compaction report event(s), but no usable payload", matchingEvents)
	}

	// Sort by date
	sort.Slice(reports, func(i, j int) bool {
		return reports[i].Date < reports[j].Date
	})

	return reports, nil
}

func normalizeCompactionAnomaly(anomaly string) string {
	if anomaly == "0 patrol wisps (patrol agents may be down)" {
		return zeroPatrolReportingGap
	}
	return anomaly
}

// formatWeeklyRollup renders the markdown weekly rollup.
func formatWeeklyRollup(rollup *weeklyRollup) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("## Weekly Wisp Compaction: %s to %s\n\n", rollup.WeekStart, rollup.WeekEnd))
	sb.WriteString(fmt.Sprintf("**Days reported:** %d\n\n", rollup.Days))
	if rollup.Days == 0 {
		sb.WriteString("### Coverage\n")
		sb.WriteString("- No eligible daily compaction reports were found in this date range; patrol health was not assessed.\n\n")
	}

	// Totals table
	sb.WriteString("### Totals\n")
	sb.WriteString("| Category | Deleted | Promoted | Active (latest) |\n")
	sb.WriteString("|----------|---------|----------|----------------|\n")

	totalDeleted := 0
	totalPromoted := 0

	for _, cat := range categoryOrder {
		stats := rollup.Totals[cat]
		if stats.Deleted == 0 && stats.Promoted == 0 && stats.Active == 0 {
			continue
		}
		sb.WriteString(fmt.Sprintf("| %s | %d | %d | %d |\n",
			cat, stats.Deleted, stats.Promoted, stats.Active))
		totalDeleted += stats.Deleted
		totalPromoted += stats.Promoted
	}

	// Rates
	sb.WriteString(fmt.Sprintf("\n### Rates\n"))
	sb.WriteString(fmt.Sprintf("- **Total deleted:** %d\n", totalDeleted))
	sb.WriteString(fmt.Sprintf("- **Total promoted:** %d\n", totalPromoted))
	if totalDeleted+totalPromoted > 0 {
		rate := float64(totalPromoted) / float64(totalDeleted+totalPromoted) * 100
		sb.WriteString(fmt.Sprintf("- **Promotion rate:** %.1f%%\n", rate))
	}
	if rollup.Days > 0 {
		sb.WriteString(fmt.Sprintf("- **Avg deleted/day:** %d\n", totalDeleted/rollup.Days))
	}

	// Anomalies across the week
	if len(rollup.Anomalies) > 0 {
		sb.WriteString("\n### Anomalies This Week\n")
		// Deduplicate
		seen := make(map[string]bool)
		for _, a := range rollup.Anomalies {
			if !seen[a] {
				sb.WriteString(fmt.Sprintf("- %s\n", a))
				seen[a] = true
			}
		}
	}

	return sb.String()
}

// findExistingCompactReport checks if a compaction digest already exists for the given date.
// Returns the bead ID if found, empty string if not found.
func findExistingCompactReport(db beads.Client, dateStr string) (string, error) {
	expectedTitle := fmt.Sprintf("Compaction Report %s", dateStr)
	events, err := closedAuditEvents(db)
	if err != nil {
		return "", err
	}
	for _, evt := range events {
		if evt.Title == expectedTitle {
			return evt.ID, nil
		}
	}
	return "", nil
}

// closedAuditEvents lists the most recent closed event beads. Audit beads
// are closed at creation, and bd list shows open issues only by default:
// without the closed filter the prior report is invisible and a duplicate
// gets sent (gt-9t9).
func closedAuditEvents(db beads.Client) ([]*beads.Issue, error) {
	return db.List(beads.ListOptions{IssueType: "event", Status: "closed", Limit: 50, Priority: -1})
}

// weeklyRollupTitle matches a rollup bead title and captures its window, e.g.
// "Weekly Compaction Rollup 2026-09-01 to 2026-09-08".
var weeklyRollupTitle = regexp.MustCompile(`^Weekly Compaction Rollup (\d{4}-\d{2}-\d{2}) to (\d{4}-\d{2}-\d{2})$`)

// findExistingWeeklyRollup checks if a weekly rollup already covers the given
// window. Returns the bead ID if found, empty string if not found.
//
// The window is a rolling (now-7d, now) pair recomputed on every invocation,
// so an exact title match only catches a same-day re-run: a run one day
// later shifts both dates by a day, produces a different title, and would
// send a second rollup for effectively the same week (gt-sqk, follow-up to
// gt-9t9). Instead, treat any existing rollup whose window overlaps the new
// one as already covering it — dates are "YYYY-MM-DD" so lexicographic and
// chronological comparison agree.
func findExistingWeeklyRollup(db beads.Client, weekStart, weekEnd string) (string, error) {
	events, err := closedAuditEvents(db)
	if err != nil {
		return "", err
	}

	for _, evt := range events {
		m := weeklyRollupTitle.FindStringSubmatch(evt.Title)
		if m == nil {
			continue
		}
		existingStart, existingEnd := m[1], m[2]
		if weekStart <= existingEnd && existingStart <= weekEnd {
			return evt.ID, nil
		}
	}
	return "", nil
}

// extractJSONObject finds the first '{' byte in data and returns from that
// point onward. Strips non-JSON prefix from subprocess output.
func extractJSONObject(data []byte) []byte {
	idx := bytes.IndexByte(data, '{')
	if idx < 0 {
		return data
	}
	return data[idx:]
}

// createWeeklyRollupBead creates a permanent audit bead for the weekly rollup.
func createWeeklyRollupBead(db beads.Client, rollup *weeklyRollup, markdown string) (string, error) {
	payloadJSON, err := json.Marshal(rollup)
	if err != nil {
		return "", fmt.Errorf("marshaling rollup payload: %w", err)
	}
	title := fmt.Sprintf("Weekly Compaction Rollup %s to %s", rollup.WeekStart, rollup.WeekEnd)
	return createAuditEvent(db, title, "wisp.compaction.weekly", string(payloadJSON), markdown, "weekly compaction rollup")
}
