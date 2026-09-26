package cmd

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/rig"
)

// P0 is the only priority excluded from batching. P1 used to be excluded too,
// which meant batching never engaged on a town that files nearly all of its
// work at P1 (gt-92ms).
func TestFilterAndSortBatchCandidates_ExcludesOnlyP0(t *testing.T) {
	t.Parallel()
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	mrs := []*refinery.MRInfo{
		{ID: "p0", Priority: 0, CreatedAt: old},
		{ID: "p1", Priority: 1, CreatedAt: old},
		{ID: "p2", Priority: 2, CreatedAt: old},
	}

	got := filterAndSortBatchCandidates(mrs, time.Hour, now)

	ids := idsOf(got)
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (only p0 excluded); got %v", len(got), ids)
	}
	if ids[0] == "p0" || ids[1] == "p0" {
		t.Errorf("got %v, want p0 absent — P0 is always single-MR, never batched", ids)
	}
	for _, mr := range got {
		if mr.Priority == 0 {
			t.Errorf("candidate %s has priority 0, P0 must never be batched", mr.ID)
		}
	}
	present := map[string]bool{}
	for _, id := range ids {
		present[id] = true
	}
	if !present["p1"] || !present["p2"] {
		t.Errorf("got %v, want both p1 and p2 present", ids)
	}
}

func TestFilterAndSortBatchCandidates_ExcludesYoungerThanMinAge(t *testing.T) {
	t.Parallel()
	now := time.Now()
	mrs := []*refinery.MRInfo{
		{ID: "old-enough", Priority: 2, CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "too-young", Priority: 2, CreatedAt: now.Add(-10 * time.Minute)},
		{ID: "zero-created-at", Priority: 2, CreatedAt: time.Time{}},
	}

	got := filterAndSortBatchCandidates(mrs, time.Hour, now)

	if len(got) != 1 || got[0].ID != "old-enough" {
		t.Fatalf("got %v, want only [old-enough]", idsOf(got))
	}
}

func TestFilterAndSortBatchCandidates_SortsByScoreDescending(t *testing.T) {
	t.Parallel()
	now := time.Now()
	mrs := []*refinery.MRInfo{
		{ID: "p4-old", Priority: 4, CreatedAt: now.Add(-48 * time.Hour)},
		{ID: "p2-old", Priority: 2, CreatedAt: now.Add(-48 * time.Hour)},
		{ID: "p2-newer", Priority: 2, CreatedAt: now.Add(-2 * time.Hour)},
	}

	got := filterAndSortBatchCandidates(mrs, time.Hour, now)

	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3; got %v", len(got), idsOf(got))
	}
	// Higher priority (lower number) and older MRs score higher.
	if got[0].ID != "p2-old" {
		t.Errorf("got[0] = %s, want p2-old (highest priority + oldest)", got[0].ID)
	}
	if got[len(got)-1].ID != "p4-old" {
		t.Errorf("got[last] = %s, want p4-old (lowest priority)", got[len(got)-1].ID)
	}
}

// gt-gz8l: a batch bisection leaves its culprit queued as 'ready', and the
// batch runs before the single-MR path on every cycle, so without this the
// same MR is stacked again and the gate, the flaky retry, and the bisection
// re-derive the same answer while the batch's good MRs wait.
func TestPartitionBatchCandidates_HoldsBackMarkedCulprits(t *testing.T) {
	t.Parallel()
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	head := "abc123"
	mrs := []*refinery.MRInfo{
		{ID: "marked-at-head", Priority: 2, CreatedAt: old, CommitSHA: head, Labels: []string{"batch-culprit:" + head}},
		{ID: "marked-other-head", Priority: 2, CreatedAt: old, CommitSHA: "def456", Labels: []string{"batch-culprit:" + head}},
		{ID: "unmarked", Priority: 2, CreatedAt: old, CommitSHA: "def456", Labels: []string{"gt:merge-request"}},
	}

	eligible, marked := partitionBatchCandidates(mrs, time.Hour, now)

	if got := idsOf(eligible); len(got) != 2 || got[0] != "marked-other-head" || got[1] != "unmarked" {
		t.Errorf("eligible = %v, want [marked-other-head unmarked] — only the MR whose head still carries the mark is held back", got)
	}
	if got := idsOf(marked); len(got) != 1 || got[0] != "marked-at-head" {
		t.Errorf("marked = %v, want [marked-at-head]", got)
	}
}

