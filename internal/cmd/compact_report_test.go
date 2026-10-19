package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
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

	activeWisps := []*beads.Issue{
		{ID: "w-10", WispType: "heartbeat"},
		{ID: "w-11", WispType: "patrol"},
		{ID: "w-12", WispType: "patrol"},
		{ID: "w-13", WispType: "error"},
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

// compactReportFixture is a compactReportRun over a shared fake, with the
// compaction answering an empty result, no active wisps and every mail
// recorded. Its workDir is a bare temp dir outside any town, so the sweep's
// target resolver yields the one "current" target (see
// TestDailyDigestOutsideATownListsOnlyTheWorkingDirectory); withTown puts it
// in a town with registered rigs.
type compactReportFixture struct {
	r     compactReportRun
	db    *beadsfake.Fake
	mails []string
}

func newCompactReportFixture(t *testing.T, now time.Time) *compactReportFixture {
	t.Helper()
	fx := &compactReportFixture{db: beadsfake.New(beadsfake.WithPrefix("hq"))}
	fx.r = compactReportRun{
		now:     now,
		workDir: t.TempDir(),
		db:      fx.db,
		wisps:   func(compactTarget) ([]*beads.Issue, error) { return nil, nil },
		compact: func() ([]byte, error) { return []byte(`{"promoted":[],"deleted":[],"skipped":0}`), nil },
		mail: func(subject, body string) error {
			fx.mails = append(fx.mails, subject)
			return nil
		},
		out: io.Discard,
	}
	return fx
}

// withTown moves the run's working directory to the root of a new town that
// registers rigs, so the digest's targets are "current" plus each rig.
func (fx *compactReportFixture) withTown(t *testing.T, rigs ...string) {
	t.Helper()
	townRoot := t.TempDir()
	writeTestRigsConfig(t, townRoot, rigs...)
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{}`), 0644); err != nil {
		t.Fatalf("write town.json: %v", err)
	}
	fx.r.workDir = townRoot
}

// event seeds an event bead in status (closed when "").
func event(id, title, status, payload string) beads.Issue {
	if status == "" {
		status = "closed"
	}
	return beads.Issue{ID: id, Title: title, Status: status, Type: "event", EventKind: "wisp.compaction", Payload: payload}
}

// events returns the fake's event beads, any status.
func (fx *compactReportFixture) events(t *testing.T) []*beads.Issue {
	t.Helper()
	got, err := fx.db.List(beads.ListOptions{IssueType: "event", Status: "all", Priority: -1})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestRunWeeklyRollupSkipsWhenAlreadySentSameDay(t *testing.T) {
	t.Parallel()
	fx := newCompactReportFixture(t, time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	fx.db.Seed(event("hq-roll", "Weekly Compaction Rollup 2026-09-23 to 2026-09-30", "", ""))

	if err := fx.r.weeklyRollup(); err != nil {
		t.Fatalf("weeklyRollup: %v", err)
	}
	if len(fx.mails) != 0 {
		t.Fatalf("mail was sent unexpectedly: %v", fx.mails)
	}
	if n := len(fx.events(t)); n != 1 {
		t.Fatalf("event beads = %d, want only the existing rollup", n)
	}
}

// A rollup already sent yesterday for yesterday's rolling window: today's
// window is shifted by one day, so an exact title match would miss it and
// re-send (gt-sqk) — the overlap check must catch it instead.
func TestRunWeeklyRollupSkipsWhenSentOneDayEarlier(t *testing.T) {
	t.Parallel()
	fx := newCompactReportFixture(t, time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	fx.db.Seed(event("hq-roll", "Weekly Compaction Rollup 2026-09-22 to 2026-09-29", "", ""))

	if err := fx.r.weeklyRollup(); err != nil {
		t.Fatalf("weeklyRollup: %v", err)
	}
	if len(fx.mails) != 0 {
		t.Fatalf("mail was sent unexpectedly: %v", fx.mails)
	}
	if n := len(fx.events(t)); n != 1 {
		t.Fatalf("event beads = %d, want only the existing rollup", n)
	}
}

// closeFails is a beads.Client whose closes all fail.
type closeFails struct{ beads.Client }

func (closeFails) CloseWithReason(string, ...string) error { return errors.New("close failed") }

func TestRunDailyDigestStopsBeforeMailWhenAuditCloseFails(t *testing.T) {
	t.Parallel()
	fx := newCompactReportFixture(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))
	fx.r.db = closeFails{fx.db}
	fx.r.date = "2026-05-15"

	err := fx.r.dailyDigest()
	if err == nil || !strings.Contains(err.Error(), "auto-closing audit bead hq-f1") {
		t.Fatalf("error = %v, want auto-close failure", err)
	}
	if len(fx.mails) != 0 {
		t.Fatalf("mail was sent unexpectedly: %v", fx.mails)
	}
}

func TestRunWeeklyRollupStopsBeforeMailWhenAuditCloseFails(t *testing.T) {
	t.Parallel()
	fx := newCompactReportFixture(t, time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	fx.r.db = closeFails{fx.db}

	err := fx.r.weeklyRollup()
	if err == nil || !strings.Contains(err.Error(), "auto-closing audit bead hq-f1") {
		t.Fatalf("error = %v, want auto-close failure", err)
	}
	if len(fx.mails) != 0 {
		t.Fatalf("mail was sent unexpectedly: %v", fx.mails)
	}
}

// A digest whose audit bead is recorded and closed is mailed to the overseer, and
// a second run the same day finds it and sends nothing.
func TestRunDailyDigestMailsAfterTheAuditBead(t *testing.T) {
	t.Parallel()
	fx := newCompactReportFixture(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))

	if err := fx.r.dailyDigest(); err != nil {
		t.Fatalf("dailyDigest: %v", err)
	}
	if len(fx.mails) != 1 || fx.mails[0] != "Wisp Compaction: 2026-05-15" {
		t.Fatalf("mails = %v, want the digest for 2026-05-15", fx.mails)
	}
	evs := fx.events(t)
	if len(evs) != 1 || evs[0].Status != "closed" || evs[0].EventKind != "wisp.compaction" || evs[0].Title != "Compaction Report 2026-05-15" {
		t.Fatalf("audit beads = %+v, want one closed wisp.compaction report", evs)
	}
	if err := fx.r.dailyDigest(); err != nil {
		t.Fatalf("second dailyDigest: %v", err)
	}
	if len(fx.mails) != 1 {
		t.Fatalf("mails = %v, want the second run to find the audit bead", fx.mails)
	}
}

// The digest's Active column counts every database the sweep covers, not just
// the working directory's: Deleted and Promoted come from a gt compact that
// reaches each registered rig, so a rig's in-TTL patrol wisps are active and
// the "0 eligible patrol wisps" anomaly stays quiet.
func TestDailyDigestActiveCountsRigDatabases(t *testing.T) {
	t.Parallel()
	fx := newCompactReportFixture(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))
	fx.withTown(t, "gastown")

	patrol := func(id string) *beads.Issue {
		return &beads.Issue{ID: id, Title: "patrol step", WispType: "patrol", Ephemeral: true}
	}
	byTarget := map[string][]*beads.Issue{
		"current": {patrol("hq-1")},
		"gastown": {patrol("gt-1"), patrol("gt-2")},
	}
	var asked []string
	fx.r.wisps = func(target compactTarget) ([]*beads.Issue, error) {
		asked = append(asked, target.label)
		return byTarget[target.label], nil
	}
	var out bytes.Buffer
	fx.r.json = true
	fx.r.out = &out

	if err := fx.r.dailyDigest(); err != nil {
		t.Fatalf("dailyDigest: %v", err)
	}

	var report compactReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("parsing digest %q: %v", out.String(), err)
	}
	if got := report.Categories["Patrols"].Active; got != 3 {
		t.Errorf("Patrols Active = %d, want 3 (1 town + 2 rig); targets asked: %v", got, asked)
	}
	for _, a := range report.Anomalies {
		if strings.Contains(a, "0 eligible patrol wisps") {
			t.Errorf("anomalies = %v, want no zero-patrol observation while a rig holds in-TTL patrol wisps", report.Anomalies)
		}
	}
}

// A database the digest cannot list does not block the digest: it names the
// database in the Errors section, as the sweep does, and the Active column
// counts the databases that answered.
func TestDailyDigestReportsTheRigDatabaseItCannotListAndSends(t *testing.T) {
	t.Parallel()
	fx := newCompactReportFixture(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))
	fx.withTown(t, "gastown")
	fx.r.wisps = func(target compactTarget) ([]*beads.Issue, error) {
		if target.label == "gastown" {
			return nil, errors.New("dolt down")
		}
		return []*beads.Issue{{ID: "hq-1", Title: "patrol step", WispType: "patrol", Ephemeral: true}}, nil
	}
	var out bytes.Buffer
	fx.r.json = true
	fx.r.out = &out

	if err := fx.r.dailyDigest(); err != nil {
		t.Fatalf("dailyDigest: %v", err)
	}

	var report compactReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("parsing digest %q: %v", out.String(), err)
	}
	want := []string{"gastown: listing active wisps: dolt down"}
	if !reflect.DeepEqual(report.Errors, want) {
		t.Errorf("Errors = %v, want %v", report.Errors, want)
	}
	if got := report.Categories["Patrols"].Active; got != 1 {
		t.Errorf("Patrols Active = %d, want 1 from the database that answered", got)
	}
}

// The mailed digest carries the unreachable database in its Errors section.
func TestDailyDigestMailsDespiteAnUnlistableRigDatabase(t *testing.T) {
	t.Parallel()
	fx := newCompactReportFixture(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))
	fx.withTown(t, "gastown")
	fx.r.wisps = func(target compactTarget) ([]*beads.Issue, error) {
		if target.label == "gastown" {
			return nil, errors.New("dolt down")
		}
		return nil, nil
	}
	var body string
	fx.r.mail = func(subject, b string) error {
		fx.mails = append(fx.mails, subject)
		body = b
		return nil
	}

	if err := fx.r.dailyDigest(); err != nil {
		t.Fatalf("dailyDigest: %v", err)
	}
	if len(fx.mails) != 1 {
		t.Fatalf("mails = %v, want the digest sent once", fx.mails)
	}
	if !strings.Contains(body, "### Errors") || !strings.Contains(body, "gastown: listing active wisps: dolt down") {
		t.Errorf("digest body = %q, want an Errors section naming gastown", body)
	}
}

// Outside a town the resolver finds no rigs: the digest lists only the
// working directory's own database, labelled "current".
func TestDailyDigestOutsideATownListsOnlyTheWorkingDirectory(t *testing.T) {
	t.Parallel()
	fx := newCompactReportFixture(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))
	var asked []string
	fx.r.wisps = func(target compactTarget) ([]*beads.Issue, error) {
		asked = append(asked, target.label)
		return nil, nil
	}

	if err := fx.r.dailyDigest(); err != nil {
		t.Fatalf("dailyDigest: %v", err)
	}
	if want := []string{"current"}; !reflect.DeepEqual(asked, want) {
		t.Errorf("targets asked = %v, want %v", asked, want)
	}
}

// failMailOnce makes the first mail fail; later mails are recorded.
func (fx *compactReportFixture) failMailOnce() {
	attempts := 0
	fx.r.mail = func(subject, body string) error {
		attempts++
		if attempts == 1 {
			return errors.New("smtp down")
		}
		fx.mails = append(fx.mails, subject)
		return nil
	}
}

// A failed send must not leave the day marked as sent: the next run sends it.
func TestRunDailyDigestResendsAfterFailedMail(t *testing.T) {
	t.Parallel()
	fx := newCompactReportFixture(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))
	fx.failMailOnce()

	err := fx.r.dailyDigest()
	if err == nil || !strings.Contains(err.Error(), "sending digest") {
		t.Fatalf("error = %v, want the send failure", err)
	}
	if n := len(fx.events(t)); n != 0 {
		t.Fatalf("audit beads = %d after a failed send, want none", n)
	}
	if err := fx.r.dailyDigest(); err != nil {
		t.Fatalf("second dailyDigest: %v", err)
	}
	if len(fx.mails) != 1 || fx.mails[0] != "Wisp Compaction: 2026-05-15" {
		t.Fatalf("mails = %v, want the digest sent on the second run", fx.mails)
	}
	if err := fx.r.dailyDigest(); err != nil {
		t.Fatalf("third dailyDigest: %v", err)
	}
	if len(fx.mails) != 1 {
		t.Fatalf("mails = %v, want the third run to report already sent", fx.mails)
	}
}

func TestRunWeeklyRollupResendsAfterFailedMail(t *testing.T) {
	t.Parallel()
	fx := newCompactReportFixture(t, time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC))
	fx.failMailOnce()

	if err := fx.r.weeklyRollup(); err == nil || !strings.Contains(err.Error(), "sending weekly rollup") {
		t.Fatalf("error = %v, want the send failure", err)
	}
	if err := fx.r.weeklyRollup(); err != nil {
		t.Fatalf("second weeklyRollup: %v", err)
	}
	if len(fx.mails) != 1 {
		t.Fatalf("mails = %v, want the rollup sent on the second run", fx.mails)
	}
}

// deleteFails is a beads.Client whose deletes all fail.
type deleteFails struct{ beads.Client }

func (deleteFails) DeleteIssues(...string) error { return errors.New("delete failed") }

func TestRunDailyDigestNamesTheStuckAuditBeadWhenWithdrawFails(t *testing.T) {
	t.Parallel()
	fx := newCompactReportFixture(t, time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC))
	fx.failMailOnce()
	fx.r.db = deleteFails{fx.db}

	err := fx.r.dailyDigest()
	if err == nil || !strings.Contains(err.Error(), "smtp down") || !strings.Contains(err.Error(), "hq-f1 was not withdrawn") {
		t.Fatalf("error = %v, want the send failure and the stuck bead", err)
	}
}

func TestQueryCompactionReportsReadsPayloadField(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(event("hq-report", "Compaction Report 2026-07-12", "", `{"date":"2026-07-12","categories":{"Patrols":{"active":2}}}`))
	reports, err := queryCompactionReports(db, "2026-07-05", "2026-07-12")
	if err != nil {
		t.Fatalf("queryCompactionReports: %v", err)
	}
	if len(reports) != 1 || reports[0].Categories["Patrols"].Active != 2 {
		t.Fatalf("reports = %+v, want one with Patrols.Active 2", reports)
	}
}

func TestQueryCompactionReportsIncludesOpenAndClosedEvents(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(
		event("hq-a", "Compaction Report 2026-07-11", "", `{"date":"2026-07-11","categories":{}}`),
		event("hq-b", "Compaction Report 2026-07-12", "open", `{"date":"2026-07-12","categories":{}}`),
		beads.Issue{ID: "hq-task", Title: "Compaction Report 2026-07-10", Type: "task"},
	)
	reports, err := queryCompactionReports(db, "2026-07-05", "2026-07-12")
	if err != nil || len(reports) != 2 {
		t.Fatalf("reports = %v, err %v; want the 2 events, not the task", reports, err)
	}
}

func TestQueryCompactionReportsDeduplicatesReportDates(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	old := event("hq-old", "Compaction Report 2026-07-08", "", `{"date":"2026-07-08","categories":{"Patrols":{"active":1}}}`)
	old.CreatedAt = "2026-07-08T00:00:00Z"
	latest := event("hq-new", "Compaction Report 2026-07-08", "", `{"date":"2026-07-08","categories":{"Patrols":{"active":2}}}`)
	latest.CreatedAt = "2026-07-08T01:00:00Z"
	db.Seed(old, latest)
	reports, err := queryCompactionReports(db, "2026-07-05", "2026-07-12")
	if err != nil {
		t.Fatalf("queryCompactionReports: %v", err)
	}
	if len(reports) != 1 || reports[0].Categories["Patrols"].Active != 2 {
		t.Fatalf("reports = %+v, want one report per date with the latest value 2", reports)
	}
}

func TestQueryCompactionReportsRejectsMatchingEventsWithoutUsablePayload(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(event("hq-report", "Compaction Report 2026-07-12", "", "not-json"))
	if _, err := queryCompactionReports(db, "2026-07-05", "2026-07-12"); err == nil || !strings.Contains(err.Error(), "no usable payload") {
		t.Fatalf("error = %v, want no usable payload diagnostic", err)
	}
}

// TestFindExistingWeeklyRollup: the idempotency check sees closed rollup
// beads (gt-9t9), treats an overlapping later window as already sent
// (gt-sqk), and lets a window that only touches the previous one at its end
// day through, as the run seven days later does (gt-o8skm). The boundary is
// exclusive on the start side too, and zero-length windows are covered only
// when they sit strictly inside an existing window (gt-wvyp4).
func TestFindExistingWeeklyRollup(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(event("hq-roll", "Weekly Compaction Rollup 2026-09-01 to 2026-09-08", "", ""))
	for _, tc := range []struct{ start, end, want string }{
		{"2026-09-01", "2026-09-08", "hq-roll"},
		{"2026-09-02", "2026-09-09", "hq-roll"},
		{"2026-09-08", "2026-09-15", ""},
		{"2026-09-16", "2026-09-23", ""},
		{"2026-08-25", "2026-09-01", ""},
		{"2026-08-26", "2026-09-02", "hq-roll"},
		{"2026-09-04", "2026-09-04", "hq-roll"},
		{"2026-09-01", "2026-09-01", ""},
		{"2026-09-08", "2026-09-08", ""},
	} {
		id, err := findExistingWeeklyRollup(db, tc.start, tc.end)
		if err != nil || id != tc.want {
			t.Errorf("window %s..%s: id %q err %v, want %q", tc.start, tc.end, id, err, tc.want)
		}
	}
}

// TestFindExistingWeeklyRollupZeroLengthExisting: a zero-length rollup covers
// no day, so only an identical window or one strictly around it counts as
// already sent.
func TestFindExistingWeeklyRollupZeroLengthExisting(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(event("hq-zero", "Weekly Compaction Rollup 2026-09-04 to 2026-09-04", "", ""))
	for _, tc := range []struct{ start, end, want string }{
		{"2026-09-04", "2026-09-04", "hq-zero"},
		{"2026-09-01", "2026-09-08", "hq-zero"},
		{"2026-09-04", "2026-09-11", ""},
		{"2026-08-28", "2026-09-04", ""},
	} {
		id, err := findExistingWeeklyRollup(db, tc.start, tc.end)
		if err != nil || id != tc.want {
			t.Errorf("window %s..%s: id %q err %v, want %q", tc.start, tc.end, id, err, tc.want)
		}
	}
}
