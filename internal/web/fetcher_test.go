package web

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/activity"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/wisp"
)

func TestCalculateWorkStatus(t *testing.T) {
	tests := []struct {
		name          string
		completed     int
		total         int
		activityColor string
		want          string
	}{
		{
			name:          "complete when all done",
			completed:     5,
			total:         5,
			activityColor: activity.ColorGreen,
			want:          "complete",
		},
		{
			name:          "complete overrides activity color",
			completed:     3,
			total:         3,
			activityColor: activity.ColorRed,
			want:          "complete",
		},
		{
			name:          "active when green",
			completed:     2,
			total:         5,
			activityColor: activity.ColorGreen,
			want:          "active",
		},
		{
			name:          "stale when yellow",
			completed:     2,
			total:         5,
			activityColor: activity.ColorYellow,
			want:          "stale",
		},
		{
			name:          "stuck when red",
			completed:     2,
			total:         5,
			activityColor: activity.ColorRed,
			want:          "stuck",
		},
		{
			name:          "waiting when unknown color",
			completed:     2,
			total:         5,
			activityColor: activity.ColorUnknown,
			want:          "waiting",
		},
		{
			name:          "waiting when empty color",
			completed:     0,
			total:         5,
			activityColor: "",
			want:          "waiting",
		},
		{
			name:          "waiting when no work yet",
			completed:     0,
			total:         0,
			activityColor: activity.ColorUnknown,
			want:          "waiting",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := calculateWorkStatus(tt.completed, tt.total, tt.activityColor)
			if got != tt.want {
				t.Errorf("calculateWorkStatus(%d, %d, %q) = %q, want %q",
					tt.completed, tt.total, tt.activityColor, got, tt.want)
			}
		})
	}
}

func TestDetermineCIStatus(t *testing.T) {
	tests := []struct {
		name   string
		checks []struct {
			State      string `json:"state"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		}
		want string
	}{
		{
			name:   "pending when no checks",
			checks: nil,
			want:   "pending",
		},
		{
			name: "pass when all success",
			checks: []struct {
				State      string `json:"state"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			}{
				{Conclusion: "success"},
				{Conclusion: "success"},
			},
			want: "pass",
		},
		{
			name: "pass with skipped checks",
			checks: []struct {
				State      string `json:"state"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			}{
				{Conclusion: "success"},
				{Conclusion: "skipped"},
			},
			want: "pass",
		},
		{
			name: "fail when any failure",
			checks: []struct {
				State      string `json:"state"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			}{
				{Conclusion: "success"},
				{Conclusion: "failure"},
			},
			want: "fail",
		},
		{
			name: "fail when cancelled",
			checks: []struct {
				State      string `json:"state"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			}{
				{Conclusion: "cancelled"},
			},
			want: "fail",
		},
		{
			name: "fail when timed_out",
			checks: []struct {
				State      string `json:"state"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			}{
				{Conclusion: "timed_out"},
			},
			want: "fail",
		},
		{
			name: "pending when in_progress",
			checks: []struct {
				State      string `json:"state"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			}{
				{Conclusion: "success"},
				{Status: "in_progress"},
			},
			want: "pending",
		},
		{
			name: "pending when queued",
			checks: []struct {
				State      string `json:"state"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			}{
				{Status: "queued"},
			},
			want: "pending",
		},
		{
			name: "fail from state FAILURE",
			checks: []struct {
				State      string `json:"state"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			}{
				{State: "FAILURE"},
			},
			want: "fail",
		},
		{
			name: "pending from state PENDING",
			checks: []struct {
				State      string `json:"state"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			}{
				{State: "PENDING"},
			},
			want: "pending",
		},
		{
			name: "failure takes precedence over pending",
			checks: []struct {
				State      string `json:"state"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			}{
				{Conclusion: "failure"},
				{Status: "in_progress"},
			},
			want: "fail",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := determineCIStatus(tt.checks)
			if got != tt.want {
				t.Errorf("determineCIStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDetermineMergeableStatus(t *testing.T) {
	tests := []struct {
		name      string
		mergeable string
		want      string
	}{
		{"ready when MERGEABLE", "MERGEABLE", "ready"},
		{"ready when lowercase mergeable", "mergeable", "ready"},
		{"conflict when CONFLICTING", "CONFLICTING", "conflict"},
		{"conflict when lowercase conflicting", "conflicting", "conflict"},
		{"pending when UNKNOWN", "UNKNOWN", "pending"},
		{"pending when empty", "", "pending"},
		{"pending when other value", "something_else", "pending"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := determineMergeableStatus(tt.mergeable)
			if got != tt.want {
				t.Errorf("determineMergeableStatus(%q) = %q, want %q",
					tt.mergeable, got, tt.want)
			}
		})
	}
}

func TestDetermineColorClass(t *testing.T) {
	tests := []struct {
		name      string
		ciStatus  string
		mergeable string
		want      string
	}{
		{"green when pass and ready", "pass", "ready", "mq-green"},
		{"red when CI fails", "fail", "ready", "mq-red"},
		{"red when conflict", "pass", "conflict", "mq-red"},
		{"red when both fail and conflict", "fail", "conflict", "mq-red"},
		{"yellow when CI pending", "pending", "ready", "mq-yellow"},
		{"yellow when merge pending", "pass", "pending", "mq-yellow"},
		{"yellow when both pending", "pending", "pending", "mq-yellow"},
		{"yellow for unknown states", "unknown", "unknown", "mq-yellow"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := determineColorClass(tt.ciStatus, tt.mergeable)
			if got != tt.want {
				t.Errorf("determineColorClass(%q, %q) = %q, want %q",
					tt.ciStatus, tt.mergeable, got, tt.want)
			}
		})
	}
}

func TestGetRefineryStatusHint(t *testing.T) {
	// Create a minimal fetcher for testing
	f := &LiveConvoyFetcher{}

	tests := []struct {
		name            string
		mergeQueueCount int
		want            string
	}{
		{"idle when no PRs", 0, "Idle - Waiting for PRs"},
		{"singular PR", 1, "Processing 1 PR"},
		{"multiple PRs", 2, "Processing 2 PRs"},
		{"many PRs", 10, "Processing 10 PRs"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := f.getRefineryStatusHint(tt.mergeQueueCount)
			if got != tt.want {
				t.Errorf("getRefineryStatusHint(%d) = %q, want %q",
					tt.mergeQueueCount, got, tt.want)
			}
		})
	}
}

func TestParseActivityTimestamp(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantUnix  int64
		wantValid bool
	}{
		{"valid timestamp", "1704312345", 1704312345, true},
		{"zero timestamp", "0", 0, false},
		{"empty string", "", 0, false},
		{"invalid string", "abc", 0, false},
		{"negative", "-123", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unix, valid := parseActivityTimestamp(tt.input)
			if valid != tt.wantValid {
				t.Errorf("parseActivityTimestamp(%q) valid = %v, want %v",
					tt.input, valid, tt.wantValid)
			}
			if valid && unix != tt.wantUnix {
				t.Errorf("parseActivityTimestamp(%q) = %d, want %d",
					tt.input, unix, tt.wantUnix)
			}
		})
	}
}

func TestFormatTimestamp_ConvertsToLocalZone(t *testing.T) {
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skipf("tzdata not available: %v", err)
	}
	orig := time.Local
	time.Local = loc
	defer func() { time.Local = orig }()

	// Fixed UTC input (as produced by time.Parse(time.RFC3339, ...) on a
	// "...Z" value) from a past year so the formatter takes the
	// year-qualified branch regardless of when the test runs.
	utcInput := time.Date(2024, time.January, 2, 2, 59, 0, 0, time.UTC)
	// 02:59 UTC on Jan 2 is 20:59 CST on Jan 1 in America/Chicago (UTC-6, no DST in January).
	want := "Jan 1 2024, 8:59 PM"

	got := formatTimestamp(utcInput)
	if got != want {
		t.Errorf("formatTimestamp(%v) = %q, want %q (must convert to local before formatting)", utcInput, got, want)
	}
}

// --- calculateWorkerWorkStatus with configurable thresholds ---

func TestCalculateWorkerWorkStatus_DefaultThresholds(t *testing.T) {
	stale := 5 * time.Minute
	stuck := 30 * time.Minute

	tests := []struct {
		name       string
		age        time.Duration
		issueID    string
		workerName string
		want       string
	}{
		{"refinery always working", 1 * time.Hour, "gt-123", "refinery", "working"},
		{"refinery working even without issue", 0, "", "refinery", "working"},
		{"no issue means idle", 0, "", "dag", "idle"},
		{"no issue means idle even if active", 1 * time.Second, "", "nux", "idle"},
		{"very recent is working", 1 * time.Second, "gt-123", "dag", "working"},
		{"just under stale is working", stale - 1*time.Second, "gt-123", "dag", "working"},
		{"at stale boundary is stale", stale, "gt-123", "dag", "stale"},
		{"between stale and stuck is stale", 15 * time.Minute, "gt-123", "dag", "stale"},
		{"just under stuck is stale", stuck - 1*time.Second, "gt-123", "dag", "stale"},
		{"at stuck boundary is stuck", stuck, "gt-123", "dag", "stuck"},
		{"well past stuck is stuck", 2 * time.Hour, "gt-123", "dag", "stuck"},
		{"zero age with issue is working", 0, "gt-456", "nux", "working"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := calculateWorkerWorkStatus(tt.age, tt.issueID, tt.workerName, stale, stuck)
			if got != tt.want {
				t.Errorf("calculateWorkerWorkStatus(%v, %q, %q, %v, %v) = %q, want %q",
					tt.age, tt.issueID, tt.workerName, stale, stuck, got, tt.want)
			}
		})
	}
}

func TestCalculateWorkerWorkStatus_CustomThresholds(t *testing.T) {
	// Use very different thresholds to prove they're actually used
	stale := 1 * time.Minute
	stuck := 5 * time.Minute

	tests := []struct {
		name    string
		age     time.Duration
		issueID string
		want    string
	}{
		{"30s is working with 1m stale", 30 * time.Second, "gt-1", "working"},
		{"90s is stale with 1m stale", 90 * time.Second, "gt-1", "stale"},
		{"3m is stale with 5m stuck", 3 * time.Minute, "gt-1", "stale"},
		{"6m is stuck with 5m stuck", 6 * time.Minute, "gt-1", "stuck"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := calculateWorkerWorkStatus(tt.age, tt.issueID, "dag", stale, stuck)
			if got != tt.want {
				t.Errorf("calculateWorkerWorkStatus(%v, %q, dag, %v, %v) = %q, want %q",
					tt.age, tt.issueID, stale, stuck, got, tt.want)
			}
		})
	}
}

func TestCalculateWorkerWorkStatus_LargeThresholds(t *testing.T) {
	// Very large thresholds — everything should be "working"
	stale := 24 * time.Hour
	stuck := 48 * time.Hour

	got := calculateWorkerWorkStatus(12*time.Hour, "gt-1", "dag", stale, stuck)
	if got != "working" {
		t.Errorf("12h with 24h stale threshold should be working, got %q", got)
	}

	got = calculateWorkerWorkStatus(36*time.Hour, "gt-1", "dag", stale, stuck)
	if got != "stale" {
		t.Errorf("36h with 24h/48h thresholds should be stale, got %q", got)
	}
}

func TestCalculateWorkerWorkStatus_ZeroThresholds(t *testing.T) {
	// Zero thresholds: everything with an issue should be stuck
	got := calculateWorkerWorkStatus(0, "gt-1", "dag", 0, 0)
	if got != "stuck" {
		t.Errorf("0 age with 0/0 thresholds should be stuck, got %q", got)
	}
}

// --- NewConvoyHandler timeout ---

func TestNewConvoyHandler_StoresTimeout(t *testing.T) {
	mock := &MockConvoyFetcher{}
	timeout := 15 * time.Second

	handler, err := NewConvoyHandler(mock, timeout, "test-token")
	if err != nil {
		t.Fatalf("NewConvoyHandler: %v", err)
	}

	if handler.fetchTimeout != timeout {
		t.Errorf("fetchTimeout = %v, want %v", handler.fetchTimeout, timeout)
	}
}

func TestNewConvoyHandler_ZeroTimeout(t *testing.T) {
	mock := &MockConvoyFetcher{}
	handler, err := NewConvoyHandler(mock, 0, "test-token")
	if err != nil {
		t.Fatalf("NewConvoyHandler: %v", err)
	}

	if handler.fetchTimeout != 0 {
		t.Errorf("fetchTimeout = %v, want 0", handler.fetchTimeout)
	}
}

// --- NewAPIHandler timeout ---

func TestNewAPIHandler_StoresTimeouts(t *testing.T) {
	defTimeout := 45 * time.Second
	maxTimeout := 90 * time.Second

	handler := NewAPIHandler(defTimeout, maxTimeout, "test-token")
	if handler.defaultRunTimeout != defTimeout {
		t.Errorf("defaultRunTimeout = %v, want %v", handler.defaultRunTimeout, defTimeout)
	}
	if handler.maxRunTimeout != maxTimeout {
		t.Errorf("maxRunTimeout = %v, want %v", handler.maxRunTimeout, maxTimeout)
	}
}

// --- NewDashboardMux nil config ---

func TestNewDashboardMux_NilConfig(t *testing.T) {
	mock := &MockConvoyFetcher{}
	mux, err := NewDashboardMux(mock, nil)
	if err != nil {
		t.Fatalf("NewDashboardMux(nil config): %v", err)
	}
	if mux == nil {
		t.Fatal("NewDashboardMux returned nil handler")
	}
}

func TestRunCmd_SuccessAndTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based command test")
	}

	// Use generous timeout for success case — not testing timeout behavior here.
	// 500ms was flaky under CI load where process startup can take >1s.
	out, err := runCmd(30*time.Second, "sh", "-c", "printf 'ok'")
	if err != nil {
		t.Fatalf("runCmd success case failed: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "ok" {
		t.Fatalf("runCmd output = %q, want %q", got, "ok")
	}

	// Use "exec sleep" so sleep replaces the shell process — avoids orphan child
	// holding stdout open. Use 200ms timeout (not 30ms) for stability under load.
	_, err = runCmd(200*time.Millisecond, "sh", "-c", "exec sleep 10")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got: %v", err)
	}
}

func TestRunBdCmd_ReturnsStdoutOnNonZeroAndTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based command test")
	}

	binDir := t.TempDir()
	bdPath := filepath.Join(binDir, "bd")
	// Use "exec sleep" so the sleep process replaces the shell — no orphan
	// child processes that hold stdout open after the parent is killed.
	script := `#!/bin/sh
case "$1" in
  warn)
    echo "partial output"
    exit 1
    ;;
  sleep)
    exec sleep 10
    ;;
  *)
    echo "ok"
    exit 0
    ;;
