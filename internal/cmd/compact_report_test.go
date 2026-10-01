package cmd

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestWispTypeToCategory(t *testing.T) {
	t.Parallel()
	tests := []struct {
		wispType string
		title    string
		want     string
	}{
		{"heartbeat", "", "Heartbeats"},
		{"ping", "", "Heartbeats"},
		{"patrol", "", "Patrols"},
		{"gc_report", "", "Patrols"},
		{"error", "", "Errors"},
		{"recovery", "", "Errors"},
		{"escalation", "", "Errors"},
		{"", "", "Untyped"},
		{"unknown", "", "Untyped"},
		{"default", "", "Untyped"},
		{"", "Patrol report", "Patrols"},
	}

	for _, tc := range tests {
		t.Run(tc.wispType+"/"+tc.title, func(t *testing.T) {
			got := wispTypeToCategory(tc.wispType, tc.title)
			if got != tc.want {
				t.Errorf("wispTypeToCategory(%q, %q) = %q, want %q", tc.wispType, tc.title, got, tc.want)
			}
		})
	}
}

func TestWispTypeToCategory_TitlePatrolFallback(t *testing.T) {
	t.Parallel()
	got := wispTypeToCategory("", "nightly patrol sweep")
	if got != "Patrols" {
		t.Errorf("empty wisp_type + patrol in title = %q, want Patrols", got)
	}
}

func TestBuildReport(t *testing.T) {
	t.Parallel()
	result := &compactResult{
		Deleted: []compactAction{
			{ID: "w-1", Title: "Heartbeat 1", WispType: "heartbeat"},
			{ID: "w-2", Title: "Heartbeat 2", WispType: "heartbeat"},
			{ID: "w-3", Title: "Patrol cycle", WispType: "patrol"},
		},
		Promoted: []compactAction{
			{ID: "w-4", Title: "Stuck error", WispType: "error", Reason: "open past TTL"},
		},
		Skipped: 5,
	}

	activeWisps := []*compactIssue{
		{Issue: beads.Issue{ID: "w-10"}, WispType: "heartbeat"},
		{Issue: beads.Issue{ID: "w-11"}, WispType: "patrol"},
		{Issue: beads.Issue{ID: "w-12"}, WispType: "patrol"},
		{Issue: beads.Issue{ID: "w-13"}, WispType: "error"},
	}

	report := buildReport("2026-02-09", result, activeWisps)

	if report.Date != "2026-02-09" {
		t.Errorf("Date = %q, want %q", report.Date, "2026-02-09")
	}

	// Check heartbeats category
	hb := report.Categories["Heartbeats"]
	if hb.Deleted != 2 {
		t.Errorf("Heartbeats.Deleted = %d, want 2", hb.Deleted)
	}
	if hb.Active != 1 {
		t.Errorf("Heartbeats.Active = %d, want 1", hb.Active)
	}

	// Check patrols category
	p := report.Categories["Patrols"]
	if p.Deleted != 1 {
		t.Errorf("Patrols.Deleted = %d, want 1", p.Deleted)
	}
	if p.Active != 2 {
		t.Errorf("Patrols.Active = %d, want 2", p.Active)
	}

	// Check errors category
	e := report.Categories["Errors"]
	if e.Promoted != 1 {
		t.Errorf("Errors.Promoted = %d, want 1", e.Promoted)
	}
	if e.Active != 1 {
		t.Errorf("Errors.Active = %d, want 1", e.Active)
	}

	// Check promotions list
	if len(report.Promotions) != 1 {
		t.Fatalf("len(Promotions) = %d, want 1", len(report.Promotions))
	}
	if report.Promotions[0].ID != "w-4" {
		t.Errorf("Promotions[0].ID = %q, want %q", report.Promotions[0].ID, "w-4")
	}
}

