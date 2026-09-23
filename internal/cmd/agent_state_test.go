package cmd

import (
	"maps"
	"strings"
	"testing"
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

func TestParseAgentBeadLabels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		stdout     []byte
		stderr     []byte
		agentBead  string
		wantLabels []string
		wantErr    string
	}{
		{
			name:       "valid response with labels",
			stdout:     []byte(`[{"id":"gt-test","labels":["idle:3","gt:agent"]}]`),
			stderr:     nil,
			agentBead:  "gt-test",
			wantLabels: []string{"idle:3", "gt:agent"},
			wantErr:    "",
		},
		{
			name:       "valid response with no labels",
			stdout:     []byte(`[{"id":"gt-test","labels":[]}]`),
			stderr:     nil,
			agentBead:  "gt-test",
			wantLabels: []string{},
			wantErr:    "",
		},
		{
			name:       "valid response with null labels",
			stdout:     []byte(`[{"id":"gt-test","labels":null}]`),
			stderr:     nil,
			agentBead:  "gt-test",
			wantLabels: nil,
			wantErr:    "",
		},
		{
			name:      "empty stdout with stderr",
			stdout:    []byte{},
			stderr:    []byte("database mismatch: client expects dolt but daemon has different backend"),
			agentBead: "gt-test",
			wantErr:   "database mismatch",
		},
		{
			name:      "empty stdout without stderr",
			stdout:    []byte{},
			stderr:    nil,
			agentBead: "gt-test",
			wantErr:   "agent bead query returned no output: gt-test",
		},
		{
			name:      "empty array response",
			stdout:    []byte(`[]`),
			stderr:    nil,
			agentBead: "gt-test",
			wantErr:   "agent bead not found: gt-test",
		},
		{
			name:      "invalid JSON",
			stdout:    []byte(`{not valid json`),
			stderr:    nil,
			agentBead: "gt-test",
			wantErr:   "parsing agent bead response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels, err := parseAgentBeadLabels(tt.stdout, tt.stderr, tt.agentBead)

			if tt.wantErr != "" {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.wantErr)
					return
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}

			if len(labels) != len(tt.wantLabels) {
				t.Errorf("got %d labels, want %d", len(labels), len(tt.wantLabels))
				return
			}

			for i, label := range labels {
				if label != tt.wantLabels[i] {
					t.Errorf("labels[%d] = %q, want %q", i, label, tt.wantLabels[i])
				}
			}
		})
	}
}
