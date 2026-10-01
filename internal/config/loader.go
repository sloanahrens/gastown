package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// resolveConfigMu serializes agent config resolution across all callers.
// It dates from when resolutions shared a process-global agent registry.
// Each resolution now builds its own registry for its town and rig
// (AgentRegistryFor, gt-rg4f1), so the lock no longer guards registry state;
// it is kept so this fix does not also change resolution concurrency.
var resolveConfigMu sync.Mutex

var (
	// ErrNotFound indicates the config file does not exist.
	ErrNotFound = errors.New("config file not found")

	// ErrInvalidVersion indicates an unsupported schema version.
	ErrInvalidVersion = errors.New("unsupported config version")

	// ErrInvalidType indicates an unexpected config type.
	ErrInvalidType = errors.New("invalid config type")

	// ErrMissingField indicates a required field is missing.
	ErrMissingField = errors.New("missing required field")
)

// LoadTownConfig loads and validates a town configuration file.
func LoadTownConfig(path string) (*TownConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is from trusted config location
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var config TownConfig
	if err := DecodeJSONFile(path, data, &config); err != nil {
		return nil, err
	}

	if err := validateTownConfig(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

// SaveTownConfig saves a town configuration to a file.
func SaveTownConfig(path string, config *TownConfig) error {
	if err := validateTownConfig(config); err != nil {
		return err
	}

	return WriteConfigJSON(path, config, 0600)
}

// LoadRigsConfig loads and validates a rigs registry file. Every writer is
// atomic (WriteConfigJSON), so a file that does not parse is damage and is
// reported, not retried.
func LoadRigsConfig(path string) (*RigsConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally, not from user input
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var config RigsConfig
	if err := DecodeJSONFile(path, data, &config); err != nil {
		return nil, err
	}

	if err := validateRigsConfig(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

// SaveRigsConfig saves a rigs registry to a file atomically.
// Writes to a temp file in the same directory then renames into place; the
// rename is atomic on POSIX, so concurrent readers never observe a zero-byte
// or partially-written rigs.json.
func SaveRigsConfig(path string, config *RigsConfig) error {
	if err := validateRigsConfig(config); err != nil {
		return err
	}

	return WriteConfigJSON(path, config, 0600)
}

// validateTownConfig validates a TownConfig.
func validateTownConfig(c *TownConfig) error {
	if c.Type != "town" && c.Type != "" {
		return fmt.Errorf("%w: expected type 'town', got '%s'", ErrInvalidType, c.Type)
	}
	if c.Version > CurrentTownVersion {
		return fmt.Errorf("%w: got %d, max supported %d", ErrInvalidVersion, c.Version, CurrentTownVersion)
	}
	if c.Name == "" {
		return fmt.Errorf("%w: name", ErrMissingField)
	}
	if c.Dolt != nil && (c.Dolt.Port < 1 || c.Dolt.Port > 65535) {
		return fmt.Errorf("dolt.port %d is not a TCP port", c.Dolt.Port)
	}
	return nil
}

// validateRigsConfig validates a RigsConfig.
func validateRigsConfig(c *RigsConfig) error {
	if c.Version > CurrentRigsVersion {
		return fmt.Errorf("%w: got %d, max supported %d", ErrInvalidVersion, c.Version, CurrentRigsVersion)
	}
	if c.Rigs == nil {
		c.Rigs = make(map[string]RigEntry)
	}
	return nil
}

// LoadRigConfig loads and validates a rig configuration file.
func LoadRigConfig(path string) (*RigConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally, not from user input
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var config RigConfig
	if err := DecodeJSONFile(path, data, &config); err != nil {
		return nil, err
	}

	if err := validateRigConfig(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

// SaveRigConfig saves a rig configuration to a file.
func SaveRigConfig(path string, config *RigConfig) error {
	if err := validateRigConfig(config); err != nil {
		return err
	}

	return WriteConfigJSON(path, config, 0644)
}

// validateRigConfig validates a RigConfig (identity only).
func validateRigConfig(c *RigConfig) error {
	if c.Type != "rig" && c.Type != "" {
		return fmt.Errorf("%w: expected type 'rig', got '%s'", ErrInvalidType, c.Type)
	}
	if c.Version > CurrentRigConfigVersion {
		return fmt.Errorf("%w: got %d, max supported %d", ErrInvalidVersion, c.Version, CurrentRigConfigVersion)
	}
	if c.Name == "" {
		return fmt.Errorf("%w: name", ErrMissingField)
	}
	return nil
}

// validateRigSettings validates a RigSettings.
func validateRigSettings(c *RigSettings) error {
	if c.Type != "rig-settings" && c.Type != "" {
		return fmt.Errorf("%w: expected type 'rig-settings', got '%s'", ErrInvalidType, c.Type)
	}
	if c.Version > CurrentRigSettingsVersion {
		return fmt.Errorf("%w: got %d, max supported %d", ErrInvalidVersion, c.Version, CurrentRigSettingsVersion)
	}
	if c.MergeQueue != nil {
		if err := validateMergeQueueConfig(c.MergeQueue); err != nil {
			return err
		}
	}
	return nil
}

// validateMergeQueueConfig validates a MergeQueueConfig.
func validateMergeQueueConfig(c *MergeQueueConfig) error {
	// Zero is the documented "guard off" value, so only a negative is invalid:
	// a typo'd -1 would silently disable the ceiling the operator meant to set.
	if c.MaxReadyForDispatch < 0 {
		return fmt.Errorf("%w: max_ready_for_dispatch must be non-negative", ErrMissingField)
	}

	return nil
}

// NewRigConfig creates a new RigConfig (identity only).
func NewRigConfig(name, gitURL string) *RigConfig {
	return &RigConfig{
		Type:    "rig",
		Version: CurrentRigConfigVersion,
		Name:    name,
		GitURL:  gitURL,
	}
}

// NewRigSettings creates a new RigSettings with defaults.
func NewRigSettings() *RigSettings {
	return &RigSettings{
		Type:       "rig-settings",
		Version:    CurrentRigSettingsVersion,
		MergeQueue: DefaultMergeQueueConfig(),
		Namepool:   DefaultNamepoolConfig(),
	}
}

// RepoSettingsPath is the conventional path within a repository where
// gastown rig settings can be stored. This file is committed to git and
// provides durable defaults that survive rig re-scaffolding.
const RepoSettingsPath = ".gastown/settings.json"

// LoadRepoSettings loads rig settings from a repository's .gastown/settings.json.
// Returns nil, nil if the file does not exist (repo has no gastown settings).
func LoadRepoSettings(repoRoot string) (*RigSettings, error) {
	path := filepath.Join(repoRoot, RepoSettingsPath)
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading repo settings: %w", err)
	}

	var settings RigSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("parsing repo settings %s: %w", path, err)
	}

	return &settings, nil
}

// MergeSettingsCommand merges a repo-sourced MergeQueueConfig (floor) with
// a local override. Non-empty fields in the override take precedence.
// Returns a new config without mutating either input.
func MergeSettingsCommand(repo, local *MergeQueueConfig) *MergeQueueConfig {
	if repo == nil && local == nil {
		return nil
	}
	result := &MergeQueueConfig{}
	// Start from repo defaults
	if repo != nil {
		*result = *repo
	}
	// Overlay local overrides (non-empty fields win)
	if local != nil {
		if local.SetupCommand != "" {
			result.SetupCommand = local.SetupCommand
		}
		if local.TypecheckCommand != "" {
			result.TypecheckCommand = local.TypecheckCommand
		}
		if local.LintCommand != "" {
			result.LintCommand = local.LintCommand
		}
		if local.TestCommand != "" {
			result.TestCommand = local.TestCommand
		}
		if local.PresubmitCommand != "" {
			result.PresubmitCommand = local.PresubmitCommand
		}
		if local.BuildCommand != "" {
			result.BuildCommand = local.BuildCommand
		}
		// Merge non-command fields from local if explicitly set
		if local.MergeStrategy != "" {
			result.MergeStrategy = local.MergeStrategy
		}
		if local.MaxReadyForDispatch > 0 {
			result.MaxReadyForDispatch = local.MaxReadyForDispatch
		}
		if local.RequireReview != nil {
			result.RequireReview = local.RequireReview
		}
		if local.IntegrationBranchPolecatEnabled != nil {
			result.IntegrationBranchPolecatEnabled = local.IntegrationBranchPolecatEnabled
		}
		if local.Editorial != nil {
			result.Editorial = local.Editorial
		}
	}
	return result
}

// LoadRigSettings loads and validates a rig settings file.
func LoadRigSettings(path string) (*RigSettings, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally, not from user input
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("reading settings: %w", err)
	}

	var settings RigSettings
	if err := DecodeJSONFile(path, data, &settings); err != nil {
		return nil, err
	}

	if err := validateRigSettings(&settings); err != nil {
		return nil, err
	}

	return &settings, nil
}

// DeprecatedMergeQueueKeys lists merge_queue config keys that have been
// removed. Strict decoding rejects them in a rig settings file; gt doctor
// names them and --fix deletes them.
// target_branch and integration_branches were replaced by rig default_branch
// and per-epic integration branch metadata. The rest configured the deleted
// refinery and gt done's old test-verify gate; nothing read them after the
// landing worker replaced both (gt-5nlvq).
var DeprecatedMergeQueueKeys = []string{
	"target_branch", "integration_branches",
	"enabled", "integration_branch_refinery_enabled", "integration_branch_template",
	"integration_branch_auto_land", "vcs_provider", "on_conflict", "run_tests",
	"test_verify_run_timeout", "test_verify_slot_timeout", "test_verify_command",
	"delete_merged_branches", "retry_flaky_tests", "poll_interval", "max_concurrent",
	"stale_claim_timeout", "judgment_enabled", "review_depth", "batch_enabled",
	"batch_min_age", "batch_max", "batch_min_count", "cycle_session_after_merge",
}

// SaveRigSettings saves rig settings to a file.
func SaveRigSettings(path string, settings *RigSettings) error {
	if err := validateRigSettings(settings); err != nil {
		return err
	}

	// 0600 for a new file: agent presets carry API tokens until gt-y3pgh.5.
	return WriteConfigJSON(path, settings, 0o600)
}

// LoadMayorConfig loads and validates a mayor config file.
func LoadMayorConfig(path string) (*MayorConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally, not from user input
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var config MayorConfig
	if err := DecodeJSONFile(path, data, &config); err != nil {
		return nil, err
	}

	if err := validateMayorConfig(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

// SaveMayorConfig saves a mayor config to a file.
func SaveMayorConfig(path string, config *MayorConfig) error {
	if err := validateMayorConfig(config); err != nil {
		return err
	}

	return WriteConfigJSON(path, config, 0644)
}

// validateMayorConfig validates a MayorConfig.
func validateMayorConfig(c *MayorConfig) error {
	if c.Type != "mayor-config" && c.Type != "" {
		return fmt.Errorf("%w: expected type 'mayor-config', got '%s'", ErrInvalidType, c.Type)
	}
	if c.Version > CurrentMayorConfigVersion {
		return fmt.Errorf("%w: got %d, max supported %d", ErrInvalidVersion, c.Version, CurrentMayorConfigVersion)
	}
	return nil
}

// NewMayorConfig creates a new MayorConfig with defaults.
func NewMayorConfig() *MayorConfig {
	return &MayorConfig{
		Type:    "mayor-config",
		Version: CurrentMayorConfigVersion,
	}
}

// DaemonPatrolConfigPath returns the path to the daemon patrol config file.
func DaemonPatrolConfigPath(townRoot string) string {
	return filepath.Join(townRoot, constants.DirMayor, DaemonPatrolConfigFileName)
}

// LoadDaemonPatrolConfig loads and validates a daemon patrol config file.
func LoadDaemonPatrolConfig(path string) (*DaemonPatrolConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("reading daemon patrol config: %w", err)
	}

	var config DaemonPatrolConfig
	if err := DecodeJSONFile(path, data, &config); err != nil {
		return nil, err
	}

	if err := validateDaemonPatrolConfig(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

// SaveDaemonPatrolConfig saves a daemon patrol config to a file.
func SaveDaemonPatrolConfig(path string, config *DaemonPatrolConfig) error {
	if err := validateDaemonPatrolConfig(config); err != nil {
		return err
	}

	return WriteConfigJSON(path, config, 0644)
}

func validateDaemonPatrolConfig(c *DaemonPatrolConfig) error {
	if c.Type != "daemon-patrol-config" && c.Type != "" {
		return fmt.Errorf("%w: expected type 'daemon-patrol-config', got '%s'", ErrInvalidType, c.Type)
	}
	if c.Version > CurrentDaemonPatrolConfigVersion {
		return fmt.Errorf("%w: got %d, max supported %d", ErrInvalidVersion, c.Version, CurrentDaemonPatrolConfigVersion)
	}
	return nil
}

// EnsureDaemonPatrolConfig creates the daemon patrol config if it doesn't
// exist. An existing file is left alone, and one that does not parse is a
// *ParseError: it is neither replaced nor accepted.
func EnsureDaemonPatrolConfig(townRoot string) error {
	return UpdateConfigJSON(DaemonPatrolConfigPath(townRoot), 0o644, func(cfg *DaemonPatrolConfig, exists bool) error {
		if !exists {
			*cfg = *NewDaemonPatrolConfig()
		}
		return nil
	})
}

// LoadAccountsConfig loads and validates an accounts configuration file.
func LoadAccountsConfig(path string) (*AccountsConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally, not from user input
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("reading accounts config: %w", err)
	}

	var config AccountsConfig
	if err := DecodeJSONFile(path, data, &config); err != nil {
		return nil, err
	}

	if err := validateAccountsConfig(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

// SaveAccountsConfig saves an accounts configuration to a file.
func SaveAccountsConfig(path string, config *AccountsConfig) error {
	if err := validateAccountsConfig(config); err != nil {
		return err
	}

	return WriteConfigJSON(path, config, 0644)
}

// validateAccountsConfig validates an AccountsConfig.
func validateAccountsConfig(c *AccountsConfig) error {
	if c.Version > CurrentAccountsVersion {
		return fmt.Errorf("%w: got %d, max supported %d", ErrInvalidVersion, c.Version, CurrentAccountsVersion)
	}
	if c.Accounts == nil {
		c.Accounts = make(map[string]Account)
	}
	// Validate default refers to an existing account (if set and accounts exist)
	if c.Default != "" && len(c.Accounts) > 0 {
		if _, ok := c.Accounts[c.Default]; !ok {
			return fmt.Errorf("%w: default account '%s' not found in accounts", ErrMissingField, c.Default)
		}
	}
	// Validate each account has required fields
	for handle, acct := range c.Accounts {
		if acct.ConfigDir == "" {
			return fmt.Errorf("%w: config_dir for account '%s'", ErrMissingField, handle)
		}
	}
	return nil
}

// NewAccountsConfig creates a new AccountsConfig with defaults.
func NewAccountsConfig() *AccountsConfig {
	return &AccountsConfig{
		Version:  CurrentAccountsVersion,
		Accounts: make(map[string]Account),
	}
}

// GetAccount returns an account by handle, or nil if not found.
func (c *AccountsConfig) GetAccount(handle string) *Account {
	if acct, ok := c.Accounts[handle]; ok {
		return &acct
	}
	return nil
}

// GetDefaultAccount returns the default account, or nil if not set.
func (c *AccountsConfig) GetDefaultAccount() *Account {
	if c.Default == "" {
		return nil
	}
	return c.GetAccount(c.Default)
}

// ResolveAccountConfigDir resolves the CLAUDE_CONFIG_DIR for account selection.
// Priority order:
//  1. GT_ACCOUNT environment variable
//  2. accountFlag (from --account command flag)
//  3. Default account from config
//
// Returns empty string if no account configured or resolved.
// Returns the handle that was resolved as second value.
func ResolveAccountConfigDir(accountsPath, accountFlag string) (configDir, handle string, err error) {
	// Load accounts config
	cfg, loadErr := LoadAccountsConfig(accountsPath)
	if loadErr != nil {
		// No accounts configured - that's OK, return empty
		return "", "", nil
	}

	// Priority 1: GT_ACCOUNT env var
	if envAccount := os.Getenv("GT_ACCOUNT"); envAccount != "" {
		acct := cfg.GetAccount(envAccount)
		if acct == nil {
			return "", "", fmt.Errorf("GT_ACCOUNT '%s' not found in accounts config", envAccount)
		}
		return expandPath(acct.ConfigDir), envAccount, nil
	}

	// Priority 2: --account flag
	if accountFlag != "" {
		acct := cfg.GetAccount(accountFlag)
		if acct == nil {
			return "", "", fmt.Errorf("account '%s' not found in accounts config", accountFlag)
		}
		return expandPath(acct.ConfigDir), accountFlag, nil
	}

	// Priority 3: Default account
	if cfg.Default != "" {
		acct := cfg.GetDefaultAccount()
		if acct != nil {
			return expandPath(acct.ConfigDir), cfg.Default, nil
		}
	}

	return "", "", nil
}

// expandPath expands ~ to home directory.
func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// LoadMessagingConfig loads and validates a messaging configuration file.
func LoadMessagingConfig(path string) (*MessagingConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally, not from user input
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("reading messaging config: %w", err)
	}

	var config MessagingConfig
	if err := DecodeJSONFile(path, data, &config); err != nil {
		return nil, err
	}

	if err := validateMessagingConfig(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

// SaveMessagingConfig saves a messaging configuration to a file.
func SaveMessagingConfig(path string, config *MessagingConfig) error {
	if err := validateMessagingConfig(config); err != nil {
		return err
	}

	return WriteConfigJSON(path, config, 0644)
}

// validateMessagingConfig validates a MessagingConfig.
func validateMessagingConfig(c *MessagingConfig) error {
	if c.Type != "messaging" && c.Type != "" {
		return fmt.Errorf("%w: expected type 'messaging', got '%s'", ErrInvalidType, c.Type)
	}
	if c.Version > CurrentMessagingVersion {
		return fmt.Errorf("%w: got %d, max supported %d", ErrInvalidVersion, c.Version, CurrentMessagingVersion)
	}

	// Initialize nil maps
	if c.Lists == nil {
		c.Lists = make(map[string][]string)
	}
	if c.Queues == nil {
		c.Queues = make(map[string]QueueConfig)
	}
	if c.Announces == nil {
		c.Announces = make(map[string]AnnounceConfig)
	}
	if c.NudgeChannels == nil {
		c.NudgeChannels = make(map[string][]string)
	}

	// Validate lists have at least one recipient
	for name, recipients := range c.Lists {
		if len(recipients) == 0 {
			return fmt.Errorf("%w: list '%s' has no recipients", ErrMissingField, name)
		}
	}

	// Validate queues have at least one worker
	for name, queue := range c.Queues {
		if len(queue.Workers) == 0 {
			return fmt.Errorf("%w: queue '%s' workers", ErrMissingField, name)
		}
		if queue.MaxClaims < 0 {
			return fmt.Errorf("%w: queue '%s' max_claims must be non-negative", ErrMissingField, name)
		}
	}

	// Validate announces have at least one reader
	for name, announce := range c.Announces {
		if len(announce.Readers) == 0 {
			return fmt.Errorf("%w: announce '%s' readers", ErrMissingField, name)
		}
		if announce.RetainCount < 0 {
			return fmt.Errorf("%w: announce '%s' retain_count must be non-negative", ErrMissingField, name)
		}
	}

	// Validate nudge channels have non-empty names and at least one recipient
	for name, recipients := range c.NudgeChannels {
		if name == "" {
			return fmt.Errorf("%w: nudge channel name cannot be empty", ErrMissingField)
		}
		if len(recipients) == 0 {
			return fmt.Errorf("%w: nudge channel '%s' has no recipients", ErrMissingField, name)
		}
	}

	return nil
}

// MessagingConfigPath returns the standard path for messaging config in a town.
func MessagingConfigPath(townRoot string) string {
	return filepath.Join(townRoot, "config", "messaging.json")
}

// LoadOrCreateMessagingConfig loads the messaging config, creating a default if not found.
func LoadOrCreateMessagingConfig(path string) (*MessagingConfig, error) {
	config, err := LoadMessagingConfig(path)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return NewMessagingConfig(), nil
		}
		return nil, err
	}
	return config, nil
}

// TownSettingsPath returns the path to town settings file.
func TownSettingsPath(townRoot string) string {
	return filepath.Join(townRoot, "settings", "config.json")
}

// RigSettingsPath returns the path to rig settings file.
func RigSettingsPath(rigPath string) string {
	return filepath.Join(rigPath, "settings", "config.json")
}

// LoadOrCreateTownSettings loads town settings or creates defaults if missing.
func LoadOrCreateTownSettings(path string) (*TownSettings, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally
	if err != nil {
		if os.IsNotExist(err) {
			return NewTownSettings(), nil
		}
		return nil, err
	}

	var settings TownSettings
	if err := DecodeJSONFile(path, data, &settings); err != nil {
		return nil, err
	}
	return &settings, nil
}

// SaveTownSettings saves town settings to a file.
func SaveTownSettings(path string, settings *TownSettings) error {
	if settings.Type != "town-settings" && settings.Type != "" {
		return fmt.Errorf("%w: expected type 'town-settings', got '%s'", ErrInvalidType, settings.Type)
	}
	if settings.Version > CurrentTownSettingsVersion {
		return fmt.Errorf("%w: got %d, max supported %d", ErrInvalidVersion, settings.Version, CurrentTownSettingsVersion)
	}

	// 0600 for a new file: agent presets carry API tokens until gt-y3pgh.5.
	return WriteConfigJSON(path, settings, 0o600)
}

// ResolveAgentConfig resolves the agent configuration for a rig.
// It looks up the agent by name in town settings (custom agents) and built-in presets.
//
// Resolution order:
//  1. If rig has Runtime set directly, use it (backwards compatibility)
//  2. If rig has Agent set, look it up in:
//     a. Town's custom agents (from TownSettings.Agents)
//     b. Built-in presets (claude, groq-compound)
//  3. If rig has no Agent set, use town's default_agent
//  4. Fall back to claude defaults
//
// townRoot is the path to the town directory (e.g., ~/gt).
// rigPath is the path to the rig directory (e.g., ~/gt/gastown).
func ResolveAgentConfig(townRoot, rigPath string) *RuntimeConfig {
	return resolveAgentConfig(processHost, townRoot, rigPath)
}

func resolveAgentConfig(h host, townRoot, rigPath string) *RuntimeConfig {
	resolveConfigMu.Lock()
	defer resolveConfigMu.Unlock()
	return resolveAgentConfigInternal(agentRegistryFor(h, townRoot, rigPath), townRoot, rigPath)
}

// resolveAgentConfigInternal is the lock-free version of ResolveAgentConfig.
// Caller must hold resolveConfigMu.
func resolveAgentConfigInternal(reg *AgentRegistry, townRoot, rigPath string) *RuntimeConfig {
	// Load rig settings
	rigSettings, err := LoadRigSettings(RigSettingsPath(rigPath))
	if err != nil {
		rigSettings = nil
	}

	// Backwards compatibility: if Runtime is set directly, use it
	if rigSettings != nil && rigSettings.Runtime != nil {
		rc := fillRuntimeDefaultsIn(reg, rigSettings.Runtime)
		if rc.ResolvedAgent == "" {
			rc.ResolvedAgent = inferAgentName(rc)
		}
		return rc
	}

	// Load town settings for agent lookup
	townSettings, err := LoadOrCreateTownSettings(TownSettingsPath(townRoot))
	if err != nil {
		townSettings = NewTownSettings()
	}

	// Determine which agent name to use
	agentName := ""
	if rigSettings != nil && rigSettings.Agent != "" {
		agentName = rigSettings.Agent
	} else if townSettings.DefaultAgent != "" {
		agentName = townSettings.DefaultAgent
	} else {
		agentName = "claude" // ultimate fallback
	}

	rc := lookupAgentConfig(reg, agentName, townSettings, rigSettings)
	rc.ResolvedAgent = agentName
	return rc
}

// ResolveAgentConfigWithOverride resolves the agent configuration for a rig, with an optional override.
// If agentOverride is non-empty, it is used instead of rig/town defaults.
// Returns the resolved RuntimeConfig, the selected agent name, and an error if the override name
// does not exist in town custom agents or built-in presets.
func ResolveAgentConfigWithOverride(townRoot, rigPath, agentOverride string) (*RuntimeConfig, string, error) {
	return resolveAgentConfigWithOverride(processHost, townRoot, rigPath, agentOverride)
}

func resolveAgentConfigWithOverride(h host, townRoot, rigPath, agentOverride string) (*RuntimeConfig, string, error) {
	resolveConfigMu.Lock()
	defer resolveConfigMu.Unlock()
	return resolveAgentConfigWithOverrideInternal(agentRegistryFor(h, townRoot, rigPath), townRoot, rigPath, agentOverride)
}

// resolveAgentConfigWithOverrideInternal is the lock-free version.
// Caller must hold resolveConfigMu.
func resolveAgentConfigWithOverrideInternal(reg *AgentRegistry, townRoot, rigPath, agentOverride string) (*RuntimeConfig, string, error) {
	// Load rig settings
	rigSettings, err := LoadRigSettings(RigSettingsPath(rigPath))
	if err != nil {
		rigSettings = nil
	}

	// Backwards compatibility: if Runtime is set directly, use it (but still report agentOverride if present)
	if rigSettings != nil && rigSettings.Runtime != nil && agentOverride == "" {
		rc := fillRuntimeDefaultsIn(reg, rigSettings.Runtime)
		if rc.ResolvedAgent == "" {
			rc.ResolvedAgent = inferAgentName(rc)
		}
		return rc, "", nil
	}

	// Load town settings for agent lookup
	townSettings, err := LoadOrCreateTownSettings(TownSettingsPath(townRoot))
	if err != nil {
		townSettings = NewTownSettings()
	}

	// Determine which agent name to use
	agentName := ""
	var extraArgs []string
	if agentOverride != "" {
		// Handle agent overrides with extra args (e.g., "claude --model opus")
		parts := strings.Fields(agentOverride)
		if len(parts) > 0 {
			agentName = parts[0]
			if len(parts) > 1 {
				extraArgs = parts[1:]
			}
		}
	} else if rigSettings != nil && rigSettings.Agent != "" {
		agentName = rigSettings.Agent
	} else if townSettings.DefaultAgent != "" {
		agentName = townSettings.DefaultAgent
	} else {
		agentName = "claude" // ultimate fallback
	}

	// If an override is requested, validate it exists
	if agentOverride != "" {
		var rc *RuntimeConfig
		// Check rig-level custom agents first
		if rigSettings != nil && rigSettings.Agents != nil {
			if custom, ok := rigSettings.Agents[agentName]; ok && custom != nil {
				rc = fillRuntimeDefaultsIn(reg, custom)
			}
		}
		// Then check town-level custom agents
		if rc == nil && townSettings.Agents != nil {
			if custom, ok := townSettings.Agents[agentName]; ok && custom != nil {
				rc = fillRuntimeDefaultsIn(reg, custom)
			}
		}
		// Then check the registry (built-in presets plus settings/agents.json)
		if rc == nil {
			if preset := reg.Preset(agentName); preset != nil {
				rc = reg.RuntimeConfigFromPreset(AgentPreset(agentName))
			}
		}

		if rc == nil {
			return nil, "", fmt.Errorf("agent '%s' not found", agentName)
		}

		rc.ResolvedAgent = agentName

		// Append extra arguments from the override
		if len(extraArgs) > 0 {
			rc.Args = append(rc.Args, extraArgs...)
		}
		return rc, agentName, nil
	}

	// Normal lookup path (no override)
	rc := lookupAgentConfig(reg, agentName, townSettings, rigSettings)
	rc.ResolvedAgent = agentName

	// If we have extra arguments from the override, append them to the config
	if len(extraArgs) > 0 {
		rc.Args = append(rc.Args, extraArgs...)
	}

	return rc, agentName, nil
}

// ValidateAgentConfig checks if an agent configuration is valid, the binary
// exists, and the env it references can be resolved.
// Returns an error describing the issue, or nil if valid.
func ValidateAgentConfig(reg *AgentRegistry, agentName string, townSettings *TownSettings, rigSettings *RigSettings) error {
	// Check if agent exists in config
	rc := lookupAgentConfigIfExists(reg, agentName, townSettings, rigSettings)
	if rc == nil {
		return fmt.Errorf("agent %q not found in config or built-in presets", agentName)
	}

	// Check if binary exists on system
	if _, err := reg.host().lookPath(rc.Command); err != nil {
		return fmt.Errorf("agent %q binary %q not found in PATH", agentName, rc.Command)
	}

	// An unset ${VAR} reference would expand to an empty credential and fail
	// only once the agent is already running, so report it here — callers
	// treat this as "unusable in this environment" and fall back, which beats
	// an auth error in a tmux pane nobody is watching (gt-yih1). This covers
	// an agent named directly (the builtin preset); an agent resolved out of
	// settings skips this function, and BuildStartupCommandWithAgentOverride
	// stops the spawn instead.
	if missing := unsetEnvRefs(rc.Env, reg.host().getenv); len(missing) > 0 {
		return fmt.Errorf("agent %q env references %s, which is not set in the environment",
			agentName, strings.Join(missing, ", "))
	}

	return nil
}

// unsetEnvRefs returns the ${VAR} names referenced by env values that getenv
// has no value for, deduplicated and sorted.
func unsetEnvRefs(env map[string]string, getenv func(string) string) []string {
	seen := make(map[string]bool)
	var missing []string
	for _, v := range env {
		for _, name := range envRefNames(v) {
			if getenv(name) != "" || seen[name] {
				continue
			}
			seen[name] = true
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// lookupAgentConfigIfExists looks up an agent by name but returns nil if not found
// (instead of falling back to default). Used for validation.
func lookupAgentConfigIfExists(reg *AgentRegistry, name string, townSettings *TownSettings, rigSettings *RigSettings) *RuntimeConfig {
	// Check rig's custom agents
	if rigSettings != nil && rigSettings.Agents != nil {
		if custom, ok := rigSettings.Agents[name]; ok && custom != nil {
			return fillRuntimeDefaultsIn(reg, custom)
		}
	}

	// Check town's custom agents
	if townSettings != nil && townSettings.Agents != nil {
		if custom, ok := townSettings.Agents[name]; ok && custom != nil {
			return fillRuntimeDefaultsIn(reg, custom)
		}
	}

	// Check the registry (built-in presets plus settings/agents.json)
	if preset := reg.Preset(name); preset != nil {
		return reg.RuntimeConfigFromPreset(AgentPreset(name))
	}

	// Cost-tier preset names. ApplyCostTier persists RoleAgents entries such as
	// "claude-haiku" and "claude-sonnet", but those are constructors here rather
	// than registry presets — and the standard tier does not write them into
	// settings.Agents at all. Without this fallback the entry resolves to nil,
	// logs a "not found" warning, and the tier mapping silently does nothing.
	// Settings lookups above win, so a user-defined agent of the same name still
	// takes precedence.
	if rc := costTierPresetByName(name); rc != nil {
		return fillRuntimeDefaultsIn(reg, rc)
	}

	return nil
}

// costTierPresetByName returns the RuntimeConfig for a cost-tier preset name,
// or nil if the name is not one of the tier-managed Claude presets.
func costTierPresetByName(name string) *RuntimeConfig {
	switch name {
	case "claude-haiku":
		return claudeHaikuPreset()
	case "claude-sonnet":
		return claudeSonnetPreset()
	default:
		return nil
	}
}

// ResolveRoleAgentConfig resolves the agent configuration for a specific role.
// It checks role-specific agent assignments before falling back to the default agent.
//
// Resolution order:
//  1. Rig's RoleAgents[role] - if set, look up that agent
//  2. Town's RoleAgents[role] - if set, look up that agent
//  3. Fall back to ResolveAgentConfig (rig's Agent → town's DefaultAgent → "claude")
//
// If a configured agent is not found or its binary doesn't exist, a warning is
// printed to stderr and it falls back to the default agent.
//
// role is one of: "mayor", "deacon", "witness", "refinery", "polecat", "crew", "boot".
// townRoot is the path to the town directory (e.g., ~/gt).
// rigPath is the path to the rig directory (e.g., ~/gt/gastown), or empty for town-level roles.
func ResolveRoleAgentConfig(role, townRoot, rigPath string) *RuntimeConfig {
	return resolveRoleAgentConfig(processHost, role, townRoot, rigPath)
}

func resolveRoleAgentConfig(h host, role, townRoot, rigPath string) *RuntimeConfig {
	resolveConfigMu.Lock()
	defer resolveConfigMu.Unlock()
	reg := agentRegistryFor(h, townRoot, rigPath)
	rc := resolveRoleAgentConfigCore(reg, role, townRoot, rigPath)
	rc = withRoleSettingsFlag(rc, role, rigPath)
	return withRoleSystemPromptFlag(reg, rc, role, townRoot, rigPath, "")
}

// tryResolveNamedAgent attempts to resolve a named agent through the custom agent
// and standard lookup pipelines. Returns the resolved config with ResolvedAgent set,
// or nil if validation fails. The warnPrefix is used in the fallback warning message
// (e.g., "worker_agents[denali]" or "crew_agents[denali]").
func tryResolveNamedAgent(reg *AgentRegistry, agentName, warnPrefix string, townSettings *TownSettings, rigSettings *RigSettings) *RuntimeConfig {
	if rc := lookupCustomAgentConfig(reg, agentName, townSettings, rigSettings); rc != nil {
		rc.ResolvedAgent = agentName
		return rc
	}
	if err := ValidateAgentConfig(reg, agentName, townSettings, rigSettings); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %s=%s - %v, falling back\n", warnPrefix, agentName, err)
		return nil
	}
	rc := lookupAgentConfig(reg, agentName, townSettings, rigSettings)
	rc.ResolvedAgent = agentName
	return rc
}

// ResolveWorkerAgentConfig resolves the agent configuration for a named crew worker.
// Resolution order:
//  1. Rig's WorkerAgents[workerName] — per-worker override
//  2. Town's CrewAgents[workerName] — town-wide per-crew override
//  3. Falls back to ResolveRoleAgentConfig("crew", ...) for remaining resolution
//
// workerName is the crew member name (e.g., "denali").
func ResolveWorkerAgentConfig(workerName, townRoot, rigPath string) *RuntimeConfig {
	return resolveWorkerAgentConfig(processHost, workerName, townRoot, rigPath)
}

func resolveWorkerAgentConfig(h host, workerName, townRoot, rigPath string) *RuntimeConfig {
	resolveConfigMu.Lock()
	defer resolveConfigMu.Unlock()
	reg := agentRegistryFor(h, townRoot, rigPath)

	// Tier 1: rig's per-worker override
	if workerName != "" && rigPath != "" {
		if rigSettings, err := LoadRigSettings(RigSettingsPath(rigPath)); err == nil && rigSettings != nil {
			if agentName, ok := rigSettings.WorkerAgents[workerName]; ok && agentName != "" {
				townSettings, err := LoadOrCreateTownSettings(TownSettingsPath(townRoot))
				if err != nil {
					townSettings = NewTownSettings()
				}
				if rc := tryResolveNamedAgent(reg, agentName, fmt.Sprintf("worker_agents[%s]", workerName), townSettings, rigSettings); rc != nil {
					return withRoleSystemPromptFlag(reg, withRoleSettingsFlag(rc, "crew", rigPath), "crew", townRoot, rigPath, workerName)
				}
			}
		}
	}

	// Tier 2: town's per-crew override
	if workerName != "" && townRoot != "" {
		townSettings, err := LoadOrCreateTownSettings(TownSettingsPath(townRoot))
		if err == nil && townSettings != nil {
			if agentName, ok := townSettings.CrewAgents[workerName]; ok && agentName != "" {
				var rigSettings *RigSettings
				if rigPath != "" {
					rigSettings, _ = LoadRigSettings(RigSettingsPath(rigPath))
				}
				if rc := tryResolveNamedAgent(reg, agentName, fmt.Sprintf("crew_agents[%s]", workerName), townSettings, rigSettings); rc != nil {
					return withRoleSystemPromptFlag(reg, withRoleSettingsFlag(rc, "crew", rigPath), "crew", townRoot, rigPath, workerName)
				}
			}
		}
	}

	// Tier 3: fall back to crew role resolution (already holds lock; use core function)
	rc := resolveRoleAgentConfigCore(reg, "crew", townRoot, rigPath)
	return withRoleSystemPromptFlag(reg, withRoleSettingsFlag(rc, "crew", rigPath), "crew", townRoot, rigPath, workerName)
}

// ResolveRoleEffort resolves the effort level for a role.
// Resolution order:
//  1. Rig's RoleEffort[role]
//  2. Town's RoleEffort[role] (gt config cost-tier writes it)
//  3. Returns "" (caller falls back to the default "high")
//
// Invalid effort levels are warned about and skipped.
func ResolveRoleEffort(role, townRoot, rigPath string) string {
	// Rig-level override
	if rigPath != "" {
		if rigSettings, err := LoadRigSettings(RigSettingsPath(rigPath)); err == nil && rigSettings != nil {
			if effort, ok := rigSettings.RoleEffort[role]; ok && effort != "" {
				if !IsValidEffortLevel(effort) {
					fmt.Fprintf(os.Stderr, "warning: rig role_effort[%s]=%q is not a valid effort level, ignoring\n", role, effort)
				} else {
					return effort
				}
			}
		}
	}

	// Town-level setting
	if townRoot != "" {
		if townSettings, err := LoadOrCreateTownSettings(TownSettingsPath(townRoot)); err == nil && townSettings != nil {
			if effort, ok := townSettings.RoleEffort[role]; ok && effort != "" {
				if !IsValidEffortLevel(effort) {
					fmt.Fprintf(os.Stderr, "warning: town role_effort[%s]=%q is not a valid effort level, ignoring\n", role, effort)
				} else {
					return effort
				}
			}
		}
	}

	return "" // Caller uses env var fallback, then "high" default
}

// withRoleSettingsFlag appends --settings to the Args for roles whose
// settings directory differs from the session working directory. Claude Code
// resolves project-level settings from its working directory only; the --settings
// flag tells it where to find them when they live in a parent directory.
func withRoleSettingsFlag(rc *RuntimeConfig, role, rigPath string) *RuntimeConfig {
	if rc == nil || rigPath == "" {
		return rc
	}

	// Guard against double-adding (ResolveRoleAgentConfig already calls this)
	for _, arg := range rc.Args {
		if arg == "--settings" {
			return rc
		}
	}

	settingsDir := RoleSettingsDir(role, rigPath)
	if settingsDir == "" {
		return rc
	}

	rc.Args = append(rc.Args, "--settings", filepath.Join(settingsDir, ".claude", "settings.json"))
	return rc
}

// RoleSettingsDir returns the shared settings directory for roles whose session
// working directory differs from their settings location. Returns empty for
// roles where settings and session directory are the same (mayor).
func RoleSettingsDir(role, rigPath string) string {
	switch role {
	case constants.RoleCrew:
		return filepath.Join(rigPath, role)
	case constants.RolePolecat:
		return filepath.Join(rigPath, "polecats")
	default:
		return ""
	}
}

func resolveRoleAgentConfigCore(reg *AgentRegistry, role, townRoot, rigPath string) *RuntimeConfig {
	// Load rig settings (may be nil for town-level roles like mayor/deacon)
	var rigSettings *RigSettings
	if rigPath != "" {
		var err error
		rigSettings, err = LoadRigSettings(RigSettingsPath(rigPath))
		if err != nil {
			rigSettings = nil
		}
	}

	// Load town settings
	townSettings, err := LoadOrCreateTownSettings(TownSettingsPath(townRoot))
	if err != nil {
		townSettings = NewTownSettings()
	}

	// Check rig's RoleAgents first
	if rigSettings != nil && rigSettings.RoleAgents != nil {
		if agentName, ok := rigSettings.RoleAgents[role]; ok && agentName != "" {
			if rc := lookupCustomAgentConfig(reg, agentName, townSettings, rigSettings); rc != nil {
				rc.ResolvedAgent = agentName
				return rc
			}
			if err := ValidateAgentConfig(reg, agentName, townSettings, rigSettings); err != nil {
				fmt.Fprintf(os.Stderr, "warning: role_agents[%s]=%s - %v, falling back to default\n", role, agentName, err)
			} else {
				rc := lookupAgentConfig(reg, agentName, townSettings, rigSettings)
				rc.ResolvedAgent = agentName
				return rc
			}
		}
	}

	// Check town's RoleAgents
	if townSettings.RoleAgents != nil {
		if agentName, ok := townSettings.RoleAgents[role]; ok && agentName != "" {
			if rc := lookupCustomAgentConfig(reg, agentName, townSettings, rigSettings); rc != nil {
				rc.ResolvedAgent = agentName
				return rc
			}
			if err := ValidateAgentConfig(reg, agentName, townSettings, rigSettings); err != nil {
				fmt.Fprintf(os.Stderr, "warning: role_agents[%s]=%s - %v, falling back to default\n", role, agentName, err)
			} else {
				rc := lookupAgentConfig(reg, agentName, townSettings, rigSettings)
				rc.ResolvedAgent = agentName
				return rc
			}
		}
	}

	// Fall back to existing resolution (rig's Agent → town's DefaultAgent → "claude")
	// Use internal version — caller already holds resolveConfigMu.
	return resolveAgentConfigInternal(reg, townRoot, rigPath)
}

// ResolveRoleAgentName returns the agent name that would be used for a specific role.
// This is useful for logging and diagnostics.
// Returns the agent name and whether it came from role-specific configuration.
func ResolveRoleAgentName(role, townRoot, rigPath string) (agentName string, isRoleSpecific bool) {
	// Load rig settings
	var rigSettings *RigSettings
	if rigPath != "" {
		var err error
		rigSettings, err = LoadRigSettings(RigSettingsPath(rigPath))
		if err != nil {
			rigSettings = nil
		}
	}

	// Load town settings
	townSettings, err := LoadOrCreateTownSettings(TownSettingsPath(townRoot))
	if err != nil {
		townSettings = NewTownSettings()
	}

	// Check rig's RoleAgents first
	if rigSettings != nil && rigSettings.RoleAgents != nil {
		if name, ok := rigSettings.RoleAgents[role]; ok && name != "" {
			return name, true
		}
	}

	// Check town's RoleAgents
	if townSettings.RoleAgents != nil {
		if name, ok := townSettings.RoleAgents[role]; ok && name != "" {
			return name, true
		}
	}

	// Fall back to existing resolution
	if rigSettings != nil && rigSettings.Agent != "" {
		return rigSettings.Agent, false
	}
	if townSettings.DefaultAgent != "" {
		return townSettings.DefaultAgent, false
	}
	return "claude", false
}

// ResolveAgentConfigByName looks up an agent's RuntimeConfig by name without requiring
// the agent binary to be installed. Checks custom agents first, then built-in presets.
// Returns nil if the agent name is unknown. Used by hooks sync, which needs the preset's
// hooks metadata regardless of whether the binary is installed on this machine.
func ResolveAgentConfigByName(name, townRoot, rigPath string) *RuntimeConfig {
	resolveConfigMu.Lock()
	defer resolveConfigMu.Unlock()

	var rigSettings *RigSettings
	if rigPath != "" {
		if rs, err := LoadRigSettings(RigSettingsPath(rigPath)); err == nil {
			rigSettings = rs
		}
	}

	townSettings, err := LoadOrCreateTownSettings(TownSettingsPath(townRoot))
	if err != nil {
		townSettings = NewTownSettings()
	}

	return lookupAgentConfigIfExists(AgentRegistryFor(townRoot, rigPath), name, townSettings, rigSettings)
}

// HasExplicitRoleAgent returns true if role_agents (rig or town level)
// explicitly maps this role to a named agent. This distinguishes between
// "role_agents says use claude-sonnet" and "no role_agents entry, falling
// back to defaults". When an explicit mapping exists, the TOML start_command
// should be skipped in favor of BuildStartupCommandFromConfig which honors
// the model/settings from the mapped agent definition.
func HasExplicitRoleAgent(role, townRoot, rigPath string) bool {
	_, isRoleSpecific := ResolveRoleAgentName(role, townRoot, rigPath)
	return isRoleSpecific
}

// lookupAgentConfig looks up an agent by name.
// Checks rig-level custom agents first, then town's custom agents, then built-in presets from agents.go.
// Falls back to DefaultRuntimeConfig() if no match is found.
func lookupAgentConfig(reg *AgentRegistry, name string, townSettings *TownSettings, rigSettings *RigSettings) *RuntimeConfig {
	if rc := lookupAgentConfigIfExists(reg, name, townSettings, rigSettings); rc != nil {
		return rc
	}
	return defaultRuntimeConfigIn(reg)
}

// lookupCustomAgentConfig looks up custom agents only (rig or town).
// It skips binary validation so tests and config resolution can proceed
// even if the command isn't on PATH yet.
func lookupCustomAgentConfig(reg *AgentRegistry, name string, townSettings *TownSettings, rigSettings *RigSettings) *RuntimeConfig {
	if rigSettings != nil && rigSettings.Agents != nil {
		if custom, ok := rigSettings.Agents[name]; ok && custom != nil {
			return fillRuntimeDefaultsIn(reg, custom)
		}
	}

	if townSettings != nil && townSettings.Agents != nil {
		if custom, ok := townSettings.Agents[name]; ok && custom != nil {
			return fillRuntimeDefaultsIn(reg, custom)
		}
	}

	return nil
}

// fillRuntimeDefaults fills in default values for empty RuntimeConfig fields.
// It creates a deep copy to prevent mutation of the original config.
//
// Default behavior:
//   - Command defaults to "claude" if empty
//   - Args defaults to ["--dangerously-skip-permissions"] if nil
//   - Empty Args slice ([]string{}) means "no args" and is preserved as-is
//
// All fields are deep-copied: modifying the returned config will not affect
// the input config, including nested structs and slices.
func fillRuntimeDefaults(rc *RuntimeConfig) *RuntimeConfig {
	return fillRuntimeDefaultsIn(nil, rc)
}

// fillRuntimeDefaultsIn is fillRuntimeDefaults with preset defaults taken from
// reg (nil means the built-in presets).
func fillRuntimeDefaultsIn(reg *AgentRegistry, rc *RuntimeConfig) *RuntimeConfig {
	if rc == nil {
		return defaultRuntimeConfigIn(reg)
	}

	// Create result with scalar fields (strings are immutable in Go)
	result := &RuntimeConfig{
		Provider:      rc.Provider,
		Command:       rc.Command,
		InitialPrompt: rc.InitialPrompt,
		ResolvedAgent: rc.ResolvedAgent,
	}

	// Deep copy Args slice to avoid sharing backing array
	if rc.Args != nil {
		result.Args = make([]string, len(rc.Args))
		copy(result.Args, rc.Args)
	}

	// Deep copy ExecWrapper slice
	if rc.ExecWrapper != nil {
		result.ExecWrapper = make([]string, len(rc.ExecWrapper))
		copy(result.ExecWrapper, rc.ExecWrapper)
	}

	// Deep copy Env map
	if len(rc.Env) > 0 {
		result.Env = make(map[string]string, len(rc.Env))
		for k, v := range rc.Env {
			result.Env[k] = v
		}
	}

	// Deep copy nested structs (nil checks prevent panic on access)
	if rc.Session != nil {
		result.Session = &RuntimeSessionConfig{
			SessionIDEnv: rc.Session.SessionIDEnv,
			ConfigDirEnv: rc.Session.ConfigDirEnv,
		}
	}

	if rc.Tmux != nil {
		result.Tmux = &RuntimeTmuxConfig{
			ReadyPromptPrefix: rc.Tmux.ReadyPromptPrefix,
			ReadyDelayMs:      rc.Tmux.ReadyDelayMs,
		}
		// Deep copy ProcessNames slice
		if rc.Tmux.ProcessNames != nil {
			result.Tmux.ProcessNames = make([]string, len(rc.Tmux.ProcessNames))
			copy(result.Tmux.ProcessNames, rc.Tmux.ProcessNames)
		}
	}

	if rc.Instructions != nil {
		result.Instructions = &RuntimeInstructionsConfig{
			File: rc.Instructions.File,
		}
	}

	// Resolve preset for data-driven defaults.
	// Use provider if set, otherwise try to match by command name.
	presetName := result.Provider
	if presetName == "" && result.Command != "" {
		presetName = result.Command
	}
	preset := reg.Preset(presetName)
	if preset == nil {
		preset = reg.Preset(string(AgentClaude)) // fall back to Claude defaults
	}

	// Apply defaults for required fields from preset
	if result.Command == "" && preset != nil {
		result.Command = preset.Command
	}
	if result.Args == nil && preset != nil {
		result.Args = append([]string(nil), preset.Args...)
	}

	// Auto-fill Session defaults from preset.
	if result.Session == nil && preset != nil && (preset.SessionIDEnv != "" || preset.ConfigDirEnv != "") {
		result.Session = &RuntimeSessionConfig{
			SessionIDEnv: preset.SessionIDEnv,
			ConfigDirEnv: preset.ConfigDirEnv,
		}
	}

	// Auto-fill Tmux defaults from preset (process detection, readiness).
	if result.Tmux == nil && preset != nil && (len(preset.ProcessNames) > 0 || preset.ReadyPromptPrefix != "" || preset.ReadyDelayMs > 0) {
		result.Tmux = &RuntimeTmuxConfig{
			ProcessNames:      append([]string(nil), preset.ProcessNames...),
			ReadyPromptPrefix: preset.ReadyPromptPrefix,
			ReadyDelayMs:      preset.ReadyDelayMs,
		}
	}

	// Auto-fill Instructions defaults from preset.
	if result.Instructions == nil && preset != nil && preset.InstructionsFile != "" {
		result.Instructions = &RuntimeInstructionsConfig{
			File: preset.InstructionsFile,
		}
	}

	// Auto-fill Session defaults from preset when not explicitly set.
	// Custom agents (e.g., "claude-opus" with Command:"claude") inherit
	// SessionIDEnv/ConfigDirEnv from the matched preset, enabling session
	// resume and GT_SESSION_ID_ENV propagation in handoffs.
	if result.Session == nil && preset != nil && (preset.SessionIDEnv != "" || preset.ConfigDirEnv != "") {
		result.Session = &RuntimeSessionConfig{
			SessionIDEnv: preset.SessionIDEnv,
			ConfigDirEnv: preset.ConfigDirEnv,
		}
	}

	// Auto-fill Tmux defaults from preset for process detection and readiness.
	// Custom agents matching a known preset by command (e.g., "claude-opus" →
	// claude preset) get ProcessNames and ReadyPromptPrefix needed for
	// WaitForRuntimeReady to detect agent startup correctly.
	if result.Tmux == nil && preset != nil && (len(preset.ProcessNames) > 0 || preset.ReadyPromptPrefix != "" || preset.ReadyDelayMs > 0) {
		result.Tmux = &RuntimeTmuxConfig{
			ReadyPromptPrefix: preset.ReadyPromptPrefix,
			ReadyDelayMs:      preset.ReadyDelayMs,
		}
		if len(preset.ProcessNames) > 0 {
			result.Tmux.ProcessNames = append([]string(nil), preset.ProcessNames...)
		}
	}

	// Auto-fill Env defaults from preset.
	if preset != nil && len(preset.Env) > 0 {
		if result.Env == nil {
			result.Env = make(map[string]string)
		}
		for k, v := range preset.Env {
			if _, ok := result.Env[k]; !ok {
				result.Env[k] = v
			}
		}
	}

	return result
}

// inferAgentName determines the agent name from a legacy RuntimeConfig.
// It mirrors the preset resolution logic in fillRuntimeDefaults:
// use Provider if set, otherwise Command, falling back to "claude".
func inferAgentName(rc *RuntimeConfig) string {
	if rc.Provider != "" {
		return rc.Provider
	}
	if rc.Command != "" {
		return rc.Command
	}
	return "claude"
}

// GetRuntimeCommand is a convenience function that returns the full command string
// for starting an LLM session. It resolves the agent config and builds the command.
func GetRuntimeCommand(rigPath string) string {
	if rigPath == "" {
		// Try to detect town root from cwd for town-level agents (mayor, deacon)
		townRoot, err := findTownRootFromCwd(processHost)
		if err != nil {
			return DefaultRuntimeConfig().BuildCommand()
		}
		return ResolveAgentConfig(townRoot, "").BuildCommand()
	}
	// Derive town root from rig path (rig is typically ~/gt/<rigname>)
	townRoot := filepath.Dir(rigPath)
	return ResolveAgentConfig(townRoot, rigPath).BuildCommand()
}

// GetRuntimeCommandWithAgentOverride returns the full command for starting an LLM session,
// using agentOverride if non-empty.
func GetRuntimeCommandWithAgentOverride(rigPath, agentOverride string) (string, error) {
	if rigPath == "" {
		townRoot, err := findTownRootFromCwd(processHost)
		if err != nil {
			return DefaultRuntimeConfig().BuildCommand(), nil
		}
		rc, _, resolveErr := ResolveAgentConfigWithOverride(townRoot, "", agentOverride)
		if resolveErr != nil {
			return "", resolveErr
		}
		return rc.BuildCommand(), nil
	}

	townRoot := filepath.Dir(rigPath)
	rc, _, err := ResolveAgentConfigWithOverride(townRoot, rigPath, agentOverride)
	if err != nil {
		return "", err
	}
	return rc.BuildCommand(), nil
}

// GetRuntimeCommandWithPrompt returns the full command with an initial prompt.
func GetRuntimeCommandWithPrompt(rigPath, prompt string) string {
	if rigPath == "" {
		// Try to detect town root from cwd for town-level agents (mayor, deacon)
		townRoot, err := findTownRootFromCwd(processHost)
		if err != nil {
			return DefaultRuntimeConfig().BuildCommandWithPrompt(prompt)
		}
		return ResolveAgentConfig(townRoot, "").BuildCommandWithPrompt(prompt)
	}
	townRoot := filepath.Dir(rigPath)
	return ResolveAgentConfig(townRoot, rigPath).BuildCommandWithPrompt(prompt)
}

// GetRuntimeCommandWithPromptAndAgentOverride returns the full command with an initial prompt,
// using agentOverride if non-empty.
func GetRuntimeCommandWithPromptAndAgentOverride(rigPath, prompt, agentOverride string) (string, error) {
	if rigPath == "" {
		townRoot, err := findTownRootFromCwd(processHost)
		if err != nil {
			return DefaultRuntimeConfig().BuildCommandWithPrompt(prompt), nil
		}
		rc, _, resolveErr := ResolveAgentConfigWithOverride(townRoot, "", agentOverride)
		if resolveErr != nil {
			return "", resolveErr
		}
		return rc.BuildCommandWithPrompt(prompt), nil
	}

	townRoot := filepath.Dir(rigPath)
	rc, _, err := ResolveAgentConfigWithOverride(townRoot, rigPath, agentOverride)
	if err != nil {
		return "", err
	}
	return rc.BuildCommandWithPrompt(prompt), nil
}

// findTownRootFromCwd locates the town root by walking up from cwd.
// It looks for the mayor/town.json marker file.
// Returns empty string and no error if not found (caller should use defaults).
func findTownRootFromCwd(h host) (string, error) {
	cwd, err := h.getwd()
	if err != nil {
		return "", fmt.Errorf("getting cwd: %w", err)
	}

	absDir, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolving path: %w", err)
	}

	const marker = "mayor/town.json"

	current := absDir
	for {
		if _, err := os.Stat(filepath.Join(current, marker)); err == nil {
			return current, nil
		}

		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("town root not found (no %s marker)", marker)
		}
		current = parent
	}
}

// ExtractSimpleRole extracts the simple role name from a GT_ROLE value.
// GT_ROLE can be:
//   - Simple: "mayor", "deacon"
//   - Compound: "rig/witness", "rig/refinery", "rig/crew/name", "rig/polecats/name"
//
// For compound format, returns the role segment (second part), mapping the
// plural "polecats" segment to its role.
// For simple format, returns the role as-is.
func ExtractSimpleRole(gtRole string) string {
	if gtRole == "" {
		return ""
	}
	parts := strings.Split(gtRole, "/")
	switch len(parts) {
	case 1:
		// Simple format: "mayor", "deacon"
		return parts[0]
	case 2:
		// "rig/witness", "rig/refinery"
		return parts[1]
	case 3:
		// "rig/crew/name" → "crew", "rig/polecats/name" → "polecat"
		role := parts[1]
		if role == "polecats" {
			return constants.RolePolecat
		}
		return role
	default:
		return gtRole
	}
}

// BuildStartupCommand builds a full startup command with environment exports.
// envVars is a map of environment variable names to values.
// rigPath is optional - if empty, uses envVars["GT_ROOT"] to find town root,
// falling back to cwd detection if GT_ROOT is not set.
// prompt is optional - if provided, appended as the initial prompt.
//
// If envVars contains GT_ROLE, the function uses role-based agent resolution
// (ResolveRoleAgentConfig) to select the appropriate agent for the role.
// This enables per-role model selection via role_agents in settings.
//
// An unset ${VAR} reference in the resolved agent's env returns an error and no
// command, so a caller that restarts a live session in place leaves that
// session alone rather than typing an empty credential into its pane
// (gt-wisp-jsm).
func BuildStartupCommand(envVars map[string]string, rigPath, prompt string) (string, error) {
	return buildStartupCommand(processHost, envVars, rigPath, prompt)
}

func buildStartupCommand(h host, envVars map[string]string, rigPath, prompt string) (string, error) {
	var rc *RuntimeConfig
	var townRoot string

	// Extract role from envVars for role-based agent resolution.
	// GT_ROLE may be compound format (e.g., "rig/refinery") so we extract
	// the simple role name for role_agents lookup.
	role := ExtractSimpleRole(envVars["GT_ROLE"])

	if rigPath != "" {
		// Derive town root from rig path
		townRoot = filepath.Dir(rigPath)
		if role == "crew" && envVars["GT_CREW"] != "" {
			// Per-worker agent resolution: check worker_agents before role_agents
			rc = resolveWorkerAgentConfig(h, envVars["GT_CREW"], townRoot, rigPath)
		} else if role != "" {
			// Use role-based agent resolution for per-role model selection
			rc = resolveRoleAgentConfig(h, role, townRoot, rigPath)
		} else {
			rc = resolveAgentConfig(h, townRoot, rigPath)
		}
	} else {
		// For town-level agents (mayor, deacon), prefer GT_ROOT from envVars
		// (set by AgentEnv) over cwd detection. This ensures role_agents config
		// is respected even when the daemon runs outside the town hierarchy.
		townRoot = envVars["GT_ROOT"]
		if townRoot == "" {
			var err error
			townRoot, err = findTownRootFromCwd(h)
			if err != nil {
				rc = DefaultRuntimeConfig()
			}
		}
		if rc == nil {
			if role != "" {
				rc = resolveRoleAgentConfig(h, role, townRoot, "")
			} else {
				rc = resolveAgentConfig(h, townRoot, "")
			}
		}
	}

	// Apply exec wrapper from rig/town settings if not already set on the resolved config.
	// ExecWrapper is a deployment-level setting (sandbox/container) independent of agent choice.
	if len(rc.ExecWrapper) == 0 {
		rc.ExecWrapper = resolveExecWrapper(rigPath)
	}

	// Copy env vars to avoid mutating caller map
	resolvedEnv := make(map[string]string, len(envVars)+2)
	for k, v := range envVars {
		resolvedEnv[k] = v
	}
	// Add GT_ROOT so agents can find town-level resources (formulas, etc.)
	if townRoot != "" {
		resolvedEnv["GT_ROOT"] = townRoot
	}
	if rc.Session != nil && rc.Session.SessionIDEnv != "" {
		resolvedEnv["GT_SESSION_ID_ENV"] = rc.Session.SessionIDEnv
	}
	// Set GT_AGENT from resolved agent name so IsAgentAliveChecked can resolve
	// the session's preset (process names, Escape policy) rather than guess.
	// See: gt-agent-role-agents.
	if rc.ResolvedAgent != "" {
		resolvedEnv["GT_AGENT"] = rc.ResolvedAgent
	}
	// Set GT_PROCESS_NAMES for accurate liveness detection. Custom agents may
	// shadow built-in preset names (e.g., custom "claude" running a wrapper script),
	// or wrap the real binary with a launcher (e.g., `env -u VAR claude ...`).
	// Pass rc.Args so wrapper-unwrap can find the real binary.
	processNames := agentRegistryFor(h, townRoot, rigPath).ResolveProcessNames(rc.ResolvedAgent, rc.Command, rc.Args...)
	resolvedEnv["GT_PROCESS_NAMES"] = strings.Join(processNames, ",")
	// Merge agent-specific env vars (e.g., ANTHROPIC_BASE_URL for a backend),
	// resolving any ${VAR} reference here rather than at config load so it
	// reads the environment of the process doing the spawning. An unresolved
	// reference is an error, not an empty export: this is the only check an
	// agent resolved out of settings passes (gt-yih1).
	name := rc.ResolvedAgent
	if name == "" {
		name = rc.Provider
	}
	secretPrefix, err := mergeAgentEnv(h, townRoot, name, rc.Env, resolvedEnv)
	if err != nil {
		return "", err
	}

	SanitizeAgentEnv(resolvedEnv, envVars)

	var cmd string
	if runtime.GOOS == "windows" {
		// On Windows, tmux (psmux) uses PowerShell and send-keys has line length
		// limits. Write env vars + agent command to a temp .ps1 script and invoke
		// that instead. This avoids send-keys corrupting long commands.
		var scriptLines []string
		keys := make([]string, 0, len(resolvedEnv))
		for k := range resolvedEnv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			scriptLines = append(scriptLines, fmt.Sprintf("$env:%s=%s", k, psQuote(resolvedEnv[k])))
		}

		var agentCmd string
		if len(rc.ExecWrapper) > 0 {
			agentCmd = strings.Join(rc.ExecWrapper, " ") + " "
		}
		if prompt != "" {
			agentCmd += "& " + rc.BuildCommandWithPrompt(prompt)
		} else {
			agentCmd += "& " + rc.BuildCommand()
		}
		scriptLines = append(scriptLines, agentCmd)

		// Write script to temp file in town's daemon dir
		townRoot := resolvedEnv["GT_ROOT"]
		if townRoot == "" {
			townRoot = os.TempDir()
		}
		scriptDir := filepath.Join(townRoot, "daemon", "scripts")
		_ = os.MkdirAll(scriptDir, 0755)
		role := resolvedEnv["GT_ROLE"]
		if role == "" {
			role = "agent"
		}
		// Sanitize role for filename (replace / with -)
		safeRole := strings.ReplaceAll(role, "/", "-")
		scriptPath := filepath.Join(scriptDir, safeRole+"-startup.ps1")
		scriptContent := strings.Join(scriptLines, "\n") + "\n"
		if err := os.WriteFile(scriptPath, []byte(scriptContent), 0644); err != nil {
			// Fallback: inline command (may fail if too long)
			cmd = strings.Join(scriptLines, "; ")
		} else {
			cmd = "& " + psQuote(scriptPath)
		}
	} else {
		// Build environment export prefix (POSIX shell)
		var exports []string
		for k, v := range resolvedEnv {
			exports = append(exports, fmt.Sprintf("%s=%s", k, ShellQuote(v)))
		}

		// Sort for deterministic output
		sort.Strings(exports)

		if len(exports) > 0 {
			// Use 'exec env' instead of 'export ... &&' so the agent process
			// replaces the shell. This allows WaitForCommand to detect the
			// running agent via pane_current_command (which shows the direct
			// process, not child processes).
			cmd = "exec env " + strings.Join(exports, " ") + " "
		}
		cmd = secretPrefix + cmd

		// Insert exec wrapper between env vars and agent command if configured.
		// Example: exec env VAR=val ... exitbox run --profile=foo -- claude ...
		if len(rc.ExecWrapper) > 0 {
			cmd += strings.Join(rc.ExecWrapper, " ") + " "
		}

		// Add runtime command
		if prompt != "" {
			cmd += rc.BuildCommandWithPrompt(prompt)
		} else {
			cmd += rc.BuildCommand()
		}
	}

	return cmd, nil
}

// SanitizeAgentEnv clears environment variables that are known to break agent
// startup when inherited from the parent shell/tmux environment.
//
// This is a SUPPLEMENTAL guard for paths that don't use AgentEnv() (which is
// the primary guard — see env.go). It protects: lifecycle.go's default path
// (non-polecat/non-crew roles) and handoff.go's manual export building.
// For callers that pass AgentEnv()-produced maps, this is a no-op since
// AgentEnv() already sets NODE_OPTIONS="".
//
// callerEnv is the original env map from the caller (before rc.Env merging).
// resolvedEnv is the post-merge map that may also contain values from rc.Env.
// NODE_OPTIONS is only cleared if neither callerEnv nor resolvedEnv (via rc.Env)
// explicitly provides it.
func SanitizeAgentEnv(resolvedEnv, callerEnv map[string]string) {
	// NODE_OPTIONS may contain debugger flags (e.g., --inspect from VSCode)
	// that cause Claude's Node.js runtime to crash with "Debugger attached" errors.
	// Only clear if not explicitly provided by the caller or agent config (rc.Env).
	if _, ok := callerEnv["NODE_OPTIONS"]; !ok {
		// Inner guard: preserve if rc.Env already set it in resolvedEnv
		if _, ok := resolvedEnv["NODE_OPTIONS"]; !ok {
			resolvedEnv["NODE_OPTIONS"] = ""
		}
	}

	// CLAUDECODE is set by Claude Code v2.x on startup and triggers nested session
	// detection. When gt sling is invoked from within a Claude Code session, tmux
	// inherits this variable into its global environment, causing new polecat sessions
	// to fail with "Nested sessions share runtime resources and will crash all active
	// sessions." Clear it unless the caller explicitly provides it.
	// See: https://github.com/steveyegge/gastown/issues/1666
	if _, ok := callerEnv["CLAUDECODE"]; !ok {
		resolvedEnv["CLAUDECODE"] = ""
	}

	clearBDTargetSelectorEnv(resolvedEnv)
}

// PrependEnv prepends export statements to a command string.
// Values containing special characters are properly shell-quoted.
// On Windows, uses PowerShell $env: syntax.
func PrependEnv(command string, envVars map[string]string) string {
	if len(envVars) == 0 {
		return command
	}

	var exports []string
	for k, v := range envVars {
		if runtime.GOOS == "windows" {
			exports = append(exports, fmt.Sprintf("$env:%s=%s", k, psQuote(v)))
		} else {
			exports = append(exports, fmt.Sprintf("%s=%s", k, ShellQuote(v)))
		}
	}

	sort.Strings(exports)
	if runtime.GOOS == "windows" {
		return strings.Join(exports, "; ") + "; " + command
	}
	return "export " + strings.Join(exports, " ") + " && " + command
}

// BuildStartupCommandWithAgentOverride builds a startup command like BuildStartupCommand,
// but uses agentOverride if non-empty.
//
// Resolution priority:
//  1. agentOverride (explicit override)
//  2. role_agents[GT_ROLE] (if GT_ROLE is in envVars)
//  3. Default agent resolution (rig's Agent → town's DefaultAgent → "claude")
func BuildStartupCommandWithAgentOverride(envVars map[string]string, rigPath, prompt, agentOverride string) (string, error) {
	return buildStartupCommandWithAgentOverride(processHost, envVars, rigPath, prompt, agentOverride)
}

func buildStartupCommandWithAgentOverride(h host, envVars map[string]string, rigPath, prompt, agentOverride string) (string, error) {
	var rc *RuntimeConfig
	var townRoot string

	// Extract role from envVars for role-based agent resolution (when no override)
	role := ExtractSimpleRole(envVars["GT_ROLE"])

	if rigPath != "" {
		townRoot = filepath.Dir(rigPath)
		if agentOverride != "" {
			var err error
			rc, _, err = resolveAgentConfigWithOverride(h, townRoot, rigPath, agentOverride)
			if err != nil {
				return "", err
			}
		} else if role == "crew" && envVars["GT_CREW"] != "" {
			// Per-worker agent resolution: check worker_agents before role_agents
			rc = resolveWorkerAgentConfig(h, envVars["GT_CREW"], townRoot, rigPath)
		} else if role != "" {
			// No override, use role-based agent resolution
			rc = resolveRoleAgentConfig(h, role, townRoot, rigPath)
		} else {
			rc = resolveAgentConfig(h, townRoot, rigPath)
		}
	} else {
		// For town-level agents (mayor, deacon), prefer GT_ROOT from envVars
		// (set by AgentEnv) over cwd detection. This ensures role_agents config
		// is respected even when the daemon runs outside the town hierarchy.
		townRoot = envVars["GT_ROOT"]
		if townRoot == "" {
			var err error
			townRoot, err = findTownRootFromCwd(h)
			if err != nil {
				// Can't find town root from cwd - but if agentOverride is specified,
				// try to use the preset directly. This allows `gt deacon start --agent claude`
				// to work even when run from outside the town directory.
				if agentOverride != "" {
					if preset := GetAgentPresetByName(agentOverride); preset != nil {
						rc = RuntimeConfigFromPreset(AgentPreset(agentOverride))
					} else {
						return "", fmt.Errorf("agent '%s' not found", agentOverride)
					}
				} else {
					rc = DefaultRuntimeConfig()
				}
			}
		}
		if rc == nil {
			if agentOverride != "" {
				var resolveErr error
				rc, _, resolveErr = resolveAgentConfigWithOverride(h, townRoot, "", agentOverride)
				if resolveErr != nil {
					return "", resolveErr
				}
			} else if role != "" {
				rc = resolveRoleAgentConfig(h, role, townRoot, "")
			} else {
				rc = resolveAgentConfig(h, townRoot, "")
			}
		}
	}

	// Ensure Claude agents get --settings when their settings directory
	// differs from the session working directory. This must run for ALL
	// resolution paths (including agent overrides) — previously only the
	// non-override ResolveRoleAgentConfig path included it, causing hooks
	// to silently not fire for polecats launched with --agent.
	reg := agentRegistryFor(h, townRoot, rigPath)
	rc = withRoleSettingsFlag(rc, role, rigPath)
	// Same for the rendered role system prompt: when the agent's file exists,
	// Claude gets it via --append-system-prompt-file and gt prime omits the
	// static role text from its hook output. Polecat and crew files are per
	// agent, so the name comes from the identity env vars.
	agentName := envVars["GT_POLECAT"]
	if agentName == "" {
		agentName = envVars["GT_CREW"]
	}
	rc = withRoleSystemPromptFlag(reg, rc, role, townRoot, rigPath, agentName)

	// Apply exec wrapper from rig/town settings if not already set on the resolved config.
	if len(rc.ExecWrapper) == 0 {
		rc.ExecWrapper = resolveExecWrapper(rigPath)
	}

	// Copy env vars to avoid mutating caller map
	resolvedEnv := make(map[string]string, len(envVars)+2)
	for k, v := range envVars {
		resolvedEnv[k] = v
	}
	// Add GT_ROOT so agents can find town-level resources (formulas, etc.)
	if townRoot != "" {
		resolvedEnv["GT_ROOT"] = townRoot
	}
	if rc.Session != nil && rc.Session.SessionIDEnv != "" {
		resolvedEnv["GT_SESSION_ID_ENV"] = rc.Session.SessionIDEnv
	}
	// Record agent name so IsAgentAliveChecked can detect the running process.
	// Explicit override takes priority; fall back to resolved agent name.
	agentForProcess := rc.ResolvedAgent
	if agentOverride != "" {
		resolvedEnv["GT_AGENT"] = agentOverride
		resolvedEnv[EnvAgentOverride] = "1"
		agentForProcess = agentOverride
	} else if rc.ResolvedAgent != "" {
		resolvedEnv["GT_AGENT"] = rc.ResolvedAgent
	}
	// Set GT_PROCESS_NAMES for accurate liveness detection of custom agents.
	// Pass rc.Args so wrapper-unwrap (env/sudo/nohup wrapping a real binary)
	// can find the real agent binary.
	processNamesOverride := reg.ResolveProcessNames(agentForProcess, rc.Command, rc.Args...)
	resolvedEnv["GT_PROCESS_NAMES"] = strings.Join(processNamesOverride, ",")
	// Merge agent-specific env vars (e.g., ANTHROPIC_BASE_URL for a backend),
	// resolving any ${VAR} reference here rather than at config load so it
	// reads the environment of the process doing the spawning. An unresolved
	// reference stops the spawn: this is the only check an agent resolved out
	// of settings passes (gt-yih1).
	name := agentForProcess
	if name == "" {
		name = rc.Provider
	}
	secretPrefix, err := mergeAgentEnv(h, townRoot, name, rc.Env, resolvedEnv)
	if err != nil {
		return "", err
	}

	SanitizeAgentEnv(resolvedEnv, envVars)

	var cmd string
	if runtime.GOOS == "windows" {
		// Write env vars + agent command to a temp .ps1 script to avoid
		// send-keys line length limits in psmux.
		var scriptLines []string
		keys := make([]string, 0, len(resolvedEnv))
		for k := range resolvedEnv {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			scriptLines = append(scriptLines, fmt.Sprintf("$env:%s=%s", k, psQuote(resolvedEnv[k])))
		}

		var agentCmd string
		if len(rc.ExecWrapper) > 0 {
			agentCmd = strings.Join(rc.ExecWrapper, " ") + " "
		}
		if prompt != "" {
			agentCmd += "& " + rc.BuildCommandWithPrompt(prompt)
		} else {
			agentCmd += "& " + rc.BuildCommand()
		}
		scriptLines = append(scriptLines, agentCmd)

		townRoot := resolvedEnv["GT_ROOT"]
		if townRoot == "" {
			townRoot = os.TempDir()
		}
		scriptDir := filepath.Join(townRoot, "daemon", "scripts")
		_ = os.MkdirAll(scriptDir, 0755)
		role := resolvedEnv["GT_ROLE"]
		if role == "" {
			role = "agent"
		}
		safeRole := strings.ReplaceAll(role, "/", "-")
		scriptPath := filepath.Join(scriptDir, safeRole+"-startup.ps1")
		scriptContent := strings.Join(scriptLines, "\n") + "\n"
		if err := os.WriteFile(scriptPath, []byte(scriptContent), 0644); err != nil {
			cmd = strings.Join(scriptLines, "; ")
		} else {
			cmd = "& " + psQuote(scriptPath)
		}
	} else {
		// Build environment export prefix (POSIX shell)
		var exports []string
		for k, v := range resolvedEnv {
			exports = append(exports, fmt.Sprintf("%s=%s", k, ShellQuote(v)))
		}
		sort.Strings(exports)

		if len(exports) > 0 {
			cmd = "exec env " + strings.Join(exports, " ") + " "
		}
		cmd = secretPrefix + cmd

		if len(rc.ExecWrapper) > 0 {
			cmd += strings.Join(rc.ExecWrapper, " ") + " "
		}

		if prompt != "" {
			cmd += rc.BuildCommandWithPrompt(prompt)
		} else {
			cmd += rc.BuildCommand()
		}
	}

	return cmd, nil
}

// BuildStartupCommandFromConfig builds a startup command from a complete AgentEnvConfig.
// Use this (instead of Build*StartupCommand helpers) when you need a field the
// helpers do not set, such as SessionName.
// The rigPath, prompt, and agentOverride are passed through directly.
func BuildStartupCommandFromConfig(cfg AgentEnvConfig, rigPath, prompt, agentOverride string) (string, error) {
	return buildStartupCommandFromConfig(processHost, cfg, rigPath, prompt, agentOverride)
}

func buildStartupCommandFromConfig(h host, cfg AgentEnvConfig, rigPath, prompt, agentOverride string) (string, error) {
	if cfg.Getenv == nil {
		cfg.Getenv = h.getenv
	}
	envVars := AgentEnv(cfg)
	return buildStartupCommandWithAgentOverride(h, envVars, rigPath, prompt, agentOverride)
}

// BuildAgentStartupCommand is a convenience function for starting agent
// sessions. It uses AgentEnv to set all standard environment variables.
// For rig-level roles (witness, refinery), pass the rig name and rigPath.
// For town-level roles (mayor, deacon, boot), pass empty rig and rigPath, but
// provide townRoot.
func BuildAgentStartupCommand(role, rig, townRoot, rigPath, prompt string) (string, error) {
	return buildAgentStartupCommand(processHost, role, rig, townRoot, rigPath, prompt)
}

func buildAgentStartupCommand(h host, role, rig, townRoot, rigPath, prompt string) (string, error) {
	envVars := AgentEnv(AgentEnvConfig{
		Role:     role,
		Rig:      rig,
		TownRoot: townRoot,
		Getenv:   h.getenv,
	})
	return buildStartupCommand(h, envVars, rigPath, prompt)
}

// BuildAgentStartupCommandWithAgentOverride is like BuildAgentStartupCommand, but uses agentOverride if non-empty.
func BuildAgentStartupCommandWithAgentOverride(role, rig, townRoot, rigPath, prompt, agentOverride string) (string, error) {
	return buildAgentStartupCommandWithAgentOverride(processHost, role, rig, townRoot, rigPath, prompt, agentOverride)
}

func buildAgentStartupCommandWithAgentOverride(h host, role, rig, townRoot, rigPath, prompt, agentOverride string) (string, error) {
	envVars := AgentEnv(AgentEnvConfig{
		Role:     role,
		Rig:      rig,
		TownRoot: townRoot,
		Getenv:   h.getenv,
	})
	return buildStartupCommandWithAgentOverride(h, envVars, rigPath, prompt, agentOverride)
}

// BuildPolecatStartupCommand builds the startup command for a polecat.
// Sets GT_ROLE, GT_RIG, GT_POLECAT, BD_ACTOR, GIT_AUTHOR_NAME, and GT_ROOT.
func BuildPolecatStartupCommand(rigName, polecatName, rigPath, prompt string) (string, error) {
	var townRoot string
	if rigPath != "" {
		townRoot = filepath.Dir(rigPath)
	}
	envVars := AgentEnv(AgentEnvConfig{
		Role:      constants.RolePolecat,
		Rig:       rigName,
		AgentName: polecatName,
		TownRoot:  townRoot,
	})
	return BuildStartupCommand(envVars, rigPath, prompt)
}

// BuildPolecatStartupCommandWithAgentOverride is like BuildPolecatStartupCommand, but uses agentOverride if non-empty.
func BuildPolecatStartupCommandWithAgentOverride(rigName, polecatName, rigPath, prompt, agentOverride string) (string, error) {
	var townRoot string
	if rigPath != "" {
		townRoot = filepath.Dir(rigPath)
	}
	envVars := AgentEnv(AgentEnvConfig{
		Role:      constants.RolePolecat,
		Rig:       rigName,
		AgentName: polecatName,
		TownRoot:  townRoot,
	})
	return BuildStartupCommandWithAgentOverride(envVars, rigPath, prompt, agentOverride)
}

// BuildCrewStartupCommand builds the startup command for a crew member.
// Sets GT_ROLE, GT_RIG, GT_CREW, BD_ACTOR, GIT_AUTHOR_NAME, and GT_ROOT.
func BuildCrewStartupCommand(rigName, crewName, rigPath, prompt string) (string, error) {
	var townRoot string
	if rigPath != "" {
		townRoot = filepath.Dir(rigPath)
	}
	envVars := AgentEnv(AgentEnvConfig{
		Role:      constants.RoleCrew,
		Rig:       rigName,
		AgentName: crewName,
		TownRoot:  townRoot,
	})
	return BuildStartupCommand(envVars, rigPath, prompt)
}

// BuildCrewStartupCommandWithAgentOverride is like BuildCrewStartupCommand, but uses agentOverride if non-empty.
func BuildCrewStartupCommandWithAgentOverride(rigName, crewName, rigPath, prompt, agentOverride string) (string, error) {
	var townRoot string
	if rigPath != "" {
		townRoot = filepath.Dir(rigPath)
	}
	envVars := AgentEnv(AgentEnvConfig{
		Role:      constants.RoleCrew,
		Rig:       rigName,
		AgentName: crewName,
		TownRoot:  townRoot,
	})
	return BuildStartupCommandWithAgentOverride(envVars, rigPath, prompt, agentOverride)
}

// resolveExecWrapper loads the exec_wrapper from rig settings.
// ExecWrapper is a deployment-level setting (sandbox/container) that wraps the agent binary.
// It is independent of agent choice — exitbox wraps the Claude CLI and its wrappers.
func resolveExecWrapper(rigPath string) []string {
	if rigPath != "" {
		if rigSettings, err := LoadRigSettings(RigSettingsPath(rigPath)); err == nil && rigSettings != nil {
			if rigSettings.Runtime != nil && len(rigSettings.Runtime.ExecWrapper) > 0 {
				return rigSettings.Runtime.ExecWrapper
			}
		}
	}
	return nil
}

// ExpectedPaneCommands returns tmux pane command names that indicate the runtime is running.
// Claude can report as "node" (older versions) or "claude" (newer versions).
// Other runtimes typically report their executable name.
func ExpectedPaneCommands(rc *RuntimeConfig) []string {
	if rc == nil || rc.Command == "" {
		return nil
	}
	if filepath.Base(rc.Command) == "claude" {
		return []string{"node", "claude"}
	}
	return []string{filepath.Base(rc.Command)}
}

// GetDefaultFormula returns the default formula for a rig from settings/config.json.
// Returns empty string if no default is configured.
// rigPath is the path to the rig directory (e.g., ~/gt/gastown).
func GetDefaultFormula(rigPath string) string {
	settingsPath := RigSettingsPath(rigPath)
	settings, err := LoadRigSettings(settingsPath)
	if err != nil {
		return ""
	}
	if settings.Workflow == nil {
		return ""
	}
	return settings.Workflow.DefaultFormula
}

// GetRigPrefix returns the beads prefix for a rig from rigs.json.
// Falls back to "gt" if the rig isn't found or has no prefix configured.
// townRoot is the path to the town directory (e.g., ~/gt).
func GetRigPrefix(townRoot, rigName string) string {
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := LoadRigsConfig(rigsConfigPath)
	if err != nil {
		return "gt" // fallback
	}

	entry, ok := rigsConfig.Rigs[rigName]
	if !ok {
		return "gt" // fallback
	}

	if entry.BeadsConfig == nil || entry.BeadsConfig.Prefix == "" {
		return "gt" // fallback
	}

	// Strip trailing hyphen if present (prefix stored as "gt-" but used as "gt")
	prefix := entry.BeadsConfig.Prefix
	return strings.TrimSuffix(prefix, "-")
}

// AllRigPrefixes returns a sorted list of all rig beads prefixes from rigs.json.
// Trailing hyphens are stripped (e.g. "gt-" becomes "gt").
// Returns nil on error (caller should handle the fallback).
func AllRigPrefixes(townRoot string) []string {
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := LoadRigsConfig(rigsConfigPath)
	if err != nil {
		return nil
	}
	var prefixes []string
	for _, entry := range rigsConfig.Rigs {
		if entry.BeadsConfig != nil && entry.BeadsConfig.Prefix != "" {
			prefixes = append(prefixes, strings.TrimSuffix(entry.BeadsConfig.Prefix, "-"))
		}
	}
	sort.Strings(prefixes)
	return prefixes
}

// EscalationConfigPath returns the standard path for escalation config in a town.
func EscalationConfigPath(townRoot string) string {
	return filepath.Join(townRoot, "settings", "escalation.json")
}

// LoadEscalationConfig loads and validates an escalation configuration file.
func LoadEscalationConfig(path string) (*EscalationConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is constructed internally, not from user input
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("reading escalation config: %w", err)
	}

	var config EscalationConfig
	if err := DecodeJSONFile(path, data, &config); err != nil {
		return nil, err
	}

	if err := validateEscalationConfig(&config); err != nil {
		return nil, err
	}

	return &config, nil
}

// LoadOrCreateEscalationConfig loads the escalation config, creating a default if not found.
func LoadOrCreateEscalationConfig(path string) (*EscalationConfig, error) {
	config, err := LoadEscalationConfig(path)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return NewEscalationConfig(), nil
		}
		return nil, err
	}
	return config, nil
}

// SaveEscalationConfig saves an escalation configuration to a file.
func SaveEscalationConfig(path string, config *EscalationConfig) error {
	if err := validateEscalationConfig(config); err != nil {
		return err
	}

	return WriteConfigJSON(path, config, 0644)
}

// validateEscalationConfig validates an EscalationConfig.
func validateEscalationConfig(c *EscalationConfig) error {
	if c.Type != "escalation" && c.Type != "" {
		return fmt.Errorf("%w: expected type 'escalation', got '%s'", ErrInvalidType, c.Type)
	}
	if c.Version > CurrentEscalationVersion {
		return fmt.Errorf("%w: got %d, max supported %d", ErrInvalidVersion, c.Version, CurrentEscalationVersion)
	}

	// Validate stale_threshold if specified
	if c.StaleThreshold != "" {
		if _, err := time.ParseDuration(c.StaleThreshold); err != nil {
			return fmt.Errorf("invalid stale_threshold: %w", err)
		}
	}

	// Validate renotify_window if specified
	if c.RenotifyWindow != "" {
		if _, err := time.ParseDuration(c.RenotifyWindow); err != nil {
			return fmt.Errorf("invalid renotify_window: %w", err)
		}
	}

	// Initialize nil maps
	if c.Routes == nil {
		c.Routes = make(map[string][]string)
	}

	// Validate severity route keys
	for severity := range c.Routes {
		if !IsValidSeverity(severity) {
			return fmt.Errorf("%w: unknown severity '%s' (valid: low, medium, high, critical)", ErrMissingField, severity)
		}
	}

	// Validate max_reescalations is non-negative
	if c.MaxReescalations != nil && *c.MaxReescalations < 0 {
		return fmt.Errorf("%w: max_reescalations must be non-negative", ErrMissingField)
	}

	return nil
}

// GetStaleThreshold returns the stale threshold as a time.Duration.
// Returns 4 hours if not configured or invalid.
func (c *EscalationConfig) GetStaleThreshold() time.Duration {
	if c.StaleThreshold == "" {
		return 4 * time.Hour
	}
	d, err := time.ParseDuration(c.StaleThreshold)
	if err != nil {
		return 4 * time.Hour
	}
	return d
}

// GetRenotifyWindow returns the renotify window as a time.Duration.
// Returns 1 hour if not configured or invalid.
func (c *EscalationConfig) GetRenotifyWindow() time.Duration {
	if c.RenotifyWindow == "" {
		return time.Hour
	}
	d, err := time.ParseDuration(c.RenotifyWindow)
	if err != nil {
		return time.Hour
	}
	return d
}

// GetRouteForSeverity returns the escalation route actions for a given severity.
// Falls back to ["bead", "mail:mayor"] if no specific route is configured.
func (c *EscalationConfig) GetRouteForSeverity(severity string) []string {
	if route, ok := c.Routes[severity]; ok {
		return route
	}
	// Fallback to default route
	return []string{"bead", "mail:mayor"}
}

// GetMaxReescalations returns the maximum number of re-escalations allowed.
// Returns 2 if not configured (nil). Explicit 0 means "never re-escalate".
func (c *EscalationConfig) GetMaxReescalations() int {
	if c.MaxReescalations == nil {
		return 2
	}
	return *c.MaxReescalations
}