esac
`
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	// Use bdBin with full path instead of t.Setenv("PATH", ...) to avoid
	// process-wide PATH mutation that can race under concurrent test suites.
	// Use generous 30s timeout — this fetcher tests exit-code behavior, not
	// timeouts. The 2s value was flaky under CI load (process startup alone
	// can take >1s under heavy contention).
	f := &LiveConvoyFetcher{cmdTimeout: 30 * time.Second, bdBin: bdPath}

	t.Run("non-zero exit with stdout returns output", func(t *testing.T) {
		stdout, err := f.runBdCmd(t.TempDir(), "warn")
		if err != nil {
			t.Fatalf("runBdCmd warn returned error: %v", err)
		}
		if got := strings.TrimSpace(stdout.String()); got != "partial output" {
			t.Fatalf("runBdCmd warn output = %q, want %q", got, "partial output")
		}
	})

	t.Run("timeout returns explicit error", func(t *testing.T) {
		// Use 200ms timeout (not 20ms) to avoid flakiness from process startup
		// overhead under system load. The script sleeps for 10s so the timeout
		// always fires first with wide margin.
		tf := &LiveConvoyFetcher{cmdTimeout: 200 * time.Millisecond, bdBin: bdPath}
		_, err := tf.runBdCmd(t.TempDir(), "sleep")
		if err == nil {
			t.Fatal("expected timeout error")
		}
		if !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("expected timeout error, got: %v", err)
		}
	})
}

// TestFetchEscalations_SkipsMailDeliveryBeads verifies that escalation
// mail-delivery beads (labeled gt:message, routed by mail.Router alongside
// the escalation wisp — see mail.Router.buildLabels) are excluded from the
// dashboard's escalation count, matching the beads-package filtering already
// applied by filterEscalationRecords for `gt escalate list`.
//
// Regression test for gt-kl7: the dashboard's `bd list --label=gt:escalation`
// query returned both the escalation wisp and its routed mail-delivery
// bead(s), which don't get closed by `gt escalate close`, so resolved
// incidents kept inflating the dashboard's open/unacked P1-P2 count.
func TestFetchEscalations_SkipsMailDeliveryBeads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based command test")
	}

	binDir := t.TempDir()
	bdPath := filepath.Join(binDir, "bd")
	script := `#!/bin/sh
echo '[
  {"id":"hq-wisp1","title":"Real escalation","created_at":"2026-09-08T00:00:00Z","created_by":"gastown/witness","labels":["gt:escalation","severity:high"]},
  {"id":"hq-885m","title":"[HIGH] Real escalation","created_at":"2026-09-08T00:00:00Z","created_by":"gastown/witness","labels":["gt:escalation","gt:message","msg-type:escalation","thread:hq-wisp1"]}
]'
exit 0
`
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	f := &LiveConvoyFetcher{cmdTimeout: 5 * time.Second, bdBin: bdPath, townRoot: t.TempDir()}
	rows, err := f.FetchEscalations()
	if err != nil {
		t.Fatalf("FetchEscalations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("FetchEscalations returned %d rows, want 1 (mail-delivery bead should be skipped): %#v", len(rows), rows)
	}
	if rows[0].ID != "hq-wisp1" {
		t.Fatalf("FetchEscalations returned %q, want the escalation wisp hq-wisp1", rows[0].ID)
	}
}

// TestFetchEscalations_IncludesInfraBeads proves the dashboard's escalation
// query surfaces an open escalation. Escalations are ephemeral wisps
// (--ephemeral --wisp-type=escalation, gt-fcsf), which `bd list` hides
// unless --include-infra is passed; the stub below only returns the
// escalation when that flag is present, so a regression that drops it fails
// this test the way it failed in production (an empty dashboard), not just
// an argv grep.
func TestFetchEscalations_IncludesInfraBeads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based command test")
	}

	binDir := t.TempDir()
	bdPath := filepath.Join(binDir, "bd")
	script := `#!/bin/sh
case "$*" in
  *--include-infra*)
    echo '[{"id":"hq-wisp1","title":"Dolt: server unreachable","created_at":"2026-09-08T00:00:00Z","created_by":"gastown/witness","labels":["gt:escalation","severity:critical"]}]'
    ;;
  *)
    echo '[]'
    ;;
esac
exit 0
`
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	f := &LiveConvoyFetcher{cmdTimeout: 5 * time.Second, bdBin: bdPath, townRoot: t.TempDir()}
	rows, err := f.FetchEscalations()
	if err != nil {
		t.Fatalf("FetchEscalations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("FetchEscalations returned %d rows, want 1 (the open ephemeral escalation): %#v", len(rows), rows)
	}
	if rows[0].ID != "hq-wisp1" {
		t.Fatalf("FetchEscalations returned %q, want hq-wisp1", rows[0].ID)
	}
}

func TestFetchConvoysBreakerBacksOffAfterBdFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based command test")
	}

	tests := []struct {
		name   string
		script string
	}{
		{
			name: "nonzero exit",
			script: `#!/bin/sh
printf x >> "$0.count"
exit 1
`,
		},
		{
			name: "invalid JSON",
			script: `#!/bin/sh
printf x >> "$0.count"
printf '{invalid'
exit 0
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bdPath := filepath.Join(t.TempDir(), "bd")
			if err := os.WriteFile(bdPath, []byte(tt.script), 0o755); err != nil {
				t.Fatalf("write fake bd: %v", err)
			}

			f := &LiveConvoyFetcher{townRoot: t.TempDir(), cmdTimeout: 5 * time.Second, bdBin: bdPath}
			if _, err := f.FetchConvoys(); err == nil {
				t.Fatal("expected first FetchConvoys call to fail")
			}

			// The immediate retry is backed off — it must not re-invoke bd —
			// but it still must report the breaker-open error rather than
			// (nil, nil): a caller cannot tell "still failing" from "town is
			// empty" from a nil error (gt-jwf7).
			if _, err := f.FetchConvoys(); !errors.Is(err, errConvoyBreakerOpen) {
				t.Fatalf("expected immediate retry to report errConvoyBreakerOpen, got: %v", err)
			}

			countBytes, err := os.ReadFile(bdPath + ".count")
			if err != nil {
				t.Fatalf("read fake bd call count: %v", err)
			}
			if got := len(countBytes); got != 1 {
				t.Fatalf("fake bd calls = %d, want 1", got)
			}
		})
	}
}

// TestGetTrackedIssues_FallsBackToShowForExternalEdges verifies gt-q0is: when
// both `bd dep list` forms return empty, getTrackedIssues falls back to
// `bd show` and still resolves the tracked issue instead of silently
// reporting zero tracked issues. The show fallback is the last resort for
// bd builds without the raw-edge form (see rawTrackedDeps).
func TestGetTrackedIssues_FallsBackToShowForExternalEdges(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based command test")
	}

	bdPath := filepath.Join(t.TempDir(), "bd")
	script := `#!/bin/sh
case "$1" in
  dep)
    echo '[]'
    ;;
  show)
    case "$2" in
      hq-cv-432tu)
        echo '[{"dependencies":[{"id":"external:gt:gt-dcku","status":"open","dependency_type":"tracks"},{"id":"hq-other","status":"open","dependency_type":"blocks"}]}]'
        ;;
      *)
        echo '[{"id":"gt-dcku","title":"Fix the thing","status":"open","assignee":"gastown/polecats/flint","updated_at":"2026-09-10T22:00:00Z"}]'
        ;;
    esac
    ;;
esac
`
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	restore := fetcherRunCmd
	defer func() { fetcherRunCmd = restore }()
	fetcherRunCmd = func(_ time.Duration, name string, args ...string) (*bytes.Buffer, error) {
		// Only tmux still runs through this seam — bd calls all exec the fake
		// script so they can be steered per-argument.
		if name != "tmux" {
			t.Fatalf("unexpected command: %s %v", name, args)
		}
		return bytes.NewBufferString(""), nil
	}

	f := &LiveConvoyFetcher{townRoot: t.TempDir(), cmdTimeout: 5 * time.Second, bdBin: bdPath}

	tracked, err := f.getTrackedIssues("hq-cv-432tu")
	if err != nil {
		t.Fatalf("getTrackedIssues returned error: %v", err)
	}
	if len(tracked) != 1 {
		t.Fatalf("tracked issues = %d, want 1 (external edge should resolve via bd show fallback)", len(tracked))
	}
	if tracked[0].ID != "gt-dcku" {
		t.Fatalf("tracked[0].ID = %q, want %q", tracked[0].ID, "gt-dcku")
	}
	if tracked[0].Assignee != "gastown/polecats/flint" {
		t.Fatalf("tracked[0].Assignee = %q, want %q", tracked[0].Assignee, "gastown/polecats/flint")
	}
}

// TestGetTrackedIssues_CrossRigRawEdges verifies gt-44z1: a convoy whose
// tracked beads all live in another rig must still render real progress.
//
// The fixture is the live shape that broke the dashboard: `bd show <convoy>
// --json` reports "dependencies": null (bd only fills that array for edges
// whose target resolves in the same database), while `bd dep list <convoy>
// <convoy> --json` — the raw-edge form bd's own warning points at — returns
// depends_on_id "external:om:om-59p" records. It also carries two edges that
// must not become tracked issues: a non-tracks edge, and a malformed
// external:<rig>:<id> whose "id" is really a title.
//
// If resolution ever regresses to the show path, the null fixture makes this
// test report 0/0 instead of 2/3.
func TestGetTrackedIssues_CrossRigRawEdges(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based command test")
	}

	bdPath := filepath.Join(t.TempDir(), "bd")
	script := `#!/bin/sh
case "$1" in
  dep)
    echo '[{"issue_id":"hq-cv-7rzqg","depends_on_id":"external:om:om-59p","type":"tracks"},{"issue_id":"hq-cv-7rzqg","depends_on_id":"external:om:om-8mb","type":"tracks"},{"issue_id":"hq-cv-7rzqg","depends_on_id":"external:om:om-v3b","type":"tracks"},{"issue_id":"hq-cv-7rzqg","depends_on_id":"external:om:om-gate coverage: om","type":"tracks"},{"issue_id":"hq-cv-7rzqg","depends_on_id":"hq-something","type":"blocks"}]'
    ;;
  show)
    case "$2" in
      hq-cv-7rzqg)
        echo '[{"id":"hq-cv-7rzqg","dependency_count":3,"dependencies":null}]'
        ;;
      *)
        echo '[{"id":"om-59p","title":"om-gate T12","status":"closed","assignee":"om/polecats/onyx","updated_at":"2026-09-11T03:00:00Z"},{"id":"om-8mb","title":"om-gate T13","status":"closed","assignee":"om/polecats/quartz","updated_at":"2026-09-11T03:00:00Z"},{"id":"om-v3b","title":"om-gate T14","status":"open","assignee":"om/polecats/jasper","updated_at":"2026-09-11T03:00:00Z"}]'
        ;;
    esac
    ;;
esac
`
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	restore := fetcherRunCmd
	defer func() { fetcherRunCmd = restore }()
	fetcherRunCmd = func(_ time.Duration, name string, args ...string) (*bytes.Buffer, error) {
		if name != "tmux" {
			t.Fatalf("unexpected command: %s %v", name, args)
		}
		return bytes.NewBufferString(""), nil
	}

	f := &LiveConvoyFetcher{townRoot: t.TempDir(), cmdTimeout: 5 * time.Second, bdBin: bdPath}

	tracked, err := f.getTrackedIssues("hq-cv-7rzqg")
	if err != nil {
		t.Fatalf("getTrackedIssues returned error: %v", err)
	}

	gotIDs := make([]string, 0, len(tracked))
	completed := 0
	for _, tr := range tracked {
		gotIDs = append(gotIDs, tr.ID)
		if tr.Status == "closed" {
			completed++
		}
	}
	wantIDs := []string{"om-59p", "om-8mb", "om-v3b"}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("tracked IDs = %q, want %q (malformed and non-tracks edges must be skipped)", gotIDs, wantIDs)
	}
	if got := fmt.Sprintf("%d/%d", completed, len(tracked)); got != "2/3" {
		t.Fatalf("progress = %s, want 2/3", got)
	}
	for _, tr := range tracked {
		if tr.Status == "unknown" {
			t.Fatalf("tracked %s resolved to unknown status — cross-rig bd show did not run from the town root", tr.ID)
		}
	}
}

