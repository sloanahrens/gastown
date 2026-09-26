package deacon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The deacon's RECOVERED_BEAD handling is the one dispatcher that overrides a
// dispatch hold by design: it re-slings with --force. These tests pin the two
// decisions it makes on a recovered bead with the hold stubbed out — the rule
// itself is convoy.DispatchHoldReason's, tested in internal/convoy, and what
// the deacon owes it is honoring whatever reason comes back.

// stubDispatchTools puts a `bd` and a `gt` on PATH that log every invocation
// to a file, so a test can see which dispatch the handler chose — a sling, an
// escalation mail, a needs_human label, or none of the three — without a live
// town. The returned function reads the log back as one line per invocation.
func stubDispatchTools(t *testing.T) func() []string {
	t.Helper()
	return stubDispatchToolsSlinging(t, "", 0)
}

// stubDispatchToolsSlinging is stubDispatchTools with a `gt sling` that writes
// slingStderr to stderr and exits slingExit, so a test can play back what a
// real sling reports when it refuses or fails.
func stubDispatchToolsSlinging(t *testing.T, slingStderr string, slingExit int) func() []string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows - shell stubs")
	}

	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "calls.log")
	stderrPath := filepath.Join(binDir, "sling.stderr")
	if err := os.WriteFile(stderrPath, []byte(slingStderr), 0644); err != nil {
		t.Fatalf("write sling stderr: %v", err)
	}

	// One line per invocation, with any newline in an argument folded to a
	// space: `gt mail send` bodies are multi-line, and a raw log would split
	// one call across several lines and invite a substring match to pass on
	// the wrong one.
	script := `#!/bin/sh
TOOL="$(basename "$0")"
printf '%s\t' "$TOOL" >> "` + logPath + `"
printf '%s ' "$@" | tr '\n' ' ' >> "` + logPath + `"
printf '\n' >> "` + logPath + `"
if [ "$TOOL" = "bd" ] && [ "$1" = "show" ]; then
  printf '%s\n' '[{"id":"gt-stubbed","status":"open"}]'
fi
if [ "$TOOL" = "gt" ] && [ "$1" = "sling" ]; then
  cat "` + stderrPath + `" >&2
  exit ` + fmt.Sprint(slingExit) + `
fi
exit 0
`
	for _, tool := range []string{"bd", "gt"} {
		path := filepath.Join(binDir, tool)
		if err := os.WriteFile(path, []byte(script), 0755); err != nil {
			t.Fatalf("write %s stub: %v", tool, err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return func() []string {
		data, err := os.ReadFile(logPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			t.Fatalf("read call log: %v", err)
		}
		var lines []string
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line != "" {
				lines = append(lines, line)
			}
		}
		return lines
	}
}

// callResembling returns the first logged invocation containing every
// fragment, or "".
func callResembling(calls []string, fragments ...string) string {
	for _, call := range calls {
		matched := true
		for _, fragment := range fragments {
			if !strings.Contains(call, fragment) {
				matched = false
				break
			}
		}
		if matched {
			return call
		}
	}
	return ""
}

// callInvoking returns the first logged invocation whose argv starts with
// argvPrefix — the tab the stub writes after the tool name keeps this off a
// phrase another invocation merely mentions. The escalation mail's body says
// "re-sling manually", so a substring search for "sling" would find the mail
// that is the proof no sling happened.
func callInvoking(calls []string, tool, argvPrefix string) string {
	prefix := tool + "\t" + argvPrefix
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			return call
		}
	}
	return ""
}

// editorialRejectionNotes is a source bead's notes for a rejection that came
// from an actual om review, as refinery.formatMergeRejectionNote writes them
// (om-gate T10): the MERGE REJECTION block an editorial resubmit leaves, with
// the machine-readable receipt `gt deacon redispatch` reads back.
func editorialRejectionNotes(score float64, unresolved string) string {
	notes := "MERGE REJECTION (attempt 2): editorial - review found 1 major\n" +
		"Branch: polecat/garnet/gt-thing\n" +
		"Target: main\n" +
		"MR: gt-mr-abc\n" +
		"- id:abc123def456 sev:major internal/deacon/redispatch.go:1 — a finding"
	notes += fmt.Sprintf("\nScore: %.4f", score)
	if unresolved != "" {
		notes += "\nUnresolved: " + unresolved
	}
	return notes
}

