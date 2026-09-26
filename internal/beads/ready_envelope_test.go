package beads

import (
	"errors"
	"testing"
)

// TestParseReadyOutput is the gt-m7pq unit test for the CLI-path parser:
// every shape bd's ready --json can emit must come back as the right (data,
// sentinel) pair — a capped page is a loud answer, an uncapped page and a
// legacy bare array are not, and garbage is an error, never a silent empty
// board.
func TestParseReadyOutput(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantID     string // "" means the call is expected to fail
		wantCapped bool
		wantTotal  int
	}{
		{
			name:   "capped envelope",
			in:     `{"schema_version":1,"data":[{"id":"gt-a","title":"one","status":"open","priority":2,"issue_type":"task"}],"pagination":{"returned":1,"total":373,"truncated":true}}`,
			wantID: "gt-a", wantCapped: true, wantTotal: 373,
		},
		{
			name:   "envelope without pagination",
			in:     `{"schema_version":1,"data":[{"id":"gt-b","title":"one","status":"open","priority":2,"issue_type":"task"}]}`,
			wantID: "gt-b",
		},
		{
			name:   "envelope with pagination but not truncated",
			in:     `{"schema_version":1,"data":[{"id":"gt-c","title":"one","status":"open","priority":2,"issue_type":"task"}],"pagination":{"returned":1,"truncated":false}}`,
			wantID: "gt-c",
		},
		{
			name:   "legacy plain array",
			in:     `[{"id":"gt-d","title":"one","status":"open","priority":2,"issue_type":"task"}]`,
			wantID: "gt-d",
		},
		{
			name: "empty input",
			in:   "",
		},
		{
			name: "malformed json",
			in:   `{not json`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issues, err := parseReadyOutput([]byte(tt.in))

			if tt.wantID == "" {
				if err == nil {
					t.Fatalf("expected an error, got %v", issues)
				}
				return
			}

			if err == nil && !tt.wantCapped {
				if len(issues) != 1 || issues[0].ID != tt.wantID {
					ids := make([]string, len(issues))
					for i, issue := range issues {
						ids[i] = issue.ID
					}
					t.Fatalf("issues = %v, want [%s]", ids, tt.wantID)
				}
				return
			}

			var capped *ErrReadyTruncated
			if !errors.As(err, &capped) {
				t.Fatalf("expected ErrReadyTruncated, got %v", err)
			}
			if len(issues) != 1 || issues[0].ID != tt.wantID {
				ids := make([]string, len(issues))
				for i, issue := range issues {
					ids[i] = issue.ID
				}
				t.Fatalf("issues = %v, want [%s]", ids, tt.wantID)
			}
			if capped.TrueCount != tt.wantTotal {
				t.Errorf("TrueCount = %d, want %d", capped.TrueCount, tt.wantTotal)
			}
		})
	}
}
