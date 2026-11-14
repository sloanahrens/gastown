package config

import (
	"testing"
)

// The lesson-class patrol is opt-in: a daemon.json that does not name it leaves
// the block nil, which the daemon reads as off.
func TestOMClassPatrolIsOffByDefault(t *testing.T) {
	t.Parallel()
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", []byte(`{"patrols":{"dolt_server":{"enabled":true,"port":3307}}}`), &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.Patrols.JevOMClass != nil {
		t.Errorf("JevOMClass = %+v, want nil when daemon.json does not name it", cfg.Patrols.JevOMClass)
	}
}

// An explicit block decodes its switch and its price, and the strict decoder
// accepts the key rather than rejecting it as unknown.
func TestOMClassPatrolDecodesItsSwitchAndPrice(t *testing.T) {
	t.Parallel()
	var cfg DaemonPatrolConfig
	data := []byte(`{"patrols":{"jev_omclass":{"enabled":true,"max_usd_per_day":2,"usd_per_billion_tokens":4}}}`)
	if err := DecodeJSONFile("daemon.json", data, &cfg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := cfg.Patrols.JevOMClass
	if got == nil || !got.Enabled || got.MaxUSDPerDay != 2 || got.USDPerBillionTokens != 4 {
		t.Errorf("JevOMClass = %+v, want enabled with a $2 cap at $4 per billion tokens", got)
	}
}
