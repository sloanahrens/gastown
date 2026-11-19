package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// adminKubeconfigFixture writes text as a kubeconfig in a new temp dir and
// returns its path.
func adminKubeconfigFixture(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// adminKubeconfigResultText is every line of a result, so a test can assert a
// credential value never reaches one.
func adminKubeconfigResultText(res *CheckResult) string {
	text := res.Name + "\n" + res.Message + "\n" + res.FixHint
	for _, detail := range res.Details {
		text += "\n" + detail
	}
	return text
}

// TestAdminKubeconfigCheckFinds: a control-cluster context whose user embeds a
// credential is an error that names the context and user, never the value.
func TestAdminKubeconfigCheckFinds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		kubeconfig  string
		wantContext string
		wantUser    string
		secret      string
	}{
		{
			name: "control context name embeds a certificate and key",
			kubeconfig: `apiVersion: v1
kind: Config
clusters:
  - name: control
    cluster:
      server: https://10.20.30.40:6443
contexts:
  - name: colima-gt-control
    context:
      cluster: control
      user: control-admin
users:
  - name: control-admin
    user:
      client-certificate-data: c2VjcmV0LWNlcnQ=
      client-key-data: c2VjcmV0LWtleQ==
`,
			wantContext: "colima-gt-control",
			wantUser:    "control-admin",
			secret:      "c2VjcmV0LWNlcnQ=",
		},
		{
			name: "loopback server embeds a token",
			kubeconfig: `apiVersion: v1
kind: Config
clusters:
  - name: local
    cluster:
      server: https://127.0.0.1:6443
contexts:
  - name: gt-local
    context:
      cluster: local
      user: gt-admin
users:
  - name: gt-admin
    user:
      token: dG9rZW4tYnl0ZXM=
`,
			wantContext: "gt-local",
			wantUser:    "gt-admin",
			secret:      "dG9rZW4tYnl0ZXM=",
		},
		{
			name: "localhost server embeds an exec command",
			kubeconfig: `apiVersion: v1
kind: Config
clusters:
  - name: local
    cluster:
      server: https://localhost:6443
contexts:
  - name: local
    context:
      cluster: local
      user: exec-admin
users:
  - name: exec-admin
    user:
      exec:
        command: /usr/local/bin/exec-credential
`,
			wantContext: "local",
			wantUser:    "exec-admin",
			secret:      "/usr/local/bin/exec-credential",
		},
		{
			name: "IPv6 loopback embeds a key",
			kubeconfig: `apiVersion: v1
kind: Config
clusters:
  - name: local
    cluster:
      server: https://[::1]:6443
contexts:
  - name: v6
    context:
      cluster: local
      user: v6-admin
users:
  - name: v6-admin
    user:
      client-key-data: djo2LWtleQ==
`,
			wantContext: "v6",
			wantUser:    "v6-admin",
			secret:      "djo2LWtleQ==",
		},
		{
			name: "127/8 address embeds a token",
			kubeconfig: `apiVersion: v1
kind: Config
clusters:
  - name: local
    cluster:
      server: https://127.9.9.9:6443
contexts:
  - name: odd-loop
    context:
      cluster: local
      user: odd-admin
users:
  - name: odd-admin
    user:
      token: bG9vcGJhY2stdG9rZW4=
`,
			wantContext: "odd-loop",
			wantUser:    "odd-admin",
			secret:      "bG9vcGJhY2stdG9rZW4=",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := adminKubeconfigFixture(t, tc.kubeconfig)
			res := NewAdminKubeconfigCheck(path).Run(&CheckContext{})
			if res.Status != StatusError {
				t.Fatalf("Status = %v, want StatusError: %+v", res.Status, res)
			}
			want := fmt.Sprintf("context %q uses user %q", tc.wantContext, tc.wantUser)
			if got := strings.Join(res.Details, "\n"); !strings.Contains(got, want) {
				t.Errorf("details = %q, want a line containing %q", got, want)
			}
			if got := adminKubeconfigResultText(res); strings.Contains(got, tc.secret) {
				t.Errorf("result text prints a credential value: %q", got)
			}
		})
	}
}

