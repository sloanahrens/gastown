package ominstaller

import (
	"slices"
	"testing"
	"time"
)

// envWith is the environment a resolve test runs in: a town at /town and a home
// directory, with overrides applied. An empty value reads as unset.
func envWith(overrides map[string]string) func(string) string {
	env := map[string]string{"GT_TOWN_ROOT": "/town", "HOME": "/home/op"}
	for k, v := range overrides {
		env[k] = v
	}
	return func(k string) string { return env[k] }
}

// filesExist reports only the listed paths as existing.
func filesExist(files []string) func(string) bool {
	return func(p string) bool { return slices.Contains(files, p) }
}

func TestResolvePrecedence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		cfg   Config
		env   map[string]string
		cwd   string
		files []string
		field func(Config) string
		want  string
	}{
		{
			name:  "om path: Config beats INSTALL_OM_PATH beats the default",
			cfg:   Config{OMPath: "/cfg/om"},
			env:   map[string]string{envOMPath: "/env/om"},
			field: func(c Config) string { return c.OMPath },
			want:  "/cfg/om",
		},
		{
			name:  "om path: INSTALL_OM_PATH beats the default",
			env:   map[string]string{envOMPath: "/env/om"},
			field: func(c Config) string { return c.OMPath },
			want:  "/env/om",
		},
		{
			name:  "om path: the default is HOME/go/bin/om",
			field: func(c Config) string { return c.OMPath },
			want:  "/home/op/go/bin/om",
		},
		{
			name:  "town: GT_TOWN_ROOT beats the search",
			cwd:   "/work/rig",
			files: []string{"/work/mayor/town.json"},
			field: func(c Config) string { return c.TownRoot },
			want:  "/town",
		},
		{
			name:  "town: with no GT_TOWN_ROOT the outermost town above the working directory",
			env:   map[string]string{envTownRoot: ""},
			cwd:   "/work/rig/src",
			files: []string{"/work/mayor/town.json", "/work/rig/mayor/town.json"},
			field: func(c Config) string { return c.TownRoot },
			want:  "/work",
		},
		{
			name:  "clone: Config beats INSTALL_OM_SRC_DIR beats <town>/.runtime/om-src",
			cfg:   Config{SrcDir: "/cfg/src"},
			env:   map[string]string{envSrcDir: "/env/src"},
			field: func(c Config) string { return c.SrcDir },
			want:  "/cfg/src",
		},
		{
			name:  "clone: INSTALL_OM_SRC_DIR beats the default",
			env:   map[string]string{envSrcDir: "/env/src"},
			field: func(c Config) string { return c.SrcDir },
			want:  "/env/src",
		},
		{
			name:  "clone: the default is <town>/.runtime/om-src",
			field: func(c Config) string { return c.SrcDir },
			want:  "/town/.runtime/om-src",
		},
		{
			name:  "signing directory: Config beats INSTALL_OM_SIGN_DIR beats <town>/.runtime/signing",
			cfg:   Config{SignDir: "/cfg/sign"},
			env:   map[string]string{envSignDir: "/env/sign"},
			field: func(c Config) string { return c.SignDir },
			want:  "/cfg/sign",
		},
		{
			name:  "signing directory: INSTALL_OM_SIGN_DIR beats the default",
			env:   map[string]string{envSignDir: "/env/sign"},
			field: func(c Config) string { return c.SignDir },
			want:  "/env/sign",
		},
		{
			name:  "signing directory: the default is <town>/.runtime/signing",
			field: func(c Config) string { return c.SignDir },
			want:  "/town/.runtime/signing",
		},
		{
			name:  "lock wait: Config beats INSTALL_OM_LOCK_WAIT beats 300 s",
			cfg:   Config{LockWait: 5 * time.Second},
			env:   map[string]string{envLockWait: "7"},
			field: func(c Config) string { return c.LockWait.String() },
			want:  "5s",
		},
		{
			name:  "lock wait: INSTALL_OM_LOCK_WAIT beats the default",
			env:   map[string]string{envLockWait: "7"},
			field: func(c Config) string { return c.LockWait.String() },
			want:  "7s",
		},
		{
			name:  "lock wait: the default is 300 s",
			field: func(c Config) string { return c.LockWait.String() },
			want:  "5m0s",
		},
		{
			name:  "sign wait: Config beats INSTALL_OM_SIGN_TIMEOUT beats 30 s",
			cfg:   Config{SignWait: 9 * time.Second},
			env:   map[string]string{envSignWait: "11"},
			field: func(c Config) string { return c.SignWait.String() },
			want:  "9s",
		},
		{
			name:  "sign wait: INSTALL_OM_SIGN_TIMEOUT beats the default",
			env:   map[string]string{envSignWait: "11"},
			field: func(c Config) string { return c.SignWait.String() },
			want:  "11s",
		},
		{
			name:  "sign wait: the default is 30 s",
			field: func(c Config) string { return c.SignWait.String() },
			want:  "30s",
		},
		{
			name:  "smoke wait: Config beats INSTALL_OM_SMOKE_TIMEOUT beats 30 s",
			cfg:   Config{SmokeWait: 4 * time.Second},
			env:   map[string]string{envSmokeWait: "13"},
			field: func(c Config) string { return c.SmokeWait.String() },
			want:  "4s",
		},
		{
			name:  "smoke wait: INSTALL_OM_SMOKE_TIMEOUT beats the default",
			env:   map[string]string{envSmokeWait: "13"},
			field: func(c Config) string { return c.SmokeWait.String() },
			want:  "13s",
		},
		{
			name:  "smoke wait: the default is 30 s",
			field: func(c Config) string { return c.SmokeWait.String() },
			want:  "30s",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveConfig(tc.cfg, tc.cwd, envWith(tc.env), filesExist(tc.files))
			if err != nil {
				t.Fatalf("resolveConfig: %v", err)
			}
			if g := tc.field(got); g != tc.want {
				t.Errorf("resolved value = %q, want %q", g, tc.want)
			}
		})
	}
}

func TestResolveRefusesAValueItCannotResolve(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		env   map[string]string
		cwd   string
		files []string
	}{
		{
			name: "no GT_TOWN_ROOT and no town above the working directory",
			env:  map[string]string{envTownRoot: ""},
			cwd:  "/work/rig",
		},
		{
			name: "no INSTALL_OM_PATH and no HOME for the default",
			env:  map[string]string{envOMPath: "", "HOME": ""},
		},
		{
			name: "a lock wait that is not a number",
			env:  map[string]string{envLockWait: "soon"},
		},
		{
			name: "a negative sign wait",
			env:  map[string]string{envSignWait: "-1"},
		},
		{
			name: "a smoke wait too large for a duration",
			env:  map[string]string{envSmokeWait: "99999999999"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := resolveConfig(Config{}, tc.cwd, envWith(tc.env), filesExist(tc.files)); err == nil {
				t.Error("resolveConfig returned no error, want a refusal")
			}
		})
	}
}
