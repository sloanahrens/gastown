package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doltpause"
)

func TestDoltPauseCheck(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 30, 3, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		marker *doltpause.Marker
		raw    string
		status CheckStatus
		want   string
	}{
		{name: "none", status: StatusOK, want: "not paused"},
		{name: "fresh", marker: &doltpause.Marker{Actor: "deacon", Reason: "gc", Since: now, Until: now.Add(time.Hour)}, status: StatusOK, want: "Dolt paused by deacon"},
		{name: "lapsed", marker: &doltpause.Marker{Actor: "deacon", Reason: "gc", Since: now.Add(-2 * time.Hour), Until: now.Add(-time.Hour)}, status: StatusWarning, want: "lapsed"},
		{name: "old", marker: &doltpause.Marker{Actor: "deacon", Reason: "gc", Since: now.Add(-30 * time.Hour), Until: now.Add(time.Hour)}, status: StatusWarning, want: "older than"},
		{name: "unreadable", raw: "{", status: StatusWarning, want: "Unreadable"},
	}
	for _, tc := range cases {
		town := t.TempDir()
		if tc.marker != nil {
			if err := doltpause.Write(town, *tc.marker); err != nil {
				t.Fatal(err)
			}
		}
		if tc.raw != "" {
			if err := os.MkdirAll(filepath.Join(town, "daemon"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(doltpause.Path(town), []byte(tc.raw), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		c := NewDoltPauseCheck()
		c.now = func() time.Time { return now }
		r := c.Run(&CheckContext{TownRoot: town})
		if r.Status != tc.status || !strings.Contains(r.Message, tc.want) {
			t.Errorf("%s: status=%v message=%q; want %v containing %q", tc.name, r.Status, r.Message, tc.status, tc.want)
		}
	}
}