// filterAndSortBatchCandidates is the entry point the callers use, so the
// exclusion has to hold there too.
func TestFilterAndSortBatchCandidates_ExcludesMarkedCulprit(t *testing.T) {
	t.Parallel()
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	mrs := []*refinery.MRInfo{
		{ID: "culprit", Priority: 2, CreatedAt: old, CommitSHA: "abc123", Labels: []string{"batch-culprit:abc123"}},
		{ID: "clean", Priority: 2, CreatedAt: old, CommitSHA: "def456"},
	}

	got := filterAndSortBatchCandidates(mrs, time.Hour, now)

	if len(got) != 1 || got[0].ID != "clean" {
		t.Fatalf("got %v, want only [clean]", idsOf(got))
	}
}

// A mark on an MR too young to batch is not why it was excluded, so it must
// not be reported as held back — the count is what tells an operator why a
// batch declined to assemble.
func TestPartitionBatchCandidates_MarkOnTooYoungMRIsNotReported(t *testing.T) {
	t.Parallel()
	now := time.Now()
	mrs := []*refinery.MRInfo{
		{ID: "young-culprit", Priority: 2, CreatedAt: now.Add(-time.Minute), CommitSHA: "abc123", Labels: []string{"batch-culprit:abc123"}},
	}

	eligible, marked := partitionBatchCandidates(mrs, time.Hour, now)

	if len(eligible) != 0 || len(marked) != 0 {
		t.Errorf("eligible = %v, marked = %v, want both empty", idsOf(eligible), idsOf(marked))
	}
}

func idsOf(mrs []*refinery.MRInfo) []string {
	ids := make([]string, len(mrs))
	for i, mr := range mrs {
		ids[i] = mr.ID
	}
	return ids
}

