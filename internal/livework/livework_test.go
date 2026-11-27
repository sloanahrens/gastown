package livework

import (
	"errors"
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

func TestCheck(t *testing.T) {
	t.Parallel()
	boom := errors.New("dolt: connection refused")
	tests := []struct {
		name, status string
		found, want  bool
		readErr      error
		wantErr      string
	}{
		{"open is live", "open", true, true, nil, ""},
		{"hooked is live", "hooked", true, true, nil, ""},
		{"in_progress is live", "in_progress", true, true, nil, ""},
		{"closed is not live", "closed", true, false, nil, ""},
		{"deferred is not live", "deferred", true, false, nil, ""},
		{"blocked is not live", "blocked", true, false, nil, ""},
		{"an unknown status is not live", "pinned", true, false, nil, ""},
		{"a failed read is an error", "", false, false, boom, "cannot read the hooked bead gt-abc: dolt: connection refused"},
		{"a missing bead is an error", "", false, false, nil, "the hooked bead gt-abc reads as no bead"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := Check("gt-abc", func(_ string) (string, bool, error) {
				return tt.status, tt.found, tt.readErr
			})
			if got != tt.want || (err == nil) != (tt.wantErr == "") || (err != nil && err.Error() != tt.wantErr) {
				t.Errorf("Check(%q) = %v, %v; want %v, %q", tt.status, got, err, tt.want, tt.wantErr)
			}
		})
	}
}