// TestIsResolvableIssueID pins the filter that keeps a malformed edge from
// blanking out a convoy row (gt-44z1).
func TestIsResolvableIssueID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"gt-44z1", true},
		{"hq-cv-7rzqg", true},
		{"om-59p", true},
		{"ap-qtsup.16", true},
		{"be-wisp-o3q", true},
		{"om-gate coverage: om", false}, // title used as an ID (live garbage edge)
		{"external:om", false},          // malformed wrapper, left unstripped
		{"", false},
		{"-leading-dash", false},
		{"has/slash", false},
	}
	for _, tt := range tests {
		if got := isResolvableIssueID(tt.id); got != tt.want {
			t.Errorf("isResolvableIssueID(%q) = %v, want %v", tt.id, got, tt.want)
		}
	}
}

// TestFetchConvoys_NoAssigneeNeverReportsStuck verifies gt-q0is: a convoy
// with tracked issues but no assignee (or no tracked issues at all) must
// never be colored STUCK based on an unrelated polecat's tmux activity. It
// should render "unassigned"/"waiting" instead.
func TestFetchConvoys_NoAssigneeNeverReportsStuck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based command test")
	}

	bdPath := filepath.Join(t.TempDir(), "bd")
	script := `#!/bin/sh
case "$1" in
  list)
    echo '[{"id":"hq-cv-1","title":"Convoy","status":"open","issue_type":"convoy","labels":[]}]'
    ;;
  dep)
    echo '[{"issue_id":"hq-cv-1","depends_on_id":"gt-abc","type":"tracks"}]'
    ;;
  show)
    case "$2" in
      hq-cv-1)
        echo '[{"id":"hq-cv-1","dependency_count":1,"dependencies":null}]'
        ;;
      *)
        # getIssueDetailsBatch: "bd show gt-abc --json" — no assignee.
        echo '[{"id":"gt-abc","title":"Untouched","status":"open","assignee":"","updated_at":""}]'
        ;;
    esac
    ;;
esac
`
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	restore := fetcherRunCmd
	defer func() { fetcherRunCmd = restore }()
	fetcherRunCmd = func(_ time.Duration, name string, args ...string) (*bytes.Buffer, error) {
		if name != "tmux" {
			t.Fatalf("unexpected command: %s %v", name, args)
		}
		// Simulate a long-idle, totally unrelated polecat session still
		// running elsewhere in the town — this must NOT leak into this
		// convoy's status.
		return bytes.NewBufferString("gt-otherrig-somepolecat|1\n"), nil
	}

	// The registry has to actually resolve the unrelated session below, or the
	// pre-fix "any running polecat" fallback skips every tmux line it is handed
	// (ParseSessionNameWithRegistry errors on an unknown prefix) and returns
	// nil — which is exactly how this test used to pass against the code it
	// was written to guard (gt-f1td).
	registry := session.NewPrefixRegistry()
	registry.Register("gt", "gastown")
	if id, err := session.ParseSessionNameWithRegistry("gt-otherrig-somepolecat", registry); err != nil || id.Role != session.RolePolecat {
		t.Fatalf("fixture session name must parse as a polecat under the test registry, got %+v (err=%v)", id, err)
	}

	f := &LiveConvoyFetcher{townRoot: t.TempDir(), cmdTimeout: 5 * time.Second, bdBin: bdPath, registry: registry}

	rows, err := f.FetchConvoys()
	if err != nil {
		t.Fatalf("FetchConvoys returned error: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.WorkStatus == "stuck" {
		t.Fatalf("WorkStatus = %q, want anything but stuck for an unassigned convoy", row.WorkStatus)
	}
	if row.WorkStatus != "waiting" {
		t.Fatalf("WorkStatus = %q, want %q", row.WorkStatus, "waiting")
	}
	if !strings.Contains(row.LastActivity.FormattedAge, "unassigned") {
		t.Fatalf("LastActivity.FormattedAge = %q, want it to mention unassigned", row.LastActivity.FormattedAge)
	}
}

// TestFetchConvoys_TimedOutDetailReadKeepsConvoyCounted verifies gt-huzu: a
// convoy whose tracked-issue read times out must still be counted and rendered,
// marked unreadable. Dropping the row made a partial read indistinguishable
// from a complete one — the panel showed a convoy count short by exactly the
// convoys bd was slowest on, with nothing to say it was short.
func TestFetchConvoys_TimedOutDetailReadKeepsConvoyCounted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based command test")
	}

	bdPath := filepath.Join(t.TempDir(), "bd")
	// hq-cv-slow spins on its dep reads until the fetcher's deadline kills it.
	// The spin forks nothing (kill is a shell builtin), so no child outlives
	// the kill holding a copy of the stdout pipe open — the call returns at
	// the deadline rather than waiting out a stray sleep. It spins only while
	// its parent lives: if the test binary itself is killed mid-test (a suite
	// timeout, an orphan sweep), an unconditional spin is reparented to
	// launchd/init and burns a core forever — one ran for 10h at ~80% CPU on
	// the gate host (2026-09-23).
	script := `#!/bin/sh
case "$1" in
  list)
    echo '[{"id":"hq-cv-ok","title":"Readable","status":"open","issue_type":"convoy","labels":[]},{"id":"hq-cv-slow","title":"Unreadable","status":"open","issue_type":"convoy","labels":[]}]'
    ;;
  dep)
    case "$*" in
      *hq-cv-slow*) while kill -0 "$PPID" 2>/dev/null; do :; done ;;
    esac
    echo '[{"depends_on_id":"gt-abc","type":"tracks"}]'
    ;;
  show)
    case "$2" in
      hq-cv-ok) echo '[{"id":"hq-cv-ok","dependencies":null}]' ;;
      *) echo '[{"id":"gt-abc","title":"Work","status":"open","assignee":"","updated_at":""}]' ;;
    esac
    ;;
esac
`
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	// Only hq-cv-slow's dep read gets the short deadline it exists to blow.
	// The list/show/dep calls around it keep a generous budget: one shared
	// 300ms cmdTimeout killed the trivially fast list call on a loaded gate
	// host ("listing convoys: bd timed out after 300ms"), failing the test
	// before it reached the timeout path it is about. The slow call spins
	// until killed, so its deadline is always the thing that ends it, however
	// loaded the host is.
	f := &LiveConvoyFetcher{
		townRoot:   t.TempDir(),
		cmdTimeout: 30 * time.Second,
		bdBin:      bdPath,
		bdTimeoutFor: func(args []string) time.Duration {
			if len(args) > 0 && args[0] == "dep" && strings.Contains(strings.Join(args, " "), "hq-cv-slow") {
				return 300 * time.Millisecond
			}
			return 30 * time.Second
		},
	}

	rows, err := f.FetchConvoys()
	if err != nil {
		t.Fatalf("FetchConvoys returned error: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 — the timed-out convoy must still be counted: %#v", len(rows), rows)
	}

	byID := make(map[string]ConvoyRow, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}

	ok, found := byID["hq-cv-ok"]
	if !found {
		t.Fatal("readable convoy hq-cv-ok missing from the panel")
	}
	if ok.DetailErr != "" {
		t.Fatalf("hq-cv-ok DetailErr = %q, want empty", ok.DetailErr)
	}
	if ok.Total != 1 || ok.Progress != "0/1" {
		t.Fatalf("hq-cv-ok Total = %d, Progress = %q, want 1 and 0/1", ok.Total, ok.Progress)
	}

	slow, found := byID["hq-cv-slow"]
	if !found {
		t.Fatal("timed-out convoy hq-cv-slow was dropped from the panel (gt-huzu)")
	}
	if slow.DetailErr != convoyDetailUnavailable {
		t.Fatalf("hq-cv-slow DetailErr = %q, want %q — an unreadable row must be marked, not rendered as a zero",
			slow.DetailErr, convoyDetailUnavailable)
	}
	if slow.LastActivity.FormattedAge != convoyDetailUnavailable {
		t.Fatalf("hq-cv-slow activity = %q, want %q", slow.LastActivity.FormattedAge, convoyDetailUnavailable)
	}
	if got := countUnreadableConvoys(rows); got != 1 {
		t.Fatalf("countUnreadableConvoys = %d, want 1", got)
	}
}

func TestFetchConvoysBreakerPreventsConcurrentStampede(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based command test")
	}

	bdPath := filepath.Join(t.TempDir(), "bd")
	script := `#!/bin/sh
printf x >> "$0.count"
sleep 0.2
printf '{invalid'
exit 0
`
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	f := &LiveConvoyFetcher{townRoot: t.TempDir(), cmdTimeout: 5 * time.Second, bdBin: bdPath}
	start := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.FetchConvoys()
			errCh <- err
		}()
	}

	close(start)
	wg.Wait()
	close(errCh)

	// Exactly one goroutine wins the in-flight slot and hits the fake bd; the
	// rest are turned away by the breaker. All 8 must still report a non-nil
	// error — a backed-off caller reporting nil would read as "no convoys"
	// rather than "still failing" (gt-jwf7).
	realErrs, breakerOpenErrs := 0, 0
	for err := range errCh {
		switch {
		case errors.Is(err, errConvoyBreakerOpen):
			breakerOpenErrs++
		case err != nil:
			realErrs++
		}
	}
	if realErrs != 1 {
		t.Fatalf("FetchConvoys real errors = %d, want 1 (the single caller that hit bd)", realErrs)
	}
	if breakerOpenErrs != 7 {
		t.Fatalf("FetchConvoys errConvoyBreakerOpen errors = %d, want 7 (every backed-off caller)", breakerOpenErrs)
	}

	countBytes, err := os.ReadFile(bdPath + ".count")
	if err != nil {
		t.Fatalf("read fake bd call count: %v", err)
	}
	if got := len(countBytes); got != 1 {
		t.Fatalf("fake bd calls = %d, want 1", got)
	}
}

func withMayorFetcherHooks(t *testing.T, sessionEnv func(sessionName, key string) (string, error), runCmdFunc func(time.Duration, string, ...string) (*bytes.Buffer, error)) {
	t.Helper()

	originalGetEnv := fetcherGetSessionEnv
	originalRunCmd := fetcherRunCmd
	t.Cleanup(func() {
		fetcherGetSessionEnv = originalGetEnv
		fetcherRunCmd = originalRunCmd
	})
	t.Cleanup(config.ResetRegistryForTesting)

	if sessionEnv != nil {
		fetcherGetSessionEnv = sessionEnv
	}
	if runCmdFunc != nil {
		fetcherRunCmd = runCmdFunc
	}
}

