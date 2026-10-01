package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
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

// awaitSignalTown is a minimal town whose agent bead lives in a shared fake
// carrying the given labels. showFails lists the 1-based reads of the bead
// that fail, to model a transient or lasting read failure.
type awaitSignalTown struct {
	townRoot, beadsDir string
	db                 *agentBeadsRecorder
	stderr             bytes.Buffer
	run                awaitSignalRun
}

// agentBeadsRecorder is a beads.Client that numbers its Show calls, failing
// the ones in showFails, and records every Update and the database each
// call was opened on.
type agentBeadsRecorder struct {
	beads.Client
	mu        sync.Mutex
	shows     int
	showFails []int
	updates   []beads.UpdateOptions
	dirs      []string
}

func (r *agentBeadsRecorder) Show(id string) (*beads.Issue, error) {
	r.mu.Lock()
	r.shows++
	n := r.shows
	r.mu.Unlock()
	if slices.Contains(r.showFails, n) {
		return nil, fmt.Errorf("show %d failed", n)
	}
	return r.Client.Show(id)
}

func (r *agentBeadsRecorder) Update(id string, opts beads.UpdateOptions) error {
	r.mu.Lock()
	r.updates = append(r.updates, opts)
	r.mu.Unlock()
	return r.Client.Update(id, opts)
}

// open is an awaitSignalRun.db that records the beads directory asked for.
func (r *agentBeadsRecorder) open(beadsDir string) beads.Client {
	r.mu.Lock()
	r.dirs = append(r.dirs, beadsDir)
	r.mu.Unlock()
	return r
}

func newAwaitSignalTown(t *testing.T, labels []string, showFails ...int) *awaitSignalTown {
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
	fake := beadsfake.New()
	fake.Seed(beads.Issue{ID: "hq-deacon", Labels: labels})
	db := &agentBeadsRecorder{Client: fake, showFails: showFails}
	w := &awaitSignalTown{townRoot: townRoot, beadsDir: beadsDir, db: db}
	w.run = awaitSignalRun{
		agentBead:   "hq-deacon",
		rig:         awaitSignalRigAny,
		quiet:       true,
		db:          db.open,
		out:         io.Discard,
		errOut:      &w.stderr,
		eventRig:    resolveEventRig,
		drainNudges: func(string) []nudge.QueuedNudge { return nil },
	}
	return w
}

// idleLabelChanges returns the idle:N labels the run's updates wrote.
func idleLabelChanges(db *agentBeadsRecorder) []string {
	db.mu.Lock()
	defer db.mu.Unlock()
	var idle []string
	for _, u := range db.updates {
		for _, l := range u.AddLabels {
			if strings.HasPrefix(l, "idle:") {
				idle = append(idle, l)
			}
		}
	}
	return idle
}

// TestAwaitSignalOpensTheAgentBeadsDatabase: every read and write
// await-signal makes goes to the beads directory it was given.
func TestAwaitSignalOpensTheAgentBeadsDatabase(t *testing.T) {
	t.Parallel()
	w := newAwaitSignalTown(t, []string{"gt:agent", "idle:0"})
	w.run.backoff = awaitSignalBackoff{timeout: "1ms", mult: 2}
	if err := w.run.run(w.beadsDir, w.townRoot); err != nil {
		t.Fatalf("await-signal: %v", err)
	}
	if len(w.db.dirs) == 0 {
		t.Fatal("the agent bead database was never opened")
	}
	for _, d := range w.db.dirs {
		if d != w.beadsDir {
			t.Errorf("opened %q, want %q", d, w.beadsDir)
		}
	}
}

// A real event must reset the idle counter inside await-signal. The formula
// used to leave the reset to the agent, which skipped it 20 of 20 times, so
// every deacon wait sat at the backoff cap (claude-9jq).
func TestRunMoleculeAwaitSignal_SignalResetsIdle(t *testing.T) {
	t.Parallel()
	w := newAwaitSignalTown(t, []string{"gt:agent", "idle:5"})
	w.run.backoff = awaitSignalBackoff{base: "20s", mult: 2, max: "20s"}

	wakeOnSignal(w)

	if err := w.run.run(w.beadsDir, w.townRoot); err != nil {
		t.Fatalf("runMoleculeAwaitSignal: %v", err)
	}
	if got := idleLabelChanges(w.db); len(got) != 1 || got[0] != "idle:0" {
		t.Fatalf("idle label updates = %q, want exactly [idle:0]", got)
	}
}

