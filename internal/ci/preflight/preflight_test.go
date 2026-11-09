package preflight_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/ci/preflight"
)

const (
	// green is the commit the job builds at.
	green = "0123456789abcdef0123456789abcdef01234567"
	// tag is its twelve-character tag.
	tag = "0123456789ab"
	// mango is the commit the mango repository variable pins.
	mango = "fedcba9876543210fedcba9876543210fedcba98"
	// host is the Forgejo host:port the pins, the images and mango name.
	host = "forgejo:3000"
	// mangoURL is the mango source the repository variable names.
	mangoURL = "http://forgejo:3000/example-owner/mango.git"
)

// lookPath answers with the tools in present and a miss for the rest.
func lookPath(present []string) func(string) (string, error) {
	return func(name string) (string, error) {
		if slices.Contains(present, name) {
			return "/usr/local/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
}

// installed is every tool the job runs.
func installed() []string {
	return slices.Clone(preflight.Tools)
}

// options is a passing check at the job's fixture values.
func options(t *testing.T) (preflight.Options, *strings.Builder) {
	t.Helper()
	env := &strings.Builder{}
	return preflight.Options{
		LookPath:    lookPath(installed()),
		Version:     func() (string, error) { return preflight.JQVersion, nil },
		SHA:         green,
		MangoURL:    mangoURL,
		MangoCommit: mango,
		Host:        host,
		EnvOut:      env,
	}, env
}

// TestCheckAcceptsTheRunnerAndTheGreenCommit holds the passing branch: every
// rule holds, and the SHA and TAG the later steps read are written.
func TestCheckAcceptsTheRunnerAndTheGreenCommit(t *testing.T) {
	t.Parallel()
	o, env := options(t)
	o.Version = func() (string, error) { return preflight.JQVersion + "\n", nil }

	got, err := preflight.Check(o)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got.SHA != green || got.Tag != tag {
		t.Errorf("Check answered %+v, want the green commit %s and tag %s", got, green, tag)
	}
	want := "SHA=" + green + "\nTAG=" + tag + "\n"
	if got := env.String(); got != want {
		t.Errorf("Check wrote %q to the environment, want %q", got, want)
	}
}

// TestCheckRefusesAMissingTool holds the tool rule: a tool the runner does not
// carry is named and the job stops, with no commit written for the steps after
// it.
func TestCheckRefusesAMissingTool(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		present []string
		want    string
		absent  []string
	}{
		{"jq", []string{"go", "docker", "git"}, "missing: jq", []string{"missing: go", "missing: docker", "missing: git"}},
		{"go", []string{"jq", "docker", "git"}, "missing: go", []string{"missing: jq", "missing: docker"}},
		{"docker", []string{"jq", "go", "git"}, "missing: docker", []string{"missing: jq", "missing: go"}},
		{"git", []string{"jq", "go", "docker"}, "missing: git", []string{"missing: jq", "missing: docker"}},
		{"two tools", []string{"jq", "go"}, "missing: docker git", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o, env := options(t)
			o.LookPath = lookPath(tc.present)

			_, err := preflight.Check(o)
			if err == nil {
				t.Fatalf("Check passed with %s missing", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Check error = %q, want it to contain %q", err, tc.want)
			}
			for _, absent := range tc.absent {
				if strings.Contains(err.Error(), absent) {
					t.Errorf("Check error = %q, want it not to name %q", err, absent)
				}
			}
			if env.Len() != 0 {
				t.Errorf("Check wrote %q with a tool missing", env.String())
			}
		})
	}
}

// TestCheckRefusesAnotherJQ holds the jq rule: the version the runner prints
// must be the base image's 1.7.1, and the error names both.
func TestCheckRefusesAnotherJQ(t *testing.T) {
	t.Parallel()
	o, env := options(t)
	o.Version = func() (string, error) { return "jq-1.6\n", nil }

	_, err := preflight.Check(o)
	if err == nil {
		t.Fatal("Check passed with another jq")
	}
	for _, want := range []string{"jq-1.6", "jq-1.7.1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Check error = %q, want it to name %q", err, want)
		}
	}
	if env.Len() != 0 {
		t.Errorf("Check wrote %q with another jq", env.String())
	}
}

// TestCheckRefusesAJQItCannotRead holds the fail-closed branch: a version read
// that fails is not read as the base image's jq.
func TestCheckRefusesAJQItCannotRead(t *testing.T) {
	t.Parallel()
	o, env := options(t)
	o.Version = func() (string, error) { return "", errors.New("exit status 127") }

	_, err := preflight.Check(o)
	if err == nil {
		t.Fatal("Check passed when jq --version failed")
	}
	if !strings.Contains(err.Error(), "jq --version") {
		t.Errorf("Check error = %q, want it to name the command", err)
	}
	if env.Len() != 0 {
		t.Errorf("Check wrote %q when it could not read jq's version", env.String())
	}
}

// TestCheckRefusesAGreenCommitThatIsNotAFullSHA holds the commit rule: the
// green commit is a full lowercase sha, the one form the tag and the image
// label take.
func TestCheckRefusesAGreenCommitThatIsNotAFullSHA(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, sha string }{
		{"empty", ""},
		{"the short tag", tag},
		{"uppercase hex", strings.ToUpper(green)},
		{"a non-hex character", strings.Repeat("g", 40)},
		{"a character too many", green + "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o, env := options(t)
			o.SHA = tc.sha

			_, err := preflight.Check(o)
			if err == nil {
				t.Fatalf("Check accepted the green commit %q", tc.sha)
			}
			if !strings.Contains(err.Error(), "green commit") {
				t.Errorf("Check error = %q, want it to name the green commit", err)
			}
			if env.Len() != 0 {
				t.Errorf("Check wrote %q for a commit it refused", env.String())
			}
		})
	}
}

