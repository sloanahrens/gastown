package beads

import (
	"errors"
	"testing"
)

// A failed formula verb reads as bd's own message, taken from the machine
// envelope's typed error as well as the legacy {"error": "..."} string; any
// other output keeps the error as it was.
func TestWithBDMessage(t *testing.T) {
	t.Parallel()
	cause := errors.New("exit status 1")
	tests := []struct {
		name string
		out  string
		want string
	}{
		{"machine envelope", `{"schema_version":1,"contract_version":1,"data":null,"error":{"kind":"internal","message":"Error 1213 (40001): serialization failure"}}`, "Error 1213 (40001): serialization failure"},
		{"legacy string", `{"error":"creating wisp: boom"}`, "creating wisp: boom"},
		{"other shape", "not json\n", "bd cook x: exit status 1"},
		{"empty", "", "bd cook x: exit status 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := withBDMessage(&unavailableError{msg: "bd cook x: exit status 1", cause: cause, stdout: []byte(tt.out)})
			if err.Error() != tt.want {
				t.Fatalf("message = %q, want %q", err.Error(), tt.want)
			}
			if !errors.Is(err, ErrUnavailable) || !errors.Is(err, cause) {
				t.Fatalf("%v lost its causes", err)
			}
		})
	}
}
