package sbxcreds_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/sbxcreds"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain holds the unit tier's run checks, as TestUnitTierMain requires of every package.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// TestRequestArgv fixes the exact kubeadm call, both inclusive validity bounds, and the canonical duration.
func TestRequestArgv(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ validity, want string }{{"24h", "24h0m0s"}, {"1h", "1h0m0s"}, {"8760h", "8760h0m0s"}} {
		req := sbxcreds.Request{Container: "gt-sbx-control-plane", ClientName: "gt-deployer-apply", Group: "gt-deployer-apply", Validity: tc.validity}
		want := []string{"docker", "exec", "gt-sbx-control-plane", "kubeadm", "kubeconfig", "user", "--client-name", "gt-deployer-apply", "--org", "gt-deployer-apply", "--validity-period", tc.want}
		got, err := req.Argv()
		if err != nil {
			t.Fatalf("Argv() with validity %q: %v", tc.validity, err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("Argv() with validity %q = %q, want %q", tc.validity, got, want)
		}
	}
}

// TestRequestRefuses drives every rule: a bad container, client name or group, and a bad validity.
func TestRequestRefuses(t *testing.T) {
	t.Parallel()
	valid := sbxcreds.Request{Container: "gt-sbx-control-plane", ClientName: "gt-deployer-read", Group: "gt-deployer-read", Validity: "24h"}
	for _, tc := range []struct {
		name  string
		apply func(*sbxcreds.Request)
		want  string
	}{
		{"an empty container", func(r *sbxcreds.Request) { r.Container = "" }, "container"},
		{"a dash-led container", func(r *sbxcreds.Request) { r.Container = "-v" }, "container"},
		{"a dot in the client name", func(r *sbxcreds.Request) { r.ClientName = "gt.deployer" }, "client name"},
		{"an uppercase client name", func(r *sbxcreds.Request) { r.ClientName = "GT-deployer" }, "client name"},
		{"an underscore in the group", func(r *sbxcreds.Request) { r.Group = "gt_deployer" }, "group"},
		{"a validity under one hour", func(r *sbxcreds.Request) { r.Validity = "59m" }, "outside"},
		{"a validity over 8760 hours", func(r *sbxcreds.Request) { r.Validity = "8761h" }, "outside"},
		{"an unparsable validity", func(r *sbxcreds.Request) { r.Validity = "until-revoked" }, "not a duration"},
	} {
		req := valid
		tc.apply(&req)
		argv, err := req.Argv()
		if err == nil {
			t.Fatalf("%s: Argv() accepted the request, want a refusal", tc.name)
		}
		if len(argv) != 0 {
			t.Errorf("%s: argv = %q beside error %v, want no argv", tc.name, argv, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not name %q", tc.name, err, tc.want)
		}
	}
}
