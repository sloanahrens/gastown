package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/nudge"
)

func TestCalculateEffectiveTimeout(t *testing.T) {
	t.Parallel()
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
			backoffBase: "8m",
			backoffMax:  "5m",
			want:        5 * time.Minute,
		},
		{
			// The deacon formula's 15m cap outlived the 10m agent tool-call
			// limit, so every capped wait was backgrounded (claude-9jq).
			name:        "backoff max above the single-wait bound is clamped",
			timeout:     "60s",
			backoffBase: "60s",
			backoffMult: 2,
			backoffMax:  "15m",
			idleCycles:  7,
			want:        9 * time.Minute,
		},
		{
			name:        "uncapped backoff is clamped to the single-wait bound",
			timeout:     "60s",
			backoffBase: "30s",
			backoffMult: 2,
			idleCycles:  20,
			want:        9 * time.Minute,
		},
		{
			name:        "backoff base above the single-wait bound is clamped",
			timeout:     "60s",
			backoffBase: "15m",
			backoffMax:  "10m",
			want:        9 * time.Minute,
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
			t.Parallel()
			bo := awaitSignalBackoff{timeout: tt.timeout, base: tt.backoffBase, mult: tt.backoffMult, max: tt.backoffMax}
			if tt.backoffMult == 0 {
				bo.mult = 2 // default
			}

			got, err := bo.effectiveTimeout(tt.idleCycles)
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
	t.Parallel()
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

func TestEventRelevantToRig(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

func TestBackoffWindowResumption(t *testing.T) {
	t.Parallel()
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

// awaitSignalTown is a minimal town with an in-process bd that reports the
// given agent labels and logs every call. showFails lists the 1-based show
// calls that fail, to model a transient or lasting read failure.
type awaitSignalTown struct {
	townRoot, beadsDir string
	bd                 *inprocBD
	stderr             bytes.Buffer
	run                awaitSignalRun
}

func newAwaitSignalTown(t *testing.T, labels string, showFails ...int) *awaitSignalTown {
	t.Helper()
	townRoot := filepath.Join(t.TempDir(), "gt")
	beadsDir := filepath.Join(townRoot, ".beads")
	for _, dir := range []string{filepath.Join(townRoot, "mayor"), beadsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	metadata := []byte(`{"dolt_database":"hq","dolt_server_host":"127.0.0.1","dolt_server_port":3307}`)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), metadata, 0o644); err != nil {
		t.Fatal(err)
	}
	shows := 0
	bd := &inprocBD{answer: func(f *inprocBD, cmd string, args []string) bdAnswer {
		f.logLine(cmd + " " + strings.Join(args, " "))
		switch cmd {
		case "show":
			f.mu.Lock()
			shows++
			n := shows
			f.mu.Unlock()
			for _, fail := range showFails {
				if n == fail {
					return bdAnswer{stderr: fmt.Sprintf("show %d failed", n), code: 1}
				}
			}
			return bdOut(`[{"labels":` + labels + `}]`)
		case "update":
			return bdOut("")
		}
		return bdAnswer{stderr: "unexpected bd command: " + cmd, code: 1}
	}}
	w := &awaitSignalTown{townRoot: townRoot, beadsDir: beadsDir, bd: bd}
	w.run = awaitSignalRun{
		agentBead:   "hq-deacon",
		rig:         awaitSignalRigAny,
		quiet:       true,
		bd:          bd.run,
		out:         io.Discard,
		errOut:      &w.stderr,
		eventRig:    resolveEventRig,
		drainNudges: func(string) []nudge.QueuedNudge { return nil },
	}
	return w
}

// idleLabelChanges returns the idle:N values bd updates wrote that differ from
// initial. The fake's show always returns the initial labels, so other
// read-modify-write updates (heartbeat, backoff-until) re-send the initial
// idle label unchanged; only a differing value is an idle write.
func idleLabelChanges(bd *inprocBD, initial string) []string {
	var idle []string
	for _, line := range strings.Split(bd.log(), "\n") {
		if !strings.HasPrefix(line, "update ") {
			continue
		}
		for _, arg := range strings.Fields(line) {
			if strings.HasPrefix(arg, "--set-labels=idle:") && arg != "--set-labels="+initial {
				idle = append(idle, strings.TrimPrefix(arg, "--set-labels="))
			}
		}
	}
	return idle
}

// TestAwaitSignalPinsBDToTheRigBeads: every bd call await-signal makes is
// pinned to the agent bead's beads directory and database, reads read-only
// and writes auto-committed, whatever the session inherited.
func TestAwaitSignalPinsBDToTheRigBeads(t *testing.T) {
	t.Parallel()
	w := newAwaitSignalTown(t, `["gt:agent","idle:0"]`)
	var calls []beads.BDCall
	var mu sync.Mutex
	inner := w.bd.run
	w.run.bd = func(ctx context.Context, c beads.BDCall) ([]byte, []byte, error) {
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		return inner(ctx, c)
	}
	w.run.backoff = awaitSignalBackoff{timeout: "1ms", mult: 2}
	if err := w.run.run(w.beadsDir, w.townRoot); err != nil {
		t.Fatalf("await-signal: %v", err)
	}
	if len(calls) == 0 {
		t.Fatal("bd was not called")
	}
	for _, c := range calls {
		env := envMap(envSlice(c.Env))
		if env("BEADS_DIR") != w.beadsDir || env("BEADS_DOLT_SERVER_DATABASE") != "hq" {
			t.Errorf("bd %v ran with BEADS_DIR=%q DB=%q, want %q and hq", c.Args, env("BEADS_DIR"), env("BEADS_DOLT_SERVER_DATABASE"), w.beadsDir)
		}
		switch c.Args[0] {
		case "show":
			if env("BD_READONLY") != "true" || env("BD_DOLT_AUTO_COMMIT") != "off" {
				t.Errorf("read %v not read-only pinned: READONLY=%q AUTO=%q", c.Args, env("BD_READONLY"), env("BD_DOLT_AUTO_COMMIT"))
			}
		case "update":
			if env("BD_READONLY") != "" || env("BD_DOLT_AUTO_COMMIT") != "on" {
				t.Errorf("write %v not auto-commit pinned: READONLY=%q AUTO=%q", c.Args, env("BD_READONLY"), env("BD_DOLT_AUTO_COMMIT"))
			}
		}
	}
}

// envSlice turns KEY=VALUE pairs into a map; a later pair wins, as exec's
// environment does.
func envSlice(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return m
}

// A real event must reset the idle counter inside await-signal. The formula
// used to leave the reset to the agent, which skipped it 20 of 20 times, so
// every deacon wait sat at the backoff cap (claude-9jq).
func TestRunMoleculeAwaitSignal_SignalResetsIdle(t *testing.T) {
	t.Parallel()
	w := newAwaitSignalTown(t, `["gt:agent","idle:5"]`)
	w.run.backoff = awaitSignalBackoff{base: "20s", mult: 2, max: "20s"} // a missed wake fails in 20s, not minutes

	keepAppendingEvents(t, w.townRoot)

	start := time.Now()
	if err := w.run.run(w.beadsDir, w.townRoot); err != nil {
		t.Fatalf("runMoleculeAwaitSignal: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 20*time.Second {
		t.Fatalf("wait ran %v; the event should have woken it", elapsed)
	}
	if got := idleLabelChanges(w.bd, "idle:5"); len(got) != 1 || got[0] != "idle:0" {
		t.Fatalf("idle label updates = %q, want exactly [idle:0]", got)
	}
}

// Timeouts keep backing off: idle goes up by one, never back to zero.
func TestRunMoleculeAwaitSignal_TimeoutIncrementsIdle(t *testing.T) {
	t.Parallel()
	w := newAwaitSignalTown(t, `["gt:agent","idle:5"]`)
	w.run.backoff = awaitSignalBackoff{base: "1ms", mult: 1}

	if err := w.run.run(w.beadsDir, w.townRoot); err != nil {
		t.Fatalf("runMoleculeAwaitSignal: %v", err)
	}
	if got := idleLabelChanges(w.bd, "idle:5"); len(got) != 1 || got[0] != "idle:6" {
		t.Fatalf("idle label updates = %q, want exactly [idle:6]", got)
	}
}

// A signal at idle 0 has nothing to reset, so no extra bd write is spent.
func TestRunMoleculeAwaitSignal_SignalAtIdleZeroSkipsWrite(t *testing.T) {
	t.Parallel()
	w := newAwaitSignalTown(t, `["gt:agent","idle:0"]`)
	w.run.backoff = awaitSignalBackoff{base: "20s", mult: 2, max: "20s"} // a missed wake fails in 20s, not minutes

	keepAppendingEvents(t, w.townRoot)

	if err := w.run.run(w.beadsDir, w.townRoot); err != nil {
		t.Fatalf("runMoleculeAwaitSignal: %v", err)
	}
	// Every update re-sends idle:0 unchanged, so count updates instead. The
	// fake never reports a backoff-until label, so clearing it is a no-op.
	data := w.bd.log()
	if n := strings.Count(string(data), "update "); n != 2 {
		t.Fatalf("bd updates = %d, want 2 (backoff-until set, heartbeat); an idle reset at idle 0 is a wasted write\n%s", n, data)
	}
}

// keepAppendingEvents appends a town event every 200ms until the test ends.
// A single append at a fixed delay races the fake-bd calls that run before
// the wait opens the events file: under a loaded full-package run they took
// longer than the delay, the append landed before the tail's seek-to-end,
// and the wait slept to its cap.
func keepAppendingEvents(t *testing.T, townRoot string) {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	t.Cleanup(func() { close(stop); <-done })
	go func() {
		defer close(done)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				f, err := os.OpenFile(filepath.Join(townRoot, ".events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				if err != nil {
					continue
				}
				_, _ = f.WriteString(`{"ts":"now","type":"mail","actor":"mayor"}` + "\n")
				_ = f.Close()
			}
		}
	}()
}

// When the idle read failed (a transient bd error), the counter is unknown and
// may be high: a real wake must still try to reset it.
func TestRunMoleculeAwaitSignal_SignalResetsIdleWhenReadFailed(t *testing.T) {
	t.Parallel()
	w := newAwaitSignalTown(t, `["gt:agent","idle:5"]`, 1)               // only the initial idle read fails
	w.run.backoff = awaitSignalBackoff{base: "20s", mult: 2, max: "20s"} // a missed wake fails in 20s, not minutes

	keepAppendingEvents(t, w.townRoot)
	if err := w.run.run(w.beadsDir, w.townRoot); err != nil {
		t.Fatalf("runMoleculeAwaitSignal: %v", err)
	}
	if got := idleLabelChanges(w.bd, "idle:5"); len(got) != 1 || got[0] != "idle:0" {
		t.Fatalf("idle label changes = %q, want exactly [idle:0]", got)
	}
}

// A failed reset is reported on stderr even under --quiet: the formula tells
// the agent to reset by hand only when it sees this warning.
func TestRunMoleculeAwaitSignal_ResetFailureWarnsUnderQuiet(t *testing.T) {
	t.Parallel()
	// Show calls: 1 idle read, 2 backoff-until set, 3 heartbeat, 4 idle
	// reset. Fail the reset's read so the reset itself fails.
	w := newAwaitSignalTown(t, `["gt:agent","idle:5"]`, 4)
	w.run.backoff = awaitSignalBackoff{base: "20s", mult: 2, max: "20s"} // a missed wake fails in 20s, not minutes
	w.run.quiet = true

	keepAppendingEvents(t, w.townRoot)
	if err := w.run.run(w.beadsDir, w.townRoot); err != nil {
		t.Fatalf("runMoleculeAwaitSignal: %v", err)
	}
	stderr := w.stderr.String()
	if !strings.Contains(stderr, "Failed to reset agent bead idle count") {
		t.Fatalf("stderr = %q, want the reset-failure warning", stderr)
	}
}
