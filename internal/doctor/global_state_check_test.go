package doctor

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/state"
)

func TestGlobalStateCheck_Run(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		load        func() (*state.State, error)
		wantStatus  CheckStatus
		wantMessage string
		wantFixHint string
		wantDetails []string
	}{
		{
			name:        "missing state is a warning that does not name the retired --shell flag",
			load:        func() (*state.State, error) { return nil, os.ErrNotExist },
			wantStatus:  StatusWarning,
			wantMessage: "Global state not initialized",
			wantFixHint: "Run from the town root: gt install . --force",
		},
		{
			// Exact details: a check that still inspected the startup files
			// would report a line beyond these three.
			name: "enabled state reports only the state facts",
			load: func() (*state.State, error) {
				return &state.State{Enabled: true, Version: "1.2.3", MachineID: "abc12345"}, nil
			},
			wantStatus:  StatusOK,
			wantMessage: "Global state healthy",
			wantDetails: []string{"Gas Town: enabled", "Version: 1.2.3", "Machine ID: abc12345"},
		},
		{
			name: "disabled state is a warning",
			load: func() (*state.State, error) {
				return &state.State{Enabled: false, MachineID: "abc12345"}, nil
			},
			wantStatus:  StatusWarning,
			wantMessage: "Gas Town is disabled globally",
			wantFixHint: "Run from the town root: gt install . --force",
			wantDetails: []string{"Gas Town: disabled", "Machine ID: abc12345"},
		},
		{
			name:        "unreadable state is an error, never healthy",
			load:        func() (*state.State, error) { return nil, errors.New("permission denied") },
			wantStatus:  StatusError,
			wantMessage: "Cannot read global state",
			wantDetails: []string{"permission denied"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			check := NewGlobalStateCheck()
			check.load = tt.load

			got := check.Run(&CheckContext{})

			if got.Status != tt.wantStatus {
				t.Errorf("Status = %v, want %v (message %q)", got.Status, tt.wantStatus, got.Message)
			}
			if got.Message != tt.wantMessage {
				t.Errorf("Message = %q, want %q", got.Message, tt.wantMessage)
			}
			if got.FixHint != tt.wantFixHint {
				t.Errorf("FixHint = %q, want %q", got.FixHint, tt.wantFixHint)
			}
			if !reflect.DeepEqual(got.Details, tt.wantDetails) {
				t.Errorf("Details = %v, want %v", got.Details, tt.wantDetails)
			}
		})
	}
}