func TestDetectAnomalies(t *testing.T) {
	t.Parallel()
	t.Run("high heartbeat volume", func(t *testing.T) {
		report := &compactReport{
			Categories: map[string]*categoryStats{
				"Heartbeats": {Deleted: 1500},
				"Patrols":    {Active: 5},
				"Errors":     {},
				"Untyped":    {},
			},
		}
		anomalies := detectAnomalies(report)
		found := false
		for _, a := range anomalies {
			if strings.Contains(a, "heartbeat volume") {
				found = true
			}
		}
		if !found {
			t.Error("expected heartbeat volume anomaly, got none")
		}
	})

	t.Run("zero patrols", func(t *testing.T) {
		report := &compactReport{
			Categories: map[string]*categoryStats{
				"Heartbeats": {Deleted: 100},
				"Patrols":    {Active: 0, Deleted: 0, Promoted: 0},
				"Errors":     {},
				"Untyped":    {},
			},
		}
		anomalies := detectAnomalies(report)
		found := false
		for _, a := range anomalies {
			if strings.Contains(a, "0 eligible patrol wisps") {
				found = true
			}
		}
		if !found {
			t.Error("expected zero patrol anomaly, got none")
		}
		for _, a := range anomalies {
			if strings.Contains(a, "patrol agents may be down") {
				t.Fatalf("reporting gap must not claim agent health: %q", a)
			}
		}
	})

	t.Run("high promotion rate", func(t *testing.T) {
		report := &compactReport{
			Categories: map[string]*categoryStats{
				"Heartbeats": {Deleted: 3, Promoted: 15},
				"Patrols":    {Active: 5},
				"Errors":     {},
				"Untyped":    {},
			},
		}
		anomalies := detectAnomalies(report)
		found := false
		for _, a := range anomalies {
			if strings.Contains(a, "promotion rate") {
				found = true
			}
		}
		if !found {
			t.Error("expected high promotion rate anomaly, got none")
		}
	})

	t.Run("no anomalies", func(t *testing.T) {
		report := &compactReport{
			Categories: map[string]*categoryStats{
				"Heartbeats": {Deleted: 100, Active: 20},
				"Patrols":    {Active: 5, Deleted: 10},
				"Errors":     {Active: 2},
				"Untyped":    {},
			},
		}
		anomalies := detectAnomalies(report)
		if len(anomalies) != 0 {
			t.Errorf("expected no anomalies, got %v", anomalies)
		}
	})
}

func TestFormatDailyDigest(t *testing.T) {
	t.Parallel()
	report := &compactReport{
		Date: "2026-02-09",
		Categories: map[string]*categoryStats{
			"Heartbeats": {Deleted: 2847, Promoted: 0, Active: 23},
			"Patrols":    {Deleted: 42, Promoted: 1, Active: 48},
			"Errors":     {Deleted: 2, Promoted: 3, Active: 7},
			"Untyped":    {Deleted: 15, Promoted: 0, Active: 4},
		},
		Promotions: []compactAction{
			{ID: "gt-wisp-abc", Title: "Polecat crash during convoy", Reason: "has comments"},
		},
		Anomalies: []string{"gastown: 3x normal heartbeat volume (possible restart loop)"},
	}

	md := formatDailyDigest(report)

	// Check structure
	if !strings.Contains(md, "## Wisp Compaction: 2026-02-09") {
		t.Error("missing header")
	}
	if !strings.Contains(md, "### Summary") {
		t.Error("missing summary section")
	}
	if !strings.Contains(md, "| Heartbeats | 2847 | 0 | 23 |") {
		t.Error("missing heartbeats row")
	}
	if !strings.Contains(md, "### Promotions") {
		t.Error("missing promotions section")
	}
	if !strings.Contains(md, "gt-wisp-abc") {
		t.Error("missing promotion entry")
	}
	if !strings.Contains(md, "### Anomalies") {
		t.Error("missing anomalies section")
	}
	if !strings.Contains(md, "heartbeat volume") {
		t.Error("missing anomaly entry")
	}
}