func TestBuildBatchGateSteps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		mq   *config.MergeQueueConfig
		want []refinery.GateStep
	}{
		{name: "nil config", mq: nil, want: nil},
		{name: "no commands configured", mq: &config.MergeQueueConfig{}, want: nil},
		{
			name: "orders configured steps setup/typecheck/lint/build/test",
			mq: &config.MergeQueueConfig{
				TestCommand:      "make test",
				SetupCommand:     "make setup",
				BuildCommand:     "make build",
				LintCommand:      "make lint",
				TypecheckCommand: "make typecheck",
			},
			want: []refinery.GateStep{
				{Name: "setup", Cmd: "make setup"},
				{Name: "typecheck", Cmd: "make typecheck"},
				{Name: "lint", Cmd: "make lint"},
				{Name: "build", Cmd: "make build"},
				{Name: "test", Cmd: "make test"},
			},
		},
		{
			name: "skips unset steps but preserves order",
			mq: &config.MergeQueueConfig{
				LintCommand: "make lint",
				TestCommand: "make test",
			},
			want: []refinery.GateStep{
				{Name: "lint", Cmd: "make lint"},
				{Name: "test", Cmd: "make test"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildBatchGateSteps(tt.mq)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("buildBatchGateSteps() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestApplyBatchEditorialConfig is gt-ww20's acceptance case: refinery.NewEngineer
// never loads config.json (Engineer.LoadConfig has no production caller), so
// runMQBatchRun's Engineer used to leave Editorial nil regardless of the
// rig's config — reviewBatchCandidates and ejectPatchIDChanged both no-op on
// a nil Editorial, so 'gt mq batch run' landed MRs with no om review at all
// on a rig with editorial.required=true (a silent bypass, not a refusal).
// applyBatchEditorialConfig is the wiring that closes that gap; this guards
// it directly rather than through the full cobra command, which needs a
// live rig/workspace to resolve.
func TestApplyBatchEditorialConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		mq         *config.MergeQueueConfig
		wantNil    bool
		wantResult config.EditorialConfig
	}{
		{name: "nil merge queue config", mq: nil, wantNil: true},
		{name: "no editorial section configured", mq: &config.MergeQueueConfig{}, wantNil: true},
		{
			name: "editorial.required=true is wired through with defaults applied",
			mq: &config.MergeQueueConfig{
				Editorial: &config.EditorialConfig{Required: true},
			},
			wantResult: config.EditorialConfig{
				Required:          true,
				Command:           "scripts/om-gate.sh",
				MaxAttempts:       5,
				ReviewParallelism: 3,
			},
		},
		{
			name: "editorial.required=false is still wired through (not a no-op)",
			mq: &config.MergeQueueConfig{
				Editorial: &config.EditorialConfig{Required: false},
			},
			wantResult: config.EditorialConfig{
				Required:          false,
				Command:           "scripts/om-gate.sh",
				MaxAttempts:       5,
				ReviewParallelism: 3,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eng := refinery.NewEngineer(&rig.Rig{Name: "test-rig", Path: t.TempDir()})
			if got := eng.Config().Editorial; got != nil {
				t.Fatalf("precondition: fresh Engineer already has Editorial set: %+v", got)
			}

			applyBatchEditorialConfig(eng, tt.mq)

			got := eng.Config().Editorial
			if tt.wantNil {
				if got != nil {
					t.Fatalf("Config().Editorial = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("Config().Editorial = nil, want %+v", tt.wantResult)
			}
			if *got != tt.wantResult {
				t.Errorf("Config().Editorial = %+v, want %+v", *got, tt.wantResult)
			}
		})
	}
}

func TestResolveBatchMinAge(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		flag    string
		mq      *config.MergeQueueConfig
		want    time.Duration
		wantErr bool
	}{
		{name: "flag wins", flag: "30m", mq: &config.MergeQueueConfig{BatchMinAge: "2h"}, want: 30 * time.Minute},
		{name: "falls back to rig config", flag: "", mq: &config.MergeQueueConfig{BatchMinAge: "2h"}, want: 2 * time.Hour},
		{name: "falls back to 1h with no rig config", flag: "", mq: nil, want: time.Hour},
		{name: "invalid flag duration errors", flag: "not-a-duration", mq: nil, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveBatchMinAge(tt.flag, tt.mq)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (result %v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveBatchMinAge() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveBatchMax(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		flag int
		mq   *config.MergeQueueConfig
		want int
	}{
		{name: "flag wins", flag: 5, mq: &config.MergeQueueConfig{BatchMax: 20}, want: 5},
		{name: "falls back to rig config", flag: 0, mq: &config.MergeQueueConfig{BatchMax: 20}, want: 20},
		{name: "falls back to 12 with no rig config", flag: 0, mq: nil, want: 12},
		{name: "negative flag treated as unset", flag: -1, mq: &config.MergeQueueConfig{BatchMax: 20}, want: 20},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveBatchMax(tt.flag, tt.mq)
			if got != tt.want {
				t.Errorf("resolveBatchMax() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestResolveBatchMinCount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		flag int
		mq   *config.MergeQueueConfig
		want int
	}{
		{name: "flag wins", flag: 2, mq: &config.MergeQueueConfig{BatchMinCount: 6}, want: 2},
		{name: "falls back to rig config", flag: 0, mq: &config.MergeQueueConfig{BatchMinCount: 6}, want: 6},
		{name: "falls back to 4 with no rig config", flag: 0, mq: nil, want: 4},
		{name: "negative flag treated as unset", flag: -1, mq: &config.MergeQueueConfig{BatchMinCount: 6}, want: 6},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveBatchMinCount(tt.flag, tt.mq)
			if got != tt.want {
				t.Errorf("resolveBatchMinCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

// belowBatchMinCount is the guard runMQBatchRun calls after assembling
// candidates and before it acquires the gate slot, so a short candidate list
// never reaches AssembleBatch/ProcessBatch. Driving it directly is what keeps
// "no ProcessBatch call" true by construction: the call site sits behind it.
func TestBelowBatchMinCount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		candidates int
		minCount   int
		wantSkip   bool
	}{
		{name: "below threshold skips", candidates: 2, minCount: 3, wantSkip: true},
		{name: "exactly at threshold batches", candidates: 3, minCount: 3, wantSkip: false},
		{name: "above threshold batches", candidates: 9, minCount: 3, wantSkip: false},
		{name: "empty candidate list skips", candidates: 0, minCount: 1, wantSkip: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			got := belowBatchMinCount(&buf, tt.candidates, tt.minCount)
			if got != tt.wantSkip {
				t.Errorf("belowBatchMinCount(%d, %d) = %v, want %v", tt.candidates, tt.minCount, got, tt.wantSkip)
			}
			out := buf.String()
			if !tt.wantSkip {
				if out != "" {
					t.Errorf("above threshold wrote %q, want no output", out)
				}
				return
			}
			want := fmt.Sprintf("%d eligible < min_count %d, not batching", tt.candidates, tt.minCount)
			if !strings.Contains(out, want) {
				t.Errorf("output %q does not contain %q", out, want)
			}
		})
	}
}

func TestNewBatchConfig_KeepsDefaultsOtherThanMaxBatchSize(t *testing.T) {
	t.Parallel()
	def := refinery.DefaultBatchConfig()
	got := newBatchConfig(7)

	if got.MaxBatchSize != 7 {
		t.Errorf("MaxBatchSize = %d, want 7", got.MaxBatchSize)
	}
	if got.RetryBatchOnFlaky != def.RetryBatchOnFlaky {
		t.Errorf("RetryBatchOnFlaky = %v, want default %v (bare struct literal would silently zero this)", got.RetryBatchOnFlaky, def.RetryBatchOnFlaky)
	}
	if got.BatchWaitTime != def.BatchWaitTime {
		t.Errorf("BatchWaitTime = %v, want default %v", got.BatchWaitTime, def.BatchWaitTime)
	}
}
