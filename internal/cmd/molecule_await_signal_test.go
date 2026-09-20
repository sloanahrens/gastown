package cmd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCalculateEffectiveTimeout(t *testing.T) {
	tests := []struct {
		name        string
		timeout     string
		backoffBase string
		backoffMult int
		backoffMax  string
		idleCycles  int
		want        time.Duration
		wantErr     bool
	}{
		{
			name:    "simple timeout 60s",
			timeout: "60s",
			want:    60 * time.Second,
		},
		{
			name:    "simple timeout 5m",
			timeout: "5m",
			want:    5 * time.Minute,
		},
		{
			name:        "backoff base only, idle=0",
			timeout:     "60s",
			backoffBase: "30s",
			idleCycles:  0,
			want:        30 * time.Second,
		},
		{
			name:        "backoff with idle=1, mult=2",
			timeout:     "60s",
			backoffBase: "30s",
			backoffMult: 2,
			idleCycles:  1,
			want:        60 * time.Second,
		},
		{
			name:        "backoff with idle=2, mult=2",
			timeout:     "60s",
			backoffBase: "30s",
			backoffMult: 2,
			idleCycles:  2,
			want:        2 * time.Minute,
		},
		{
			name:        "backoff with max cap",
			timeout:     "60s",
			backoffBase: "30s",
			backoffMult: 2,
			backoffMax:  "5m",
			idleCycles:  10, // Would be 30s * 2^10 = ~8.5h but capped at 5m
			want:        5 * time.Minute,
		},
		{
			name:        "backoff overflow guard: idle=34 with max cap",
			timeout:     "60s",
			backoffBase: "30s",
			backoffMult: 2,
			backoffMax:  "5m",
			idleCycles:  34, // 30s * 2^34 overflows int64; must clamp to 5m
			want:        5 * time.Minute,
		},
		{
			name:        "backoff base exceeds max",
			timeout:     "60s",
			backoffBase: "15m",
			backoffMax:  "10m",
			want:        10 * time.Minute,
		},
		{
			name:    "invalid timeout",
			timeout: "invalid",
			wantErr: true,
		},
		{
			name:        "invalid backoff base",
			timeout:     "60s",
			backoffBase: "invalid",
			wantErr:     true,
		},
		{
			name:        "invalid backoff max",
			timeout:     "60s",
			backoffBase: "30s",
			backoffMax:  "invalid",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set package-level variables
			awaitSignalTimeout = tt.timeout
			awaitSignalBackoffBase = tt.backoffBase
			awaitSignalBackoffMult = tt.backoffMult
			if tt.backoffMult == 0 {
				awaitSignalBackoffMult = 2 // default
			}
			awaitSignalBackoffMax = tt.backoffMax

			got, err := calculateEffectiveTimeout(tt.idleCycles)
			if (err != nil) != tt.wantErr {
				t.Errorf("calculateEffectiveTimeout() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("calculateEffectiveTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAwaitSignalResult(t *testing.T) {
	// Test that result struct marshals correctly
	result := AwaitSignalResult{
		Reason:  "signal",
		Elapsed: 5 * time.Second,
		Signal:  "[12:34:56] + gt-abc created · New issue",
	}

	if result.Reason != "signal" {
		t.Errorf("expected reason 'signal', got %q", result.Reason)
	}
	if result.Signal == "" {
		t.Error("expected signal to be set")
	}
}

func TestWaitForEventsFile_MissingFile(t *testing.T) {
	// When the events file doesn't exist, waitForEventsFile creates it and
	// waits for new events. With no events, it should return timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	result, err := waitForEventsFile(ctx, filepath.Join(t.TempDir(), "nonexistent.jsonl"), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reason != "timeout" {
		t.Errorf("expected reason 'timeout', got %q", result.Reason)
	}
}

func TestWaitForEventsFile_Timeout(t *testing.T) {
	// When no new events are appended, waitForEventsFile should return timeout.
	eventsPath := filepath.Join(t.TempDir(), ".events.jsonl")
	if err := os.WriteFile(eventsPath, []byte(`{"ts":"2024-01-01","type":"test"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	result, err := waitForEventsFile(ctx, eventsPath, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reason != "timeout" {
		t.Errorf("expected reason 'timeout', got %q", result.Reason)
	}
}

func TestWaitForEventsFile_Signal(t *testing.T) {
	// When a new event is appended, waitForEventsFile should return signal.
	eventsPath := filepath.Join(t.TempDir(), ".events.jsonl")
	// Write initial content (will be skipped — we seek to end)
	if err := os.WriteFile(eventsPath, []byte(`{"ts":"old","type":"ignore"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Append a new line after a short delay
	go func() {
		time.Sleep(300 * time.Millisecond)
		f, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString(`{"ts":"new","type":"sling","actor":"test"}` + "\n")
	}()

	result, err := waitForEventsFile(ctx, eventsPath, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reason != "signal" {
		t.Errorf("expected reason 'signal', got %q", result.Reason)
	}
	if result.Signal == "" {
		t.Error("expected signal line to be set")
	}
}

func TestWaitForActivitySignal_PathWiring(t *testing.T) {
	// Verify waitForActivitySignal constructs the correct events path from
	// townRoot. The events file should be at <townRoot>/.events.jsonl.
	townRoot := t.TempDir()
	eventsPath := filepath.Join(townRoot, ".events.jsonl")
	if err := os.WriteFile(eventsPath, []byte(`{"ts":"old","type":"ignore"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Append a new event after a short delay
	go func() {
		time.Sleep(200 * time.Millisecond)
		f, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString(`{"ts":"new","type":"sling"}` + "\n")
	}()

	result, err := waitForActivitySignal(ctx, townRoot, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reason != "signal" {
		t.Errorf("expected reason 'signal', got %q", result.Reason)
	}
}

func TestEventRelevantToRig(t *testing.T) {
	// Shapes below are copied from a live ~/gt/.events.jsonl, which is what an
	// idle rig's witness was being woken by (gt-qwfp).
	tests := []struct {
		name  string
		line  string
		rig   string
		want  bool
		notes string
	}{
		{
			name: "own rig actor wakes",
			line: `{"type":"done","actor":"om/polecats/garnet","payload":{"bead":"om-1"}}`,
			rig:  "om", want: true,
		},
		{
			name: "bare rig actor wakes",
			line: `{"type":"session_start","actor":"om","payload":{}}`,
			rig:  "om", want: true,
		},
		{
			name: "another rig's polecat is skipped",
			line: `{"type":"done","actor":"gastown/polecats/garnet","payload":{"bead":"gt-2bj"}}`,
			rig:  "om", want: false,
			notes: "the exact cross-rig wake that kept om patrolling at full effort",
		},
		{
			name: "dog nudge to the deacon is skipped",
			line: `{"type":"nudge","actor":"dog","payload":{"reason":"DOG_DONE: compactor-dog check-only","rig":"","target":"deacon"}}`,
			rig:  "om", want: false,
		},
		{
			name: "mail addressed to my rig wakes",
			line: `{"type":"mail","actor":"mayor/","payload":{"subject":"Deacon line rejected","to":"om/witness"}}`,
			rig:  "om", want: true,
		},
		{
			name: "mail addressed to another rig is skipped",
			line: `{"type":"mail","actor":"mayor/","payload":{"subject":"Deacon line rejected","to":"gastown/witness"}}`,
			rig:  "om", want: false,
		},
		{
			name: "nudge targeting my rig wakes",
			line: `{"type":"nudge","actor":"mayor","payload":{"reason":"wake up","rig":"","target":"om/witness"}}`,
			rig:  "om", want: true,
		},
		{
			name: "sling targeting my polecat wakes",
			line: `{"type":"sling","actor":"mayor","payload":{"bead":"om-hd2","target":"om/polecats/jasper"}}`,
			rig:  "om", want: true,
		},
		{
			name: "spawn in my rig wakes",
			line: `{"type":"spawn","actor":"gt","payload":{"polecat":"jasper","rig":"om"}}`,
			rig:  "om", want: true,
		},
		{
			name: "spawn in another rig is skipped",
			line: `{"type":"spawn","actor":"gt","payload":{"polecat":"flint","rig":"gastown"}}`,
			rig:  "om", want: false,
		},
		{
			name: "town-scoped nudge without a rig target is skipped",
			line: `{"type":"nudge","actor":"dog","payload":{"reason":"DOG_DONE","rig":"","target":"deacon"}}`,
			rig:  "om", want: false,
		},
		{
			name: "town-wide boot wakes only a town scope",
			line: `{"type":"boot","actor":"gt","payload":{"rig":"town","agents":[]}}`,
			rig:  "om", want: false,
		},
		{
			name: "empty rig accepts anything",
			line: `{"type":"done","actor":"gastown/polecats/garnet","payload":{}}`,
			rig:  "", want: true,
		},
		{
			name: "empty rig accepts an unparseable line",
			line: `not json at all`,
			rig:  "", want: true,
		},
		{
			name: "unparseable line is skipped under a rig scope",
			line: `not json at all`,
			rig:  "om", want: false,
		},
		{
			name: "trailing slash on a town actor is not my rig",
			line: `{"type":"mail","actor":"mayor/","payload":{"to":"mayor/"}}`,
			rig:  "om", want: false,
		},
		{
			name: "missing payload does not panic",
			line: `{"type":"done","actor":"dog"}`,
			rig:  "om", want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := eventRelevantToRig(tt.line, tt.rig); got != tt.want {
				t.Errorf("eventRelevantToRig(%s, %q) = %v, want %v", tt.line, tt.rig, got, tt.want)
			}
		})
	}
}

func TestAddressInRig(t *testing.T) {
	tests := []struct {
		addr, rig string
		want      bool
	}{
		{"om", "om", true},
		{"om/witness", "om", true},
		{"om/polecats/jasper", "om", true},
		{"mayor/", "om", false},
		{"gastown/witness", "om", false},
		// Prefix boundary: a longer rig name must not match a shorter one.
		{"beads/witness", "be", false},
		{"om2/witness", "om", false},
		{"", "om", false},
		{"om", "", false},
	}

	for _, tt := range tests {
		if got := addressInRig(tt.addr, tt.rig); got != tt.want {
			t.Errorf("addressInRig(%q, %q) = %v, want %v", tt.addr, tt.rig, got, tt.want)
		}
	}
}

func TestWaitForEventsFile_CrossRigActivityTimesOut(t *testing.T) {
	// The gt-qwfp bug: an idle rig's witness was woken by any town event, so
	// idle backoff never engaged. Cross-rig activity must not wake it.
	eventsPath := filepath.Join(t.TempDir(), ".events.jsonl")
	if err := os.WriteFile(eventsPath, []byte(`{"ts":"old"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	go func() {
		f, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return
		}
		defer f.Close()
		for _, line := range []string{
			`{"type":"nudge","actor":"dog","payload":{"target":"deacon"}}`,
			`{"type":"done","actor":"gastown/polecats/garnet","payload":{"bead":"gt-2bj"}}`,
			`{"type":"mail","actor":"mayor/","payload":{"to":"gastown/witness"}}`,
		} {
			_, _ = f.WriteString(line + "\n")
		}
	}()

	result, err := waitForEventsFile(ctx, eventsPath, "om")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reason != "timeout" {
		t.Errorf("expected reason 'timeout', got %q (woke on %s)", result.Reason, result.Signal)
	}
}

func TestWaitForEventsFile_WakesOnOwnRigAfterSkippingOthers(t *testing.T) {
	// Foreign events arriving first must be skipped, not swallowed, so a later
	// event for this rig still wakes the waiter.
	eventsPath := filepath.Join(t.TempDir(), ".events.jsonl")
	if err := os.WriteFile(eventsPath, []byte(`{"ts":"old"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		f, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString(`{"type":"nudge","actor":"dog","payload":{"target":"deacon"}}` + "\n")
		time.Sleep(300 * time.Millisecond)
		_, _ = f.WriteString(`{"type":"sling","actor":"mayor","payload":{"target":"om/polecats/jasper"}}` + "\n")
	}()

	result, err := waitForEventsFile(ctx, eventsPath, "om")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reason != "signal" {
		t.Fatalf("expected reason 'signal', got %q", result.Reason)
	}
	if !strings.Contains(result.Signal, "om/polecats/jasper") {
		t.Errorf("woke on the wrong event: %s", result.Signal)
	}
}

func TestWaitForEventsFile_WakesOnEventSplitAcrossWrites(t *testing.T) {
	// A poller can catch a line mid-write. The prefix must be held until its
	// newline arrives, or the event is judged unparseable and skipped.
	eventsPath := filepath.Join(t.TempDir(), ".events.jsonl")
	if err := os.WriteFile(eventsPath, []byte(`{"ts":"old"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		f, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString(`{"type":"done","actor":"om/pole`)
		time.Sleep(600 * time.Millisecond) // several poll ticks land mid-line
		_, _ = f.WriteString(`cats/jasper","payload":{}}` + "\n")
	}()

	result, err := waitForEventsFile(ctx, eventsPath, "om")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reason != "signal" {
		t.Fatalf("expected reason 'signal', got %q", result.Reason)
	}
	if !strings.Contains(result.Signal, "om/polecats/jasper") {
		t.Errorf("woke on a truncated event: %s", result.Signal)
	}
}

func TestWaitForEventsFile_DrainsBacklogOfOtherRigs(t *testing.T) {
	// Skipped lines must all be consumed each tick. If only one were drained
	// per tick, a town producing events faster than 5/s would starve the wait
	// and a later signal for this rig would never be seen.
	eventsPath := filepath.Join(t.TempDir(), ".events.jsonl")
	if err := os.WriteFile(eventsPath, []byte(`{"ts":"old"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	const backlog = 500
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		f, err := os.OpenFile(eventsPath, os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return
		}
		defer f.Close()
		for i := 0; i < backlog; i++ {
			_, _ = f.WriteString(`{"type":"nudge","actor":"dog","payload":{"target":"deacon"}}` + "\n")
		}
		_, _ = f.WriteString(`{"type":"done","actor":"om/polecats/jasper","payload":{}}` + "\n")
	}()

	result, err := waitForEventsFile(ctx, eventsPath, "om")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Reason != "signal" {
		t.Fatalf("expected reason 'signal' after draining %d foreign lines, got %q", backlog, result.Reason)
	}
	if !strings.Contains(result.Signal, "om/polecats/jasper") {
		t.Errorf("woke on the wrong event: %s", result.Signal)
	}
}

func TestBackoffWindowResumption(t *testing.T) {
	// Test the backoff window resumption logic that makes await-signal
	// resilient to interrupts. When a backoff-until timestamp is in the
	// future and remaining time <= full timeout, use remaining time.
	now := time.Now()

	tests := []struct {
		name           string
		fullTimeout    time.Duration
		backoffUntil   time.Time
		wantResumed    bool
		wantApproxTime time.Duration // approximate expected timeout
	}{
		{
			name:           "no stored window - use full timeout",
			fullTimeout:    5 * time.Minute,
			backoffUntil:   time.Time{}, // zero value
			wantResumed:    false,
			wantApproxTime: 5 * time.Minute,
		},
		{
			name:           "window in future - resume with remaining",
			fullTimeout:    5 * time.Minute,
			backoffUntil:   now.Add(2 * time.Minute),
			wantResumed:    true,
			wantApproxTime: 2 * time.Minute,
		},
		{
			name:           "window expired - use full timeout",
			fullTimeout:    5 * time.Minute,
			backoffUntil:   now.Add(-1 * time.Minute), // in the past
			wantResumed:    false,
			wantApproxTime: 5 * time.Minute,
		},
		{
			name:           "window exceeds full timeout (stale) - use full timeout",
			fullTimeout:    2 * time.Minute,
			backoffUntil:   now.Add(10 * time.Minute), // remaining > full
			wantResumed:    false,
			wantApproxTime: 2 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			timeout := tt.fullTimeout
			resumed := false

			if !tt.backoffUntil.IsZero() && tt.backoffUntil.After(now) {
				remaining := tt.backoffUntil.Sub(now)
				if remaining <= tt.fullTimeout {
					timeout = remaining
					resumed = true
				}
			}

			if resumed != tt.wantResumed {
				t.Errorf("resumed = %v, want %v", resumed, tt.wantResumed)
			}

			// Allow 2s tolerance for timing
			diff := timeout - tt.wantApproxTime
			if diff < 0 {
				diff = -diff
			}
			if diff > 2*time.Second {
				t.Errorf("timeout = %v, want ~%v (diff: %v)", timeout, tt.wantApproxTime, diff)
			}
		})
	}
}

func TestRunMoleculeAwaitSignalAgentBeadUsesCwdRigBeadsDirWhenBeadsDirPointsTown(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell fake bd")
	}

	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	townRoot := filepath.Join(tmp, "gt")
	townBeads := filepath.Join(townRoot, ".beads")
	rigWorkDir := filepath.Join(townRoot, "gastown", "refinery", "rig")
	rigRedirect := filepath.Join(rigWorkDir, ".beads")
	rigBeads := filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")

	for _, dir := range []string{
		filepath.Join(townRoot, "mayor"),
		townBeads,
		rigRedirect,
		rigBeads,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("write town marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rigRedirect, "redirect"), []byte("../../mayor/rig/.beads"), 0o644); err != nil {
		t.Fatalf("write rig redirect: %v", err)
	}
	metadata := []byte(`{"dolt_database":"rigdb","dolt_server_host":"127.0.0.1","dolt_server_port":3307}`)
	if err := os.WriteFile(filepath.Join(rigBeads, "metadata.json"), metadata, 0o644); err != nil {
		t.Fatalf("write rig metadata: %v", err)
	}

	binDir := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	logPath := filepath.Join(tmp, "bd.log")
	bdScript := `#!/bin/sh
printf 'cmd=%s BEADS_DIR=%s DB=%s READONLY=%s AUTO=%s\n' "$1" "${BEADS_DIR-}" "${BEADS_DOLT_SERVER_DATABASE-}" "${BD_READONLY-}" "${BD_DOLT_AUTO_COMMIT-}" >> "$BD_LOG"
case "$1" in
  show)
    printf '[{"labels":["gt:agent","idle:0"]}]\n'
    ;;
  update)
    ;;
  *)
    printf 'unexpected bd command: %s\n' "$1" >&2
    exit 1
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(bdScript), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_LOG", logPath)
	t.Setenv("BEADS_DIR", townBeads)
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "town")
	t.Setenv("BD_READONLY", "true")
	t.Setenv("BD_DOLT_AUTO_COMMIT", "off")

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() { _ = os.Chdir(oldWd) }()
	if err := os.Chdir(rigWorkDir); err != nil {
		t.Fatalf("chdir rig work dir: %v", err)
	}

	oldTimeout := awaitSignalTimeout
	oldBackoffBase := awaitSignalBackoffBase
	oldBackoffMult := awaitSignalBackoffMult
	oldBackoffMax := awaitSignalBackoffMax
	oldQuiet := awaitSignalQuiet
	oldAgentBead := awaitSignalAgentBead
	oldJSON := moleculeJSON
	t.Cleanup(func() {
		awaitSignalTimeout = oldTimeout
		awaitSignalBackoffBase = oldBackoffBase
		awaitSignalBackoffMult = oldBackoffMult
		awaitSignalBackoffMax = oldBackoffMax
		awaitSignalQuiet = oldQuiet
		awaitSignalAgentBead = oldAgentBead
		moleculeJSON = oldJSON
	})

	awaitSignalTimeout = "1ms"
	awaitSignalBackoffBase = ""
	awaitSignalBackoffMult = 2
	awaitSignalBackoffMax = ""
	awaitSignalQuiet = true
	awaitSignalAgentBead = "gt-gastown-refinery"
	moleculeJSON = false

	if err := runMoleculeAwaitSignal(nil, nil); err != nil {
		t.Fatalf("runMoleculeAwaitSignal() error = %v", err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	log := strings.TrimSpace(string(data))
	if log == "" {
		t.Fatal("fake bd was not invoked")
	}

	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, "BEADS_DIR="+rigBeads) {
			t.Fatalf("bd call was not pinned to rig beads %q: %s\nfull log:\n%s", rigBeads, line, log)
		}
		if strings.Contains(line, "BEADS_DIR="+townBeads) {
			t.Fatalf("bd call used inherited town BEADS_DIR %q: %s\nfull log:\n%s", townBeads, line, log)
		}
		if !strings.Contains(line, "DB=rigdb") {
			t.Fatalf("bd call was not pinned to rig database: %s\nfull log:\n%s", line, log)
		}
		if strings.Contains(line, "DB=town") {
			t.Fatalf("bd call used inherited town database: %s\nfull log:\n%s", line, log)
		}
		if strings.Contains(line, "cmd=show") {
			if !strings.Contains(line, "READONLY=true") || !strings.Contains(line, "AUTO=off") {
				t.Fatalf("bd read was not read-only pinned: %s\nfull log:\n%s", line, log)
			}
		}
		if strings.Contains(line, "cmd=update") {
			if !strings.Contains(line, "READONLY= ") && !strings.HasSuffix(line, "READONLY= AUTO=on") {
				t.Fatalf("bd mutation inherited read-only mode: %s\nfull log:\n%s", line, log)
			}
			if !strings.Contains(line, "AUTO=on") {
				t.Fatalf("bd mutation was not auto-commit pinned: %s\nfull log:\n%s", line, log)
			}
		}
	}
}
