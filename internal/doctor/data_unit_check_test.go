package doctor

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// healthyPs is the compose ps output of a unit whose two services run healthy.
const healthyPs = "dolt running healthy\nbd running healthy\n"

// dataUnitFixture is one data unit as the rungs read it. ps is the compose ps
// output; psErr makes the status call fail. tokens is the token directory's
// files and their modes; nil means no token directory is configured. readyz is
// the gateway's /readyz status; 0 means the gateway does not answer.
type dataUnitFixture struct {
	ps         string
	psErr      bool
	tokens     map[string]os.FileMode
	missingDir bool // the token directory is configured but does not exist
	readyz     int
}

// healthyDataUnit is a unit whose every rung passes.
func healthyDataUnit() dataUnitFixture {
	return dataUnitFixture{
		ps:     healthyPs,
		tokens: map[string]os.FileMode{"daemon.gt-wrap.token": 0o600, "read.dash.token": 0o600},
		readyz: http.StatusOK,
	}
}

// dataUnitCalls counts the calls one check makes to the compose status and the
// gateway, so a test can say a rung was or was not reached.
type dataUnitCalls struct {
	ps     int
	readyz int
}

// newDataUnitCheck builds the check over fx, named on the command line: the
// status call answers from the fixture, the token directory is a temp dir with
// the fixture's modes, and /readyz is answered in process by a handler that
// accepts only the daemon token.
func newDataUnitCheck(t *testing.T, fx dataUnitFixture) (*DataUnitCheck, *dataUnitCalls) {
	t.Helper()
	calls := &dataUnitCalls{}
	dir := ""
	if fx.missingDir {
		dir = filepath.Join(t.TempDir(), "absent")
	}
	if fx.tokens != nil {
		dir = t.TempDir()
		for name, mode := range fx.tokens {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte("tok-"+name), 0o600); err != nil {
				t.Fatalf("WriteFile %s: %v", name, err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatalf("Chmod %s: %v", name, err)
			}
		}
	}

	c := NewDataUnitCheck(true, func(string) ([]byte, error) {
		calls.ps++
		if fx.psErr {
			return nil, errors.New("docker compose ps for gt-data: docker exited with status 1")
		}
		return []byte(fx.ps), nil
	}, func(string) (string, error) {
		if fx.tokens == nil && !fx.missingDir {
			return "", errors.New("the bd gateway's token directory is not in the town config")
		}
		return dir, nil
	})
	if fx.readyz == 0 {
		c.http = &http.Client{Transport: failingTransport{}}
	} else {
		c.http = &http.Client{Transport: handlerTransport{dataUnitReadyzHandler(fx.readyz, calls)}}
	}
	return c, calls
}

// dataUnitReadyzHandler answers /readyz with status. It counts the requests, and
// answers 401 unless the bearer is the daemon token the fixture holds, so a check
// that presents the wrong token reads as not ready.
func dataUnitReadyzHandler(status int, calls *dataUnitCalls) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.readyz++
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok-daemon.gt-wrap.token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
	})
}