// TestRedispatchRecoveredBead_HeldBeadIsSkipped covers the routing rules
// gt-tq6l centralised in convoy, as the deacon sees them: whatever reason the
// bead's own record gives, the RECOVERED_BEAD handler skips the bead and
// touches nothing. A held bead is not slung, not escalated about, and does
// not accumulate a re-dispatch attempt against a decision a human already
// made.
func TestRedispatchRecoveredBead_HeldBeadIsSkipped(t *testing.T) {
	holds := []string{
		"status deferred",
		"status pinned",
		"label needs-sonnet",
		"label needs-mayor-review",
		"MAYOR DESIGN DECISION in notes",
		"do not redispatch in comment",
		"record unreadable (connection refused)",
		"no record for gt-held",
	}

	for _, hold := range holds {
		t.Run(hold, func(t *testing.T) {
			calls := stubDispatchTools(t)
			townRoot := t.TempDir()

			// An editorial rejection whose resubmit is not converging: the
			// editorial route would stop and escalate on these notes. The
			// hold is answered first, so not even that runs.
			state := &RedispatchState{Beads: map[string]*BeadRedispatchState{
				"gt-held": {
					BeadID:       "gt-held",
					AttemptCount: 1,
					LastReceipt:  &ReceiptSummary{Score: 0.6},
				},
			}}
			if err := SaveRedispatchState(townRoot, state); err != nil {
				t.Fatalf("SaveRedispatchState: %v", err)
			}

			rec := RecoveredBeadRecord{
				Notes: editorialRejectionNotes(0.6, "abc123def456"),
				Hold:  hold,
			}
			result := RedispatchRecoveredBead(rec, townRoot, "gt-held", "gastown", 0, 0)

			if result.Action != "skipped" {
				t.Fatalf("Action = %q, want %q (message: %s)", result.Action, "skipped", result.Message)
			}
			if !strings.Contains(result.Message, hold) {
				t.Errorf("Message = %q, want it to name the hold %q", result.Message, hold)
			}
			if got := calls(); len(got) != 0 {
				t.Errorf("a held bead was acted on: %v", got)
			}

			after, err := LoadRedispatchState(townRoot)
			if err != nil {
				t.Fatalf("LoadRedispatchState: %v", err)
			}
			before := state.Beads["gt-held"]
			if got := after.Beads["gt-held"]; got.AttemptCount != before.AttemptCount {
				t.Errorf("AttemptCount = %d, want %d — a held bead recorded an attempt",
					got.AttemptCount, before.AttemptCount)
			}
		})
	}
}

// TestRedispatchRecoveredBead_EditorialRejectionRoutesThroughConvergence is
// the wiring gt-wuqn is about: an editorial rejection reaches
// RedispatchEditorial's convergence gate, so a resubmit that keeps failing
// the same finding stops and is labeled needs_human instead of being
// re-slung on attempt count alone.
func TestRedispatchRecoveredBead_EditorialRejectionRoutesThroughConvergence(t *testing.T) {
	calls := stubDispatchTools(t)
	townRoot := t.TempDir()

	// One resubmit in, whose prior attempt scored 0.6.
	state := &RedispatchState{Beads: map[string]*BeadRedispatchState{
		"gt-editalpha": {
			BeadID:       "gt-editalpha",
			AttemptCount: 1,
			LastReceipt:  &ReceiptSummary{Score: 0.6},
		},
	}}
	if err := SaveRedispatchState(townRoot, state); err != nil {
		t.Fatalf("SaveRedispatchState: %v", err)
	}

	rec := RecoveredBeadRecord{Notes: editorialRejectionNotes(0.6, "abc123def456")}
	result := RedispatchRecoveredBead(rec, townRoot, "gt-editalpha", "gastown", 0, 0)

	if result.Action != "escalated" {
		t.Fatalf("Action = %q, want %q (message: %s)", result.Action, "escalated", result.Message)
	}
	wantReason := "not converging: unresolved abc123def456 — escalating"
	if !strings.Contains(result.Message, wantReason) {
		t.Errorf("Message = %q, want it to carry %q", result.Message, wantReason)
	}

	logged := calls()
	if call := callInvoking(logged, "gt", "sling "); call != "" {
		t.Errorf("a non-converging editorial resubmit was re-slung: %s", call)
	}
	if call := callResembling(logged, "mail send mayor/", "needs_human"); call == "" {
		t.Errorf("no needs_human escalation to the mayor; calls: %v", logged)
	}
	if call := callResembling(logged, "update gt-editalpha", "--add-label needs_human"); call == "" {
		t.Errorf("the bead was not labeled needs_human; calls: %v", logged)
	}
}

