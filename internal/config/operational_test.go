package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionThresholds_Defaults(t *testing.T) {
	t.Parallel()

	// Nil OperationalConfig should return defaults
	var op *OperationalConfig
	session := op.GetSessionConfig()

	if got := session.StartupNudgeVerifyDelayD(); got != DefaultStartupNudgeVerifyDelay {
		t.Errorf("StartupNudgeVerifyDelay: got %v, want %v", got, DefaultStartupNudgeVerifyDelay)
	}
	if got := session.StartupNudgeMaxRetriesV(); got != DefaultStartupNudgeMaxRetries {
		t.Errorf("StartupNudgeMaxRetries: got %v, want %v", got, DefaultStartupNudgeMaxRetries)
	}
}

func TestSessionThresholds_Overrides(t *testing.T) {
	t.Parallel()

	retries := 5
	op := &OperationalConfig{
		Session: &SessionThresholds{
			StartupNudgeVerifyDelay: "40s",
			StartupNudgeMaxRetries:  &retries,
		},
	}

	session := op.GetSessionConfig()
	if got := session.StartupNudgeVerifyDelayD(); got != 40*time.Second {
		t.Errorf("StartupNudgeVerifyDelay: got %v, want 40s", got)
	}
	if got := session.StartupNudgeMaxRetriesV(); got != 5 {
		t.Errorf("StartupNudgeMaxRetries: got %v, want 5", got)
	}
}

func TestNudgeThresholds_Defaults(t *testing.T) {
	t.Parallel()

	op := &OperationalConfig{}
	nudge := op.GetNudgeConfig()

	if got := nudge.MaxQueueDepthV(); got != DefaultNudgeMaxQueueDepth {
		t.Errorf("MaxQueueDepth: got %v, want %v", got, DefaultNudgeMaxQueueDepth)
	}
	if got := nudge.StaleClaimThresholdD(); got != DefaultNudgeStaleClaimTimeout {
		t.Errorf("StaleClaimThreshold: got %v, want %v", got, DefaultNudgeStaleClaimTimeout)
	}
	if got := nudge.MaxDeliveryAttemptsV(); got != DefaultNudgeMaxDeliveryAttempts {
		t.Errorf("MaxDeliveryAttempts: got %v, want %v", got, DefaultNudgeMaxDeliveryAttempts)
	}
	if got := nudge.RequeueBackoffD(); got != DefaultNudgeRequeueBackoff {
		t.Errorf("RequeueBackoff: got %v, want %v", got, DefaultNudgeRequeueBackoff)
	}
}

func TestNudgeThresholds_RequeueOverrides(t *testing.T) {
	t.Parallel()

	// A non-positive MaxDeliveryAttempts is meaningless (a nudge could never
	// be requeued at all), so it falls back to the default; zero backoff is
	// valid and disables retry spacing.
	zero, negative := 0, -1
	cases := []struct {
		name         string
		thresholds   *NudgeThresholds
		wantAttempts int
		wantBackoff  time.Duration
	}{
		{"nil", nil, DefaultNudgeMaxDeliveryAttempts, DefaultNudgeRequeueBackoff},
		{"zero attempts", &NudgeThresholds{MaxDeliveryAttempts: &zero}, DefaultNudgeMaxDeliveryAttempts, DefaultNudgeRequeueBackoff},
		{"negative attempts", &NudgeThresholds{MaxDeliveryAttempts: &negative}, DefaultNudgeMaxDeliveryAttempts, DefaultNudgeRequeueBackoff},
		{"explicit", &NudgeThresholds{MaxDeliveryAttempts: intPtr(1), RequeueBackoff: "0s"}, 1, 0},
		{"invalid backoff", &NudgeThresholds{RequeueBackoff: "banana"}, DefaultNudgeMaxDeliveryAttempts, DefaultNudgeRequeueBackoff},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.thresholds.MaxDeliveryAttemptsV(); got != tc.wantAttempts {
				t.Errorf("MaxDeliveryAttemptsV: got %v, want %v", got, tc.wantAttempts)
			}
			if got := tc.thresholds.RequeueBackoffD(); got != tc.wantBackoff {
				t.Errorf("RequeueBackoffD: got %v, want %v", got, tc.wantBackoff)
			}
		})
	}
}

