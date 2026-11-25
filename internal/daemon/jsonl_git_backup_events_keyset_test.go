package daemon

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestEventsPageKeyset: the keyset a page's last row yields is its created_at
// and id, and a row the next page cannot be read after is refused (gt-8ffoqv).
func TestEventsPageKeyset(t *testing.T) {
	t.Parallel()
	const (
		id = "6f0c1e3a-0000-4000-8000-000000000001"
		at = "2026-10-31 10:00:00"
	)
	for _, tt := range []struct {
		name          string
		row           string
		wantCreatedAt string
		wantID        string
		wantErr       string
	}{
		{
			name:          "a row carries its created_at and id",
			row:           `{"id":"` + id + `","created_at":"` + at + `"}`,
			wantCreatedAt: at,
			wantID:        id,
		},
		{
			name:    "not json",
			row:     "nope",
			wantErr: "the page's last row",
		},
		{
			name:    "no id",
			row:     `{"created_at":"` + at + `"}`,
			wantErr: "no id",
		},
		{
			name:    "no created_at",
			row:     `{"id":"` + id + `"}`,
			wantErr: "created_at",
		},
		{
			name:    "created_at is not a timestamp",
			row:     `{"id":"` + id + `","created_at":"yesterday"}`,
			wantErr: "created_at",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			createdAt, gotID, err := eventsPageKeyset(json.RawMessage(tt.row))
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("row %s returned keyset %q, %q; want an error", tt.row, createdAt, gotID)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v; want it to name %q", err, tt.wantErr)
				}
				if createdAt != "" || gotID != "" {
					t.Errorf("the refused row returned keyset %q, %q; want empty strings", createdAt, gotID)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if createdAt != tt.wantCreatedAt || gotID != tt.wantID {
				t.Errorf("keyset = %q, %q; want %q, %q", createdAt, gotID, tt.wantCreatedAt, tt.wantID)
			}
		})
	}
}
