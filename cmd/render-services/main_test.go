package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func fakeRead(files map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		if s, ok := files[p]; ok {
			return []byte(s), nil
		}
		return nil, errors.New("no such file")
	}
}

func TestRenderServicesWritesFourServicesForTheHostsFile(t *testing.T) {
	t.Parallel()
	var out, errw bytes.Buffer
	read := fakeRead(map[string]string{"values/lab/hosts.yaml": "control_plane_host: cp.example\n"})
	if code := run([]string{"values/lab/hosts.yaml"}, read, &out, &errw); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errw.String())
	}
	if n := strings.Count(out.String(), "externalName: cp.example}"); n != 4 {
		t.Errorf("%d Services name cp.example, want 4:\n%s", n, out.String())
	}
	if !strings.Contains(out.String(), "values/lab/hosts.yaml") {
		t.Errorf("the header does not name the environment's hosts file:\n%s", out.String())
	}
}

func TestRenderServicesRefusesWithEmptyStdout(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		file string
		code int
	}{
		{"no argument", nil, "", 2},
		{"unreadable", []string{"missing/hosts.yaml"}, "", 1},
		{"no host", []string{"values/e/hosts.yaml"}, "registry_host: r\n", 1},
		{"host with a newline", []string{"values/e/hosts.yaml"}, "control_plane_host: \"a\\n---\\nkind: Secret\"\n", 1},
	}
	for _, tc := range cases {
		var out, errw bytes.Buffer
		read := fakeRead(map[string]string{"values/e/hosts.yaml": tc.file})
		if code := run(tc.args, read, &out, &errw); code != tc.code {
			t.Errorf("%s: exit %d, want %d (stderr %q)", tc.name, code, tc.code, errw.String())
		}
		if out.Len() != 0 {
			t.Errorf("%s: stdout is not empty: %q", tc.name, out.String())
		}
	}
}
