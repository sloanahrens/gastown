//go:build integration

package sandbox

import (
	"context"
	"testing"
)

// TestIntegrationExistsThroughExecRunner reads the not-found status from the
// stderr the real exec runner returns with a failed process, so TestExists
// does not rest on a Fake that hands stderr over alongside the error.
func TestIntegrationExistsThroughExecRunner(t *testing.T) {
	tests := []struct {
		name    string
		stderr  string
		want    bool
		wantErr bool
	}{
		{name: "not_found", stderr: `Error:   × code: 'Some requested entity was not found', message: "sandbox not found"`},
		{name: "gateway_unreachable", stderr: "transport error: connection refused", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A shell prints the stderr and fails, as openshell does. The
			// message rides as $1, not env, so env redaction has nothing to
			// match.
			run := func(ctx context.Context, _ []string, stdin []byte, env []string) ([]byte, []byte, error) {
				return execRunner(ctx, []string{"sh", "-c", `printf %s "$1" >&2; exit 1`, "sh", tt.stderr}, stdin, env)
			}
			got, err := (&CLI{Run: run}).Exists(context.Background(), "x")
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Fatalf("Exists = %v, %v; want %v (error: %v)", got, err, tt.want, tt.wantErr)
			}
		})
	}
}