// TestCheckRefusesAMangoURLTheTokenMayNotReach holds the pin rule: the mango
// source is an http(s) URL on the Forgejo host, the one host the read-only
// token is given to, so no pin sends it elsewhere or in cleartext off-host.
func TestCheckRefusesAMangoURLTheTokenMayNotReach(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		url     string
		wantErr bool
		want    string
	}{
		{"https on the Forgejo host", "https://forgejo:3000/example-owner/mango.git", false, ""},
		{"http on the Forgejo host", mangoURL, false, ""},
		{"https on another host", "https://example.com/mango.git", true, `names "example.com", but the read-only token is only given to`},
		{"http on another host", "http://example.com/mango.git", true, `names "example.com", but the read-only token is only given to`},
		{"another scheme", "ftp://forgejo:3000/mango.git", true, "is not an http or https URL"},
		{"no scheme", "forgejo:3000/example-owner/mango.git", true, "is not an http or https URL"},
		{"a URL that does not parse", "http://[::1", true, "is not a URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o, env := options(t)
			o.MangoURL = tc.url

			_, err := preflight.Check(o)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Check refused %s: %v", tc.url, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Check accepted the mango URL %s", tc.url)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Check error = %q, want it to contain %q", err, tc.want)
			}
			if env.Len() != 0 {
				t.Errorf("Check wrote %q for a pin it refused", env.String())
			}
		})
	}
}

// TestCheckRefusesAnEmptyMangoPin holds the missing-variable branch: the
// operator's two repository variables must both be set.
func TestCheckRefusesAnEmptyMangoPin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		clear func(*preflight.Options)
	}{
		{"MANGO_URL", func(o *preflight.Options) { o.MangoURL = "" }},
		{"MANGO_COMMIT", func(o *preflight.Options) { o.MangoCommit = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o, env := options(t)
			tc.clear(&o)

			_, err := preflight.Check(o)
			if err == nil {
				t.Fatalf("Check passed with %s empty", tc.name)
			}
			if !strings.Contains(err.Error(), "MANGO_URL and MANGO_COMMIT") {
				t.Errorf("Check error = %q, want it to name both variables", err)
			}
			if env.Len() != 0 {
				t.Errorf("Check wrote %q with %s empty", env.String(), tc.name)
			}
		})
	}
}

// TestCheckRefusesAMalformedMangoCommit holds the mango commit rule: the pin
// names a full lowercase sha, as the daemon image's build takes it.
func TestCheckRefusesAMalformedMangoCommit(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, commit string }{
		{"short", mango[:12]},
		{"uppercase hex", strings.ToUpper(mango)},
		{"a non-hex character", strings.Repeat("z", 40)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o, env := options(t)
			o.MangoCommit = tc.commit

			_, err := preflight.Check(o)
			if err == nil {
				t.Fatalf("Check accepted the mango commit %q", tc.commit)
			}
			if !strings.Contains(err.Error(), "MANGO_COMMIT") {
				t.Errorf("Check error = %q, want it to name MANGO_COMMIT", err)
			}
			if env.Len() != 0 {
				t.Errorf("Check wrote %q for a pin it refused", env.String())
			}
		})
	}
}

// TestCheckRefusesAnUnwiredCheck holds the fail-closed rule: a seam that is
// not wired is refused rather than read as passing.
func TestCheckRefusesAnUnwiredCheck(t *testing.T) {
	t.Parallel()
	fields := []struct {
		name  string
		clear func(*preflight.Options)
		want  string
	}{
		{"a tool lookup", func(o *preflight.Options) { o.LookPath = nil }, "no tool lookup is wired"},
		{"a jq version read", func(o *preflight.Options) { o.Version = nil }, "no jq version read is wired"},
		{"an environment output", func(o *preflight.Options) { o.EnvOut = nil }, "no environment output is wired"},
	}
	for _, field := range fields {
		t.Run("without "+field.name, func(t *testing.T) {
			t.Parallel()
			o, _ := options(t)
			field.clear(&o)

			_, err := preflight.Check(o)
			if err == nil {
				t.Fatalf("Check passed without %s", field.name)
			}
			if !strings.Contains(err.Error(), field.want) {
				t.Errorf("Check error = %q, want it to contain %q", err, field.want)
			}
		})
	}
}

// failingWriter is an environment output that cannot be written.
type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) { return 0, errors.New("no space left") }

// TestCheckRefusesAnEnvironmentItCannotWrite holds the write rule: a commit
// the later steps cannot read is a failure, not a pass.
func TestCheckRefusesAnEnvironmentItCannotWrite(t *testing.T) {
	t.Parallel()
	o, _ := options(t)
	o.EnvOut = failingWriter{}

	_, err := preflight.Check(o)
	if err == nil {
		t.Fatal("Check passed when the environment could not be written")
	}
	if !strings.Contains(err.Error(), "writing SHA and TAG") {
		t.Errorf("Check error = %q, want it to name the environment write", err)
	}
}
