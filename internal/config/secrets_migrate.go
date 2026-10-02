package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// SecretMove is one literal token gt config secrets migrate moves out of
// settings/config.json: where it is, and the settings/daemon.env name its
// value takes. It never carries the value.
type SecretMove struct {
	LiteralSecret
	// Var is the daemon.env name the value is referenced by.
	Var string
	// Added is true when Var is a new daemon.env entry, false when an
	// existing entry (or an earlier move) already holds the same value.
	Added bool
}

// PlanSecretMoves decides where each literal token in s goes, given the
// daemon.env entries the town already has. A value daemon.env already holds
// reuses that entry; equal values share one new entry, named after the first
// agent and key that hold it (DEEPSEEK_FLASH_ANTHROPIC_AUTH_TOKEN), never the
// key itself, so the daemon's own environment cannot hand one provider's
// token to every agent. It reads s and fileEnv only.
func PlanSecretMoves(s *TownSettings, fileEnv map[string]string) []SecretMove {
	byValue := make(map[string]string, len(fileEnv))
	for _, name := range sortedNames(fileEnv) {
		if _, ok := byValue[fileEnv[name]]; !ok {
			byValue[fileEnv[name]] = name
		}
	}
	taken := make(map[string]bool, len(fileEnv))
	for name := range fileEnv {
		taken[name] = true
	}
	var moves []SecretMove
	for _, lit := range FindLiteralSecrets(s) {
		value := literalValue(s, lit)
		if name, ok := byValue[value]; ok {
			moves = append(moves, SecretMove{LiteralSecret: lit, Var: name})
			continue
		}
		name := secretVarName(lit, taken)
		taken[name] = true
		byValue[value] = name
		moves = append(moves, SecretMove{LiteralSecret: lit, Var: name, Added: true})
	}
	return moves
}

// literalValue reads lit's current value from s.
func literalValue(s *TownSettings, lit LiteralSecret) string {
	if lit.KeyPath == doltPasswordPath {
		return *s.Operational.Dolt.Password
	}
	return s.Agents[lit.Agent].Env[lit.Key]
}

// setLiteralValue rewrites lit's value in s.
func setLiteralValue(s *TownSettings, lit LiteralSecret, value string) {
	if lit.KeyPath == doltPasswordPath {
		s.Operational.Dolt.Password = &value
		return
	}
	s.Agents[lit.Agent].Env[lit.Key] = value
}

// secretVarBase is the daemon.env name a literal moves to: AGENT_KEY in
// env-name form for an agent env value, DOLT_PASSWORD for the Dolt password.
func secretVarBase(lit LiteralSecret) string {
	if lit.KeyPath == doltPasswordPath {
		return "DOLT_PASSWORD"
	}
	var b strings.Builder
	for _, c := range strings.ToUpper(lit.Agent + "_" + lit.Key) {
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	base := b.String()
	if base[0] >= '0' && base[0] <= '9' {
		base = "_" + base
	}
	return base
}

// secretVarName is secretVarBase, suffixed until it is not taken.
func secretVarName(lit LiteralSecret, taken map[string]bool) string {
	base := secretVarBase(lit)
	name := base
	for i := 2; taken[name]; i++ {
		name = fmt.Sprintf("%s_%d", base, i)
	}
	return name
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PlanSecretMigration is the plan MigrateSecrets would apply to the town at
// townRoot, without writing anything.
func PlanSecretMigration(townRoot string) ([]SecretMove, error) {
	path := TownSettingsPath(townRoot)
	data, err := os.ReadFile(path) //nolint:gosec // G304: town settings path
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var s TownSettings
	if err := DecodeJSONFile(path, data, &s); err != nil {
		return nil, err
	}
	fileEnv, err := LoadDaemonEnv(townRoot)
	if err != nil {
		return nil, err
	}
	return PlanSecretMoves(&s, fileEnv), nil
}

// MigrateSecrets moves every literal token in settings/config.json's agent
// env blocks into settings/daemon.env and rewrites each to a ${VAR}
// reference, under the settings file's lock. daemon.env is written first
// (its existing lines kept, mode 0600), so a crash between the two writes
// leaves the tokens in both files and a rerun reuses the entries; the
// settings file is never left referencing a name daemon.env lacks.
func MigrateSecrets(townRoot string) ([]SecretMove, error) {
	var moves []SecretMove
	err := UpdateConfigJSON(TownSettingsPath(townRoot), 0o600, func(s *TownSettings, exists bool) error {
		if !exists {
			return fmt.Errorf("%s does not exist", TownSettingsPath(townRoot))
		}
		fileEnv, err := LoadDaemonEnv(townRoot)
		if err != nil {
			return err
		}
		moves = PlanSecretMoves(s, fileEnv)
		if len(moves) == 0 {
			return errNothingToMigrate
		}
		if err := appendDaemonEnv(townRoot, s, moves); err != nil {
			return err
		}
		for _, m := range moves {
			setLiteralValue(s, m.LiteralSecret, "${"+m.Var+"}")
		}
		return nil
	})
	if errors.Is(err, errNothingToMigrate) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return moves, nil
}

var errNothingToMigrate = errors.New("no literal secrets")

// appendDaemonEnv adds the Added moves' values to settings/daemon.env under
// its lock, keeping the file's existing bytes, and leaves it at mode 0600.
func appendDaemonEnv(townRoot string, s *TownSettings, moves []SecretMove) error {
	path := DaemonEnvPath(townRoot)
	unlock, err := lockConfigFile(path)
	if err != nil {
		return err
	}
	defer unlock()
	data, err := os.ReadFile(path) //nolint:gosec // G304: daemon env path
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	var b strings.Builder
	b.Write(data)
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		b.WriteByte('\n')
	}
	added := false
	for _, m := range moves {
		if !m.Added {
			continue
		}
		if !added {
			b.WriteString("# Tokens moved from settings/config.json by gt config secrets migrate.\n")
			added = true
		}
		fmt.Fprintf(&b, "%s=%s\n", m.Var, literalValue(s, m.LiteralSecret))
	}
	return replaceFile(path, []byte(b.String()), 0o600)
}
