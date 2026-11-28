package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/gocacheprog"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// checkRefused pins one refused run: 1, empty stdout, one stderr line holding want (gt-6i1x13).
func checkRefused(t *testing.T, args []string, check func(string) error, want string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(args, strings.NewReader(""), &stdout, &stderr, check); code != 1 || stdout.Len() != 0 {
		t.Fatalf("run = %d, stdout %q: want 1 and empty", code, stdout.String())
	}
	if got := stderr.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, want) {
		t.Errorf("stderr = %q, want one line containing %q", got, want)
	}
}

// TestRunRefusesBadArgs: a missing, relative or unusable -dir and an unknown
// flag each exit 1 with one cause line (gt-6i1x13).
func TestRunRefusesBadArgs(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"missing":      {nil, "-dir is required"},
		"relative":     {[]string{"-dir", "cache"}, "not absolute"},
		"unusable":     {[]string{"-dir", filepath.Join(file, "store")}, "store"},
		"unknown flag": {[]string{"-bogus"}, "flag provided but not defined"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			checkRefused(t, tc.args, func(string) error { return nil }, tc.want)
		})
	}
}

// TestRunEmptyStdinIsHandshakeOnly: a usable -dir with no requests exits 0
// with the handshake on stdout, nothing on stderr and the directory empty
// (gt-6i1x13, gt-dpjby4).
func TestRunEmptyStdinIsHandshakeOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-dir", dir}, strings.NewReader(""), &stdout, &stderr, writableCheck); code != 0 || stderr.Len() != 0 {
		t.Fatalf("run = %d, stderr %q: want 0 and empty", code, stderr.String())
	}
	if got := responses(t, stdout.String()); len(got) != 1 || got[0].ID != 0 {
		t.Errorf("stdout = %q: want the handshake alone", stdout.String())
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("dir = %v, %v: want empty", entries, err)
	}
}

// TestRunFailingCheck: a writability check that refuses exits 1 with its cause
// on stderr (gt-6i1x13).
func TestRunFailingCheck(t *testing.T) {
	t.Parallel()
	cause := errors.New("disk is read-only")
	checkRefused(t, []string{"-dir", t.TempDir()}, func(string) error { return cause }, cause.Error())
}

// responses decodes the JSON lines out holds, failing on a line that is not a
// response.
func responses(t *testing.T, out string) []gocacheprog.Response {
	t.Helper()
	var got []gocacheprog.Response
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var resp gocacheprog.Response
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("stdout line %q is not a response: %v", line, err)
		}
		got = append(got, resp)
	}
	return got
}

// TestProgServeClose: a close request gets the handshake and its answer, and
// the command exits 0 with nothing on stderr (gt-dpjby4).
func TestProgServeClose(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	in := strings.NewReader(`{"ID":1,"Command":"close"}` + "\n")
	if code := run([]string{"-dir", t.TempDir()}, in, &stdout, &stderr, writableCheck); code != 0 || stderr.Len() != 0 {
		t.Fatalf("run = %d, stderr %q: want 0 and empty", code, stderr.String())
	}
	want := []gocacheprog.Response{{ID: 0, KnownCommands: []gocacheprog.Cmd{gocacheprog.CmdGet, gocacheprog.CmdPut, gocacheprog.CmdClose}}, {ID: 1}}
	if got := responses(t, stdout.String()); !reflect.DeepEqual(got, want) {
		t.Errorf("responses = %+v, want the handshake then the close answer %+v", got, want)
	}
}

// TestProgServeMalformed: a request line that is not JSON exits 1 with its
// cause on stderr and no answer written (gt-dpjby4).
func TestProgServeMalformed(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run([]string{"-dir", t.TempDir()}, strings.NewReader("{not json}\n"), &stdout, &stderr, writableCheck)
	if got := stderr.String(); code != 1 || strings.Count(got, "\n") != 1 || !strings.Contains(got, "malformed request") {
		t.Fatalf("run = %d, stderr %q: want exit 1 and one line naming a malformed request", code, got)
	}
	if got := responses(t, stdout.String()); len(got) != 1 || got[0].ID != 0 {
		t.Errorf("stdout = %q: want the handshake alone", stdout.String())
	}
}

// TestProbeWritable: a failed create, close or remove refuses the directory by
// name (gt-6i1x13).
func TestProbeWritable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	closed, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		create func(string, string) (*os.File, error)
		remove func(string) error
		want   string
	}{
		"create fails": {func(string, string) (*os.File, error) { return nil, errors.New("create refused") }, os.Remove, "create refused"},
		"close fails":  {func(string, string) (*os.File, error) { return closed, nil }, os.Remove, "file already closed"},
		"remove fails": {os.CreateTemp, func(string) error { return errors.New("remove refused") }, "remove refused"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := probeWritable(dir, tc.create, tc.remove); err == nil || !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("probeWritable = %v, want an error naming %q and %q", err, dir, tc.want)
			}
		})
	}
}