func TestDaemonThresholds_Defaults(t *testing.T) {
	t.Parallel()

	op := &OperationalConfig{}
	daemon := op.GetDaemonConfig()

	if got := daemon.PolecatIdleSessionTimeoutD(); got != DefaultPolecatIdleSessionTimeout {
		t.Errorf("PolecatIdleSessionTimeout: got %v, want %v", got, DefaultPolecatIdleSessionTimeout)
	}
	if got := daemon.RecoveryHeartbeatIntervalD(); got != DefaultRecoveryHeartbeatInterval {
		t.Errorf("RecoveryHeartbeatInterval: got %v, want %v", got, DefaultRecoveryHeartbeatInterval)
	}
}

func TestDaemonThresholds_Overrides(t *testing.T) {
	t.Parallel()

	op := &OperationalConfig{
		Daemon: &DaemonThresholds{
			PolecatIdleSessionTimeout: "2h",
		},
	}

	daemon := op.GetDaemonConfig()
	if got := daemon.PolecatIdleSessionTimeoutD(); got != 2*time.Hour {
		t.Errorf("PolecatIdleSessionTimeout: got %v, want 2h", got)
	}
	// Non-overridden fields should still return defaults
	if got := daemon.RecoveryHeartbeatIntervalD(); got != DefaultRecoveryHeartbeatInterval {
		t.Errorf("RecoveryHeartbeatInterval: got %v, want %v (default)", got, DefaultRecoveryHeartbeatInterval)
	}
}

func TestDaemonThresholds_NewFieldOverrides(t *testing.T) {
	t.Parallel()

	op := &OperationalConfig{
		Daemon: &DaemonThresholds{
			RecoveryHeartbeatInterval: "5m",
		},
	}

	daemon := op.GetDaemonConfig()
	if got := daemon.RecoveryHeartbeatIntervalD(); got != 5*time.Minute {
		t.Errorf("RecoveryHeartbeatInterval: got %v, want 5m", got)
	}
}

func TestLoadOperationalConfig_NonexistentDir(t *testing.T) {
	t.Parallel()

	op := LoadOperationalConfig("/nonexistent/town/root")
	// Should return valid empty config, not nil
	if op == nil {
		t.Fatal("LoadOperationalConfig should never return nil")
	}
	// Defaults should work
	if got := op.GetSessionConfig().StartupNudgeMaxRetriesV(); got != DefaultStartupNudgeMaxRetries {
		t.Errorf("expected default startup nudge retries, got %v", got)
	}
}