func TestFormatDailyDigestEmpty(t *testing.T) {
	t.Parallel()
	report := &compactReport{
		Date: "2026-02-09",
		Categories: map[string]*categoryStats{
			"Heartbeats": {},
			"Patrols":    {},
			"Errors":     {},
			"Untyped":    {},
		},
	}

	md := formatDailyDigest(report)

	// Should have header and summary but no promotions/anomalies sections
	if !strings.Contains(md, "## Wisp Compaction: 2026-02-09") {
		t.Error("missing header")
	}
	if strings.Contains(md, "### Promotions") {
		t.Error("should not have promotions section when empty")
	}
	if strings.Contains(md, "### Anomalies") {
		t.Error("should not have anomalies section when empty")
	}
}

func TestFormatWeeklyRollup(t *testing.T) {
	t.Parallel()
	rollup := &weeklyRollup{
		WeekStart: "2026-02-02",
		WeekEnd:   "2026-02-09",
		Days:      7,
		Totals: map[string]*categoryStats{
			"Heartbeats": {Deleted: 15000, Promoted: 0, Active: 25},
			"Patrols":    {Deleted: 280, Promoted: 5, Active: 50},
			"Errors":     {Deleted: 10, Promoted: 8, Active: 3},
			"Untyped":    {Deleted: 90, Promoted: 2, Active: 6},
		},
		Promotions: 15,
		Anomalies:  []string{"high heartbeat volume on 2026-02-05"},
	}

	md := formatWeeklyRollup(rollup)

	if !strings.Contains(md, "## Weekly Wisp Compaction: 2026-02-02 to 2026-02-09") {
		t.Error("missing header")
	}
	if !strings.Contains(md, "**Days reported:** 7") {
		t.Error("missing days count")
	}
	if !strings.Contains(md, "### Totals") {
		t.Error("missing totals section")
	}
	if !strings.Contains(md, "### Rates") {
		t.Error("missing rates section")
	}
	if !strings.Contains(md, "Promotion rate") {
		t.Error("missing promotion rate")
	}
	if !strings.Contains(md, "### Anomalies This Week") {
		t.Error("missing anomalies section")
	}
}

func TestFormatWeeklyRollupExplainsZeroDayCoverage(t *testing.T) {
	t.Parallel()
	rollup := &weeklyRollup{
		WeekStart: "2026-07-05",
		WeekEnd:   "2026-07-12",
		Totals: map[string]*categoryStats{
			"Heartbeats": {},
			"Patrols":    {},
			"Errors":     {},
			"Untyped":    {},
		},
	}

	md := formatWeeklyRollup(rollup)
	if !strings.Contains(md, "No eligible daily compaction reports") {
		t.Fatalf("zero-day rollup lacks coverage explanation:\n%s", md)
	}
}

func TestNormalizeCompactionAnomalyRemovesUnsupportedHealthClaim(t *testing.T) {
	t.Parallel()
	got := normalizeCompactionAnomaly("0 patrol wisps (patrol agents may be down)")
	if strings.Contains(got, "agents may be down") {
		t.Fatalf("normalized anomaly still claims agent health: %q", got)
	}
	if !strings.Contains(got, "patrol health not assessed") {
		t.Fatalf("normalized anomaly = %q, want reporting-scope explanation", got)
	}
}

