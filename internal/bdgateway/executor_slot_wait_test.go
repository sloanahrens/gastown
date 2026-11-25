package bdgateway

import (
	"errors"
	"testing"
	"time"
)

// TestExecutorSlotWaitOption: a zero SlotWait takes DefaultSlotWait, a positive
// one is kept as given, and a negative one is refused by name like the other
// durations.
func TestExecutorSlotWaitOption(t *testing.T) {
	t.Parallel()
	base := Options{
		Binary:        "/opt/bd/bd",
		WorkspaceRoot: "/ws",
		Home:          "/home/bdgw",
		Path:          "/usr/bin:/bin",
		DoltHost:      "gt-dolt",
		DoltPort:      "3307",
	}
	cases := []struct {
		name     string
		slotWait time.Duration
		want     time.Duration
		refused  bool
	}{
		{name: "zero takes the default", slotWait: 0, want: DefaultSlotWait},
		{name: "positive is kept", slotWait: 3 * time.Second, want: 3 * time.Second},
		{name: "negative is refused", slotWait: -time.Second, refused: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := base
			opts.SlotWait = tc.slotWait
			e, err := New(opts)
			if tc.refused {
				if !errors.Is(err, ErrUnconfigured) {
					t.Fatalf("New = %v, want ErrUnconfigured", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if e.slotWait != tc.want {
				t.Errorf("slot wait = %s, want %s", e.slotWait, tc.want)
			}
		})
	}
}
