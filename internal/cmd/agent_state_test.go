package cmd

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

func TestParseStateLabels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		labels   []string
		wantKeys []string
	}{
		{
			name:     "empty labels",
			labels:   []string{},
			wantKeys: []string{},
		},
		{
			name:     "only non-state labels",
			labels:   []string{"role_type", "urgent"},
			wantKeys: []string{},
		},
		{
			name:     "only state labels",
			labels:   []string{"idle:3", "backoff:2m"},
			wantKeys: []string{"idle", "backoff"},
		},
		{
			name:     "mixed labels",
			labels:   []string{"role_type", "idle:5", "urgent", "backoff:30s"},
			wantKeys: []string{"idle", "backoff"},
		},
		{
			name:     "label with multiple colons",
			labels:   []string{"last_activity:2025-01-01T12:00:00Z"},
			wantKeys: []string{"last_activity"},
		},
		{
			name:     "heartbeat is state like any other",
			labels:   []string{"gt:agent", "heartbeat:1758000000"},
			wantKeys: []string{"gt", "heartbeat"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := parseStateLabels(tt.labels)
			if len(labels) != len(tt.wantKeys) {
				t.Errorf("got %d labels, want %d", len(labels), len(tt.wantKeys))
				return
			}
			for _, key := range tt.wantKeys {
				if _, ok := labels[key]; !ok {
					t.Errorf("missing expected key: %s", key)
				}
			}
		})
	}
}

func TestApplyLabelOperations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		initial   map[string]string
		setOps    []string
		incrKey   string
		delKeys   []string
		wantKeys  map[string]string
		wantError bool
	}{
		{
			name:     "set new label",
			initial:  map[string]string{},
			setOps:   []string{"idle=0"},
			wantKeys: map[string]string{"idle": "0"},
		},
		{
			name:     "set overwrites existing",
			initial:  map[string]string{"idle": "5"},
			setOps:   []string{"idle=0"},
			wantKeys: map[string]string{"idle": "0"},
		},
		{
			name:     "increment missing key creates with 1",
			initial:  map[string]string{},
			incrKey:  "idle",
			wantKeys: map[string]string{"idle": "1"},
		},
		{
			name:     "increment existing key",
			initial:  map[string]string{"idle": "3"},
			incrKey:  "idle",
			wantKeys: map[string]string{"idle": "4"},
		},
		{
			name:     "delete existing key",
			initial:  map[string]string{"idle": "3", "backoff": "2m"},
			delKeys:  []string{"idle"},
			wantKeys: map[string]string{"backoff": "2m"},
		},
		{
			name:     "delete non-existent key is noop",
			initial:  map[string]string{"idle": "3"},
			delKeys:  []string{"nonexistent"},
			wantKeys: map[string]string{"idle": "3"},
		},
		{
			name:      "invalid set format",
			initial:   map[string]string{},
			setOps:    []string{"invalid"},
			wantError: true,
		},
		{
			name:     "increment non-numeric value restarts at 1",
			initial:  map[string]string{"idle": "2m"},
			incrKey:  "idle",
			wantKeys: map[string]string{"idle": "1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := maps.Clone(tt.initial)
			err := applyLabelOperations(labels, tt.setOps, tt.incrKey, tt.delKeys)

			if tt.wantError {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if len(labels) != len(tt.wantKeys) {
				t.Errorf("got %d labels, want %d", len(labels), len(tt.wantKeys))
				return
			}

			for key, wantVal := range tt.wantKeys {
				if gotVal, ok := labels[key]; !ok {
					t.Errorf("missing expected key: %s", key)
				} else if gotVal != wantVal {
					t.Errorf("labels[%s] = %s, want %s", key, gotVal, wantVal)
				}
			}
		})
	}
}

// The agent-bead label helpers read and write through beads.Client: a
// missing bead reads as "agent bead not found", and replaceAgentLabel swaps
// only the labels under its prefix, writing nothing when nothing changes.
func TestAgentLabelHelpers(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(beads.Issue{ID: "hq-deacon", Labels: []string{"gt:agent", "idle:3", "backoff-until:100"}})

	if _, err := getAllAgentLabels(db, "hq-nobody"); err == nil || !strings.Contains(err.Error(), "agent bead not found: hq-nobody") {
		t.Fatalf("missing bead err = %v, want agent bead not found", err)
	}
	if err := setAgentIdleCycles(db, "hq-deacon", 0); err != nil {
		t.Fatal(err)
	}
	if err := clearAgentBackoffUntil(db, "hq-deacon"); err != nil {
		t.Fatal(err)
	}
	got, err := getAllAgentLabels(db, "hq-deacon")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"gt:agent", "idle:0"}; !slices.Equal(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}

	before, _ := db.Show("hq-deacon")
	if err := clearAgentBackoffUntil(db, "hq-deacon"); err != nil {
		t.Fatal(err)
	}
	if after, _ := db.Show("hq-deacon"); after.UpdatedAt != before.UpdatedAt {
		t.Errorf("clearing an absent backoff-until wrote the bead (updated %s -> %s)", before.UpdatedAt, after.UpdatedAt)
	}
}
