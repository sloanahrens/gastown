package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/sandbox"
)

// markerReply is the sandbox CLI's runner for one marker question: it answers
// with stdout and err, and records the argv it was given.
func markerReply(stdout string, err error, argv *[]string) sandbox.Runner {
	return func(_ context.Context, a []string, _ []byte, _ []string) ([]byte, []byte, error) {
		*argv = append([]string(nil), a...)
		return []byte(stdout), nil, err
	}
}

// The marker question is answered from the sandbox's own answer: "present" and
// "absent" are the two clean answers, and anything else, or a failed exec, is an
// error that never reads as "absent".
func TestSandboxEndedMarkerPresent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		stdout  string
		runErr  error
		want    bool
		wantErr bool
	}{
		{name: "marker_is_set", stdout: "present\n", want: true},
		{name: "marker_is_not_set", stdout: "absent\n", want: false},
		{name: "exec_fails_is_an_error", runErr: errors.New("gateway unreachable"), wantErr: true},
		{name: "empty_answer_is_an_error", stdout: "", wantErr: true},
		{name: "unknown_answer_is_an_error", stdout: "maybe\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var argv []string
			cli := &sandbox.CLI{Run: markerReply(tt.stdout, tt.runErr, &argv)}
			got, err := sandboxEndedMarkerPresentWith(cli, "gt-capable")
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, want error %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("marker present = %v, want %v", got, tt.want)
			}
		})
	}
}

// The question names the marker under the sandbox home, in one exec of the
// named sandbox.
func TestSandboxEndedMarkerPresentAsksTheMarkerInTheNamedSandbox(t *testing.T) {
	t.Parallel()
	var argv []string
	cli := &sandbox.CLI{Run: markerReply("absent\n", nil, &argv)}
	if _, err := sandboxEndedMarkerPresentWith(cli, "gt-capable"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "gt-capable") {
		t.Errorf("exec argv %q does not name the sandbox gt-capable", joined)
	}
	if want := sandboxHome + "/" + sandboxEndedMarker; !strings.Contains(joined, want) {
		t.Errorf("exec argv %q does not test the marker %s", joined, want)
	}
}