func TestExtractBeadID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "clean output",
			input: "hq-1a2b\n",
			want:  "hq-1a2b",
		},
		{
			name: "noisy stdout — beads.role warning before id (regression: GH#2950)",
			// `bd` may print a multi-line warning on stdout when beads.role is
			// unset in the cwd's repo. Without extractBeadID, TrimSpace would
			// capture the whole blob, `bd close <blob>` would fail silently,
			// and the audit bead would stay open — causing one duplicate
			// compaction digest mail per patrol cycle.
			input: "warning: beads.role not configured (GH#2950).\n  Fix: git config beads.role maintainer\n  Or:  git config beads.role contributor\nhq-1a2b\n",
			want:  "hq-1a2b",
		},
		{
			name:  "trailing whitespace",
			input: "  hq-1a2b   \n",
			want:  "hq-1a2b",
		},
		{
			name:  "multi-rig prefix lengths",
			input: "co-rln\n",
			want:  "co-rln",
		},
		{
			name:  "prefix with digit",
			input: "h25-mrd\n",
			want:  "h25-mrd",
		},
		{
			name:  "hyphenated prefix",
			input: "my-rig-abc123\n",
			want:  "my-rig-abc123",
		},
		{
			name:  "long hyphenated prefix",
			input: "document-intelligence-0sa\n",
			want:  "document-intelligence-0sa",
		},
		{
			name:    "no id present",
			input:   "warning: something broke\n",
			wantErr: true,
		},
		{
			name:    "empty",
			input:   "",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractBeadID(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (id=%q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// compactReportFixture is a compactReportRun over an in-process bd, with the
// compaction answering an empty result and every mail recorded.
type compactReportFixture struct {
	r     compactReportRun
	bd    *inprocBD
	mails []string
}

func newCompactReportFixture(t *testing.T, now time.Time, answer func(f *inprocBD, cmd string, args []string) bdAnswer) *compactReportFixture {
	t.Helper()
	fx := &compactReportFixture{}
	fx.bd = &inprocBD{answer: func(f *inprocBD, cmd string, args []string) bdAnswer {
		f.logLine(cmd + " " + strings.Join(args, " "))
		return answer(f, cmd, args)
	}}
	fx.r = compactReportRun{
		now:     now,
		workDir: t.TempDir(),
		bd:      fx.bd.run,
		compact: func() ([]byte, error) { return []byte(`{"promoted":[],"deleted":[],"skipped":0}`), nil },
		mail: func(subject, body string) error {
			fx.mails = append(fx.mails, subject)
			return nil
		},
		out: io.Discard,
	}
	return fx
}

// closedRollupBD mimics bd's default status filter: the closed weekly rollup
// audit bead is listed ONLY when the query filters by closed status (or asks
// for all). The rollup bead is auto-closed right after creation, so a lookup
// without a status filter never sees it — the failure behind gt-9t9.
func closedRollupBD(rollupTitle string) func(f *inprocBD, cmd string, args []string) bdAnswer {
	return func(f *inprocBD, cmd string, args []string) bdAnswer {
		if cmd == "list" && (argsMention(args, "--status=closed") || argsMention(args, "--status=all")) {
			return bdOut(fmt.Sprintf(`[{"id":"hq-roll","title":%q,"status":"closed"}]`, rollupTitle))
		}
		if cmd == "list" {
			return bdOut("[]")
		}
		return bdAnswer{stderr: "unexpected bd command: " + cmd, code: 1}
	}
}

func TestRunWeeklyRollupSkipsWhenAlreadySentSameDay(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	title := "Weekly Compaction Rollup 2026-09-23 to 2026-09-30"
	fx := newCompactReportFixture(t, now, closedRollupBD(title))

	if err := fx.r.weeklyRollup(); err != nil {
		t.Fatalf("weeklyRollup: %v", err)
	}
	if len(fx.mails) != 0 {
		t.Fatalf("mail was sent unexpectedly: %v", fx.mails)
	}
	if strings.Contains(fx.bd.log(), "create") {
		t.Fatalf("bd create was called despite existing rollup: %s", fx.bd.log())
	}
}

// A rollup already sent yesterday for yesterday's rolling window: today's
// window is shifted by one day, so an exact title match would miss it and
// re-send (gt-sqk) — the overlap check must catch it instead.
func TestRunWeeklyRollupSkipsWhenSentOneDayEarlier(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	title := "Weekly Compaction Rollup 2026-09-22 to 2026-09-29"
	fx := newCompactReportFixture(t, now, closedRollupBD(title))

	if err := fx.r.weeklyRollup(); err != nil {
		t.Fatalf("weeklyRollup: %v", err)
	}
	if len(fx.mails) != 0 {
		t.Fatalf("mail was sent unexpectedly: %v", fx.mails)
	}
	if strings.Contains(fx.bd.log(), "create") {
		t.Fatalf("bd create was called despite an overlapping rollup sent yesterday: %s", fx.bd.log())
	}
}

// closeFailsBD lists nothing, creates h25-mrd behind bd's beads.role notice
// and fails every close.
func closeFailsBD(f *inprocBD, cmd string, args []string) bdAnswer {
	switch cmd {
	case "list":
		return bdOut("[]")
	case "create":
		return bdOut("warning: beads.role not configured (GH#2950).\n  Fix: git config beads.role maintainer\n  Or:  git config beads.role contributor\nh25-mrd\n")
	case "close":
		return bdAnswer{stderr: "close failed", code: 1}
	}
	return bdAnswer{stderr: "unexpected bd command: " + cmd, code: 1}
}

func TestRunDailyDigestStopsBeforeMailWhenAuditCloseFails(t *testing.T) {
	t.Parallel()
	fx := newCompactReportFixture(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC), closeFailsBD)
	fx.r.date = "2026-05-15"

	err := fx.r.dailyDigest()
	if err == nil {
		t.Fatal("want audit bead close error, got nil")
	}
	if !strings.Contains(err.Error(), "auto-closing report bead h25-mrd") {
		t.Fatalf("error = %v, want auto-close failure", err)
	}
	if len(fx.mails) != 0 {
		t.Fatalf("mail was sent unexpectedly: %v", fx.mails)
	}
}

func TestRunWeeklyRollupStopsBeforeMailWhenAuditCloseFails(t *testing.T) {
	t.Parallel()
	fx := newCompactReportFixture(t, time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), closeFailsBD)

	err := fx.r.weeklyRollup()
	if err == nil {
		t.Fatal("want audit bead close error, got nil")
	}
	if !strings.Contains(err.Error(), "auto-closing rollup bead h25-mrd") {
		t.Fatalf("error = %v, want auto-close failure", err)
	}
	if len(fx.mails) != 0 {
		t.Fatalf("mail was sent unexpectedly: %v", fx.mails)
	}
}

// A digest whose audit bead is recorded and closed is mailed to mayor/.
func TestRunDailyDigestMailsAfterTheAuditBead(t *testing.T) {
	t.Parallel()
	fx := newCompactReportFixture(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC), func(f *inprocBD, cmd string, args []string) bdAnswer {
		switch cmd {
		case "list":
			return bdOut("[]")
		case "create":
			return bdOut("h25-mrd\n")
		}
		return bdOut("")
	})

	if err := fx.r.dailyDigest(); err != nil {
		t.Fatalf("dailyDigest: %v", err)
	}
	if len(fx.mails) != 1 || fx.mails[0] != "Wisp Compaction: 2026-05-15" {
		t.Fatalf("mails = %v, want the digest for 2026-05-15", fx.mails)
	}
	if !fx.bd.logged("close h25-mrd --reason=daily compaction report") {
		t.Errorf("audit bead not closed; bd log:\n%s", fx.bd.log())
	}
}

