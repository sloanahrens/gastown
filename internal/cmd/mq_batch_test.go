package cmd

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/refinery"
)

func TestFilterAndSortBatchCandidates_ExcludesP0P1(t *testing.T) {
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	mrs := []*refinery.MRInfo{
		{ID: "p0", Priority: 0, CreatedAt: old},
		{ID: "p1", Priority: 1, CreatedAt: old},
		{ID: "p2", Priority: 2, CreatedAt: old},
		{ID: "p3", Priority: 3, CreatedAt: old},
	}

	got := filterAndSortBatchCandidates(mrs, time.Hour, now)

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (p0/p1 excluded); got %v", len(got), idsOf(got))
	}
	for _, mr := range got {
		if mr.Priority <= 1 {
			t.Errorf("candidate %s has priority %d, P0/P1 must never be batched", mr.ID, mr.Priority)
		}
	}
}

func TestFilterAndSortBatchCandidates_ExcludesYoungerThanMinAge(t *testing.T) {
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

func idsOf(mrs []*refinery.MRInfo) []string {
	ids := make([]string, len(mrs))
	for i, mr := range mrs {
		ids[i] = mr.ID
	}
	return ids
}

func TestBuildBatchGateCommand(t *testing.T) {
	tests := []struct {
		name string
		mq   *config.MergeQueueConfig
		want string
	}{
		{name: "nil config", mq: nil, want: ""},
		{name: "no commands configured", mq: &config.MergeQueueConfig{}, want: ""},
		{
			name: "chains configured steps in setup/typecheck/lint/build/test order",
			mq: &config.MergeQueueConfig{
				TestCommand:      "make test",
				SetupCommand:     "make setup",
				BuildCommand:     "make build",
				LintCommand:      "make lint",
				TypecheckCommand: "make typecheck",
			},
			want: "make setup && make typecheck && make lint && make build && make test",
		},
		{
			name: "skips unset steps but preserves order",
			mq: &config.MergeQueueConfig{
				LintCommand: "make lint",
				TestCommand: "make test",
			},
			want: "make lint && make test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildBatchGateCommand(tt.mq)
			if got != tt.want {
				t.Errorf("buildBatchGateCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveBatchMinAge(t *testing.T) {
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

func TestNewBatchConfig_KeepsDefaultsOtherThanMaxBatchSize(t *testing.T) {
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
