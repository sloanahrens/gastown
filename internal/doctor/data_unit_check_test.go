package doctor

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dataUnitHealthy is the inspect answer of a running container with a passing
// health check.
const dataUnitHealthy = "running healthy"

// dataUnitFixture is one data unit as the rungs read it. docker maps a container
// name to its inspect output; a container it does not name is an inspect error.
// tokens is the token directory's files and their modes; nil means no token
// directory is configured. readyz is the gateway's /readyz status; 0 means the
// gateway does not answer.
type dataUnitFixture struct {
	docker map[string]string
	tokens map[string]os.FileMode
	readyz int
}

// healthyDataUnit is a unit whose every rung passes.
func healthyDataUnit() dataUnitFixture {
	return dataUnitFixture{
		docker: map[string]string{dataUnitDoltContainer: dataUnitHealthy, dataUnitBdContainer: dataUnitHealthy},
		tokens: map[string]os.FileMode{"daemon.gt-wrap.token": 0o600, "read.dash.token": 0o600},
		readyz: http.StatusOK,
	}
}

// dataUnitCalls counts the calls one check makes to the docker runner and the
// gateway, so a test can say a rung was or was not reached.
type dataUnitCalls struct {
	docker int
	readyz int
}

// newDataUnitCheck builds the check over fx: docker answers from the fixture, the
// token directory is a temp dir with the fixture's modes, and /readyz is answered
// in process by a handler that accepts only the daemon token.
func newDataUnitCheck(t *testing.T, fx dataUnitFixture) (*DataUnitCheck, *dataUnitCalls) {
	t.Helper()
	calls := &dataUnitCalls{}
	env := map[string]string{}
	if fx.tokens != nil {
		dir := t.TempDir()
		for name, mode := range fx.tokens {
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte("tok-"+name), 0o600); err != nil {
				t.Fatalf("WriteFile %s: %v", name, err)
			}
			if err := os.Chmod(path, mode); err != nil {
				t.Fatalf("Chmod %s: %v", name, err)
			}
		}
		env[dataUnitTokenDirEnv] = dir
	}

	c := NewDataUnitCheck()
	c.selected = func(string) (bool, error) { return true, nil }
	c.getenv = func(k string) string { return env[k] }
	c.docker = func(_ context.Context, argv []string) ([]byte, error) {
		calls.docker++
		container := argv[len(argv)-1]
		out, ok := fx.docker[container]
		if !ok {
			return nil, errors.New("exit status 1")
		}
		return []byte(out), nil
	}
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
				fx.docker[dataUnitDoltContainer] = "exited none"
				return fx
			},
			wantStatus: StatusError,
			wantRung:   "dolt",
		},
		{
			name: "dolt is running but not healthy",
			fx: func() dataUnitFixture {
				fx := healthyDataUnit()
				fx.docker[dataUnitDoltContainer] = "running starting"
				return fx
			},
			wantStatus: StatusError,
			wantRung:   "dolt",
		},
		{
			name: "dolt container cannot be inspected",
			fx: func() dataUnitFixture {
				fx := healthyDataUnit()
				delete(fx.docker, dataUnitDoltContainer)
				return fx
			},
			wantStatus: StatusError,
			wantRung:   "dolt",
		},
		{
			name: "bd is not healthy, so the token rungs are not reached",
			fx: func() dataUnitFixture {
				fx := healthyDataUnit()
				fx.docker[dataUnitBdContainer] = "running unhealthy"
				return fx
			},
			wantStatus: StatusError,
			wantRung:   "bd",
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

func TestDataUnitDoctorTokenMode(t *testing.T) {
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
	tests := []struct {
		name       string
		selected   func(string) (bool, error)
		wantStatus CheckStatus
		wantText   string
	}{
		{
			name:       "compose unit not selected is skipped with the reason",
			selected:   func(string) (bool, error) { return false, nil },
			wantStatus: StatusSkipped,
			wantText:   "not the selected Dolt path",
		},
		{
			name:       "selection that cannot be read is an error, not a skip",
			selected:   func(string) (bool, error) { return false, errors.New("unreadable town settings") },
			wantStatus: StatusError,
			wantText:   "cannot read whether the compose unit is the selected Dolt path",
		},
		{
			name:       "unwired selection is refused, not skipped",
			selected:   nil,
			wantStatus: StatusError,
			wantText:   "not wired",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, calls := newDataUnitCheck(t, healthyDataUnit())
			c.selected = tt.selected
			res := c.Run(&CheckContext{TownRoot: t.TempDir()})
			if res.Status != tt.wantStatus {
				t.Fatalf("status = %v, want %v; message %q", res.Status, tt.wantStatus, res.Message)
			}
			if !strings.Contains(res.Message, tt.wantText) {
				t.Errorf("message %q does not say %q", res.Message, tt.wantText)
			}
			if calls.docker != 0 || calls.readyz != 0 {
				t.Errorf("docker calls = %d, readyz requests = %d; a check that does not apply must call nothing", calls.docker, calls.readyz)
			}
		})
	}
}