// Timeouts keep backing off: idle goes up by one, never back to zero.
func TestRunMoleculeAwaitSignal_TimeoutIncrementsIdle(t *testing.T) {
	t.Parallel()
	w := newAwaitSignalTown(t, []string{"gt:agent", "idle:5"})
	w.run.backoff = awaitSignalBackoff{base: "1ms", mult: 1}

	if err := w.run.run(w.beadsDir, w.townRoot); err != nil {
		t.Fatalf("runMoleculeAwaitSignal: %v", err)
	}
	if got := idleLabelChanges(w.db); len(got) != 1 || got[0] != "idle:6" {
		t.Fatalf("idle label updates = %q, want exactly [idle:6]", got)
	}
}

// A signal at idle 0 has nothing to reset, so no extra bd write is spent.
func TestRunMoleculeAwaitSignal_SignalAtIdleZeroSkipsWrite(t *testing.T) {
	t.Parallel()
	w := newAwaitSignalTown(t, []string{"gt:agent", "idle:0"})
	w.run.backoff = awaitSignalBackoff{base: "20s", mult: 2, max: "20s"}

	wakeOnSignal(w)

	if err := w.run.run(w.beadsDir, w.townRoot); err != nil {
		t.Fatalf("runMoleculeAwaitSignal: %v", err)
	}
	if got := idleLabelChanges(w.db); len(got) != 0 {
		t.Fatalf("idle label writes = %q, want none: an idle reset at idle 0 is a wasted write", got)
	}
}

// wakeOnSignal makes the run's wait return a signal at once, as a town event
// arriving would.
func wakeOnSignal(w *awaitSignalTown) {
	w.run.wait = func(context.Context, string, string) (*AwaitSignalResult, error) {
		return &AwaitSignalResult{Reason: "signal", Signal: `{"ts":"now","type":"mail","actor":"mayor"}`}, nil
	}
}

// When the idle read failed (a transient bd error), the counter is unknown and
// may be high: a real wake must still try to reset it.
func TestRunMoleculeAwaitSignal_SignalResetsIdleWhenReadFailed(t *testing.T) {
	t.Parallel()
	w := newAwaitSignalTown(t, []string{"gt:agent", "idle:5"}, 1) // only the initial idle read fails
	w.run.backoff = awaitSignalBackoff{base: "20s", mult: 2, max: "20s"}

	wakeOnSignal(w)
	if err := w.run.run(w.beadsDir, w.townRoot); err != nil {
		t.Fatalf("runMoleculeAwaitSignal: %v", err)
	}
	if got := idleLabelChanges(w.db); len(got) != 1 || got[0] != "idle:0" {
		t.Fatalf("idle label changes = %q, want exactly [idle:0]", got)
	}
}

// A failed reset is reported on stderr even under --quiet: the formula tells
// the agent to reset by hand only when it sees this warning.
func TestRunMoleculeAwaitSignal_ResetFailureWarnsUnderQuiet(t *testing.T) {
	t.Parallel()
	// Show calls: 1 idle read, 2 backoff-until set, 3 heartbeat, 4 idle
	// reset. Fail the reset's read so the reset itself fails.
	w := newAwaitSignalTown(t, []string{"gt:agent", "idle:5"}, 4)
	w.run.backoff = awaitSignalBackoff{base: "20s", mult: 2, max: "20s"}
	w.run.quiet = true

	wakeOnSignal(w)
	if err := w.run.run(w.beadsDir, w.townRoot); err != nil {
		t.Fatalf("runMoleculeAwaitSignal: %v", err)
	}
	stderr := w.stderr.String()
	if !strings.Contains(stderr, "Failed to reset agent bead idle count") {
		t.Fatalf("stderr = %q, want the reset-failure warning", stderr)
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