func TestDataUnitDoctor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		fx          func() dataUnitFixture
		wantStatus  CheckStatus
		wantRung    string // the rung the message names; empty for OK
		wantReadyz  int    // /readyz requests the check made
		wantDetails int    // rungs passed, listed in Details
	}{
		{
			name:        "every rung passes",
			fx:          healthyDataUnit,
			wantStatus:  StatusOK,
			wantReadyz:  1,
			wantDetails: 4,
		},
		{
			name: "dolt is not running",
			fx: func() dataUnitFixture {
				fx := healthyDataUnit()
				fx.ps = "dolt exited\nbd running healthy\n"
				return fx
			},
			wantStatus: StatusError,
			wantRung:   "dolt",
		},
		{
			name: "dolt is running but not healthy",
			fx: func() dataUnitFixture {
				fx := healthyDataUnit()
				fx.ps = "dolt running starting\nbd running healthy\n"
				return fx
			},
			wantStatus: StatusError,
			wantRung:   "dolt",
		},
		{
			name: "dolt has no health check, so it is not healthy",
			fx: func() dataUnitFixture {
				fx := healthyDataUnit()
				fx.ps = "dolt running\nbd running healthy\n"
				return fx
			},
			wantStatus: StatusError,
			wantRung:   "dolt",
		},
		{
			name: "dolt is not in the unit",
			fx: func() dataUnitFixture {
				fx := healthyDataUnit()
				fx.ps = "bd running healthy\n"
				return fx
			},
			wantStatus: StatusError,
			wantRung:   "dolt",
		},
		{
			name: "dolt is listed twice",
			fx: func() dataUnitFixture {
				fx := healthyDataUnit()
				fx.ps = "dolt running healthy\ndolt running healthy\nbd running healthy\n"
				return fx
			},
			wantStatus: StatusError,
			wantRung:   "dolt",
		},
		{
			name: "bd is not healthy, so the token rungs are not reached",
			fx: func() dataUnitFixture {
				fx := healthyDataUnit()
				fx.ps = "dolt running healthy\nbd running unhealthy\n"
				return fx
			},
			wantStatus: StatusError,
			wantRung:   "bd",
		},
		{
			name: "the compose status cannot be read",
			fx: func() dataUnitFixture {
				fx := healthyDataUnit()
				fx.psErr = true
				return fx
			},
			wantStatus: StatusError,
			wantRung:   "unit",
		},
		{
			name: "no token directory is configured",
			fx: func() dataUnitFixture {
				fx := healthyDataUnit()
				fx.tokens = nil
				return fx
			},
			wantStatus: StatusError,
			wantRung:   "tokens",
		},
		{
			name: "the token directory holds no daemon token",
			fx: func() dataUnitFixture {
				fx := healthyDataUnit()
				fx.tokens = map[string]os.FileMode{"read.dash.token": 0o600}
				return fx
			},
			wantStatus: StatusError,
			wantRung:   "tokens",
		},
		{
			name: "dolt's ps line names no state",
			fx: func() dataUnitFixture {
				fx := healthyDataUnit()
				fx.ps = "dolt\nbd running healthy\n"
				return fx
			},
			wantStatus: StatusError,
			wantRung:   "dolt",
		},
		{
			name: "the token directory cannot be read",
			fx: func() dataUnitFixture {
				fx := healthyDataUnit()
				fx.tokens = nil
				fx.missingDir = true
				return fx
			},
			wantStatus: StatusError,
			wantRung:   "tokens",
		},
		{
			name: "the token directory holds no token files",
			fx: func() dataUnitFixture {
				fx := healthyDataUnit()
				fx.tokens = map[string]os.FileMode{}
				return fx
			},
			wantStatus: StatusError,
			wantRung:   "tokens",
		},
		{
			name: "the gateway answers readyz with 401",
			fx: func() dataUnitFixture {
				fx := healthyDataUnit()
				fx.readyz = http.StatusUnauthorized
				return fx
			},
			wantStatus: StatusError,
			wantRung:   "readyz",
			wantReadyz: 1,
		},
		{
			name: "the gateway does not answer",
			fx: func() dataUnitFixture {
				fx := healthyDataUnit()
				fx.readyz = 0
				return fx
			},
			wantStatus: StatusError,
			wantRung:   "readyz",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, calls := newDataUnitCheck(t, tt.fx())
			res := c.Run(&CheckContext{TownRoot: t.TempDir()})
			if res.Status != tt.wantStatus {
				t.Fatalf("status = %v, want %v; message %q", res.Status, tt.wantStatus, res.Message)
			}
			if tt.wantRung != "" && !strings.Contains(res.Message, "data unit rung "+tt.wantRung+" failed") {
				t.Errorf("message %q does not name the first failing rung %q", res.Message, tt.wantRung)
			}
			if tt.wantRung == "" && len(res.Details) != tt.wantDetails {
				t.Errorf("details = %v, want %d rungs passed", res.Details, tt.wantDetails)
			}
			if calls.readyz != tt.wantReadyz {
				t.Errorf("readyz requests = %d, want %d", calls.readyz, tt.wantReadyz)
			}
		})
	}
}

// fakeDirEntry is a directory entry whose stat fails, the case a token file
// removed or made unreadable between the listing and the stat presents.
type fakeDirEntry struct {
	name string
	err  error
}

func (e fakeDirEntry) Name() string               { return e.name }
func (e fakeDirEntry) IsDir() bool                { return false }
func (e fakeDirEntry) Type() fs.FileMode          { return 0 }
func (e fakeDirEntry) Info() (fs.FileInfo, error) { return nil, e.err }

// TestDataUnitDoctorTokenEntryStat: a token entry whose stat fails stops the
// token rung by name, so an unreadable file never passes as mode 0600.
func TestDataUnitDoctorTokenEntryStat(t *testing.T) {
	t.Parallel()
	entries := []fs.DirEntry{fakeDirEntry{name: "daemon.gt-wrap.token", err: errors.New("stat denied")}}
	_, wide, err := dataUnitTokenEntries("/token/dir", entries)
	if err == nil || !strings.Contains(err.Error(), "daemon.gt-wrap.token cannot be stat'd") {
		t.Fatalf("dataUnitTokenEntries = %v, %v; want the stat failure named", wide, err)
	}
}

