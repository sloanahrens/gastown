package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/migrate"
)

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

func fakeRead(ds *migrate.Dataset, err error) func(context.Context, string) (*migrate.Dataset, error) {
	return func(context.Context, string) (*migrate.Dataset, error) { return ds, err }
}

func TestDryRunReadsReportsAndWritesNothing(t *testing.T) {
	t.Parallel()
	ds := &migrate.Dataset{Issues: []migrate.IssueRow{{ID: "gt-a"}, {ID: "gt-wisp-b", Wisp: true}}, Config: map[string]string{"k": "v"}}
	var out, errb bytes.Buffer
	// No TRACKER_DSN: a dry run must not need, open or touch a tracker.
	err := runWith(context.Background(), []string{"-dry-run"}, env(map[string]string{"DOLT_DSN": "x"}), &out, &errb, fakeRead(ds, nil))
	if err != nil {
		t.Fatalf("dry run failed: %v", err)
	}
	for _, want := range []string{"read: 2 issues (1 wisps)", "1 config keys", "dry run: nothing written"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q lacks %q", out.String(), want)
		}
	}
	if strings.Contains(out.String(), "written:") || strings.Contains(out.String(), "verified") {
		t.Errorf("a dry run reported writing: %q", out.String())
	}
}

func TestReadErrorsPassThroughWithoutTheDSN(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	err := runWith(context.Background(), nil, env(map[string]string{"DOLT_DSN": "u:hunter2@tcp(h:1)/db", "TRACKER_DSN": "postgres://u:pgpass@127.0.0.1:1/db"}), &out, &errb, fakeRead(nil, errors.New("dolt unreachable")))
	if err == nil || !strings.Contains(err.Error(), "dolt unreachable") {
		t.Fatalf("error = %v, want the reader's error passed through", err)
	}
	if all := err.Error() + out.String() + errb.String(); strings.Contains(all, "hunter2") || strings.Contains(all, "pgpass") {
		t.Errorf("output leaks a password: %q", all)
	}
}

func TestAnUnreachableTrackerFailsWithoutLeakingItsDSN(t *testing.T) {
	t.Parallel()
	var out, errb bytes.Buffer
	ds := &migrate.Dataset{Issues: []migrate.IssueRow{{ID: "gt-a", Title: "t", Status: "open", Type: "task"}}}
	err := runWith(context.Background(), nil, env(map[string]string{"DOLT_DSN": "u:hunter2@tcp(h:1)/db", "TRACKER_DSN": "postgres://u:pgpass@127.0.0.1:1/db?connect_timeout=1"}), &out, &errb, fakeRead(ds, nil))
	if err == nil {
		t.Fatal("an unreachable tracker must be an error")
	}
	if all := err.Error() + out.String() + errb.String(); strings.Contains(all, "hunter2") || strings.Contains(all, "pgpass") {
		t.Errorf("output leaks a password: %q", all)
	}
}

func TestDoltConfigForcesDatetimeParsingInUTCAndKeepsTheRest(t *testing.T) {
	t.Parallel()
	cfg, err := doltConfig("root:pw@tcp(127.0.0.1:3307)/gt?parseTime=false&loc=America%2FChicago&timeout=5s")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ParseTime || cfg.Loc != time.UTC {
		t.Errorf("ParseTime = %v, Loc = %v; want true and UTC whatever the DSN said", cfg.ParseTime, cfg.Loc)
	}
	if cfg.User != "root" || cfg.Addr != "127.0.0.1:3307" || cfg.DBName != "gt" || cfg.Timeout != 5*time.Second {
		t.Errorf("config = %+v lost something the DSN named", cfg)
	}
	bare, err := doltConfig("root:@tcp(127.0.0.1:1)/gt")
	if err != nil || !bare.ParseTime {
		t.Errorf("a DSN with no options = %+v, %v; want parseTime on", bare, err)
	}
}

func TestDoltConfigErrorNeverQuotesTheDSN(t *testing.T) {
	t.Parallel()
	_, err := doltConfig("root:hunter2@tcp(127.0.0.1:3307/gt")
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("doltConfig(bad) = %v, want an error without the password", err)
	}
}