// TestRedispatchRecoveredBead_NonEditorialRejectionTakesPlainPath is the
// other half of the fork: a rejection with no om verdict behind it — a
// build/test failure, a manual `gt mq reject` — leaves no Score: line, so it
// takes the plain attempt-count path and is re-slung.
func TestRedispatchRecoveredBead_NonEditorialRejectionTakesPlainPath(t *testing.T) {
	calls := stubDispatchTools(t)
	townRoot := t.TempDir()

	// The same state as the editorial case, so the only thing that differs
	// is the receipt on the notes.
	state := &RedispatchState{Beads: map[string]*BeadRedispatchState{
		"gt-buildfail": {
			BeadID:       "gt-buildfail",
			AttemptCount: 1,
			LastReceipt:  &ReceiptSummary{Score: 0.6},
		},
	}}
	if err := SaveRedispatchState(townRoot, state); err != nil {
		t.Fatalf("SaveRedispatchState: %v", err)
	}

	rec := RecoveredBeadRecord{
		Notes: "MERGE REJECTION (attempt 2): build - go build failed\nBranch: polecat/garnet/gt-thing\nTarget: main\nMR: gt-mr-abc",
	}
	result := RedispatchRecoveredBead(rec, townRoot, "gt-buildfail", "gastown", 0, 0)

	if result.Action != "redispatched" {
		t.Fatalf("Action = %q, want %q (message: %s)", result.Action, "redispatched", result.Message)
	}

	logged := calls()
	if call := callInvoking(logged, "gt", "sling gt-buildfail gastown"); call == "" {
		t.Errorf("a build-failure rejection was not re-slung; calls: %v", logged)
	}
	if call := callResembling(logged, "needs_human"); call != "" {
		t.Errorf("a non-editorial rejection was handled as editorial: %s", call)
	}
}

// holdTown returns a temp town root carrying the operator's town-wide
// dispatch hold (seat-refill.hold) — never the real town's.
func holdTown(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, "seat-refill.hold"), nil, 0644); err != nil {
		t.Fatalf("write hold: %v", err)
	}
	return townRoot
}

// TestRedispatchRecoveredBead_OperatorHoldRefusesSling is gt-ifijm: the
// operator's town-wide hold stops the deacon's --force re-sling. gt-elvf4 was
// force-slung during a hold because only the bead's own record was consulted.
// Both routes — the plain attempt count and the editorial convergence gate —
// must refuse, leave the retry budget untouched, and say why.
func TestRedispatchRecoveredBead_OperatorHoldRefusesSling(t *testing.T) {
	cases := map[string]string{
		"plain":     "MERGE REJECTION (attempt 1): build - go build failed",
		"editorial": editorialRejectionNotes(0.9, ""),
	}
	for name, notes := range cases {
		t.Run(name, func(t *testing.T) {
			calls := stubDispatchTools(t)
			townRoot := holdTown(t)

			state := &RedispatchState{Beads: map[string]*BeadRedispatchState{
				"gt-onhold": {BeadID: "gt-onhold", AttemptCount: 1, LastReceipt: &ReceiptSummary{Score: 0.6}},
			}}
			if err := SaveRedispatchState(townRoot, state); err != nil {
				t.Fatalf("SaveRedispatchState: %v", err)
			}

			result := RedispatchRecoveredBead(RecoveredBeadRecord{Notes: notes}, townRoot, "gt-onhold", "gastown", 0, 0)

			if result.Action != "deferred" {
				t.Fatalf("Action = %q, want %q (message: %s)", result.Action, "deferred", result.Message)
			}
			if !strings.Contains(result.Message, "seat-refill.hold") {
				t.Errorf("Message = %q, want it to name the hold file", result.Message)
			}
			if call := callInvoking(calls(), "gt", "sling "); call != "" {
				t.Errorf("re-slung during an operator hold: %s", call)
			}

			after, err := LoadRedispatchState(townRoot)
			if err != nil {
				t.Fatalf("LoadRedispatchState: %v", err)
			}
			got := after.Beads["gt-onhold"]
			if got.AttemptCount != 1 || !got.LastAttemptTime.IsZero() {
				t.Errorf("hold recorded an attempt: count=%d last=%v", got.AttemptCount, got.LastAttemptTime)
			}
		})
	}
}

