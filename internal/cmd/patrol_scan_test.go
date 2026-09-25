package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/witness"
)

type progressDiagnostics struct {
	bytes.Buffer
	sawProgress chan struct{}
	once        sync.Once
}

func (d *progressDiagnostics) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "still running") {
		d.once.Do(func() { close(d.sawProgress) })
	}
	return d.Buffer.Write(p)
}

func TestPatrolScanOutputJSON(t *testing.T) {
	t.Parallel()
	output := PatrolScanOutput{
		Rig:       "gastown",
		Timestamp: "2026-03-17T12:00:00Z",
		Zombies: &PatrolScanZombieOutput{
			Checked: 3,
			Found:   1,
			Zombies: []PatrolScanZombieItem{
				{
					Polecat:        "alpha",
					Classification: "session-dead-active",
					AgentState:     "working",
					HookBead:       "gas-abc",
					Action:         "restarted",
					WasActive:      true,
				},
			},
		},
		Receipts: []witness.PatrolReceipt{
			{
				Rig:               "gastown",
				Polecat:           "alpha",
				Verdict:           witness.PatrolVerdictStale,
				RecommendedAction: "restarted",
				Evidence: witness.PatrolReceiptEvidence{
					AgentState:     "working",
					Classification: witness.ZombieSessionDeadActive,
					HookBead:       "gas-abc",
				},
			},
		},
	}

	data, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("failed to marshal output: %v", err)
	}

	var parsed PatrolScanOutput
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal output: %v", err)
	}

	if parsed.Rig != "gastown" {
		t.Errorf("Rig = %q, want %q", parsed.Rig, "gastown")
	}
	if parsed.Zombies.Found != 1 {
		t.Errorf("Zombies.Found = %d, want 1", parsed.Zombies.Found)
	}
	if parsed.Zombies.Checked != 3 {
		t.Errorf("Zombies.Checked = %d, want 3", parsed.Zombies.Checked)
	}
	if len(parsed.Zombies.Zombies) != 1 {
		t.Fatalf("len(Zombies) = %d, want 1", len(parsed.Zombies.Zombies))
	}
	z := parsed.Zombies.Zombies[0]
	if z.Polecat != "alpha" {
		t.Errorf("zombie Polecat = %q, want %q", z.Polecat, "alpha")
	}
	if z.Classification != "session-dead-active" {
		t.Errorf("zombie Classification = %q, want %q", z.Classification, "session-dead-active")
	}
	if !z.WasActive {
		t.Error("zombie WasActive = false, want true")
	}
	if len(parsed.Receipts) != 1 {
		t.Fatalf("len(Receipts) = %d, want 1", len(parsed.Receipts))
	}
	if parsed.Receipts[0].Verdict != witness.PatrolVerdictStale {
		t.Errorf("receipt Verdict = %q, want %q", parsed.Receipts[0].Verdict, witness.PatrolVerdictStale)
	}
}

func TestCountActiveWorkZombies(t *testing.T) {
	t.Parallel()
	result := &witness.DetectZombiePolecatsResult{
		Zombies: []witness.ZombieResult{
			{PolecatName: "alpha", WasActive: true},
			{PolecatName: "beta", WasActive: false},
			{PolecatName: "gamma", WasActive: true},
		},
	}

	got := countActiveWorkZombies(result)
	if got != 2 {
		t.Errorf("countActiveWorkZombies() = %d, want 2", got)
	}
}

func TestCountActiveWorkZombies_Empty(t *testing.T) {
	t.Parallel()
	result := &witness.DetectZombiePolecatsResult{}
	got := countActiveWorkZombies(result)
	if got != 0 {
		t.Errorf("countActiveWorkZombies() = %d, want 0", got)
	}
}

