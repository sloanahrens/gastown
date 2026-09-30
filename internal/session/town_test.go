package session

import (
	"testing"
)

func TestTownSessions(t *testing.T) {
	sessions := TownSessions()

	if len(sessions) != 1 || sessions[0].Name != "Mayor" {
		t.Fatalf("TownSessions() = %+v, want only the Mayor", sessions)
	}
	if sessions[0].SessionID == "" {
		t.Error("Mayor SessionID should not be empty")
	}
}

func TestTownSessions_SessionIDFormats(t *testing.T) {
	sessions := TownSessions()

	for _, s := range sessions {
		if s.SessionID == "" {
			t.Errorf("TownSession %q has empty SessionID", s.Name)
		}
		// Session IDs should follow a pattern
		if len(s.SessionID) < 4 {
			t.Errorf("TownSession %q SessionID %q is too short", s.Name, s.SessionID)
		}
	}
}

func TestTownSession_StructFields(t *testing.T) {
	ts := TownSession{
		Name:      "Test",
		SessionID: "test-session",
	}

	if ts.Name != "Test" {
		t.Errorf("TownSession.Name = %q, want %q", ts.Name, "Test")
	}
	if ts.SessionID != "test-session" {
		t.Errorf("TownSession.SessionID = %q, want %q", ts.SessionID, "test-session")
	}
}

func TestTownSession_CanBeCreated(t *testing.T) {
	// Test that TownSession can be created with any values
	tests := []struct {
		name      string
		sessionID string
	}{
		{"Mayor", "hq-mayor"},
		{"Custom", "custom-session"},
	}

	for _, tt := range tests {
		ts := TownSession{
			Name:      tt.name,
			SessionID: tt.sessionID,
		}
		if ts.Name != tt.name {
			t.Errorf("TownSession.Name = %q, want %q", ts.Name, tt.name)
		}
		if ts.SessionID != tt.sessionID {
			t.Errorf("TownSession.SessionID = %q, want %q", ts.SessionID, tt.sessionID)
		}
	}
}