// TestRedispatchRecoveredBead_NoOperatorHoldStillSlings guards the other
// side: the hold check must not turn every redispatch into a deferral.
func TestRedispatchRecoveredBead_NoOperatorHoldStillSlings(t *testing.T) {
	calls := stubDispatchTools(t)
	townRoot := t.TempDir()

	result := RedispatchRecoveredBead(RecoveredBeadRecord{}, townRoot, "gt-free", "gastown", 0, 0)
	if result.Action != "redispatched" {
		t.Fatalf("Action = %q, want redispatched (message: %s)", result.Action, result.Message)
	}
	if call := callInvoking(calls(), "gt", "sling gt-free gastown --force"); call == "" {
		t.Errorf("no sling without a hold; calls: %v", calls())
	}
}

// poolFullStderr is what `gt sling --force` prints when every seat the bead
// could take is at its cap (internal/cmd/sling_pool.go poolBackpressureError),
// wrapped the way cobra and the spawn path report it.
const poolFullStderr = "Error: spawning polecat: sling refused: every local seat is taken (2/2); raise polecat_pool.max_local/max_overflow to spawn\n"

// TestRedispatch_PoolFullRefusalIsNotAnAttempt is gt-xdaq: a full pool
// refusing the sling is the town at capacity, not a failed redispatch. It
// must not burn the retry budget, start the cooldown, or march the bead
// toward a false REDISPATCH_FAILED escalation.
func TestRedispatch_PoolFullRefusalIsNotAnAttempt(t *testing.T) {
	calls := stubDispatchToolsSlinging(t, poolFullStderr, 1)
	townRoot := t.TempDir()

	// One attempt short of the cap: counting this refusal would escalate
	// the bead on the next RECOVERED_BEAD.
	state := &RedispatchState{Beads: map[string]*BeadRedispatchState{
		"gt-queued": {BeadID: "gt-queued", AttemptCount: 2},
	}}
	if err := SaveRedispatchState(townRoot, state); err != nil {
		t.Fatalf("SaveRedispatchState: %v", err)
	}

	result := RedispatchRecoveredBead(RecoveredBeadRecord{}, townRoot, "gt-queued", "gastown", 3, 0)

	if result.Action != "deferred" {
		t.Fatalf("Action = %q, want %q (message: %s, err: %v)", result.Action, "deferred", result.Message, result.Error)
	}
	if !strings.Contains(result.Message, "sling refused:") {
		t.Errorf("Message = %q, want it to carry the pool's refusal", result.Message)
	}
	if call := callInvoking(calls(), "gt", "sling gt-queued gastown"); call == "" {
		t.Errorf("the sling was never attempted; calls: %v", calls())
	}

	after, err := LoadRedispatchState(townRoot)
	if err != nil {
		t.Fatalf("LoadRedispatchState: %v", err)
	}
	got := after.Beads["gt-queued"]
	if got.AttemptCount != 2 {
		t.Errorf("AttemptCount = %d, want 2 — a capacity refusal was counted", got.AttemptCount)
	}
	if !got.LastAttemptTime.IsZero() {
		t.Errorf("LastAttemptTime = %v — a capacity refusal started the cooldown", got.LastAttemptTime)
	}

	// The next RECOVERED_BEAD retries rather than escalating.
	again := RedispatchRecoveredBead(RecoveredBeadRecord{}, townRoot, "gt-queued", "gastown", 3, 0)
	if again.Action == "escalated" {
		t.Errorf("a bead queued behind a full pool was escalated: %s", again.Message)
	}
}

