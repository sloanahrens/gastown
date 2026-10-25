package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/tracker/internal/leakcheck"
)

func TestMain(m *testing.M) { os.Exit(leakcheck.Main(m)) }

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestRunRefusesBadInvocationsBeforeConnecting(t *testing.T) {
	t.Parallel() // unit tier: fails before any connection
	for name, tc := range map[string]struct {
		args []string
		env  map[string]string
		want string
	}{
		"no dolt dsn":    {nil, nil, "DOLT_DSN"},
		"no tracker dsn": {nil, map[string]string{"DOLT_DSN": "x"}, "TRACKER_DSN"},
		"bad rig name":   {[]string{"-rig-name", "gt"}, map[string]string{"DOLT_DSN": "x", "TRACKER_DSN": "y"}, "prefix=name"},
		"unknown flag":   {[]string{"-nope"}, nil, "nope"},
	} {
		var out, errb bytes.Buffer
		err := run(context.Background(), tc.args, env(tc.env), &out, &errb)
		if err == nil || !strings.Contains(err.Error()+errb.String(), tc.want) {
			t.Errorf("%s: error = %v (%s), want one mentioning %q", name, err, errb.String(), tc.want)
		}
	}
}

func TestRunNeverEchoesADSN(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	err := run(context.Background(), nil, env(map[string]string{"DOLT_DSN": "u:hunter2@tcp(127.0.0.1:1)/db?parseTime=true", "TRACKER_DSN": "postgres://u:pgpass@127.0.0.1:1/db"}), &out, &errb)
	if err == nil {
		t.Fatal("an unreachable Dolt must be an error")
	}
	if all := err.Error() + out.String() + errb.String(); strings.Contains(all, "hunter2") || strings.Contains(all, "pgpass") {
		t.Errorf("output leaks a password: %q", all)
	}
}

func TestDryRunNeedsNoTrackerDSN(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	err := run(context.Background(), []string{"-dry-run"}, env(map[string]string{"DOLT_DSN": "u:p@tcp(127.0.0.1:1)/db"}), &out, &errb)
	if err != nil && strings.Contains(err.Error(), "TRACKER_DSN") {
		t.Errorf("a dry run asked for TRACKER_DSN: %v", err)
	}
}
