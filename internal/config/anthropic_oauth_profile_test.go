package config

import (
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The anthropic-oauth provider profile hands a Claude subscription token to a
// sandbox as a placeholder the proxy swaps (claude-q3f.13). It is the one
// profile whose credential must be the setup-token variable and nothing else:
// ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN in the same environment would
// outrank the token and bill API rates. Unit tier: it reads one shipped file.

const shippedAnthropicOAuthProfile = "../../deploy/polecat/profiles/anthropic-oauth.yaml"

// claudeLauncher is the one binary that may receive the credential: the
// placeholder is only swapped for requests this binary makes, so listing node
// would widen the token's scope to every Node program in the sandbox.
const claudeLauncher = "/usr/lib/node_modules/@anthropic-ai/claude-code/bin/claude.exe"

type profileCredential struct {
	EnvVars    []string `yaml:"env_vars"`
	AuthStyle  string   `yaml:"auth_style"`
	HeaderName string   `yaml:"header_name"`
}

type profileEndpoint struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type providerProfileFile struct {
	ID          string              `yaml:"id"`
	Credentials []profileCredential `yaml:"credentials"`
	Endpoints   []profileEndpoint   `yaml:"endpoints"`
	Binaries    []string            `yaml:"binaries"`
}

// anthropicOAuthProfileProblems lists every way a profile breaks the contract.
func anthropicOAuthProfileProblems(p providerProfileFile) []string {
	var out []string
	if p.ID != "anthropic-oauth" {
		out = append(out, "id is "+p.ID+", want anthropic-oauth")
	}
	var vars []string
	for _, c := range p.Credentials {
		vars = append(vars, c.EnvVars...)
		if c.AuthStyle != "bearer" || c.HeaderName != "authorization" {
			out = append(out, "a credential is not a bearer authorization header")
		}
	}
	if !slices.Equal(vars, []string{"CLAUDE_CODE_OAUTH_TOKEN"}) {
		out = append(out, "credential env vars are "+strings.Join(vars, ",")+", want exactly CLAUDE_CODE_OAUTH_TOKEN")
	}
	if len(p.Endpoints) != 1 || p.Endpoints[0].Host != "api.anthropic.com" || p.Endpoints[0].Port != 443 {
		out = append(out, "endpoints are not exactly api.anthropic.com:443")
	}
	if !slices.Equal(p.Binaries, []string{claudeLauncher}) {
		out = append(out, "binaries are "+strings.Join(p.Binaries, ",")+", want only "+claudeLauncher)
	}
	return out
}

func TestShippedAnthropicOAuthProfileMeetsContract(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(shippedAnthropicOAuthProfile)
	if err != nil {
		t.Fatalf("reading the shipped profile: %v", err)
	}
	var p providerProfileFile
	if err := yaml.Unmarshal(src, &p); err != nil {
		t.Fatalf("parsing %s: %v", shippedAnthropicOAuthProfile, err)
	}
	if probs := anthropicOAuthProfileProblems(p); len(probs) != 0 {
		t.Fatalf("%s breaks the contract: %s", shippedAnthropicOAuthProfile, strings.Join(probs, "; "))
	}
	for _, banned := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		if strings.Contains(string(src), banned) {
			t.Errorf("%s names %s, which outranks the subscription token and bills API rates", shippedAnthropicOAuthProfile, banned)
		}
	}
}

func TestAnthropicOAuthProfileProblemsRefusesEachBreak(t *testing.T) {
	t.Parallel()
	good := func() providerProfileFile {
		return providerProfileFile{
			ID:          "anthropic-oauth",
			Credentials: []profileCredential{{EnvVars: []string{"CLAUDE_CODE_OAUTH_TOKEN"}, AuthStyle: "bearer", HeaderName: "authorization"}},
			Endpoints:   []profileEndpoint{{Host: "api.anthropic.com", Port: 443}},
			Binaries:    []string{claudeLauncher},
		}
	}
	if probs := anthropicOAuthProfileProblems(good()); len(probs) != 0 {
		t.Fatalf("the known-good profile has problems: %v", probs)
	}
	cases := map[string]func(*providerProfileFile){
		"wrong id":         func(p *providerProfileFile) { p.ID = "anthropic" },
		"api key variable": func(p *providerProfileFile) { p.Credentials[0].EnvVars = []string{"ANTHROPIC_API_KEY"} },
		"extra auth token": func(p *providerProfileFile) {
			p.Credentials[0].EnvVars = append(p.Credentials[0].EnvVars, "ANTHROPIC_AUTH_TOKEN")
		},
		"not a bearer":       func(p *providerProfileFile) { p.Credentials[0].AuthStyle = "header" },
		"other host":         func(p *providerProfileFile) { p.Endpoints[0].Host = "api.deepseek.com" },
		"second endpoint":    func(p *providerProfileFile) { p.Endpoints = append(p.Endpoints, p.Endpoints[0]) },
		"wrong port":         func(p *providerProfileFile) { p.Endpoints[0].Port = 80 },
		"no credential left": func(p *providerProfileFile) { p.Credentials = nil },
		"node may send the token": func(p *providerProfileFile) {
			p.Binaries = append(p.Binaries, "/usr/bin/node")
		},
		"no binary listed": func(p *providerProfileFile) { p.Binaries = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := good()
			mutate(&p)
			if probs := anthropicOAuthProfileProblems(p); len(probs) == 0 {
				t.Errorf("%s: the checker accepted a profile that breaks the contract", name)
			}
		})
	}
}