func TestRunPatrolScanPhaseEmitsProgressDiagnostics(t *testing.T) {
	oldInterval := patrolScanProgressInterval
	patrolScanProgressInterval = 10 * time.Millisecond
	defer func() { patrolScanProgressInterval = oldInterval }()

	diagnostics := &progressDiagnostics{sawProgress: make(chan struct{})}
	release := make(chan struct{})
	go func() {
		select {
		case <-diagnostics.sawProgress:
		case <-time.After(time.Second):
		}
		close(release)
	}()

	got := runPatrolScanPhase(diagnostics, "slow phase", func() string {
		<-release
		return "ok"
	})

	if got != "ok" {
		t.Fatalf("runPatrolScanPhase result = %q, want ok", got)
	}

	output := diagnostics.String()
	select {
	case <-diagnostics.sawProgress:
	default:
		t.Fatalf("diagnostics %q never emitted progress", output)
	}
	for _, want := range []string{
		"gt patrol scan: starting slow phase",
		"gt patrol scan: still running slow phase after",
		"gt patrol scan: finished slow phase in",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("diagnostics %q missing %q", output, want)
		}
	}
}

func TestRunPatrolScanPhaseZeroIntervalSkipsProgressTicks(t *testing.T) {
	oldInterval := patrolScanProgressInterval
	patrolScanProgressInterval = 0
	defer func() { patrolScanProgressInterval = oldInterval }()

	var diagnostics bytes.Buffer
	got := runPatrolScanPhase(&diagnostics, "fast phase", func() int {
		return 42
	})

	if got != 42 {
		t.Fatalf("runPatrolScanPhase result = %d, want 42", got)
	}

	output := diagnostics.String()
	if strings.Contains(output, "still running") {
		t.Fatalf("diagnostics should not include progress tick when interval is disabled: %q", output)
	}
	for _, want := range []string{
		"gt patrol scan: starting fast phase",
		"gt patrol scan: finished fast phase in",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("diagnostics %q missing %q", output, want)
		}
	}
}

func TestPatrolScanZombieItemSerialization(t *testing.T) {
	t.Parallel()
	item := PatrolScanZombieItem{
		Polecat:        "obsidian",
		Classification: "agent-dead-in-session",
		AgentState:     "working",
		HookBead:       "gas-xyz",
		CleanupStatus:  "has_uncommitted",
		Action:         "restarted-dirty (cleanup_status=has_uncommitted, wisp=gas-wisp-123)",
		WasActive:      true,
		Error:          "restart failed: tmux error",
	}

	data, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("failed to marshal item: %v", err)
	}

	var parsed PatrolScanZombieItem
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("failed to unmarshal item: %v", err)
	}

	if parsed.Polecat != "obsidian" {
		t.Errorf("Polecat = %q, want %q", parsed.Polecat, "obsidian")
	}
	if parsed.CleanupStatus != "has_uncommitted" {
		t.Errorf("CleanupStatus = %q, want %q", parsed.CleanupStatus, "has_uncommitted")
	}
	if parsed.Error != "restart failed: tmux error" {
		t.Errorf("Error = %q, want %q", parsed.Error, "restart failed: tmux error")
	}
}

// TestBuildActivityOutput covers the projection from real-activity snapshots to
// scan output, including the rule that an undated session is never a candidate
// and that a candidate carries the re-scan instruction (gt-xb27).
func TestBuildActivityOutput(t *testing.T) {
	oldThreshold := patrolScanActivityThreshold
	patrolScanActivityThreshold = 30 * time.Minute
	t.Cleanup(func() { patrolScanActivityThreshold = oldThreshold })

	now := time.Now()
	observed := []witness.RealActivity{
		{
			Polecat:         "opal",
			Session:         "gt-gastown-opal",
			AgentAlive:      true,
			LastActivity:    now.Add(-2 * time.Minute),
			ActivitySource:  witness.ActivitySourceTranscript,
			TranscriptPath:  "/tmp/opal.jsonl",
			TranscriptBytes: 1234,
			PaneSignature:   "aaaa",
		},
		{
			Polecat:         "lapis",
			Session:         "gt-gastown-lapis",
			AgentAlive:      true,
			LastActivity:    now.Add(-45 * time.Minute),
			ActivitySource:  witness.ActivitySourceTranscript,
			TranscriptPath:  "/tmp/lapis.jsonl",
			TranscriptBytes: 5678,
			PaneSignature:   "bbbb",
			ObservedAt:      now,
		},
		{
			Polecat:        "unknown",
			Session:        "gt-gastown-unknown",
			AgentAlive:     true,
			ActivitySource: witness.ActivitySourceNone,
			ObservedAt:     now,
		},
	}

	out := buildActivityOutput(observed, nil, now)
	if out.Checked != 3 {
		t.Errorf("Checked = %d, want 3", out.Checked)
	}
	if out.StaleCandidates != 1 {
		t.Errorf("StaleCandidates = %d, want 1", out.StaleCandidates)
	}
	if out.Threshold != "30m0s" {
		t.Errorf("Threshold = %q, want 30m0s", out.Threshold)
	}
	if len(out.Items) != 3 {
		t.Fatalf("Items = %d, want 3", len(out.Items))
	}

	working := out.Items[0]
	if working.LastActivityAgeSeconds == nil || *working.LastActivityAgeSeconds < 100 {
		t.Errorf("working item age = %v, want ~120s", working.LastActivityAgeSeconds)
	}
	if working.StallCandidate || working.Note != "" {
		t.Errorf("working polecat flagged as candidate: %+v", working)
	}

	candidate := out.Items[1]
	if !candidate.StallCandidate {
		t.Error("45m of silence at a 30m threshold should be a candidate")
	}
	if !strings.Contains(candidate.Note, "pane_signature") {
		t.Errorf("candidate note should demand a pane_signature comparison, got %q", candidate.Note)
	}

	// A polecat with no transcript cannot be judged, so it is never nominated.
	undated := out.Items[2]
	if undated.StallCandidate {
		t.Error("undated session was nominated as a stall candidate")
	}
	if undated.LastActivityAgeSeconds != nil {
		t.Errorf("undated session reported an age: %v", *undated.LastActivityAgeSeconds)
	}
}

