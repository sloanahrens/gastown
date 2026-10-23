package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestShouldSkipDrainUntilIdle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		hasPromptDetection bool
		waitErr            error
		want               bool
	}{
		{"prompt aware idle", true, nil, false},
		{"prompt aware busy", true, errors.New("timeout"), true},
		{"no prompt detection busy", false, errors.New("timeout"), false},
		{"no prompt detection idle", false, nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSkipDrainUntilIdle(tt.hasPromptDetection, tt.waitErr); got != tt.want {
				t.Errorf("shouldSkipDrainUntilIdle(%v, %v) = %v, want %v", tt.hasPromptDetection, tt.waitErr, got, tt.want)
			}
		})
	}
}

func TestNudgePollerTransientHasSessionError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		exists    bool
		probeErr  error
		wantAlive bool
	}{
		{"session present", true, nil, true},
		// HasSession maps ErrSessionNotFound and ErrNoServer to (false, nil),
		// so a genuinely absent session reaches the poller as this case.
		{"session absent", false, nil, false},
		{"transient probe error keeps polling", false, errors.New("openshell sandbox exec: exit 1"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			probe := func(string) (bool, error) { return tt.exists, tt.probeErr }
			var stderr bytes.Buffer
			if got := nudgePollerSessionAlive(probe, "gt-test", &stderr); got != tt.wantAlive {
				t.Errorf("nudgePollerSessionAlive() = %v, want %v", got, tt.wantAlive)
			}
			if tt.probeErr != nil && !strings.Contains(stderr.String(), tt.probeErr.Error()) {
				t.Errorf("stderr = %q, want the probe error %q logged", stderr.String(), tt.probeErr)
			}
		})
	}
}
