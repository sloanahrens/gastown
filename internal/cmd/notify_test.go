package cmd

import (
	"bytes"
	"errors"
	"testing"
)

func TestRunNotifyTestVerdicts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		pushErr  error
		want     string
		wantExit bool
	}{
		{"ok", nil, "NOTIFY OK\n", false},
		{"failed", errors.New("timed out after 10s"), "NOTIFY FAILED: timed out after 10s\n", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var gotName, gotTitle, gotBody string
			push := func(name, title, body string) error {
				gotName, gotTitle, gotBody = name, title, body
				return tc.pushErr
			}
			var out bytes.Buffer

			err := runNotifyTest(&out, push, "ntfy")

			if out.String() != tc.want {
				t.Errorf("output = %q, want %q", out.String(), tc.want)
			}
			if gotName != "ntfy" || gotTitle != "gastown notify test" || gotBody != "" {
				t.Errorf("push(%q, %q, %q), want ntfy / 'gastown notify test' / empty body", gotName, gotTitle, gotBody)
			}
			var exit *SilentExitError
			if tc.wantExit != errors.As(err, &exit) || (exit != nil && exit.Code != 1) {
				t.Errorf("error = %v, want silent exit 1: %v", err, tc.wantExit)
			}
		})
	}
}