// TestBuildActivityOutput_CarriesPersistedStallCheck: the JSON exposes the
// persisted sample 1 and the verdict against it, so a respawned witness reads
// the stall rule's state instead of remembering it (claude-8w7).
func TestBuildActivityOutput_CarriesPersistedStallCheck(t *testing.T) {
	now := time.Date(2026, 9, 25, 15, 0, 0, 0, time.UTC)
	observed := []witness.RealActivity{
		{Polecat: "opal", Session: "gt-opal", AgentAlive: true, ObservedAt: now},
		{Polecat: "lapis", Session: "gt-lapis", AgentAlive: true, ObservedAt: now},
	}
	checks := []witness.StallCheck{
		{Polecat: "opal", Stalled: true, Reason: "no transcript change and no pane content change for 31m0s",
			Baseline: witness.StallSample{Polecat: "opal", SampledAt: now.Add(-31 * time.Minute)}},
		{Polecat: "lapis", Recorded: true, Reason: "sample 1 recorded; compare after 30m0s",
			Baseline: witness.StallSample{Polecat: "lapis", SampledAt: now}},
	}

	out := buildActivityOutput(observed, checks, now)
	if out.Stalled != 1 {
		t.Errorf("Stalled = %d, want 1", out.Stalled)
	}
	opal, lapis := out.Items[0], out.Items[1]
	if !opal.Stalled || !strings.Contains(opal.StallReason, "no pane content change") {
		t.Errorf("opal must carry the stall verdict: %+v", opal)
	}
	if opal.StallSampleAt != now.Add(-31*time.Minute).Format(time.RFC3339) {
		t.Errorf("opal stall_sample_at = %q", opal.StallSampleAt)
	}
	if opal.StallSampleAgeSeconds == nil || *opal.StallSampleAgeSeconds != (31*time.Minute).Seconds() {
		t.Errorf("opal stall_sample_age_seconds = %v, want 1860", opal.StallSampleAgeSeconds)
	}
	if lapis.Stalled || lapis.StallSampleAgeSeconds == nil || *lapis.StallSampleAgeSeconds != 0 {
		t.Errorf("lapis just recorded sample 1 and is not stalled: %+v", lapis)
	}

	data, err := json.Marshal(opal)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"stalled":true`, `"stall_reason"`, `"stall_sample_at"`, `"stall_sample_age_seconds"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("JSON item missing %s: %s", key, data)
		}
	}
}