// listingBD is an in-process bd whose list calls answer out; every call's
// arguments are logged.
func listingBD(out string) *inprocBD {
	return &inprocBD{answer: func(f *inprocBD, cmd string, args []string) bdAnswer {
		f.logLine(cmd + " " + strings.Join(args, " "))
		if cmd == "list" {
			return bdOut(out)
		}
		return bdAnswer{stderr: "unexpected bd command: " + cmd, code: 1}
	}}
}

func TestListReportWispsIncludesInfrastructure(t *testing.T) {
	t.Parallel()
	bd := listingBD(`[{"id":"hq-wisp-patrol","title":"mol-deacon-patrol","status":"hooked","issue_type":"molecule","ephemeral":true,"wisp_type":"patrol"}]`)
	wisps, err := listReportWisps(beads.NewWithBeadsDirAndRunner(t.TempDir(), "", bd.run))
	if err != nil {
		t.Fatalf("listReportWisps: %v", err)
	}
	if len(wisps) != 1 || wisps[0].ID != "hq-wisp-patrol" {
		t.Fatalf("wisps = %#v, want patrol infrastructure wisp", wisps)
	}
	if !strings.Contains(bd.log(), "--include-infra") {
		t.Fatalf("bd args = %q, want --include-infra", bd.log())
	}
}

