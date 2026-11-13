package git

import (
	"testing"
)

// TestConfigGetReadsTheExitStatus: git config --get exits 1 only when the key is
// unset. Any other failure, such as an unreadable config file, is an error, so a
// caller never takes an unknown value for an unset one.
func TestConfigGetReadsTheExitStatus(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		attempt configReadAttempt
		want    string
		wantErr bool
	}{
		{name: "set", attempt: configReadAttempt{stdout: ".githooks\n"}, want: ".githooks"},
		{name: "unset", attempt: configReadAttempt{err: gitExit(1)}, want: ""},
		{
			name:    "unreadable config",
			attempt: configReadAttempt{stderr: "fatal: bad config line 1 in file .git/config", err: gitExit(128)},
			wantErr: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := &configReadRunner{attempts: []configReadAttempt{tt.attempt}}
			g := &Git{workDir: t.TempDir(), exec: r.run}

			got, err := g.ConfigGet("core.hooksPath")
			if (err != nil) != tt.wantErr {
				t.Fatalf("ConfigGet error = %v, want error %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("ConfigGet = %q, want %q", got, tt.want)
			}
		})
	}
}