func TestResolveMayorRuntime(t *testing.T) {
	tests := []struct {
		name        string
		sessionEnv  func(sessionName, key string) (string, error)
		setup       func(t *testing.T, townRoot string)
		wantRuntime string
	}{
		{
			name: "uses session agent env",
			sessionEnv: func(sessionName, key string) (string, error) {
				if sessionName != "hq-mayor" || key != "GT_AGENT" {
					t.Fatalf("unexpected session env lookup: %s %s", sessionName, key)
				}
				return "codex", nil
			},
			wantRuntime: "codex",
		},
		{
			name: "falls back to town settings",
			sessionEnv: func(string, string) (string, error) {
				return "", os.ErrNotExist
			},
			setup: func(t *testing.T, townRoot string) {
				t.Helper()
				settings := config.NewTownSettings()
				settings.DefaultAgent = "codex"
				if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), settings); err != nil {
					t.Fatalf("SaveTownSettings: %v", err)
				}
			},
			wantRuntime: "codex",
		},
		{
			name: "uses custom role agent alias",
			sessionEnv: func(string, string) (string, error) {
				return "", os.ErrNotExist
			},
			setup: func(t *testing.T, townRoot string) {
				t.Helper()
				settings := config.NewTownSettings()
				settings.RoleAgents[constants.RoleMayor] = "claude-sonnet"
				settings.Agents["claude-sonnet"] = &config.RuntimeConfig{
					Command: "claude",
					Args:    []string{"--dangerously-skip-permissions", "--model", "sonnet"},
				}
				if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), settings); err != nil {
					t.Fatalf("SaveTownSettings: %v", err)
				}
			},
			wantRuntime: "claude/sonnet",
		},
		{
			name: "uses registry agent from session env",
			sessionEnv: func(sessionName, key string) (string, error) {
				if sessionName != "hq-mayor" || key != "GT_AGENT" {
					t.Fatalf("unexpected session env lookup: %s %s", sessionName, key)
				}
				return "mayor-registry", nil
			},
			setup: func(t *testing.T, townRoot string) {
				t.Helper()
				registry := &config.AgentRegistry{
					Version: config.CurrentAgentRegistryVersion,
					Agents: map[string]*config.AgentPresetInfo{
						"mayor-registry": {
							Name:    "mayor-registry",
							Command: "opencode",
							Args:    []string{"run", "--model", "gpt-5"},
						},
					},
				}
				if err := config.SaveAgentRegistry(config.DefaultAgentRegistryPath(townRoot), registry); err != nil {
					t.Fatalf("SaveAgentRegistry: %v", err)
				}
			},
			wantRuntime: "opencode/gpt-5",
		},
		{
			name: "uses ephemeral tier agent from session env",
			sessionEnv: func(sessionName, key string) (string, error) {
				if sessionName != "hq-mayor" || key != "GT_AGENT" {
					t.Fatalf("unexpected session env lookup: %s %s", sessionName, key)
				}
				return "claude-sonnet", nil
			},
			setup: func(t *testing.T, townRoot string) {
				t.Helper()
				if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), config.NewTownSettings()); err != nil {
					t.Fatalf("SaveTownSettings: %v", err)
				}
				t.Setenv("GT_COST_TIER", "economy")
			},
			wantRuntime: "claude/sonnet",
		},
		{
			name: "uses provider only role agent alias",
			sessionEnv: func(string, string) (string, error) {
				return "", os.ErrNotExist
			},
			setup: func(t *testing.T, townRoot string) {
				t.Helper()
				settings := config.NewTownSettings()
				settings.RoleAgents[constants.RoleMayor] = "mayor-custom"
				settings.Agents["mayor-custom"] = &config.RuntimeConfig{
					Provider: "codex",
				}
				if err := config.SaveTownSettings(config.TownSettingsPath(townRoot), settings); err != nil {
					t.Fatalf("SaveTownSettings: %v", err)
				}
			},
			wantRuntime: "codex",
		},
		{
			name: "returns unknown alias verbatim when unresolved",
			sessionEnv: func(sessionName, key string) (string, error) {
				if sessionName != "hq-mayor" || key != "GT_AGENT" {
					t.Fatalf("unexpected session env lookup: %s %s", sessionName, key)
				}
				return "mystery-agent", nil
			},
			wantRuntime: "mystery-agent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withMayorFetcherHooks(t, tt.sessionEnv, nil)

			townRoot := t.TempDir()
			if tt.setup != nil {
				tt.setup(t, townRoot)
			}

			f := &LiveConvoyFetcher{townRoot: townRoot}
			if got := f.resolveMayorRuntime("hq-mayor"); got != tt.wantRuntime {
				t.Fatalf("resolveMayorRuntime() = %q, want %q", got, tt.wantRuntime)
			}
		})
	}
}

func TestRuntimeLabelFromConfig(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		args     []string
		fallback string
		want     string
	}{
		{
			name:     "claude model flag",
			command:  "claude",
			args:     []string{"--dangerously-skip-permissions", "--model", "sonnet"},
			fallback: "claude-sonnet",
			want:     "claude/sonnet",
		},
		{
			name:     "short model flag",
			command:  "opencode",
			args:     []string{"run", "-m", "gpt-5"},
			fallback: "custom-opencode",
			want:     "opencode/gpt-5",
		},
		{
			name:     "cgroup wrap unwraps binary",
			command:  "cgroup-wrap",
			args:     []string{"/usr/local/bin/codex", "--dangerously-bypass-approvals-and-sandbox"},
			fallback: "codex",
			want:     "codex",
		},
		{
			name:     "empty command falls back to alias",
			command:  "",
			args:     nil,
			fallback: "mystery-agent",
			want:     "mystery-agent",
		},
		{
			name:     "empty command and fallback defaults to claude",
			command:  "",
			args:     nil,
			fallback: "",
			want:     "claude",
		},
		{
			name:     "model equals form long flag",
			command:  "claude",
			args:     []string{"--dangerously-skip-permissions", "--model=sonnet"},
			fallback: "claude-sonnet",
			want:     "claude/sonnet",
		},
		{
			name:     "model equals form short flag",
			command:  "opencode",
			args:     []string{"-m=gpt-5"},
			fallback: "custom",
			want:     "opencode/gpt-5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := runtimeLabelFromConfig(tt.command, tt.args, tt.fallback); got != tt.want {
				t.Fatalf("runtimeLabelFromConfig(%q, %v, %q) = %q, want %q", tt.command, tt.args, tt.fallback, got, tt.want)
			}
		})
	}
}

func TestFetchMayor_UsesResolvedRuntime(t *testing.T) {
	withMayorFetcherHooks(
		t,
		func(sessionName, key string) (string, error) {
			if sessionName != "hq-mayor" || key != "GT_AGENT" {
				t.Fatalf("unexpected session env lookup: %s %s", sessionName, key)
			}
			return "codex", nil
		},
		func(_ time.Duration, name string, args ...string) (*bytes.Buffer, error) {
			if name != "tmux" {
				t.Fatalf("unexpected command: %s %v", name, args)
			}
			return bytes.NewBufferString("hq-mayor:1731328320\nhq-deacon:1731328300\n"), nil
		},
	)

	f := &LiveConvoyFetcher{
		townRoot:             t.TempDir(),
		mayorActiveThreshold: 24 * time.Hour,
		tmuxCmdTimeout:       time.Second,
	}

	status, err := f.FetchMayor()
	if err != nil {
		t.Fatalf("FetchMayor: %v", err)
	}
	if !status.IsAttached {
		t.Fatal("expected mayor to be attached")
	}
	if status.SessionName != "hq-mayor" {
		t.Fatalf("SessionName = %q, want %q", status.SessionName, "hq-mayor")
	}
	if status.Runtime != "codex" {
		t.Fatalf("Runtime = %q, want %q", status.Runtime, "codex")
	}
	if status.LastActivity == "" {
		t.Fatal("expected LastActivity to be populated")
	}
}

// TestFetchHealth_DeaconHeartbeatFieldName verifies that FetchHealth reads the
// "timestamp" field written by heartbeat.go, not the old "last_heartbeat" field
// that caused dashboard to always show "no timestamp". (GH#2989)
func TestFetchHealth_DeaconHeartbeatFieldName(t *testing.T) {
	townRoot := t.TempDir()
	deaconDir := filepath.Join(townRoot, "deacon")
	if err := os.MkdirAll(deaconDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write heartbeat.json using the field name heartbeat.go actually writes.
	now := time.Now().UTC().Truncate(time.Second)
	heartbeatJSON := fmt.Sprintf(`{"timestamp":%q,"cycle":42,"healthy_agents":3,"unhealthy_agents":1}`,
		now.Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(deaconDir, "heartbeat.json"), []byte(heartbeatJSON), 0644); err != nil {
		t.Fatal(err)
	}

	f := &LiveConvoyFetcher{
		townRoot:                townRoot,
		heartbeatFreshThreshold: 5 * time.Minute,
	}

	health, err := f.FetchHealth()
	if err != nil {
		t.Fatalf("FetchHealth: %v", err)
	}

	// DeaconHeartbeat must NOT be "no timestamp" — the field was read correctly.
	if health.DeaconHeartbeat == "no timestamp" {
		t.Fatal("DeaconHeartbeat = \"no timestamp\": JSON field name mismatch (GH#2989)")
	}
	if health.DeaconHeartbeat == "no heartbeat" {
		t.Fatal("DeaconHeartbeat = \"no heartbeat\": heartbeat file was not read")
	}

	// Cycle and agent counts should be populated.
	if health.DeaconCycle != 42 {
		t.Errorf("DeaconCycle = %d, want 42", health.DeaconCycle)
	}
	if health.HealthyAgents != 3 {
		t.Errorf("HealthyAgents = %d, want 3", health.HealthyAgents)
	}
	if health.UnhealthyAgents != 1 {
		t.Errorf("UnhealthyAgents = %d, want 1", health.UnhealthyAgents)
	}

	// Heartbeat should be considered fresh (written just now).
	if !health.HeartbeatFresh {
		t.Error("HeartbeatFresh = false for a just-written heartbeat")
	}
}

// ============================================
// TOWN MERGE QUEUE (gt-r65r)
// ============================================

// townRootWithRigs builds a temp town whose rigs.json lists the given rigs and
// whose rig directories exist, which is what the merge-queue derivation checks.
func townRootWithRigs(t *testing.T, rigs ...string) string {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatalf("create mayor dir: %v", err)
	}
	entries := make([]string, 0, len(rigs))
	for _, rig := range rigs {
		entries = append(entries, fmt.Sprintf("%q:{\"git_url\":\"x\",\"added_at\":\"2026-01-01T00:00:00Z\"}", rig))
		if err := os.MkdirAll(filepath.Join(townRoot, rig), 0o755); err != nil {
			t.Fatalf("create rig dir %s: %v", rig, err)
		}
	}
	rigsJSON := fmt.Sprintf(`{"version":1,"rigs":{%s}}`, strings.Join(entries, ","))
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "rigs.json"), []byte(rigsJSON), 0o644); err != nil {
		t.Fatalf("write rigs.json: %v", err)
	}
	return townRoot
}

// mrWisp builds a merge-request wisp with the description fields `gt mq submit`
// writes. Blocked MRs carry a blocker id, which is how gt mq list derives the
// blocked status.
func mrWisp(id, rig, branch, worker string, priority int, created time.Time, blocked bool) *beads.Issue {
	issue := &beads.Issue{
		ID:          id,
		Status:      "open",
		Priority:    priority,
		CreatedAt:   created.Format(time.RFC3339),
		CreatedBy:   fmt.Sprintf("%s/polecats/%s", rig, worker),
		Description: fmt.Sprintf("branch: %s\ntarget: main\nrig: %s\nworker: %s", branch, rig, worker),
	}
	if blocked {
		issue.BlockedBy = []string{"gt-blocker"}
	}
	return issue
}