func TestQueryCompactionReportsReadsPayloadField(t *testing.T) {
	t.Parallel()
	bd := listingBD(`[{"id":"hq-report","title":"Compaction Report 2026-07-12","payload":"{\"date\":\"2026-07-12\",\"categories\":{\"Patrols\":{\"active\":2}}}"}]`)
	reports, err := queryCompactionReportsVia(bd.run, "2026-07-05", "2026-07-12")
	if err != nil {
		t.Fatalf("queryCompactionReports: %v", err)
	}
	if len(reports) != 1 || reports[0].Categories["Patrols"].Active != 2 {
		t.Fatalf("reports = %+v, want one with Patrols.Active 2", reports)
	}
}

func TestQueryCompactionReportsIncludesClosedEvents(t *testing.T) {
	t.Parallel()
	bd := listingBD(`[{"id":"hq-report","title":"Compaction Report 2026-07-12","payload":"{\"date\":\"2026-07-12\",\"categories\":{}}"}]`)
	reports, err := queryCompactionReportsVia(bd.run, "2026-07-05", "2026-07-12")
	if err != nil || len(reports) != 1 {
		t.Fatalf("reports = %v, err %v; want 1 closed event", reports, err)
	}
	if !strings.Contains(bd.log(), "--status=all") {
		t.Fatalf("bd args = %q, want --status=all", bd.log())
	}
}

func TestQueryCompactionReportsDeduplicatesReportDates(t *testing.T) {
	t.Parallel()
	bd := listingBD(`[{"id":"hq-old","title":"Compaction Report 2026-07-08","created_at":"2026-07-08T00:00:00Z","payload":"{\"date\":\"2026-07-08\",\"categories\":{\"Patrols\":{\"active\":1}}}"},{"id":"hq-new","title":"Compaction Report 2026-07-08","created_at":"2026-07-08T01:00:00Z","payload":"{\"date\":\"2026-07-08\",\"categories\":{\"Patrols\":{\"active\":2}}}"}]`)
	reports, err := queryCompactionReportsVia(bd.run, "2026-07-05", "2026-07-12")
	if err != nil {
		t.Fatalf("queryCompactionReports: %v", err)
	}
	if len(reports) != 1 || reports[0].Categories["Patrols"].Active != 2 {
		t.Fatalf("reports = %+v, want one report per date with the latest value 2", reports)
	}
}

func TestQueryCompactionReportsRejectsMatchingEventsWithoutUsablePayload(t *testing.T) {
	t.Parallel()
	bd := listingBD(`[{"id":"hq-report","title":"Compaction Report 2026-07-12","payload":"not-json"}]`)
	if _, err := queryCompactionReportsVia(bd.run, "2026-07-05", "2026-07-12"); err == nil || !strings.Contains(err.Error(), "no usable payload") {
		t.Fatalf("error = %v, want no usable payload diagnostic", err)
	}
}

// TestFindExistingWeeklyRollup: the idempotency check sees closed rollup
// beads (gt-9t9), treats an overlapping later window as already sent
// (gt-sqk), and lets a non-overlapping window through.
func TestFindExistingWeeklyRollup(t *testing.T) {
	t.Parallel()
	closedRollup := `[{"id":"hq-roll","title":"Weekly Compaction Rollup 2026-09-01 to 2026-09-08","status":"closed"}]`
	bd := &inprocBD{answer: func(f *inprocBD, cmd string, args []string) bdAnswer {
		if cmd == "list" && (argsMention(args, "--status=closed") || argsMention(args, "--status=all")) {
			return bdOut(closedRollup)
		}
		return bdOut("[]")
	}}
	for _, tc := range []struct{ start, end, want string }{
		{"2026-09-01", "2026-09-08", "hq-roll"},
		{"2026-09-02", "2026-09-09", "hq-roll"},
		{"2026-09-16", "2026-09-23", ""},
	} {
		id, err := findExistingWeeklyRollupVia(bd.run, tc.start, tc.end)
		if err != nil || id != tc.want {
			t.Errorf("window %s..%s: id %q err %v, want %q", tc.start, tc.end, id, err, tc.want)
		}
	}
}