func TestLoadOperationalConfig_WithConfig(t *testing.T) {
	t.Parallel()

	// Create temp town root with settings/config.json
	dir := t.TempDir()
	settingsDir := filepath.Join(dir, "settings")
	if err := os.MkdirAll(settingsDir, 0755); err != nil {
		t.Fatal(err)
	}

	retries := 7
	settings := TownSettings{
		Type:    "town-settings",
		Version: 1,
		Operational: &OperationalConfig{
			Session: &SessionThresholds{
				StartupNudgeVerifyDelay: "45s",
				StartupNudgeMaxRetries:  &retries,
			},
			Daemon: &DaemonThresholds{
				PolecatIdleSessionTimeout: "3h",
			},
		},
	}

	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(settingsDir, "config.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	op := LoadOperationalConfig(dir)

	if got := op.GetSessionConfig().StartupNudgeVerifyDelayD(); got != 45*time.Second {
		t.Errorf("StartupNudgeVerifyDelay: got %v, want 45s", got)
	}
	if got := op.GetSessionConfig().StartupNudgeMaxRetriesV(); got != 7 {
		t.Errorf("StartupNudgeMaxRetries: got %v, want 7", got)
	}
	if got := op.GetDaemonConfig().PolecatIdleSessionTimeoutD(); got != 3*time.Hour {
		t.Errorf("PolecatIdleSessionTimeout: got %v, want 3h", got)
	}
	// Non-overridden subsystems should return defaults
	if got := op.GetNudgeConfig().MaxQueueDepthV(); got != DefaultNudgeMaxQueueDepth {
		t.Errorf("MaxQueueDepth: got %v, want %v (default)", got, DefaultNudgeMaxQueueDepth)
	}
}

func TestMailThresholds_Defaults(t *testing.T) {
	t.Parallel()

	op := &OperationalConfig{}
	mail := op.GetMailConfig()

	if got := mail.ReplyReminderDelayD(); got != DefaultMailReplyReminderDelay {
		t.Errorf("ReplyReminderDelay: got %v, want %v", got, DefaultMailReplyReminderDelay)
	}
}

func TestMailThresholds_ReplyReminderDelayOverride(t *testing.T) {
	t.Parallel()

	op := &OperationalConfig{
		Mail: &MailThresholds{
			ReplyReminderDelay: "1m",
		},
	}
	if got := op.GetMailConfig().ReplyReminderDelayD(); got != time.Minute {
		t.Errorf("ReplyReminderDelay override: got %v, want 1m", got)
	}
}

func TestMailThresholds_ReplyReminderDelayDisabled(t *testing.T) {
	t.Parallel()

	// "0s" disables reply reminders.
	op := &OperationalConfig{
		Mail: &MailThresholds{
			ReplyReminderDelay: "0s",
		},
	}
	if got := op.GetMailConfig().ReplyReminderDelayD(); got != 0 {
		t.Errorf("ReplyReminderDelay disabled: got %v, want 0", got)
	}
}

func TestRecoveryThresholds_Defaults(t *testing.T) {
	t.Parallel()

	var op *OperationalConfig
	rec := op.GetRecoveryConfig()

	if got := rec.MaxBeadRespawnsV(); got != DefaultRecoveryMaxBeadRespawns {
		t.Errorf("MaxBeadRespawns: got %v, want %v", got, DefaultRecoveryMaxBeadRespawns)
	}
	if got := rec.HeartbeatStartupGraceD(); got != DefaultRecoveryHeartbeatStartupGrace {
		t.Errorf("HeartbeatStartupGrace: got %v, want %v", got, DefaultRecoveryHeartbeatStartupGrace)
	}
}

// The recovery thresholds still live under the retired witness role's key,
// which the live settings/config.json carries.
func TestRecoveryThresholds_DecodeFromWitnessKey(t *testing.T) {
	t.Parallel()

	var op OperationalConfig
	body := `{"witness":{"max_bead_respawns":5,"heartbeat_startup_grace":"7m","done_intent_stuck_timeout":"90m"}}`
	if err := json.Unmarshal([]byte(body), &op); err != nil {
		t.Fatal(err)
	}

	rec := op.GetRecoveryConfig()
	if got := rec.MaxBeadRespawnsV(); got != 5 {
		t.Errorf("MaxBeadRespawns: got %v, want 5", got)
	}
	if got := rec.HeartbeatStartupGraceD(); got != 7*time.Minute {
		t.Errorf("HeartbeatStartupGrace: got %v, want 7m", got)
	}
}

func TestPressureThresholds_Defaults(t *testing.T) {
	t.Parallel()

	var op *OperationalConfig
	dt := op.GetDaemonConfig()

	if got := dt.PressureCPUThresholdV(); got != DefaultPressureCPUThreshold {
		t.Errorf("PressureCPUThreshold: got %v, want %v", got, DefaultPressureCPUThreshold)
	}
	if got := dt.PressureMemThresholdGBV(); got != DefaultPressureMemThresholdGB {
		t.Errorf("PressureMemThresholdGB: got %v, want %v", got, DefaultPressureMemThresholdGB)
	}
	if got := dt.PressureMaxSessionsV(); got != DefaultPressureMaxSessions {
		t.Errorf("PressureMaxSessions: got %v, want %v", got, DefaultPressureMaxSessions)
	}
}

func TestPressureThresholds_Overrides(t *testing.T) {
	t.Parallel()

	cpu := 0.5
	mem := 8.0
	sessions := 10
	dt := &DaemonThresholds{
		PressureCPUThreshold:   &cpu,
		PressureMemThresholdGB: &mem,
		PressureMaxSessions:    &sessions,
	}
	if got := dt.PressureCPUThresholdV(); got != 0.5 {
		t.Errorf("PressureCPUThreshold: got %v, want 0.5", got)
	}
	if got := dt.PressureMemThresholdGBV(); got != 8.0 {
		t.Errorf("PressureMemThresholdGB: got %v, want 8.0", got)
	}
	if got := dt.PressureMaxSessionsV(); got != 10 {
		t.Errorf("PressureMaxSessions: got %v, want 10", got)
	}
}

func TestPressureThresholds_DisabledWithZero(t *testing.T) {
	t.Parallel()

	zero := 0.0
	zeroInt := 0
	dt := &DaemonThresholds{
		PressureCPUThreshold:   &zero,
		PressureMemThresholdGB: &zero,
		PressureMaxSessions:    &zeroInt,
	}
	if dt.PressureCPUThresholdV() != 0 {
		t.Error("zero CPU threshold should disable check")
	}
	if dt.PressureMemThresholdGBV() != 0 {
		t.Error("zero mem threshold should disable check")
	}
	if dt.PressureMaxSessionsV() != 0 {
		t.Error("zero max sessions should mean unlimited")
	}
}

func TestPressureThresholds_NilReceiver(t *testing.T) {
	t.Parallel()

	var dt *DaemonThresholds
	if dt.PressureCPUThresholdV() != DefaultPressureCPUThreshold {
		t.Error("nil DaemonThresholds should return default CPU threshold")
	}
	if dt.PressureMemThresholdGBV() != DefaultPressureMemThresholdGB {
		t.Error("nil DaemonThresholds should return default mem threshold")
	}
	if dt.PressureMaxSessionsV() != DefaultPressureMaxSessions {
		t.Error("nil DaemonThresholds should return default max sessions")
	}
}

func TestPressureThresholds_JSON(t *testing.T) {
	t.Parallel()

	jsonData := `{
		"daemon": {
			"pressure_cpu_threshold": 0.7,
			"pressure_mem_threshold_gb": 4.0,
			"pressure_max_sessions": 8
		}
	}`

	dir := t.TempDir()
	configDir := filepath.Join(dir, "settings")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}

	var raw struct {
		Daemon *DaemonThresholds `json:"daemon"`
	}
	if err := json.Unmarshal([]byte(jsonData), &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Daemon.PressureCPUThresholdV() != 0.7 {
		t.Errorf("JSON CPU threshold: got %v, want 0.7", raw.Daemon.PressureCPUThresholdV())
	}
	if raw.Daemon.PressureMemThresholdGBV() != 4.0 {
		t.Errorf("JSON mem threshold: got %v, want 4.0", raw.Daemon.PressureMemThresholdGBV())
	}
	if raw.Daemon.PressureMaxSessionsV() != 8 {
		t.Errorf("JSON max sessions: got %v, want 8", raw.Daemon.PressureMaxSessionsV())
	}
}

// TestContainerGateThresholds_YieldDefaults: yielding to a running gate is on
// by default (gt-22hdp.29), with a 30-minute cap.
func TestContainerGateThresholds_YieldDefaults(t *testing.T) {
	t.Parallel()

	var op *OperationalConfig
	cg := op.GetContainerGateConfig()
	if !cg.YieldToGateV() {
		t.Error("YieldToGate: got false, want default true")
	}
	if got := cg.MaxGateYieldD(); got != 30*time.Minute {
		t.Errorf("MaxGateYield: got %v, want 30m", got)
	}
}

// TestContainerGateThresholds_YieldOverrides: the knob turns yielding off, the
// cap accepts a duration, and an invalid or non-positive cap keeps the default.
func TestContainerGateThresholds_YieldOverrides(t *testing.T) {
	t.Parallel()

	var cfg struct {
		Operational *OperationalConfig `json:"operational"`
	}
	raw := `{"operational":{"container_gate":{"slots":4,"reserved_for_gate":2,"yield_to_gate":false,"max_gate_yield":"45m"}}}`
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	cg := cfg.Operational.GetContainerGateConfig()
	if cg.YieldToGateV() {
		t.Error("YieldToGate: got true, want false from config")
	}
	if got := cg.MaxGateYieldD(); got != 45*time.Minute {
		t.Errorf("MaxGateYield: got %v, want 45m", got)
	}

	for _, bad := range []string{"soon", "0s", "-5m"} {
		cg := &ContainerGateThresholds{MaxGateYield: bad}
		if got := cg.MaxGateYieldD(); got != DefaultContainerGateMaxGateYield {
			t.Errorf("MaxGateYield %q: got %v, want default %v", bad, got, DefaultContainerGateMaxGateYield)
		}
	}
}

// TestOperationalConfig_RetiredKeysDecode: keys whose accessors were deleted
// (gt-e2kxa) still decode strictly, so an older settings file keeps loading.
func TestOperationalConfig_RetiredKeysDecode(t *testing.T) {
	t.Parallel()

	data := []byte(`{"operational": {
		"session": {"claude_start_timeout": "60s", "gupp_violation_timeout": "30m"},
		"nudge": {"ready_timeout": "10s", "urgent_ttl": "2h"},
		"daemon": {"mass_death_threshold": 3, "doctor_mol_cooldown": "5m"},
		"polecat": {"namepool_size": 50, "dolt_backoff_max": "30s"},
		"dolt": {"max_connections": 1000},
		"mail": {"bd_read_timeout": "60s", "max_concurrent_ack_ops": 8},
		"web": {"max_body_len": 100000}
	}}`)
	if err := DecodeJSONFile("config.json", data, &TownSettings{}); err != nil {
		t.Fatalf("retired operational keys must still decode: %v", err)
	}
}