func TestTownMergeQueueSnapshotDerivesRowsFromLister(t *testing.T) {
	townRoot := townRootWithRigs(t, "gastown", "beads")
	now := time.Now()

	var listDirs []string
	var listOpts []beads.ListOptions
	f := &LiveConvoyFetcher{
		townRoot: townRoot,
		listMRs: func(beadsDir string, opts beads.ListOptions) ([]*beads.Issue, error) {
			listDirs = append(listDirs, filepath.Base(beadsDir))
			listOpts = append(listOpts, opts)
			switch filepath.Base(beadsDir) {
			case "gastown":
				return []*beads.Issue{
					mrWisp("gt-wisp-new", "gastown", "polecat/quartz/gt-new", "quartz", 2, now.Add(-5*time.Minute), false),
					mrWisp("gt-wisp-old", "gastown", "polecat/flint/gt-old", "flint", 1, now.Add(-45*time.Minute), true),
				}, nil
			case "beads":
				return []*beads.Issue{
					mrWisp("be-wisp-a", "beads", "be/crew/be-a", "be/crew", 1, now.Add(-90*time.Minute), false),
				}, nil
			}
			return nil, nil
		},
		countMerges: func(string, time.Duration) (int, error) { return 0, fmt.Errorf("no clone") },
	}

	snapshot, err := f.townMergeQueueSnapshot()
	if err != nil {
		t.Fatalf("townMergeQueueSnapshot() error = %v", err)
	}

	// Every rig laid out on disk is asked, with the label and filters
	// `gt mq list` uses — a priority of 0 here would mean "P0 only".
	if len(listDirs) != 2 {
		t.Fatalf("lister called for %v, want both rigs", listDirs)
	}
	for i, opts := range listOpts {
		if opts.Label != mergeRequestLabel || opts.Status != "open" || opts.Priority != -1 || opts.Rig == "" {
			t.Errorf("listOpts[%d] = %+v, want label=%s status=open priority=-1 and a rig", i, opts, mergeRequestLabel)
		}
	}

	if got, want := len(snapshot.Rows), 3; got != want {
		t.Fatalf("len(Rows) = %d, want %d", got, want)
	}
	// Ready before blocked; then priority, then age.
	wantOrder := []string{"be-wisp-a", "gt-wisp-new", "gt-wisp-old"}
	for i, want := range wantOrder {
		if got := snapshot.Rows[i].ID; got != want {
			t.Errorf("Rows[%d].ID = %q, want %q", i, got, want)
		}
	}
	if snapshot.ReadyCount != 2 {
		t.Errorf("ReadyCount = %d, want 2", snapshot.ReadyCount)
	}

	ready := snapshot.Rows[1]
	if ready.Status != "ready" || ready.ColorClass != "mq-green" {
		t.Errorf("ready row = %+v, want status ready and mq-green", ready)
	}
	if ready.Branch != "polecat/quartz/gt-new" || ready.Rig != "gastown" || ready.Assignee != "quartz" {
		t.Errorf("ready row = %+v, want the description's branch, rig, and worker", ready)
	}
	if ready.Age != "5m" {
		t.Errorf("ready row Age = %q, want 5m", ready.Age)
	}

	blocked := snapshot.Rows[2]
	if blocked.Status != "blocked" || blocked.ColorClass != "mq-red" {
		t.Errorf("blocked row = %+v, want status blocked and mq-red", blocked)
	}
	if blocked.Age != "45m" {
		t.Errorf("blocked row Age = %q, want 45m", blocked.Age)
	}

	// A rig with no MRs contributes no rows but is not an error.
	if snapshot.Merges6hTotal != 0 {
		t.Errorf("Merges6hTotal = %d, want 0 when no clone answered", snapshot.Merges6hTotal)
	}
}

func TestTownMergeQueueSnapshotErrorsOnlyWhenEveryRigFails(t *testing.T) {
	townRoot := townRootWithRigs(t, "gastown", "beads")
	f := &LiveConvoyFetcher{
		townRoot: townRoot,
		listMRs: func(beadsDir string, _ beads.ListOptions) ([]*beads.Issue, error) {
			if filepath.Base(beadsDir) == "gastown" {
				return nil, fmt.Errorf("dolt unreachable")
			}
			return nil, nil
		},
		countMerges: func(string, time.Duration) (int, error) { return 0, fmt.Errorf("no clone") },
	}

	// One rig answered (with nothing): the town's queue is empty, not broken.
	if _, err := f.townMergeQueueSnapshot(); err != nil {
		t.Fatalf("townMergeQueueSnapshot() error = %v, want nil when a rig answered", err)
	}

	f.listMRs = func(string, beads.ListOptions) ([]*beads.Issue, error) {
		return nil, fmt.Errorf("dolt unreachable")
	}
	if _, err := f.townMergeQueueSnapshot(); err == nil {
		t.Fatal("townMergeQueueSnapshot() = nil error, want the every-rig failure reported")
	}
}

func TestFetchTownMergeQueueServesStaleSnapshotWithoutBlocking(t *testing.T) {
	now := time.Now()
	release := make(chan struct{})
	listed := make(chan struct{})
	var listCalls int

	f := &LiveConvoyFetcher{
		townRoot: townRootWithRigs(t, "gastown"),
		listMRs: func(string, beads.ListOptions) ([]*beads.Issue, error) {
			listCalls++
			<-release
			close(listed)
			return []*beads.Issue{mrWisp("gt-wisp-a", "gastown", "polecat/x/gt-a", "x", 1, now, false)}, nil
		},
		countMerges: func(string, time.Duration) (int, error) { return 0, fmt.Errorf("no clone") },
	}

	// The cold call returns immediately: the derivation runs in the background.
	if got := f.FetchTownMergeQueue(); len(got.Rows) != 0 {
		t.Fatalf("cold snapshot = %+v, want empty until the first refresh lands", got)
	}

	close(release)
	<-listed

	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := f.FetchTownMergeQueue(); len(got.Rows) == 1 {
			if got.ReadyCount != 1 {
				t.Errorf("ReadyCount = %d, want 1", got.ReadyCount)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("published snapshot never became visible")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A fresh snapshot is served from memory, so a render costs no bd process.
	for i := 0; i < 5; i++ {
		f.FetchTownMergeQueue()
	}
	if listCalls != 1 {
		t.Errorf("lister called %d times, want 1 while the snapshot is fresh", listCalls)
	}
}

func TestMergesLast6hCountsEachRigAndCaches(t *testing.T) {
	f := &LiveConvoyFetcher{townRoot: "/town"}
	counts := map[string]int{"gastown": 3, "beads": 2, "hm": 0}
	var calls []string

	f.countMerges = func(repoPath string, window time.Duration) (int, error) {
		if window != mergesWindow {
			t.Errorf("window = %v, want %v", window, mergesWindow)
		}
		rig := filepath.Base(filepath.Dir(filepath.Dir(repoPath)))
		calls = append(calls, rig)
		if rig == "om" {
			return 0, fmt.Errorf("no clone")
		}
		return counts[rig], nil
	}

	tile, total := f.mergesLast6h([]string{"hm", "gastown", "om", "beads"})
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	want := []RigMergeCount{{Rig: "beads", Count: 2}, {Rig: "gastown", Count: 3}, {Rig: "hm", Count: 0}}
	if len(tile) != len(want) {
		t.Fatalf("tile = %+v, want %+v", tile, want)
	}
	for i := range want {
		if tile[i] != want[i] {
			t.Errorf("tile[%d] = %+v, want %+v", i, tile[i], want[i])
		}
	}

	callsAfterFirst := len(calls)
	if _, total := f.mergesLast6h([]string{"hm", "gastown", "om", "beads"}); total != 5 {
		t.Errorf("cached total = %d, want 5", total)
	}
	if len(calls) != callsAfterFirst {
		t.Errorf("countMerges ran %d times after a cached second call, want %d", len(calls)-callsAfterFirst, 0)
	}
}

func TestCountMergesOnMainUsesParseableSince(t *testing.T) {
	restore := fetcherRunCmd
	defer func() { fetcherRunCmd = restore }()

	var gotArgs []string
	fetcherRunCmd = func(_ time.Duration, name string, args ...string) (*bytes.Buffer, error) {
		if name != "git" {
			t.Fatalf("ran %q, want git", name)
		}
		gotArgs = args
		return bytes.NewBufferString("4\n"), nil
	}

	n, err := countMergesOnMain("/town/gastown/mayor/rig", mergesWindow)
	if err != nil {
		t.Fatalf("countMergesOnMain() error = %v", err)
	}
	if n != 4 {
		t.Errorf("count = %d, want 4", n)
	}

	// git's approxidate silently drops --since=6h and counts the repo's whole
	// history, so the window must be spelled out (gt-r65r).
	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "--since=6 hours ago") {
		t.Errorf("git args = %q, want a parseable --since window", joined)
	}
	for _, want := range []string{"-C /town/gastown/mayor/rig", "--count", "--merges", "origin/main"} {
		if !strings.Contains(joined, want) {
			t.Errorf("git args = %q, want %q", joined, want)
		}
	}
}

// ============================================
// POLECAT INVENTORY (gt-kqi2)
// ============================================

// polecatListFixture is the shape `gt polecat list --all --json` emits after
// gt-2540 added the agent and MR fields: a working polecat with a merge request
// the refinery can take, an idle polecat whose MR already merged, a polecat with
// no MR at all, one whose active_mr points at nothing, and a done polecat whose
// MR the town is still waiting on (gt-ppja).
const polecatListFixture = `[
  {"rig":"gastown","name":"agate","state":"working","issue":"gt-kqi2",
   "agent":"claude-opus-5","mr_id":"gt-wisp-ready","mr_status":"ready"},
  {"rig":"gastown","name":"malachite","state":"done","agent":"claude-sonnet-5",
   "mr_id":"gt-wisp-merged","mr_status":"merged"},
  {"rig":"gastown","name":"opal","state":"idle","agent":"deepseek-flash"},
  {"rig":"gastown","name":"shale","state":"done","active_mr":"gt-wisp-gone",
   "mr_id":"gt-wisp-gone","mr_status":"missing"},
  {"rig":"gastown","name":"jasper","state":"done","mr_id":"gt-wisp-8cs",
   "mr_status":"ready"},
  {"rig":"beads","name":"agate","state":"working","agent":"other-rig-agent",
   "mr_id":"gt-wisp-beads","mr_status":"blocked"}
]`

func TestPolecatIndexSnapshotIndexesRowsByRigAndName(t *testing.T) {
	f := &LiveConvoyFetcher{
		listPolecats: func() ([]byte, error) {
			return []byte(polecatListFixture), nil
		},
	}

	index, err := f.polecatIndexSnapshot()
	if err != nil {
		t.Fatalf("polecatIndexSnapshot() error = %v", err)
	}

	// The same polecat name exists in two rigs; the index must not let one
	// rig's agent and MR answer for the other's.
	got, ok := index["gastown"]["agate"]
	if !ok {
		t.Fatal("index[gastown][agate] missing")
	}
	if got.Agent != "claude-opus-5" || got.MRID != "gt-wisp-ready" || got.MRStatus != "ready" {
		t.Errorf("gastown/agate = %+v, want agent=claude-opus-5 mr_id=gt-wisp-ready mr_status=ready", got)
	}

	other, ok := index["beads"]["agate"]
	if !ok {
		t.Fatal("index[beads][agate] missing")
	}
	if other.Agent != "other-rig-agent" || other.MRStatus != "blocked" {
		t.Errorf("beads/agate = %+v, want the beads rig's own agent and MR", other)
	}

	// A polecat with no MR reports neither field rather than a placeholder.
	if opal := index["gastown"]["opal"]; opal.MRStatus != "" || opal.MRID != "" {
		t.Errorf("gastown/opal = %+v, want empty MR fields", opal)
	}
}

func TestPolecatIndexSnapshotUnreadableListIsAnError(t *testing.T) {
	f := &LiveConvoyFetcher{
		listPolecats: func() ([]byte, error) {
			return nil, fmt.Errorf("gt exploded")
		},
	}

	if _, err := f.polecatIndexSnapshot(); err == nil {
		t.Fatal("polecatIndexSnapshot() error = nil, want the lister's failure surfaced")
	}
}

func TestPolecatIndexSnapshotRejectsGarbageJSON(t *testing.T) {
	f := &LiveConvoyFetcher{
		listPolecats: func() ([]byte, error) {
			return []byte("still thinking about it\n"), nil
		},
	}

	if _, err := f.polecatIndexSnapshot(); err == nil {
		t.Fatal("polecatIndexSnapshot() error = nil, want a parse error")
	}
}

// TestPolecatIndexSnapshotEmptyOutputIsAnError covers a list that returns
// nothing: the refresh runs in a background goroutine, where a panic would take
// the whole dashboard down rather than just failing one fetch.
func TestPolecatIndexSnapshotEmptyOutputIsAnError(t *testing.T) {
	f := &LiveConvoyFetcher{
		listPolecats: func() ([]byte, error) {
			return nil, nil
		},
	}

	if _, err := f.polecatIndexSnapshot(); err == nil {
		t.Fatal("polecatIndexSnapshot() error = nil, want a parse error for empty output")
	}
}

// TestWorkerPolecatIndexServesTheCachedSnapshot is the process-budget guard:
// while the snapshot is fresh, rendering must not spawn `gt polecat list` at
// all, because one call costs a tmux read per polecat and a bulk
// merge-request join per rig.
func TestWorkerPolecatIndexServesTheCachedSnapshot(t *testing.T) {
	var listCalls int32
	f := &LiveConvoyFetcher{
		listPolecats: func() ([]byte, error) {
			atomic.AddInt32(&listCalls, 1)
			return []byte(polecatListFixture), nil
		},
	}
	// Seed a fresh snapshot, as a completed refresh would leave it.
	f.polecatFetchedAt = time.Now().UTC()
	index, err := f.polecatIndexSnapshot()
	if err != nil {
		t.Fatalf("seeding snapshot: %v", err)
	}
	f.polecatIndex = index
	atomic.StoreInt32(&listCalls, 0) // the seeding call is not a render

	for i := 0; i < 5; i++ {
		got := f.workerPolecatIndex()
		if _, ok := got["gastown"]["agate"]; !ok {
			t.Fatalf("render %d: snapshot is missing gastown/agate", i)
		}
	}

	if got := atomic.LoadInt32(&listCalls); got != 0 {
		t.Errorf("gt polecat list ran %d times on fresh renders, want 0", got)
	}
}

// TestWorkerPolecatIndexStaleSnapshotRefreshesOnce proves the stale path
// single-flights: however many renders arrive while a refresh is in flight,
// exactly one list runs.
func TestWorkerPolecatIndexStaleSnapshotRefreshesOnce(t *testing.T) {
	release := make(chan struct{})
	var listCalls int32
	f := &LiveConvoyFetcher{
		listPolecats: func() ([]byte, error) {
			atomic.AddInt32(&listCalls, 1)
			<-release
			return []byte(polecatListFixture), nil
		},
	}
	// Stale snapshot: there is data to serve while the refresh runs.
	f.polecatIndex = polecatIndex{"gastown": {"agate": polecatListItem{Rig: "gastown", Name: "agate", Agent: "stale-agent"}}}
	f.polecatFetchedAt = time.Now().Add(-polecatIndexTTL - time.Minute)

	for i := 0; i < 5; i++ {
		if got := f.workerPolecatIndex(); got["gastown"]["agate"].Agent != "stale-agent" {
			t.Fatalf("render %d: got %+v, want the previous snapshot while refreshing", i, got)
		}
	}

	close(release)
	waitForPolecatRefresh(t, f)

	if got := atomic.LoadInt32(&listCalls); got != 1 {
		t.Errorf("gt polecat list ran %d times across 5 stale renders, want 1", got)
	}
	if got := f.workerPolecatIndex()["gastown"]["agate"].Agent; got != "claude-opus-5" {
		t.Errorf("after refresh, agent = %q, want the refreshed snapshot", got)
	}
}

// TestWorkerPolecatIndexColdStartDoesNotBlock covers the branch that alarms: a
// fresh dashboard must render before the list has answered, or every first page
// load would wait out the list's tens of seconds.
func TestWorkerPolecatIndexColdStartDoesNotBlock(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	finished := make(chan struct{})
	f := &LiveConvoyFetcher{
		listPolecats: func() ([]byte, error) {
			close(started)
			<-release
			close(finished)
			return []byte(polecatListFixture), nil
		},
	}

	returned := make(chan polecatIndex, 1)
	go func() { returned <- f.workerPolecatIndex() }()

	select {
	case index := <-returned:
		if index != nil {
			t.Errorf("index = %v, want nil before the first refresh lands", index)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("workerPolecatIndex blocked on the list; a render must not wait for it")
	}

	<-started
	close(release)
	<-finished
}

// waitForPolecatRefresh blocks until the published snapshot is fresh again.
func waitForPolecatRefresh(t *testing.T, f *LiveConvoyFetcher) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.polecatMu.Lock()
		fetchedAt := f.polecatFetchedAt
		f.polecatMu.Unlock()
		if !fetchedAt.IsZero() && time.Since(fetchedAt) < polecatIndexTTL {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("polecat inventory refresh never published")
}

// TestRefreshPolecatIndexPublishesAndKeepsOnFailure drives the refresh's two
// outcomes directly.
func TestRefreshPolecatIndexPublishesAndKeepsOnFailure(t *testing.T) {
	failing := false
	f := &LiveConvoyFetcher{
		listPolecats: func() ([]byte, error) {
			if failing {
				return nil, fmt.Errorf("gt exploded")
			}
			return []byte(polecatListFixture), nil
		},
	}
	f.polecatIndex = polecatIndex{"gastown": {"agate": polecatListItem{Name: "agate", Agent: "previous"}}}
	f.polecatFetchedAt = time.Now().Add(-time.Hour)

	f.refreshPolecatIndex()
	if got := f.workerPolecatIndex()["gastown"]["agate"].Agent; got != "claude-opus-5" {
		t.Errorf("after a successful refresh, agent = %q, want claude-opus-5", got)
	}

	// A failed refresh must keep the previous snapshot rather than blank the
	// panel, and must close the breaker.
	failing = true
	f.polecatFetchedAt = time.Now().Add(-time.Hour)
	f.refreshPolecatIndex()

	if got := f.workerPolecatIndex()["gastown"]["agate"].Agent; got != "claude-opus-5" {
		t.Errorf("after a failed refresh, agent = %q, want the previous snapshot kept", got)
	}
	if f.polecatBreaker.allow() {
		t.Error("a failed refresh should close the breaker")
	}
}

// TestFetchWorkersJoinsAgentAndMRFromInventory is the integration the review of
// attempt 1 asked for: the Agent and MRStatus fields must actually reach the
// panel's rows, not sit unread.
func TestFetchWorkersJoinsAgentAndMRFromInventory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based command test")
	}

	townRoot := townRootWithRigs(t, "gastown")

	bdPath := filepath.Join(t.TempDir(), "bd")
	script := `#!/bin/sh
echo '[{"id":"gt-kqi2","title":"Polecats panel","status":"in_progress","assignee":"gastown/polecats/agate"}]'
`
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	restore := fetcherRunCmd
	defer func() { fetcherRunCmd = restore }()
	now := time.Now().Unix()
	fetcherRunCmd = func(_ time.Duration, name string, args ...string) (*bytes.Buffer, error) {
		if name == "tmux" {
			return bytes.NewBufferString(fmt.Sprintf("gt-agate|%d\ngt-refinery|%d\n", now, now)), nil
		}
		return nil, fmt.Errorf("unexpected command %q", name)
	}

	registry := session.NewPrefixRegistry()
	registry.Register("gt", "gastown")

	listCalls := 0
	f := &LiveConvoyFetcher{
		townRoot:       townRoot,
		cmdTimeout:     5 * time.Second,
		bdBin:          bdPath,
		registry:       registry,
		staleThreshold: 5 * time.Minute,
		stuckThreshold: 15 * time.Minute,
		listPolecats: func() ([]byte, error) {
			listCalls++
			return []byte(polecatListFixture), nil
		},
	}

	// Warm the inventory the way NewLiveConvoyFetcher does at startup; a fresh
	// fetcher serves an empty snapshot until the first refresh lands.
	f.workerPolecatIndex()
	waitForPolecatRefresh(t, f)

	workers, err := f.FetchWorkers()
	if err != nil {
		t.Fatalf("FetchWorkers() error = %v", err)
	}

	byName := make(map[string]WorkerRow, len(workers))
	for _, w := range workers {
		byName[w.Name] = w
	}

	agate, ok := byName["agate"]
	if !ok {
		t.Fatalf("no agate worker row; got %d rows: %+v", len(workers), workers)
	}
	if agate.Agent != "claude-opus-5" {
		t.Errorf("agate.Agent = %q, want claude-opus-5", agate.Agent)
	}
	if agate.MRID != "gt-wisp-ready" {
		t.Errorf("agate.MRID = %q, want gt-wisp-ready", agate.MRID)
	}
	if agate.MRStatus != "ready" {
		t.Errorf("agate.MRStatus = %q, want ready", agate.MRStatus)
	}

	// The refinery has no polecat inventory row, so its cells stay empty rather
	// than borrowing a polecat's agent or MR.
	refinery, ok := byName["refinery"]
	if !ok {
		t.Fatal("no refinery worker row")
	}
	if refinery.Agent != "" || refinery.MRID != "" || refinery.MRStatus != "" {
		t.Errorf("refinery = %+v, want empty agent and MR", refinery)
	}

	// One bulk list for the whole panel, not one per worker.
	if listCalls != 1 {
		t.Errorf("gt polecat list ran %d times, want 1", listCalls)
	}
}

