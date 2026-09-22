package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SyncReportPath returns the path to the hooks sync report, written by
// 'gt hooks sync' on a successful, canary-verified run and read by the
// hooks-sync doctor check. Its presence is the deployment record for a
// sync: a target count or a role's mail proves nothing about whether the
// hooks that were written actually work end-to-end (claude-41j.1 D7/D8) —
// only a recorded live-fire pair result against a real settings file does.
func SyncReportPath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", "hooks", "sync-report.json")
}

// SyncReportVerdict mirrors doctor.LiveFireVerdict as a string so this
// package (which the doctor package imports) never needs to import doctor
// back. The cmd layer, which imports both, converts between them.
type SyncReportVerdict string

const (
	SyncReportPass         SyncReportVerdict = "pass"
	SyncReportFail         SyncReportVerdict = "fail"
	SyncReportInconclusive SyncReportVerdict = "inconclusive"
)

// SyncReportShape records one live-fire probe shape's verdict.
type SyncReportShape struct {
	Verdict SyncReportVerdict `json:"verdict"`
	Detail  string            `json:"detail,omitempty"`
}

// SyncReportCanary records the canary-first live-fire pair that gated
// fan-out for this sync run.
type SyncReportCanary struct {
	Target       string          `json:"target"`
	SettingsPath string          `json:"settings_path"`
	Blocked      SyncReportShape `json:"blocked"`
	Allowed      SyncReportShape `json:"allowed"`
}

// Passed reports whether both shapes of the canary pair passed.
func (c SyncReportCanary) Passed() bool {
	return c.Blocked.Verdict == SyncReportPass && c.Allowed.Verdict == SyncReportPass
}

// SyncReportRole records the effective hook set actually rendered and
// written for one role/target during a sync run — matcher, command, and if,
// exactly as written to that target's settings.json.
type SyncReportRole struct {
	Role  string      `json:"role,omitempty"`
	Rig   string      `json:"rig,omitempty"`
	Path  string      `json:"path"`
	Hooks HooksConfig `json:"hooks"`
}

// SyncReport is the deployment record written by 'gt hooks sync' on a
// successful run: which canary settled the live-fire pair, and what was
// actually rendered for every role — read back by the hooks-sync doctor
// check so "in sync" means "verified deployed", not just "files match".
type SyncReport struct {
	Timestamp time.Time                 `json:"timestamp"`
	Canary    SyncReportCanary          `json:"canary"`
	Roles     map[string]SyncReportRole `json:"roles"`
}

// WriteSyncReport writes report atomically to its canonical location under
// townRoot, creating parent directories as needed.
func WriteSyncReport(townRoot string, report *SyncReport) error {
	path := SyncReportPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("creating sync report directory: %w", err)
	}

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling sync report: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing sync report: %w", err)
	}
	return nil
}

// ReadSyncReport reads and parses the sync report at its canonical location.
// Returns an error satisfying os.IsNotExist if the report has never been
// written — callers (the hooks-sync doctor check) must treat that as
// StatusSkipped, never as a pass.
func ReadSyncReport(townRoot string) (*SyncReport, error) {
	data, err := os.ReadFile(SyncReportPath(townRoot))
	if err != nil {
		return nil, err
	}
	var report SyncReport
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("parsing sync report: %w", err)
	}
	return &report, nil
}