// TestPatrolScanHumanPrintsStallVerdict: the human output flags a positive stall
// signal with the policy (nudge, escalate, never restart alone), and stays
// quiet for a polecat that merely has a sample 1.
func TestPatrolScanHumanPrintsStallVerdict(t *testing.T) {
	now := time.Now()
	observed := []witness.RealActivity{
		{Polecat: "opal", Session: "gt-opal", AgentAlive: true, ObservedAt: now, PaneSignature: "sig"},
		{Polecat: "lapis", Session: "gt-lapis", AgentAlive: true, ObservedAt: now, PaneSignature: "sig2"},
	}
	checks := []witness.StallCheck{
		{Polecat: "opal", Stalled: true, Reason: "no transcript change and no pane content change for 31m0s",
			Baseline: witness.StallSample{SampledAt: now.Add(-31 * time.Minute)}},
		{Polecat: "lapis", Recorded: true, Reason: "sample 1 recorded; compare after 30m0s",
			Baseline: witness.StallSample{SampledAt: now}},
	}
	printed := captureStdout(t, func() {
		if err := outputPatrolScanHuman("gastown", nil, nil, nil, nil, observed, nil, checks); err != nil {
			t.Errorf("outputPatrolScanHuman: %v", err)
		}
	})
	if !strings.Contains(printed, "STALLED") || !strings.Contains(printed, "no pane content change") {
		t.Errorf("output must flag opal's stall:\n%s", printed)
	}
	if !strings.Contains(printed, "escalate") {
		t.Errorf("stall line must carry the escalate-not-restart policy:\n%s", printed)
	}
	if strings.Count(printed, "STALLED") != 1 {
		t.Errorf("only opal is stalled:\n%s", printed)
	}
}

// The human stall line names the clocks that carried evidence, and it must not
// claim a pending age it does not have. A first observation reports zero, and
// "input waiting 0s" reads as input that has just arrived — the opposite of what
// a silence-clock verdict means (gt-afa7).
func TestPatrolScanHumanRefineryLineReportsOnlyTheClocksItHas(t *testing.T) {
	tests := []struct {
		name      string
		result    witness.DetectRefineryStallResult
		want      []string
		notWanted []string
	}{
		{
			name: "silence clock alone names no pending age",
			result: witness.DetectRefineryStallResult{
				Checked: 1,
				Stalls: []witness.RefineryStallResult{{
					Agent:      "refinery",
					StallType:  "composer-stall",
					State:      "pending",
					Inactivity: 6 * time.Minute,
					Action:     witness.ComposerStallActionStillPending,
				}},
			},
			want:      []string{"silent 6m0s"},
			notWanted: []string{"input waiting"},
		},
		{
			name: "age clock names its age and its samples",
			result: witness.DetectRefineryStallResult{
				Checked: 1,
				Stalls: []witness.RefineryStallResult{{
					Agent:          "refinery",
					StallType:      "composer-stall",
					State:          "pending",
					Inactivity:     time.Second,
					PendingFor:     6 * time.Minute,
					PendingSamples: 4,
					Action:         witness.ComposerStallActionSubmittedQueued,
				}},
			},
			want: []string{"input waiting 6m0s", "4 observation(s)", "silent 1s"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.result
			printed := captureStdout(t, func() {
				if err := outputPatrolScanHuman("gastown", nil, nil, &result, nil, nil, nil, nil); err != nil {
					t.Errorf("outputPatrolScanHuman: %v", err)
				}
			})
			for _, want := range tt.want {
				if !strings.Contains(printed, want) {
					t.Errorf("output %q does not mention %q", printed, want)
				}
			}
			for _, unwanted := range tt.notWanted {
				if strings.Contains(printed, unwanted) {
					t.Errorf("output %q mentions %q, which this stall has no evidence for", printed, unwanted)
				}
			}
		})
	}
}

// The JSON carries the sample count so a reader can tell a run that was
// restarted mid-flight from one that has been continuously unattended: a large
// age beside a small count means the clock went back to zero in between.
func TestPatrolScanRefineryItemSerializesPendingSamples(t *testing.T) {
	t.Parallel()
	item := PatrolScanRefineryItem{
		Session:        "gt-gastown-refinery",
		Agent:          "refinery",
		StallType:      "composer-stall",
		State:          "pending",
		PendingSeconds: 360,
		PendingSamples: 4,
		Action:         witness.ComposerStallActionSubmittedQueued,
	}
	data, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var parsed PatrolScanRefineryItem
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.PendingSamples != 4 {
		t.Errorf("PendingSamples = %d, want 4", parsed.PendingSamples)
	}
	if !strings.Contains(string(data), "pending_samples") {
		t.Errorf("JSON %s does not carry pending_samples", data)
	}
}
