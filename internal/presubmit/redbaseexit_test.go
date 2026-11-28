package presubmit

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// exitStub answers RedBaseExit's injected mkdirTemp, removeAll and check from canned results and records each call, so the unit tier makes no directory and runs no process.
type exitStub struct {
	dir                           string
	mkdirErr, checkErr, removeErr error
	ok                            bool
	mkdirs, checks, removes       int
	dirArg, pattern, base, tree   string
}

func (s *exitStub) mkdirTemp(dir, pattern string) (string, error) {
	s.mkdirs++
	s.dirArg, s.pattern = dir, pattern
	return s.dir, s.mkdirErr
}

func (s *exitStub) removeAll(string) error { s.removes++; return s.removeErr }

func (s *exitStub) check(base, dir string, _ io.Writer) (bool, error) {
	s.checks++
	s.base, s.tree = base, dir
	return s.ok, s.checkErr
}

// TestRedBaseExit holds one row per exit code: 0 only for an ok check whose directory is removed, 1 for a not-ok check or a failed make, check or removal (the removal still runs after a failed check), and 2 for an unknown flag or an empty base, which call neither mkdirTemp nor check (gt-khdcj6).
func TestRedBaseExit(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	for _, tt := range []struct {
		name                                string
		args                                []string
		stub                                exitStub
		want                                int
		wantStderr, wantBase                string
		wantMkdirs, wantChecks, wantRemoves int
	}{
		{"an ok check exits 0 with the default base", nil, exitStub{dir: "tmp", ok: true}, 0, "", "origin/main", 1, 1, 1},
		{"a not-ok check exits 1 and its directory is removed", nil, exitStub{dir: "tmp"}, 1, "", "origin/main", 1, 1, 1},
		{"a check error exits 1 with the error on stderr", nil, exitStub{dir: "tmp", ok: true, checkErr: boom}, 1, "boom", "origin/main", 1, 1, 1},
		{"a mkdir error exits 1 without the check", nil, exitStub{mkdirErr: boom}, 1, "boom", "", 1, 0, 0},
		{"a removal error exits 1 even when the check is ok", nil, exitStub{dir: "tmp", ok: true, removeErr: boom}, 1, "boom", "origin/main", 1, 1, 1},
		{"a given base reaches the check", []string{"-base", "main"}, exitStub{dir: "tmp", ok: true}, 0, "", "main", 1, 1, 1},
		{"an empty base exits 2 before anything runs", []string{"-base", ""}, exitStub{}, 2, "must not be empty", "", 0, 0, 0},
		{"an unknown flag exits 2 before anything runs", []string{"-nope"}, exitStub{}, 2, "not defined", "", 0, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := tt.stub
			var stderr strings.Builder
			if got := RedBaseExit(tt.args, io.Discard, &stderr, s.mkdirTemp, s.removeAll, s.check); got != tt.want {
				t.Errorf("exit = %d, want %d", got, tt.want)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want one naming %q", stderr.String(), tt.wantStderr)
			}
			if s.base != tt.wantBase {
				t.Errorf("check base = %q, want %q", s.base, tt.wantBase)
			}
			if s.mkdirs != tt.wantMkdirs || s.checks != tt.wantChecks || s.removes != tt.wantRemoves {
				t.Errorf("mkdirTemp, check, removeAll calls = %d, %d, %d, want %d, %d, %d", s.mkdirs, s.checks, s.removes, tt.wantMkdirs, tt.wantChecks, tt.wantRemoves)
			}
			if s.mkdirs > 0 && (s.dirArg != "" || s.pattern != "redbase-*") {
				t.Errorf("mkdirTemp(%q, %q), want the empty dir and redbase-*", s.dirArg, s.pattern)
			}
			if s.checks > 0 && s.tree != tt.stub.dir {
				t.Errorf("check tree = %q, want %q", s.tree, tt.stub.dir)
			}
		})
	}
}
