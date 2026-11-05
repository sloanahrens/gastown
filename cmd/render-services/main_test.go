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

func TestRegistryForwardFlag(t *testing.T) {
	t.Parallel()
	const hosts = "values/lab/hosts.yaml"
	read := fakeRead(map[string]string{hosts: "control_plane_host: cp.example\nregistry_host: localhost:5001\n"})

	t.Run("renders the DaemonSet to stdout", func(t *testing.T) {
		t.Parallel()
		var out, errw bytes.Buffer
		if code := run([]string{"--registry-forward", hosts}, read, &out, &errw); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, errw.String())
		}
		if !strings.Contains(out.String(), "kind: DaemonSet") {
			t.Errorf("no DaemonSet in the output:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "TCP:cp.example:5001") {
			t.Errorf("the relay does not name the hosts file's control_plane_host:\n%s", out.String())
		}
		if strings.Contains(out.String(), "kind: Service") {
			t.Errorf("the forwarder output carries a Service:\n%s", out.String())
		}
	})

	t.Run("without the flag the Services are rendered as before", func(t *testing.T) {
		t.Parallel()
		var out, errw bytes.Buffer
		if code := run([]string{hosts}, read, &out, &errw); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, errw.String())
		}
		if strings.Contains(out.String(), "DaemonSet") || strings.Count(out.String(), "kind: Service") != 4 {
			t.Errorf("without the flag the output is not the four Services:\n%s", out.String())
		}
	})

	t.Run("wrong command lines exit 2", func(t *testing.T) {
		t.Parallel()
		for _, args := range [][]string{
			{"--registry-forward"},
			{"--registry-forward", hosts, hosts},
			{hosts, "--registry-forward"},
		} {
			var out, errw bytes.Buffer
			if code := run(args, read, &out, &errw); code != 2 {
				t.Errorf("args %q: exit %d, want 2 (stderr %q)", args, code, errw.String())
			}
			if out.Len() != 0 {
				t.Errorf("args %q: stdout is not empty: %q", args, out.String())
			}
		}
	})

	t.Run("an unreadable file exits 1 with empty stdout", func(t *testing.T) {
		t.Parallel()
		var out, errw bytes.Buffer
		if code := run([]string{"--registry-forward", "missing/hosts.yaml"}, read, &out, &errw); code != 1 {
			t.Errorf("exit %d, want 1 (stderr %q)", code, errw.String())
		}
		if out.Len() != 0 {
			t.Errorf("stdout is not empty: %q", out.String())
		}
	})

	t.Run("a hosts file without a registry_host exits 1 with empty stdout", func(t *testing.T) {
		t.Parallel()
		var out, errw bytes.Buffer
		noRegistry := fakeRead(map[string]string{hosts: "control_plane_host: cp.example\n"})
		if code := run([]string{"--registry-forward", hosts}, noRegistry, &out, &errw); code != 1 {
			t.Errorf("exit %d, want 1 (stderr %q)", code, errw.String())
		}
		if out.Len() != 0 {
			t.Errorf("stdout is not empty: %q", out.String())
		}
		if !strings.Contains(errw.String(), "registry_host") {
			t.Errorf("stderr %q does not name registry_host", errw.String())
		}
	})
}
