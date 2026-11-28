package presubmit

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRedBaseGoResult(t *testing.T) {
	t.Parallel()
	out := []byte("captured-output-7f3a")
	tests := []struct {
		name    string
		err     error
		wantOut []byte
		wantErr bool
	}{
		{"a nil error returns the output", nil, out, false},
		{"an exit error with code 1 returns the output", exitStatus(1), out, false},
		{"a wrapped exit error with code 1 returns the output", fmt.Errorf("run failed: %w", exitStatus(1)), out, false},
		{"a killed run returns no output and an error", exitStatus(-1), nil, true},
		{"an error with no exit code returns no output and an error", errors.New("go: executable file not found"), nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := RedBaseGoResult(out, tt.err)
			if !bytes.Equal(got, tt.wantOut) {
				t.Errorf("RedBaseGoResult output = %q, want %q", got, tt.wantOut)
			}
			if tt.wantErr {
				if err == nil || !errors.Is(err, tt.err) {
					t.Fatalf("RedBaseGoResult error = %v, want an error wrapping %v", err, tt.err)
				}
				if strings.Contains(err.Error(), string(out)) {
					t.Errorf("RedBaseGoResult error = %q holds the captured output", err)
				}
			} else if err != nil {
				t.Errorf("RedBaseGoResult error = %v, want nil", err)
			}
		})
	}
}
