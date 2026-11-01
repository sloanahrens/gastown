package config

import (
	"strings"
	"testing"
	"time"
)

// TestWispReaperConfigDecodesEventsRetention: the key survives strict decoding
// into the field EventsRetention reads.
func TestWispReaperConfigDecodesEventsRetention(t *testing.T) {
	t.Parallel()
	body := `{"patrols":{"wisp_reaper":{"enabled":true,"events_retention":"30d"}}}`
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(body), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Patrols.WispReaper == nil {
		t.Fatal("wisp_reaper decoded as absent")
	}
	if got := cfg.Patrols.WispReaper.EventsRetentionStr; got != "30d" {
		t.Errorf("events_retention = %q, want %q", got, "30d")
	}
}

// TestWispReaperEventsRetention: an unset events_retention means the step is
// off, a "30d" value is 720h, and a value that is set but unusable is an error
// naming the field rather than silently off or a delete-everything pass.
func TestWispReaperEventsRetention(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "unset means off", value: ""},
		{name: "blank means off", value: "   "},
		{name: "day suffix", value: "30d", want: 30 * 24 * time.Hour},
		{name: "hour syntax", value: "720h", want: 720 * time.Hour},
		{name: "unparseable", value: "soon", wantErr: true},
		{name: "zero is refused", value: "0d", wantErr: true},
		{name: "negative is refused", value: "-1h", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &WispReaperConfig{EventsRetentionStr: tt.value}
			got, err := c.EventsRetention()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("EventsRetention(%q) = %s, want an error", tt.value, got)
				}
				if !strings.Contains(err.Error(), "patrols.wisp_reaper.events_retention") {
					t.Errorf("error %q does not name the field", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("EventsRetention(%q): %v", tt.value, err)
			}
			if got != tt.want {
				t.Errorf("EventsRetention(%q) = %s, want %s", tt.value, got, tt.want)
			}
		})
	}

	var absent *WispReaperConfig
	if got, err := absent.EventsRetention(); got != 0 || err != nil {
		t.Errorf("an absent wisp_reaper section = %s, %v; want 0, nil (step off)", got, err)
	}
}
