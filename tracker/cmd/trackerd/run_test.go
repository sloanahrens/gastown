package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// Unit tier: argument handling fails before any connection is attempted.
func TestRunRefusesBadInvocations(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		args []string
		env  map[string]string
		want string
	}{
		"no dsn":               {[]string{}, nil, "TRACKER_DSN"},
		"non-loopback listen":  {[]string{"-listen", "0.0.0.0:7433"}, map[string]string{"TRACKER_DSN": "x"}, "loopback"},
		"localhost by name":    {[]string{"-listen", "localhost:7433"}, map[string]string{"TRACKER_DSN": "x"}, "loopback"},
		"unknown flag":         {[]string{"-nope"}, map[string]string{"TRACKER_DSN": "x"}, "nope"},
		"token without a file": {[]string{"-issue-token", "me"}, map[string]string{"TRACKER_DSN": "x"}, "-token-file"},
		"file without a token": {[]string{"-token-file", "/x"}, map[string]string{"TRACKER_DSN": "x"}, "-issue-token"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var out, errb bytes.Buffer
			err := run(context.Background(), tc.args, env(tc.env), &out, &errb, nil)
			if err == nil || !strings.Contains(err.Error()+errb.String(), tc.want) {
				t.Errorf("run(%v) = %v (stderr %q), want an error mentioning %q", tc.args, err, errb.String(), tc.want)
			}
		})
	}
}

// The DSN holds a password; it must never reach the output or an error.
func TestRunNeverEchoesTheDSN(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"-listen", "0.0.0.0:1"}, env(map[string]string{"TRACKER_DSN": "postgres://u:hunter2@h/db"}), &out, &errb, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if all := err.Error() + out.String() + errb.String(); strings.Contains(all, "hunter2") {
		t.Errorf("output leaks the DSN password: %q", all)
	}
}
