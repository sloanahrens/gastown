package events

import "testing"

// TestDoneEventIsSubmission pins which done events are submissions: one written
// before gt done recorded the exit type, and one whose exit type is the
// completed exit. An escalated or deferred exit submits nothing, and a status
// the package cannot read or does not know is not read as a submission, so an
// unknown value cannot pass as a submitted one (gt-qqmae).
func TestDoneEventIsSubmission(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		payload map[string]interface{}
		want    bool
	}{
		{"no status: written before the field existed", map[string]interface{}{"bead": "gt-a"}, true},
		{"an empty status", map[string]interface{}{"status": ""}, true},
		{"the completed exit", map[string]interface{}{"status": DoneStatusCompleted}, true},
		{"the escalated exit", map[string]interface{}{"status": DoneStatusEscalated}, false},
		{"the deferred exit", map[string]interface{}{"status": DoneStatusDeferred}, false},
		{"a status the package does not know", map[string]interface{}{"status": "WAITING"}, false},
		{"a status that is not a string", map[string]interface{}{"status": 3}, false},
		{"a null status", map[string]interface{}{"status": nil}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DoneEventIsSubmission(Event{Type: TypeDone, Payload: tc.payload}); got != tc.want {
				t.Errorf("DoneEventIsSubmission(%v) = %v, want %v", tc.payload, got, tc.want)
			}
		})
	}
}
