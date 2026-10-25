package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestChannelNameError pins the channel-level consequence of sharing the group
// name rule (gt-0w11c): a rejected name carries the channel's own message and
// the length bound, and channel create reports that same error before it looks
// for a town.
func TestChannelNameError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		wantErr bool
	}{
		// Valid: the strictest name at each boundary the group rule allows.
		{"ops-team", false},
		{"alerts", false},
		{"team123", false},
		{strings.Repeat("a", beads.MaxGroupNameLength), false},

		// Invalid: the stricter group rule reaches channel create.
		{"", true},
		{"Ops-Team", true},
		{"A", true},
		{"-x", true},
		{"_x", true},
		{strings.Repeat("a", beads.MaxGroupNameLength+1), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := channelNameError(tt.name)
			if (err != nil) != tt.wantErr {
				t.Fatalf("channelNameError(%q) = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
			if err == nil {
				return
			}
			if !strings.Contains(err.Error(), "invalid channel name") {
				t.Errorf("channelNameError(%q) = %v, want the channel-level message", tt.name, err)
			}
			if got := runChannelCreate(nil, []string{tt.name}); got == nil || got.Error() != err.Error() {
				t.Errorf("runChannelCreate(%q) = %v, want %v", tt.name, got, err)
			}
		})
	}
}