// TestPolecatBreakerRetriesAfterBackoff guards the wedge: a breaker whose
// in-flight flag is never cleared rejects every later refresh, so the columns
// stay blank for the life of the process. A failure must be recoverable.
func TestPolecatBreakerRetriesAfterBackoff(t *testing.T) {
	failing := true
	var listCalls int32
	f := &LiveConvoyFetcher{
		listPolecats: func() ([]byte, error) {
			atomic.AddInt32(&listCalls, 1)
			if failing {
				return nil, fmt.Errorf("gt: command not found")
			}
			return []byte(polecatListFixture), nil
		},
	}

	f.refreshPolecatIndex()
	if f.polecatBreaker.allow() {
		t.Fatal("a failed refresh should hold the next attempt off")
	}

	// Age the breaker past its backoff, as time passing would.
	f.polecatBreaker.mu.Lock()
	f.polecatBreaker.lastAttempt = time.Now().Add(-time.Hour)
	f.polecatBreaker.mu.Unlock()

	failing = false
	if got := f.workerPolecatIndex(); got != nil {
		t.Errorf("index = %v, want nil until the retry lands", got)
	}
	waitForPolecatRefresh(t, f)

	if got := atomic.LoadInt32(&listCalls); got != 2 {
		t.Errorf("gt polecat list ran %d times, want 2 (the retry must happen)", got)
	}
}

// TestMergePolecatIndexesCarriesAnAbsentRigForward covers the partial read:
// `gt polecat list --all` warns and exits 0 when one rig's database cannot be
// read, so that rig arrives absent — and absent would blank its cells for a
// whole TTL.
func TestMergePolecatIndexesCarriesAnAbsentRigForward(t *testing.T) {
	previous := polecatIndex{
		"gastown": {"agate": {Rig: "gastown", Name: "agate", Agent: "claude-opus-5", MRStatus: "ready"}},
		"beads":   {"quartz": {Rig: "beads", Name: "quartz", Agent: "gpt"}},
	}
	fresh := polecatIndex{
		"gastown": {"agate": {Rig: "gastown", Name: "agate", Agent: "claude-sonnet-5"}},
	}

	merged := mergePolecatIndexes(previous, fresh)

	if got := merged["gastown"]["agate"].Agent; got != "claude-sonnet-5" {
		t.Errorf("gastown/agate agent = %q, want the fresh reading", got)
	}
	if got := merged["beads"]["quartz"].Agent; got != "gpt" {
		t.Errorf("beads/quartz agent = %q, want the carried-forward row", got)
	}
}

// TestListTownPolecatsRunsTheRightCommand pins the argv and working directory
// the whole feature rests on. No other test executes the production lister, so
// a dropped --all (gt then demands a rig name and prints no JSON) or the wrong
// directory would leave the columns blank forever with every test still green.
func TestListTownPolecatsRunsTheRightCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based command test")
	}

	townRoot := t.TempDir()
	tmp := t.TempDir()
	recordPath := filepath.Join(tmp, "record")
	gtPath := filepath.Join(tmp, "gt")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$PWD" > %s
printf '%%s\n' "$*" >> %s
cat <<'JSON'
%s
JSON
`, recordPath, recordPath, polecatListFixture)
	if err := os.WriteFile(gtPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gt: %v", err)
	}

	f := &LiveConvoyFetcher{townRoot: townRoot, gtBin: gtPath, cmdTimeout: 5 * time.Second}
	raw, err := f.listTownPolecats()
	if err != nil {
		t.Fatalf("listTownPolecats() error = %v", err)
	}
	if !strings.Contains(string(raw), "gt-wisp-ready") {
		t.Errorf("listTownPolecats() = %q, want the list JSON", raw)
	}

	recorded, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read recorded command: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(recorded)), "\n")
	if len(lines) != 2 {
		t.Fatalf("fake gt recorded %d lines, want 2: %q", len(lines), recorded)
	}

	// sh reports the resolved path, and t.TempDir() hands back the symlinked
	// one on macOS.
	gotDir, err := filepath.EvalSymlinks(lines[0])
	if err != nil {
		t.Fatalf("resolve recorded dir %q: %v", lines[0], err)
	}
	wantDir, err := filepath.EvalSymlinks(townRoot)
	if err != nil {
		t.Fatalf("resolve town root %q: %v", townRoot, err)
	}
	if gotDir != wantDir {
		t.Errorf("gt ran in %q, want the town root %q", gotDir, wantDir)
	}
	if want := "polecat list --all --json"; lines[1] != want {
		t.Errorf("gt ran with %q, want %q", lines[1], want)
	}
}

// TestFetchWorkersCrewDoesNotBorrowAPolecatAgent covers the name collision:
// crew sessions share the polecat name space in a rig, and the inventory is
// keyed by polecat name, so a crew member must not read a polecat's row.
func TestFetchWorkersCrewDoesNotBorrowAPolecatAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based command test")
	}

	townRoot := townRootWithRigs(t, "gastown")

	bdPath := filepath.Join(t.TempDir(), "bd")
	script := `#!/bin/sh
