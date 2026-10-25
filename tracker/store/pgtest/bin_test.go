package pgtest

import (
	"errors"
	"strings"
	"testing"
)

// have returns an exists func that reports true for exactly the named paths.
func have(paths ...string) func(string) bool {
	set := map[string]bool{}
	for _, p := range paths {
		set[p] = true
	}
	return func(p string) bool { return set[p] }
}

func noPath(string) (string, error) { return "", errors.New("not found") }

func TestLocatePrefersEnvThenPathThenKnownDirs(t *testing.T) {
	t.Parallel() // unit tier: pure resolution over injected lookups
	env := func(k string) string {
		if k == "TRACKER_PG_BIN" {
			return "/opt/custom/bin"
		}
		return ""
	}
	both := func(dir string) []string { return []string{dir + "/postgres", dir + "/initdb"} }

	got, err := Locate(env, have(append(both("/opt/custom/bin"), both("/usr/bin")...)...), func(string) (string, error) { return "/usr/bin/postgres", nil })
	if err != nil || got != "/opt/custom/bin" {
		t.Errorf("Locate with env set = %q, %v; want the env dir to win", got, err)
	}

	got, err = Locate(func(string) string { return "" }, have(both("/usr/bin")...), func(string) (string, error) { return "/usr/bin/postgres", nil })
	if err != nil || got != "/usr/bin" {
		t.Errorf("Locate via PATH = %q, %v; want /usr/bin", got, err)
	}

	got, err = Locate(func(string) string { return "" }, have(both("/opt/homebrew/opt/postgresql@17/bin")...), noPath)
	if err != nil || got != "/opt/homebrew/opt/postgresql@17/bin" {
		t.Errorf("Locate via the Homebrew keg = %q, %v", got, err)
	}
}

func TestLocateRequiresBothBinaries(t *testing.T) {
	t.Parallel()
	// A dir with postgres but no initdb is not a usable install.
	_, err := Locate(func(string) string { return "/x" }, have("/x/postgres"), noPath)
	if err == nil || !strings.Contains(err.Error(), "initdb") {
		t.Errorf("Locate with postgres only: err = %v, want it to name the missing initdb", err)
	}
}

func TestLocateErrorExplainsHowToFix(t *testing.T) {
	t.Parallel()
	_, err := Locate(func(string) string { return "" }, have(), noPath)
	if err == nil {
		t.Fatal("Locate with nothing installed returned no error")
	}
	for _, want := range []string{"postgresql@17", "TRACKER_PG_BIN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestRequireMajor17(t *testing.T) {
	t.Parallel()
	for out, wantErr := range map[string]bool{
		"postgres (PostgreSQL) 17.11 (Homebrew)\n": false,
		"postgres (PostgreSQL) 17.0\n":             false,
		"postgres (PostgreSQL) 16.4\n":             true,
		"postgres (PostgreSQL) 18beta1\n":          true,
		"garbage":                                  true,
	} {
		err := requireMajor17(out)
		if (err != nil) != wantErr {
			t.Errorf("requireMajor17(%q) error = %v, want error=%v", out, err, wantErr)
		}
	}
}
