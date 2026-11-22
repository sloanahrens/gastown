package main

import (
	"flag"
	"strings"
	"testing"
)

// TestBdgwConfigRequire: a setting the mode needs but the environment left
// empty is refused by its flag's name, and a name this command does not know is
// a refusal too rather than a setting silently left unset.
func TestBdgwConfigRequire(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		c     config
		names []string
		want  string
	}{
		{name: "missing token directory", names: []string{"tokens-dir"}, want: "--tokens-dir is required"},
		{name: "missing verbs file", names: []string{"verbs"}, want: "--verbs is required"},
		{name: "missing log directory", names: []string{"log-dir"}, want: "--log-dir is required"},
		{name: "missing workspace", names: []string{"workspace"}, want: "--workspace is required"},
		{name: "missing bd", names: []string{"bd"}, want: "--bd is required"},
		{name: "missing home", names: []string{"home"}, want: "--home is required"},
		{name: "unknown setting", names: []string{"nope"}, want: `unknown setting "nope"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.c.require(tc.names...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("require(%v) = %v, want a refusal naming %q", tc.names, err, tc.want)
			}
		})
	}
}

// TestBdgwDoltCredentialFile: the Dolt credential file is empty by default, and
// a value given on the command line reaches the executor's Options.
func TestBdgwDoltCredentialFile(t *testing.T) {
	t.Parallel()
	// A zero config keeps this test off the host environment.
	var c config
	fs := flag.NewFlagSet("bdgw", flag.ContinueOnError)
	bindShared(fs, &c)
	if err := fs.Parse([]string{"--dolt-credential-file=/run/secrets/dolt.cred"}); err != nil {
		t.Fatalf("parsing the flag: %v", err)
	}
	if got := c.options().DoltCredentialFile; got != "/run/secrets/dolt.cred" {
		t.Errorf("Options.DoltCredentialFile = %q, want the flag's value", got)
	}
	if got := (config{}).options().DoltCredentialFile; got != "" {
		t.Errorf("Options.DoltCredentialFile with no flag = %q, want empty", got)
	}
}

// TestBdgwCallLogNeedsDir: the call log refuses to open without a directory,
// so a call is never run with nowhere to be recorded.
func TestBdgwCallLogNeedsDir(t *testing.T) {
	t.Parallel()
	if _, err := (config{}).callLog(nil); err == nil || !strings.Contains(err.Error(), "--log-dir is required") {
		t.Fatalf("callLog with no directory = %v, want a --log-dir refusal", err)
	}
}