// TestAdminKubeconfigCheckClean: a control context that stores no credential,
// and a context that does not reach the control cluster, are both OK.
func TestAdminKubeconfigCheckClean(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		kubeconfig string
	}{
		{
			name: "no context reaches the control cluster",
			kubeconfig: `apiVersion: v1
kind: Config
clusters:
  - name: remote
    cluster:
      server: https://10.0.0.5:6443
contexts:
  - name: prod
    context:
      cluster: remote
      user: prod-user
users:
  - name: prod-user
    user:
      client-certificate-data: cHJvZC1jZXJ0
`,
		},
		{
			name: "control context user references its key by path",
			kubeconfig: `apiVersion: v1
kind: Config
clusters:
  - name: control
    cluster:
      server: https://127.0.0.1:6443
contexts:
  - name: colima-gt-control
    context:
      cluster: control
      user: deployer
users:
  - name: deployer
    user:
      client-certificate: /var/lib/deployer/kube/deployer.crt
      client-key: /var/lib/deployer/kube/deployer.key
`,
		},
		{
			name: "control context has no user",
			kubeconfig: `apiVersion: v1
kind: Config
clusters:
  - name: control
    cluster:
      server: https://localhost:6443
contexts:
  - name: local
    context:
      cluster: control
`,
		},
		{
			name: "kubeconfig names no context",
			kubeconfig: `apiVersion: v1
kind: Config
clusters:
  - name: control
    cluster:
      server: https://127.0.0.1:6443
users:
  - name: leftover-admin
    user:
      token: bGVmdG92ZXI=
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := adminKubeconfigFixture(t, tc.kubeconfig)
			res := NewAdminKubeconfigCheck(path).Run(&CheckContext{})
			if res.Status != StatusOK {
				t.Fatalf("Status = %v, want StatusOK: %+v", res.Status, res)
			}
		})
	}
}

// TestAdminKubeconfigCheckNoLeak: a file that does not parse is an error naming
// the file, and a yaml message that quotes its content never reaches the result.
func TestAdminKubeconfigCheckNoLeak(t *testing.T) {
	t.Parallel()
	const secret = "SECRET42xy"
	cases := []struct {
		name       string
		kubeconfig string
	}{
		{
			name: "syntax error after the secret",
			kubeconfig: `contexts:
  - name: colima-gt-control
    context:
      user: ` + secret + `
users: [
`,
		},
		{
			name:       "type error that quotes the secret",
			kubeconfig: "users: " + secret + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := adminKubeconfigFixture(t, tc.kubeconfig)
			res := NewAdminKubeconfigCheck(path).Run(&CheckContext{})
			if res.Status != StatusError {
				t.Fatalf("Status = %v, want StatusError: %+v", res.Status, res)
			}
			if !strings.Contains(res.Message, path) {
				t.Errorf("Message = %q, want it to name the file %q", res.Message, path)
			}
			if got := adminKubeconfigResultText(res); strings.Contains(got, secret) {
				t.Errorf("result text prints %q from the unparseable file: %q", secret, got)
			}
		})
	}
}

// TestAdminKubeconfigCheckPathEmpty: a run with no path checks no file, so it
// refuses instead of passing.
func TestAdminKubeconfigCheckPathEmpty(t *testing.T) {
	t.Parallel()
	res := NewAdminKubeconfigCheck("").Run(&CheckContext{})
	if res.Status != StatusError {
		t.Fatalf("Status = %v, want StatusError: %+v", res.Status, res)
	}
	if !strings.Contains(res.Message, "No kubeconfig path") {
		t.Errorf("Message = %q, want it to say no path was given", res.Message)
	}
}

// TestAdminKubeconfigCheckMissingFile: a path with no file holds no admin
// credential, so the convention holds and the check says so.
func TestAdminKubeconfigCheckMissingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "absent")
	res := NewAdminKubeconfigCheck(path).Run(&CheckContext{})
	if res.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK: %+v", res.Status, res)
	}
	if !strings.Contains(res.Message, path) {
		t.Errorf("Message = %q, want it to name the path %q", res.Message, path)
	}
}

// TestAdminKubeconfigCheckUnreadable: a file the check cannot read is an error
// naming it, never a pass over the file it did not see.
func TestAdminKubeconfigCheckUnreadable(t *testing.T) {
	t.Parallel()
	path := t.TempDir()
	res := NewAdminKubeconfigCheck(path).Run(&CheckContext{})
	if res.Status != StatusError {
		t.Fatalf("Status = %v, want StatusError: %+v", res.Status, res)
	}
	if !strings.Contains(res.Message, path) {
		t.Errorf("Message = %q, want it to name the path %q", res.Message, path)
	}
}