echo '[]'
`
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	restore := fetcherRunCmd
	defer func() { fetcherRunCmd = restore }()
	now := time.Now().Unix()
	fetcherRunCmd = func(_ time.Duration, name string, args ...string) (*bytes.Buffer, error) {
		if name == "tmux" {
			// The crew member is named for a polecat in the same rig.
			return bytes.NewBufferString(fmt.Sprintf("gt-crew-agate|%d\n", now)), nil
		}
		return nil, fmt.Errorf("unexpected command %q", name)
	}

	registry := session.NewPrefixRegistry()
	registry.Register("gt", "gastown")

	f := &LiveConvoyFetcher{
		townRoot:       townRoot,
		cmdTimeout:     5 * time.Second,
		bdBin:          bdPath,
		registry:       registry,
		staleThreshold: 5 * time.Minute,
		stuckThreshold: 15 * time.Minute,
		listPolecats: func() ([]byte, error) {
			return []byte(polecatListFixture), nil
		},
	}

	// Warm the inventory, so the empty cells below are the crew gate's doing
	// and not a cold cache.
	f.workerPolecatIndex()
	waitForPolecatRefresh(t, f)

	workers, err := f.FetchWorkers()
	if err != nil {
		t.Fatalf("FetchWorkers() error = %v", err)
	}
	var crew *WorkerRow
	for i := range workers {
		if workers[i].Name == "agate" {
			crew = &workers[i]
		}
	}
	if crew == nil {
		t.Fatalf("no crew row for agate; got %+v", workers)
	}
	if crew.Agent != "" || crew.MRID != "" || crew.MRStatus != "" {
		t.Errorf("crew row = %+v, want no agent or MR borrowed from the polecat named agate", *crew)
	}
}

// A slung polecat's bead lives in the rig DB with status "hooked", so the hq
// "bd list --status=in_progress" map never matches it; the inventory row from
// gt polecat list already names the issue, and a working polecat must not
// render as idle with an empty WORKING ON cell (gt-bcfc).
func TestFetchWorkersTakesIssueFromInventoryWhenHqMapMisses(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based command test")
	}

	townRoot := townRootWithRigs(t, "gastown")

	bdPath := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(bdPath, []byte("#!/bin/sh\necho '[]'\n"), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	restore := fetcherRunCmd
	defer func() { fetcherRunCmd = restore }()
	now := time.Now().Unix()
	fetcherRunCmd = func(_ time.Duration, name string, args ...string) (*bytes.Buffer, error) {
		if name == "tmux" {
			return bytes.NewBufferString(fmt.Sprintf("gt-agate|%d\ngt-opal|%d\n", now, now)), nil
		}
		return nil, fmt.Errorf("unexpected command %q", name)
	}

	registry := session.NewPrefixRegistry()
	registry.Register("gt", "gastown")

	f := &LiveConvoyFetcher{
		townRoot:       townRoot,
		cmdTimeout:     5 * time.Second,
		bdBin:          bdPath,
		registry:       registry,
		staleThreshold: 5 * time.Minute,
		stuckThreshold: 15 * time.Minute,
		listPolecats:   func() ([]byte, error) { return []byte(polecatListFixture), nil },
	}
	f.workerPolecatIndex()
	waitForPolecatRefresh(t, f)

	workers, err := f.FetchWorkers()
	if err != nil {
		t.Fatalf("FetchWorkers() error = %v", err)
	}
	byName := make(map[string]WorkerRow, len(workers))
	for _, w := range workers {
		byName[w.Name] = w
	}

	agate, ok := byName["agate"]
	if !ok {
		t.Fatalf("no agate worker row; got %+v", workers)
	}
	if agate.IssueID != "gt-kqi2" {
		t.Errorf("agate.IssueID = %q, want gt-kqi2 from the inventory", agate.IssueID)
	}
	if agate.WorkStatus != "working" {
		t.Errorf("agate.WorkStatus = %q, want working", agate.WorkStatus)
	}

	// An idle polecat has no issue in either source and stays idle.
	opal, ok := byName["opal"]
	if !ok {
		t.Fatalf("no opal worker row; got %+v", workers)
	}
	if opal.IssueID != "" || opal.WorkStatus != "idle" {
		t.Errorf("opal = issue %q status %q, want empty/idle", opal.IssueID, opal.WorkStatus)
	}
}

// TestPendingMRWorkersMintsRowsForDonePolecatsWithAnInFlightMR covers the gate
// behind gt-ppja. Exactly one class earns a row: a finished polecat whose MR
// the town is still waiting on and whose session is gone. Everything else is
// either covered by the tmux pass, still moving, or already resolved.
func TestPendingMRWorkersMintsRowsForDonePolecatsWithAnInFlightMR(t *testing.T) {
	index := polecatIndex{
		"gastown": {
			"jasper":    {Rig: "gastown", Name: "jasper", State: "done", MRID: "gt-wisp-8cs", MRStatus: "ready"},
			"malachite": {Rig: "gastown", Name: "malachite", State: "done", MRID: "gt-wisp-merged", MRStatus: "merged"},
			"shale":     {Rig: "gastown", Name: "shale", State: "done", MRID: "gt-wisp-gone", MRStatus: "missing"},
			"agate":     {Rig: "gastown", Name: "agate", State: "working", MRID: "gt-wisp-open", MRStatus: "open"},
			"live":      {Rig: "gastown", Name: "live", State: "done", MRID: "gt-wisp-live", MRStatus: "blocked"},
			"muted":     {Rig: "gastown", Name: "muted", State: "done"},
		},
		"beads": {
			"quartz": {Rig: "beads", Name: "quartz", State: "done", MRID: "be-wisp-1", MRStatus: "open"},
		},
	}
	// "live" already has a tmux session row, so the tmux pass owns it.
	rendered := map[string]bool{"gastown/live": true}

	rows := pendingMRWorkers(index, rendered)

	got := make([]string, 0, len(rows))
	for _, row := range rows {
		got = append(got, row.Rig+"/"+row.Name)
	}
	if want := "beads/quartz,gastown/jasper"; strings.Join(got, ",") != want {
		t.Fatalf("pendingMRWorkers() rows = %v, want %s (rig then name)", got, want)
	}

	// The row has to carry what the panel renders: the MR and its state, the
	// status that keeps the row out of the idle/working vocabulary, and no
	// session it does not have.
	jasper := rows[1]
	if jasper.MRID != "gt-wisp-8cs" || jasper.MRStatus != "ready" {
		t.Errorf("jasper MR = %q/%q, want gt-wisp-8cs/ready", jasper.MRID, jasper.MRStatus)
	}
	if jasper.WorkStatus != "mr-pending" {
		t.Errorf("jasper.WorkStatus = %q, want mr-pending", jasper.WorkStatus)
	}
	if jasper.SessionID != "" {
		t.Errorf("jasper.SessionID = %q, want empty — the polecat has no live session", jasper.SessionID)
	}
	if jasper.LastActivity.FormattedAge != "unknown" {
		t.Errorf("jasper age = %q, want unknown — there is no session to read activity from", jasper.LastActivity.FormattedAge)
	}
}

// TestFetchWorkersShowsDonePolecatWithAPendingMR is the wiring half of
// gt-ppja: the panel the town reads must list the finished polecat whose MR is
// still outstanding, and must not resurrect the ones already settled.
func TestFetchWorkersShowsDonePolecatWithAPendingMR(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based command test")
	}

	townRoot := townRootWithRigs(t, "gastown")

	bdPath := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(bdPath, []byte("#!/bin/sh\necho '[]'\n"), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	restore := fetcherRunCmd
	defer func() { fetcherRunCmd = restore }()
	now := time.Now().Unix()
	fetcherRunCmd = func(_ time.Duration, name string, args ...string) (*bytes.Buffer, error) {
		if name == "tmux" {
			return bytes.NewBufferString(fmt.Sprintf("gt-agate|%d\n", now)), nil
		}
		return nil, fmt.Errorf("unexpected command %q", name)
	}

	registry := session.NewPrefixRegistry()
	registry.Register("gt", "gastown")

	f := &LiveConvoyFetcher{
		townRoot:       townRoot,
		cmdTimeout:     5 * time.Second,
		bdBin:          bdPath,
		registry:       registry,
		staleThreshold: 5 * time.Minute,
		stuckThreshold: 15 * time.Minute,
		listPolecats:   func() ([]byte, error) { return []byte(polecatListFixture), nil },
	}
	f.workerPolecatIndex()
	waitForPolecatRefresh(t, f)

	workers, err := f.FetchWorkers()
	if err != nil {
		t.Fatalf("FetchWorkers() error = %v", err)
	}
	byName := make(map[string]WorkerRow, len(workers))
	for _, w := range workers {
		byName[w.Name] = w
	}

	jasper, ok := byName["jasper"]
	if !ok {
		t.Fatalf("no jasper row — a done polecat with an in-flight MR is invisible; got %+v", workers)
	}
	if jasper.WorkStatus != "mr-pending" || jasper.MRID != "gt-wisp-8cs" {
		t.Errorf("jasper = status %q mr %q, want mr-pending/gt-wisp-8cs", jasper.WorkStatus, jasper.MRID)
	}
	if jasper.SessionID != "" {
		t.Errorf("jasper.SessionID = %q, want empty", jasper.SessionID)
	}

	// Settled MRs stay out of the panel: a merged one is not work outstanding,
	// and one with no bead behind it is not a merge request at all.
	for _, name := range []string{"malachite", "shale"} {
		if row, ok := byName[name]; ok {
			t.Errorf("%s should not render while its MR is settled: %+v", name, row)
		}
	}
}

// tmux's session_activity only advances for a session with an attached
// client, so a detached agent reported its creation time all evening; the
// panel must read the pane's window_activity instead (gt-vcfs).
func TestFetchSessionsReadsWindowActivityNotSessionActivity(t *testing.T) {
	restore := fetcherRunCmd
	defer func() { fetcherRunCmd = restore }()

	now := time.Now().Unix()
	fetcherRunCmd = func(_ time.Duration, name string, args ...string) (*bytes.Buffer, error) {
		if name != "tmux" {
			return nil, fmt.Errorf("unexpected command %q", name)
		}
		format := args[len(args)-1]
		if !strings.Contains(format, "#{window_activity}") {
			return nil, fmt.Errorf("list-sessions format %q does not ask for window_activity", format)
		}
		if strings.Contains(format, "#{session_activity}") {
			return nil, fmt.Errorf("list-sessions format %q still asks for session_activity", format)
		}
		return bytes.NewBufferString(fmt.Sprintf("hq-deacon:%d\n", now)), nil
	}

	f := &LiveConvoyFetcher{cmdTimeout: 5 * time.Second, registry: session.NewPrefixRegistry()}
	rows, err := f.FetchSessions()
	if err != nil {
		t.Fatalf("FetchSessions() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	if want := formatTimestamp(time.Unix(now, 0)); rows[0].Activity != want {
		t.Errorf("Activity = %q, want %q (window_activity)", rows[0].Activity, want)
	}
}

// windowActivityOnlyTmux is a fake tmux that answers list-sessions with the
// given rows only when the format reads the window clock; asking for the
// frozen session clock is an error, so any reader that reverts fails.
func windowActivityOnlyTmux(t *testing.T, rows string) func(time.Duration, string, ...string) (*bytes.Buffer, error) {
	t.Helper()
	return func(_ time.Duration, name string, args ...string) (*bytes.Buffer, error) {
		if name != "tmux" {
			return nil, fmt.Errorf("unexpected command %q", name)
		}
		for _, a := range args {
			if strings.Contains(a, "#{session_activity}") {
				return nil, fmt.Errorf("list-sessions format %q asks for session_activity", a)
			}
		}
		return bytes.NewBufferString(rows), nil
	}
}

// The Mayor tile and the convoy panel's tracked-issue activity read the same
// clock as the Sessions panel; the mayor session is usually attached, which
// hid the freeze there (gt-vcfs).
func TestFetchMayorReadsWindowActivity(t *testing.T) {
	now := time.Now().Unix()
	withMayorFetcherHooks(
		t,
		func(string, string) (string, error) { return "codex", nil },
		windowActivityOnlyTmux(t, fmt.Sprintf("hq-mayor:%d\n", now)),
	)
	f := &LiveConvoyFetcher{
		townRoot:             t.TempDir(),
		mayorActiveThreshold: 24 * time.Hour,
		tmuxCmdTimeout:       time.Second,
	}
	status, err := f.FetchMayor()
	if err != nil {
		t.Fatalf("FetchMayor: %v", err)
	}
	if !status.IsAttached || status.LastActivity == "" {
		t.Fatalf("mayor = %+v, want attached with activity from window_activity", status)
	}
}

func TestSessionActivityForAssigneeReadsWindowActivity(t *testing.T) {
	restore := fetcherRunCmd
	defer func() { fetcherRunCmd = restore }()
	now := time.Now().Unix()
	fetcherRunCmd = windowActivityOnlyTmux(t, fmt.Sprintf("gt-agate|%d\n", now))

	f := &LiveConvoyFetcher{cmdTimeout: 5 * time.Second}
	got := f.getSessionActivityForAssignee("gastown/polecats/agate")
	if got == nil || got.Unix() != now {
		t.Fatalf("activity = %v, want %d from window_activity", got, now)
	}
}

// ============================================
// PARKED RIGS ON THE PANELS (gt-94xz)
// ============================================

// stubOpState is the probe seam: the named rigs are parked, everything else
// accepts work. Panel tests use it so a parked row renders without a town.
func stubOpState(parked ...string) rigOpStateFunc {
	isParked := make(map[string]bool, len(parked))
	for _, name := range parked {
		isParked[name] = true
	}
	return func(rigName string) (rig.OpState, string) {
		if isParked[rigName] {
			return rig.OpStateParked, rig.OpStateSourceLocal
		}
		return rig.OpStateOperational, rig.OpStateSourceDefault
	}
}

// nobodyTmux points the fetcher at a tmux socket no server owns, so the
// live-polecat count reads zero without touching the machine's tmux.
func nobodyTmux(f *LiveConvoyFetcher) *LiveConvoyFetcher {
	f.tmuxSocket = "gt-94xz-test-no-server"
	f.tmuxCmdTimeout = 2 * time.Second
	return f
}

// TestFetchRigsMarksParkedRigs is the Rigs panel's half of the acceptance
// criterion: a parked rig must not render as an ordinary row, because its
// witness and refinery icons survive the park.
func TestFetchRigsMarksParkedRigs(t *testing.T) {
	f := nobodyTmux(&LiveConvoyFetcher{
		townRoot:   townRootWithRigs(t, "gastown", "hm"),
		rigOpState: stubOpState("hm"),
	})

	rows, err := f.FetchRigs()
	if err != nil {
		t.Fatalf("FetchRigs: %v", err)
	}

	byName := make(map[string]RigRow, len(rows))
	for _, row := range rows {
		byName[row.Name] = row
	}
	if got := byName["hm"].OpState; got != "parked" {
		t.Errorf("hm OpState = %q, want %q", got, "parked")
	}
	if got := byName["gastown"].OpState; got != "" {
		t.Errorf("gastown OpState = %q, want empty for a rig that accepts work", got)
	}
}

// TestFetchRigsReadsParkedStateFromTheTownWisp exercises the wiring with no
// seam: the state a real `gt rig park` writes must reach the row. The wisp
// layer is a local file, so this stays hermetic — the rig that is not parked
// falls through to its identity bead, which in a temp town has no database
// and answers "operational" rather than inventing a marker.
func TestFetchRigsReadsParkedStateFromTheTownWisp(t *testing.T) {
	townRoot := townRootWithRigs(t, "gastown", "hm")
	if err := wisp.NewConfig(townRoot, "hm").Set(rig.RigStatusKey, rig.RigStatusParked); err != nil {
		t.Fatalf("write wisp config: %v", err)
	}

	f := nobodyTmux(&LiveConvoyFetcher{townRoot: townRoot})

	rows, err := f.FetchRigs()
	if err != nil {
		t.Fatalf("FetchRigs: %v", err)
	}

	for _, row := range rows {
		want := ""
		if row.Name == "hm" {
			want = "parked"
		}
		if row.OpState != want {
			t.Errorf("%s OpState = %q, want %q", row.Name, row.OpState, want)
		}
	}
}

// TestMarkParkedRigsStampsRowsAndCounts covers the Merge Queue panel: every
// row in a parked rig is tagged, and the header's parked count is the number
// of those rows — not the number of parked rigs, since the point is how much
// queued work nobody will process.
func TestMarkParkedRigsStampsRowsAndCounts(t *testing.T) {
	now := time.Now()
	f := &LiveConvoyFetcher{rigOpState: stubOpState("hm")}
	snapshot := TownMergeQueue{
		Loaded: true,
		Rows: []TownMergeQueueRow{
			{ID: "hm-wisp-t7f", Rig: "hm", Status: "ready", ColorClass: "mq-green", createdAt: now},
			{ID: "hm-wisp-48l", Rig: "hm", Status: "ready", ColorClass: "mq-green", createdAt: now},
			{ID: "gt-wisp-a", Rig: "gastown", Status: "ready", ColorClass: "mq-green", createdAt: now},
		},
	}

	got := f.markParkedRigs(snapshot)

	for _, row := range got.Rows {
		want := ""
		if row.Rig == "hm" {
			want = "parked"
		}
		if row.RigOpState != want {
			t.Errorf("%s RigOpState = %q, want %q", row.ID, row.RigOpState, want)
		}
	}
	if got.ParkedCount != 2 {
		t.Errorf("ParkedCount = %d, want 2", got.ParkedCount)
	}
	if got.ReadyCount != snapshot.ReadyCount {
		t.Errorf("ReadyCount = %d, want it untouched at %d", got.ReadyCount, snapshot.ReadyCount)
	}
}

// TestMarkParkedRigsProbesEachRigOnce keeps the probe off the per-row budget:
// a five-MR rig costs one lookup, not five.
func TestMarkParkedRigsProbesEachRigOnce(t *testing.T) {
	now := time.Now()
	// The probe is fanned out one goroutine per rig (rigOpLabels), so the
	// count has to survive concurrent recording: a bare append loses a probe
	// and reports a fan-out bug the production code does not have (gt-phuy).
	var mu sync.Mutex
	probed := make(map[string]int)
	f := &LiveConvoyFetcher{rigOpState: func(rigName string) (rig.OpState, string) {
		mu.Lock()
		probed[rigName]++
		mu.Unlock()
		return rig.OpStateParked, rig.OpStateSourceLocal
	}}
	snapshot := TownMergeQueue{
		Loaded: true,
		Rows: []TownMergeQueueRow{
			{ID: "hm-wisp-a", Rig: "hm", createdAt: now},
			{ID: "hm-wisp-b", Rig: "hm", createdAt: now},
			{ID: "hm-wisp-c", Rig: "hm", createdAt: now},
			{ID: "gt-wisp-a", Rig: "gastown", createdAt: now},
		},
	}

	f.markParkedRigs(snapshot)

	mu.Lock()
	defer mu.Unlock()
	if len(probed) != 2 || probed["hm"] != 1 || probed["gastown"] != 1 {
		t.Errorf("probed %v, want each rig exactly once", probed)
	}
}

// TestMarkParkedRigsDoesNotMutateTheCachedSnapshot guards the aliasing bug
// that would make the marker sticky: the rows handed back alias the cached
// snapshot's backing array, so stamping in place would park a rig in the
// cache and leave it marked after `gt rig unpark`.
func TestMarkParkedRigsDoesNotMutateTheCachedSnapshot(t *testing.T) {
	now := time.Now()
	f := &LiveConvoyFetcher{rigOpState: stubOpState("hm")}
	snapshot := TownMergeQueue{
		Loaded: true,
		Rows:   []TownMergeQueueRow{{ID: "hm-wisp-a", Rig: "hm", createdAt: now}},
	}

	if got := f.markParkedRigs(snapshot); got.Rows[0].RigOpState != "parked" {
		t.Fatalf("stamped row = %+v, want the marker applied", got.Rows[0])
	}
	if snapshot.Rows[0].RigOpState != "" {
		t.Error("the snapshot passed in was mutated; the cached copy now carries a stale marker")
	}

	// The same snapshot, with the rig unparked, must come back unmarked.
	f.rigOpState = stubOpState()
	if got := f.markParkedRigs(snapshot); got.Rows[0].RigOpState != "" || got.ParkedCount != 0 {
		t.Errorf("after unpark = %+v, want no marker", got.Rows[0])
	}
}

// TestFetchTownMergeQueueStampsParkedRowsFromAFreshSnapshot is the acceptance
// criterion for the Merge Queue panel: the marker must be on a render that
// serves an already-cached snapshot, not only on one that happens to trigger
// the three-minute re-derivation.
func TestFetchTownMergeQueueStampsParkedRowsFromAFreshSnapshot(t *testing.T) {
	now := time.Now()
	f := &LiveConvoyFetcher{rigOpState: stubOpState("hm")}

	f.mqMu.Lock()
	f.mqSnapshot = TownMergeQueue{
		Loaded: true,
		Rows:   []TownMergeQueueRow{{ID: "hm-wisp-t7f", Rig: "hm", Status: "ready", ColorClass: "mq-green", createdAt: now}},
	}
	f.mqFetchedAt = time.Now() // fresh: no refresh will be started
	f.mqMu.Unlock()

	got := f.FetchTownMergeQueue()
	if len(got.Rows) != 1 {
		t.Fatalf("rows = %d, want the cached snapshot served as-is", len(got.Rows))
	}
	if got.Rows[0].RigOpState != "parked" {
		t.Errorf("RigOpState = %q, want %q on a fresh snapshot", got.Rows[0].RigOpState, "parked")
	}
	if got.ParkedCount != 1 {
		t.Errorf("ParkedCount = %d, want 1", got.ParkedCount)
	}
}

// TestRigOpLabelsGivesUpAtTheDeadline keeps a wedged Dolt from holding a
// render: probes that never answer are reported unmarked, which is what the
// panels did before they knew about parked rigs.
func TestRigOpLabelsGivesUpAtTheDeadline(t *testing.T) {
	restore := rigOpProbeTimeout
	rigOpProbeTimeout = 20 * time.Millisecond
	defer func() { rigOpProbeTimeout = restore }()

	release := make(chan struct{})
	defer close(release)
	f := &LiveConvoyFetcher{rigOpState: func(string) (rig.OpState, string) {
		<-release
		return rig.OpStateParked, rig.OpStateSourceLocal
	}}

	done := make(chan map[string]string, 1)
	go func() { done <- f.rigOpLabels([]string{"hm", "gastown"}) }()

	select {
	case got := <-done:
		if len(got) != 0 {
			t.Errorf("labels = %v, want none when no probe answered", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rigOpLabels never returned; the probe is unbounded")
	}
}

// TestRigOpLabelsKeepsAnswersThatBeatTheDeadline pins the property the
// deadline must not break: the wisp read `gt rig park` depends on is a local
// file, so it answers long before any bd-backed probe could, and a slow rig
// must not cost a parked one its marker.
func TestRigOpLabelsKeepsAnswersThatBeatTheDeadline(t *testing.T) {
	restore := rigOpProbeTimeout
	rigOpProbeTimeout = 250 * time.Millisecond
	defer func() { rigOpProbeTimeout = restore }()

	release := make(chan struct{})
	defer close(release)
	f := &LiveConvoyFetcher{rigOpState: func(rigName string) (rig.OpState, string) {
		if rigName == "slow" {
			<-release
			return rig.OpStateOperational, rig.OpStateSourceDefault
		}
		return rig.OpStateParked, rig.OpStateSourceLocal
	}}

	got := f.rigOpLabels([]string{"hm", "slow"})
	if got["hm"] != "parked" {
		t.Errorf("labels = %v, want hm parked", got)
	}
}

// TestIsWorkPanelNonDispatchable is the gt-b9wq rework's regression test for
// the om-editorial finding: folding the Work panel's filter into the full
// beads.IsNonDispatchableBead predicate silently started hiding gt:keep and
// gt:standing-orders beads too. Those mark a bead protected from auto-close
// (see beads.ProtectedIssueLabel), not internal bookkeeping the way mail or
// a merge slot is — a kept or standing-orders task is still real work a
// polecat can sling, so the Work panel must keep offering it.
func TestIsWorkPanelNonDispatchable(t *testing.T) {
	tests := []struct {
		name   string
		typ    string
		labels []string
		title  string
		want   bool
	}{
		{name: "ordinary task stays", typ: "task", want: false},
		{name: "mail is hidden", typ: "task", labels: []string{"gt:message"}, want: true},
		{name: "merge-request type is hidden", typ: "merge-request", want: true},
		{name: "gt:keep stays", typ: "task", labels: []string{"gt:keep"}, want: false},
		{name: "gt:standing-orders stays", typ: "task", labels: []string{"gt:standing-orders"}, want: false},
		{name: "gt:role is still hidden", typ: "task", labels: []string{"gt:role"}, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isWorkPanelNonDispatchable(tc.typ, tc.labels, tc.title)
			if got != tc.want {
				t.Errorf("isWorkPanelNonDispatchable(%q, %v, %q) = %v, want %v",
					tc.typ, tc.labels, tc.title, got, tc.want)
			}
		})
	}
}