// TestDataUnitDoctorReadyzBadURL: a gateway address the request cannot be built
// for is a readyz failure, reported before anything is sent.
func TestDataUnitDoctorReadyzBadURL(t *testing.T) {
	t.Parallel()
	err := dataUnitReadyz(&http.Client{Transport: failingTransport{}}, "http://[::1", "tok")
	if err == nil || !strings.Contains(err.Error(), "cannot be built") {
		t.Fatalf("dataUnitReadyz = %v, want the request that cannot be built", err)
	}
}

func TestDataUnitDoctorTokenMode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		file     string
		mode     os.FileMode
		wantName string
	}{
		{name: "daemon token wider than 0600", file: "daemon.gt-wrap.token", mode: 0o644, wantName: "daemon.gt-wrap.token (mode 0644)"},
		{name: "group-writable daemon token", file: "daemon.gt-wrap.token", mode: 0o660, wantName: "daemon.gt-wrap.token (mode 0660)"},
		{name: "read token wider than 0600", file: "read.dash.token", mode: 0o666, wantName: "read.dash.token (mode 0666)"},
		{name: "narrower than 0600 is not the mode either", file: "daemon.gt-wrap.token", mode: 0o400, wantName: "daemon.gt-wrap.token (mode 0400)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := healthyDataUnit()
			fx.tokens[tt.file] = tt.mode
			c, calls := newDataUnitCheck(t, fx)
			res := c.Run(&CheckContext{TownRoot: t.TempDir()})
			if res.Status != StatusError {
				t.Fatalf("status = %v, want StatusError; message %q", res.Status, res.Message)
			}
			if !strings.Contains(res.Message, tt.wantName) {
				t.Errorf("message %q does not name %q", res.Message, tt.wantName)
			}
			if calls.readyz != 0 {
				t.Errorf("readyz requests = %d, want 0: the gateway must not be asked past a wide token file", calls.readyz)
			}
		})
	}
}

func TestDataUnitDoctorSkipped(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		named      bool
		ps         bool // whether the status reader is wired
		wantStatus CheckStatus
		wantText   string
	}{
		{
			name:       "not named by --check is skipped with the reason",
			named:      false,
			ps:         true,
			wantStatus: StatusSkipped,
			wantText:   "not selected: name it with --check data-unit",
		},
		{
			name:       "not named and unwired is still skipped, never failed",
			named:      false,
			ps:         false,
			wantStatus: StatusSkipped,
			wantText:   "not selected: name it with --check data-unit",
		},
		{
			name:       "named with the status reader unwired is refused, not skipped",
			named:      true,
			ps:         false,
			wantStatus: StatusError,
			wantText:   "not wired",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := &dataUnitCalls{}
			var ps func(string) ([]byte, error)
			if tt.ps {
				ps = func(string) ([]byte, error) {
					calls.ps++
					return []byte(healthyPs), nil
				}
			}
			c := NewDataUnitCheck(tt.named, ps, func(string) (string, error) { return t.TempDir(), nil })
			c.http = &http.Client{Transport: handlerTransport{dataUnitReadyzHandler(http.StatusOK, calls)}}
			res := c.Run(&CheckContext{TownRoot: t.TempDir()})
			if res.Status != tt.wantStatus {
				t.Fatalf("status = %v, want %v; message %q", res.Status, tt.wantStatus, res.Message)
			}
			if !strings.Contains(res.Message, tt.wantText) {
				t.Errorf("message %q does not say %q", res.Message, tt.wantText)
			}
			if calls.ps != 0 || calls.readyz != 0 {
				t.Errorf("status calls = %d, readyz requests = %d; a check that does not run must call nothing", calls.ps, calls.readyz)
			}
		})
	}
}

// The check's status reader gets the town root it was run for, and an error from
// it fails the unit rung, with the error's text in the message.
func TestDataUnitDoctorReadsTheRunTown(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var got string
	c := NewDataUnitCheck(true, func(townRoot string) ([]byte, error) {
		got = townRoot
		return nil, errors.New("docker compose ps for gt-data: docker exited with status 1")
	}, func(string) (string, error) { return t.TempDir(), nil })
	res := c.Run(&CheckContext{TownRoot: town})
	if got != town {
		t.Errorf("status reader got town %q, want %q", got, town)
	}
	if res.Status != StatusError || !strings.Contains(res.Message, "data unit rung unit failed") {
		t.Errorf("status = %v, message %q; want the unit rung to fail", res.Status, res.Message)
	}
}
