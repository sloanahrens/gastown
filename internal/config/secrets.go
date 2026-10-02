package config

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
)

// Secrets by reference (gt-y3pgh.5, D5 Q6). A token lives in
// settings/daemon.env (mode 0600) and an agent's env block in
// settings/config.json names it as a ${VAR} reference:
//
//	"env": {"ANTHROPIC_AUTH_TOKEN": "${DEEPSEEK_API_KEY}"}
//
// operational.dolt.password follows the same rule (gt-y3pgh.2.4): it holds
// the Dolt SQL password as a reference ("${GT_DOLT_PASSWORD}"), resolved by
// ResolveDoltPassword from daemon.env with the process environment as the
// fallback.
//
// The braced reference is the one form the strict decoder already reads (a
// plain string), so no schema change is needed to use it. At spawn a
// reference to a daemon.env name is not expanded into the startup command:
// the command reads the value from daemon.env when it runs, so the token
// never appears in any process's argv or in the tmux pane's start command
// (G3-18). A reference daemon.env does not define expands from the spawning
// process's environment, as before.
//
// A literal value that looks like a token is reported, by its key path and
// never its value: as a warning by default, and as a load error once the
// operator sets "secrets": {"refuse_literals": true} in settings/config.json
// (gt config set secrets.refuse_literals true). gt config secrets migrate
// moves the literals into daemon.env and rewrites them to references.

// SecretsConfig is the "secrets" block of settings/config.json.
type SecretsConfig struct {
	// RefuseLiterals makes the config kernel refuse to load a
	// settings/config.json whose agent env holds a literal token, which
	// stops every session start and the daemon until it is moved to
	// settings/daemon.env. Off by default: the town warns instead.
	RefuseLiterals bool `json:"refuse_literals,omitempty"`
}

// RefusesLiteralSecrets reports whether s turns literal tokens into a load
// error. A nil s does not.
func (s *TownSettings) RefusesLiteralSecrets() bool {
	return s != nil && s.Secrets != nil && s.Secrets.RefuseLiterals
}

// LiteralSecret names one settings/config.json value that holds a token in
// plain text. It carries the location only, never the value, so it can be
// printed, logged and put in an error.
type LiteralSecret struct {
	// Agent and Key locate an agents.<agent>.env.<key> value.
	Agent string
	Key   string
	// KeyPath is the value's dotted key path when it is not in an agent env
	// block, empty otherwise. Only the Dolt password uses it today:
	// operational.dolt.password (gt-y3pgh.2.4).
	KeyPath string
}

// doltPasswordPath is the settings/config.json key path of
// operational.dolt.password.
const doltPasswordPath = "operational.dolt.password"

// Path is the value's dotted key path in settings/config.json.
func (s LiteralSecret) Path() string {
	if s.KeyPath != "" {
		return s.KeyPath
	}
	return "agents." + s.Agent + ".env." + s.Key
}

// FindLiteralSecrets lists the values in s that look like tokens, sorted by
// path: every agent env value, and the Dolt password. A nil s has none.
func FindLiteralSecrets(s *TownSettings) []LiteralSecret {
	if s == nil {
		return nil
	}
	var out []LiteralSecret
	for agent, rc := range s.Agents {
		if rc == nil {
			continue
		}
		for k, v := range rc.Env {
			if LooksLikeSecret(k, v) {
				out = append(out, LiteralSecret{Agent: agent, Key: k})
			}
		}
	}
	if s.Operational != nil && s.Operational.Dolt != nil && s.Operational.Dolt.Password != nil {
		if LooksLikeSecret("password", *s.Operational.Dolt.Password) {
			out = append(out, LiteralSecret{KeyPath: doltPasswordPath})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path() < out[j].Path() })
	return out
}

// LiteralSecretPaths is the paths of secrets, in order.
func LiteralSecretPaths(secrets []LiteralSecret) []string {
	out := make([]string, len(secrets))
	for i, s := range secrets {
		out[i] = s.Path()
	}
	return out
}