// TestRedispatch_PersistentPoolFullEscalatesAfterMaxDeferrals is gt-oo494:
// gt-qhhlr deferred behind a full pool for 3 straight patrol cycles and
// never escalated, because a deferral never advances AttemptCount and so
// never reaches the --max-attempts check. The consecutive-deferral counter
// must catch what the attempt counter structurally cannot.
func TestRedispatch_PersistentPoolFullEscalatesAfterMaxDeferrals(t *testing.T) {
	calls := stubDispatchToolsSlinging(t, poolFullStderr, 1)
	townRoot := t.TempDir()

	var last *RedispatchResult
	for i := 0; i < DefaultMaxDeferrals; i++ {
		last = RedispatchRecoveredBead(RecoveredBeadRecord{}, townRoot, "gt-stuck", "gastown", 3, 0)
		if i < DefaultMaxDeferrals-1 && last.Action != "deferred" {
			t.Fatalf("cycle %d: Action = %q, want %q (message: %s)", i+1, last.Action, "deferred", last.Message)
		}
	}

	if last.Action != "escalated" {
		t.Fatalf("final cycle: Action = %q, want %q (message: %s, err: %v)", last.Action, "escalated", last.Message, last.Error)
	}
	if !strings.Contains(last.Message, "consecutive deferrals") {
		t.Errorf("Message = %q, want it to name the consecutive deferrals", last.Message)
	}

	logged := calls()
	if call := callResembling(logged, "mail send mayor/", "consecutive deferrals"); call == "" {
		t.Errorf("no consecutive-deferral escalation mail to the mayor; calls: %v", logged)
	}

	after, err := LoadRedispatchState(townRoot)
	if err != nil {
		t.Fatalf("LoadRedispatchState: %v", err)
	}
	got := after.Beads["gt-stuck"]
	if !got.Escalated {
		t.Error("bead was not marked escalated after max consecutive deferrals")
	}
	if got.AttemptCount != 0 {
		t.Errorf("AttemptCount = %d, want 0 — deferrals must never count as attempts", got.AttemptCount)
	}

	// Further redispatch calls stop retrying rather than deferring forever.
	again := RedispatchRecoveredBead(RecoveredBeadRecord{}, townRoot, "gt-stuck", "gastown", 3, 0)
	if again.Action != "already-escalated" {
		t.Errorf("Action = %q, want %q once escalated", again.Action, "already-escalated")
	}
}

// TestRedispatch_DeferralStreakResetsOnRealAttempt checks the counter tracks
// a consecutive streak, not a lifetime total: once a real attempt succeeds
// (or fails), a fresh run of deferrals must not inherit the old count.
func TestRedispatch_DeferralStreakResetsOnRealAttempt(t *testing.T) {
	townRoot := t.TempDir()
	state := &RedispatchState{Beads: map[string]*BeadRedispatchState{
		"gt-mixed": {BeadID: "gt-mixed", DeferralCount: DefaultMaxDeferrals - 1, LastDeferralReason: "stale"},
	}}
	if err := SaveRedispatchState(townRoot, state); err != nil {
		t.Fatalf("SaveRedispatchState: %v", err)
	}

	// A successful sling breaks the streak.
	stubDispatchToolsSlinging(t, "", 0)
	result := RedispatchRecoveredBead(RecoveredBeadRecord{}, townRoot, "gt-mixed", "gastown", 3, 0)
	if result.Action != "redispatched" {
		t.Fatalf("Action = %q, want %q (message: %s)", result.Action, "redispatched", result.Message)
	}

	after, err := LoadRedispatchState(townRoot)
	if err != nil {
		t.Fatalf("LoadRedispatchState: %v", err)
	}
	if got := after.Beads["gt-mixed"].DeferralCount; got != 0 {
		t.Errorf("DeferralCount = %d, want 0 after a real dispatch attempt", got)
	}
}

// TestRedispatch_GenuineSlingFailureStillCounts keeps the budget honest: a
// sling that broke for any other reason is still an attempt.
func TestRedispatch_GenuineSlingFailureStillCounts(t *testing.T) {
	stubDispatchToolsSlinging(t, "Error: bead gt-broken: worktree add failed\n", 1)
	townRoot := t.TempDir()

	result := RedispatchRecoveredBead(RecoveredBeadRecord{}, townRoot, "gt-broken", "gastown", 3, 0)
	if result.Action != "error" {
		t.Fatalf("Action = %q, want error (message: %s)", result.Action, result.Message)
	}
	after, err := LoadRedispatchState(townRoot)
	if err != nil {
		t.Fatalf("LoadRedispatchState: %v", err)
	}
	if got := after.Beads["gt-broken"].AttemptCount; got != 1 {
		t.Errorf("AttemptCount = %d, want 1 — a real failure must count", got)
	}
}
