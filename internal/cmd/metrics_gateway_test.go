package cmd

import (
	"errors"
	"io/fs"
	"os/exec"
	"strings"
	"testing"
)

// forwarderHost is the host the gateway check reads: bd on PATH at /opt/gt/bin/bd,
// a bd.real beside it when forwarder is set, and a token file that reads.
func forwarderHost(forwarder bool) (lookPath func(string) (string, error), stat func(string) (fs.FileInfo, error), readFile func(string) ([]byte, error)) {
	lookPath = func(name string) (string, error) { return "/opt/gt/bin/" + name, nil }
	stat = func(path string) (fs.FileInfo, error) {
		if forwarder && path == "/opt/gt/bin/bd.real" {
			return struct{ fs.FileInfo }{}, nil
		}
		return nil, fs.ErrNotExist
	}
	readFile = func(string) ([]byte, error) { return []byte("token"), nil }
	return lookPath, stat, readFile
}

// TestMetricsBdGatewayRefusesTheForwarderWithoutAGateway pins the host's gateway
// check: the extract refuses when the bd on PATH is the forwarder and its gateway
// or token file is unset, and it needs nothing from a bd that is not the forwarder,
// from no bd at all, or refuses when the bd lookup itself fails (gt-g8oimf).
func TestMetricsBdGatewayRefusesTheForwarderWithoutAGateway(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		forwarder bool
		notOnPath bool
		lookErr   error
		env       map[string]string
		wantErr   string // empty means the check passes
	}{
		{name: "forwarder without a gateway URL refuses", forwarder: true, env: map[string]string{},
			wantErr: "BD_GATEWAY_URL"},
		{name: "forwarder with a URL but no token file refuses", forwarder: true,
			env: map[string]string{"BD_GATEWAY_URL": "http://gateway:7440"}, wantErr: "BD_GATEWAY_TOKEN_FILE"},
		{name: "forwarder with the gateway pair passes", forwarder: true,
			env: map[string]string{"BD_GATEWAY_URL": "http://gateway:7440", "BD_GATEWAY_TOKEN_FILE": "/run/token"}},
		{name: "a bd that is not the forwarder needs no gateway", env: map[string]string{}},
		{name: "no bd on PATH needs no gateway", notOnPath: true, env: map[string]string{}},
		{name: "a failed bd lookup refuses", lookErr: errors.New("permission denied"), env: map[string]string{},
			wantErr: "find bd on PATH"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lookPath, stat, readFile := forwarderHost(tc.forwarder)
			if tc.notOnPath {
				lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
			}
			if tc.lookErr != nil {
				lookPath = func(string) (string, error) { return "", tc.lookErr }
			}
			check := metricsBdGateway(lookPath, stat, readFile)
			err := check(func(name string) string { return tc.env[name] })
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("gateway check refused a bd that needs no gateway: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("gateway check passed, want a refusal naming %s", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("gateway check error = %q, want it to name %s", err, tc.wantErr)
			}
		})
	}
}