// secretValuePrefixes start the tokens of common providers.
var secretValuePrefixes = []string{
	"sk-", "sk_", "ghp_", "gho_", "ghu_", "ghs_", "github_pat_", "glpat-",
	"xoxb-", "xoxp-", "xapp-", "AKIA", "ASIA", "AIza", "ya29.",
}

// secretKeyMarkers mark an env key whose value is a credential.
var secretKeyMarkers = []string{
	"TOKEN", "API_KEY", "APIKEY", "SECRET", "PASSWORD", "PASSPHRASE",
	"ACCESS_KEY", "PRIVATE_KEY",
}

// minSecretLen is the shortest value treated as a token. Placeholders a local
// backend accepts in a credential slot ("ollama", "dummy") are shorter.
const minSecretLen = 16

// LooksLikeSecret reports whether value, set for env key, is a literal
// token: at least minSecretLen characters with no whitespace, not a
// reference, a path, a URL or a number, and either starting with a known
// token prefix or set for a key that names a credential.
func LooksLikeSecret(key, value string) bool {
	v := strings.TrimSpace(value)
	if len(v) < minSecretLen || strings.ContainsAny(v, " \t\n") {
		return false
	}
	if len(envRefNames(v)) > 0 || strings.HasPrefix(v, "/") || strings.HasPrefix(v, "~") || strings.Contains(v, "://") {
		return false
	}
	if strings.Trim(v, "0123456789") == "" {
		return false
	}
	for _, p := range secretValuePrefixes {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	k := strings.ToUpper(key)
	for _, m := range secretKeyMarkers {
		if strings.Contains(k, m) {
			return true
		}
	}
	return false
}

// SpawnEnv is an agent's env block resolved for a startup command.
type SpawnEnv struct {
	// Inline holds the values that go into the command as KEY=value.
	Inline map[string]string
	// FromFile maps each key whose value references a settings/daemon.env
	// entry to the shell word that reads it when the command runs.
	FromFile map[string]string
	// all holds every key's value resolved now, for a startup command that
	// is written to a script file instead of passed to a shell (Windows).
	all map[string]string
}

// ShellPrefix is the POSIX shell that sets and exports the FromFile keys,
// to run before the command's own env assignments. It is empty when there
// are none.
func (e SpawnEnv) ShellPrefix() string {
	if len(e.FromFile) == 0 {
		return ""
	}
	keys := make([]string, 0, len(e.FromFile))
	for k := range e.FromFile {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s; export %s; ", k, e.FromFile[k], k)
	}
	return b.String()
}

// Values is every key's value resolved now, for a command written to a
// script file rather than passed to a shell.
func (e SpawnEnv) Values() map[string]string {
	out := make(map[string]string, len(e.all))
	for k, v := range e.all {
		out[k] = v
	}
	return out
}

// ResolveSpawnEnvMissing is ResolveSpawnEnv with the names neither
// settings/daemon.env nor this process defines, for a caller that execs the
// command itself and so has no shell to expand a late reference: it refuses
// the run rather than pass an empty credential (gt-yih1).
func ResolveSpawnEnvMissing(townRoot string, env map[string]string) (SpawnEnv, []string, error) {
	return resolveSpawnEnv(townRoot, env, processHost.getenv)
}

// ResolveSpawnEnv resolves env for a startup command run in the town at
// townRoot, reading ${VAR} references daemon.env does not define from this
// process's environment (unset ones expand to "", as ExpandEnvRefs does).
func ResolveSpawnEnv(townRoot string, env map[string]string) (SpawnEnv, error) {
	se, _, err := ResolveSpawnEnvMissing(townRoot, env)
	return se, err
}

// resolveSpawnEnv splits env into the values the startup command carries
// inline and the ones it reads from settings/daemon.env at run time. missing
// lists the referenced names neither daemon.env nor getenv defines, sorted.
// It fails only when daemon.env exists and does not parse.
func resolveSpawnEnv(townRoot string, env map[string]string, getenv func(string) string) (SpawnEnv, []string, error) {
	se := SpawnEnv{Inline: map[string]string{}, FromFile: map[string]string{}, all: map[string]string{}}
	if len(env) == 0 {
		return se, nil, nil
	}
	fileEnv := map[string]string{}
	if townRoot != "" {
		var err error
		if fileEnv, err = LoadDaemonEnv(townRoot); err != nil {
			return SpawnEnv{}, nil, err
		}
	}
	lookup := func(name string) string {
		if v, ok := fileEnv[name]; ok {
			return v
		}
		return getenv(name)
	}
	missing := unsetEnvRefs(env, lookup)
	for k, v := range env {
		se.all[k] = expandEnvRefs(v, lookup)
		fromFile := false
		for _, name := range envRefNames(v) {
			if _, ok := fileEnv[name]; ok {
				fromFile = true
				break
			}
		}
		if !fromFile {
			se.Inline[k] = expandEnvRefs(v, getenv)
			continue
		}
		se.FromFile[k] = daemonEnvShellWord(DaemonEnvPath(townRoot), v, fileEnv, getenv)
	}
	return se, missing, nil
}

// mergeAgentEnv resolves an agent's env block into resolvedEnv for the
// startup command of a session in townRoot and returns the shell prefix that
// sets the keys read from settings/daemon.env (removed from resolvedEnv, so
// the command's own assignments do not override them). An unresolved
// reference is an error naming agent: it would reach the provider as an
// empty credential (gt-yih1).
func mergeAgentEnv(h host, townRoot, agent string, env, resolvedEnv map[string]string) (string, error) {
	se, missing, err := resolveSpawnEnv(townRoot, env, h.getenv)
	if err != nil {
		return "", err
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("agent %q env references %s, which is not set in the environment",
			agent, strings.Join(missing, ", "))
	}
	if runtime.GOOS == "windows" {
		for k, v := range se.Values() {
			resolvedEnv[k] = v
		}
		return "", nil
	}
	for k, v := range se.Inline {
		resolvedEnv[k] = v
	}
	for k := range se.FromFile {
		delete(resolvedEnv, k)
	}
	return se.ShellPrefix(), nil
}

// daemonEnvShellWord renders v as one double-quoted shell word in which each
// reference to a daemon.env name becomes a command substitution reading it
// from path, and every other reference is expanded now from getenv. The
// word names the variable, never its value.
func daemonEnvShellWord(path, v string, fileEnv map[string]string, getenv func(string) string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(v); {
		if name, end, ok := envRefAt(v, i); ok {
			if _, inFile := fileEnv[name]; inFile {
				b.WriteString(daemonEnvRead(path, name))
			} else {
				b.WriteString(doubleQuoteEscape(getenv(name)))
			}
			i = end
			continue
		}
		b.WriteString(doubleQuoteEscape(v[i : i+1]))
		i++
	}
	b.WriteByte('"')
	return b.String()
}

// daemonEnvRead is a command substitution printing name's value from the
// daemon.env file at path with LoadDaemonEnv's rules: the key and value
// trimmed, comment lines skipped, the last assignment winning. name is an
// env var name ([A-Za-z_][A-Za-z0-9_]*), so it needs no regex escaping.
func daemonEnvRead(path, name string) string {
	script := fmt.Sprintf(`/^[[:space:]]*%s[[:space:]]*=/{s/^[^=]*=[[:space:]]*//;s/[[:space:]]*$//;p;}`, name)
	return fmt.Sprintf("$(sed -n %s %s | tail -n 1)", ShellQuote(script), ShellQuote(path))
}

// doubleQuoteEscape escapes s for the inside of a double-quoted shell word.
func doubleQuoteEscape(s string) string {
	var b strings.Builder
	for _, c := range s {
		switch c {
		case '\\', '"', '$', '`':
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}
