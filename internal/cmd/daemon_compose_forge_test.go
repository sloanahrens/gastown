package cmd

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/templates"
)

const (
	composeForgeHost  = "forgejo:3000"
	composeForgeToken = "/host/forgejo/forgejo-landing.env"
)

// The command's forge pair reaches the render: the file it writes carries the
// entrypoint's flags and the read-only mount of the host's token file, at the
// same fixed path the flags name. Removing the pair from the spec turns this
// red, since the render then writes neither (gt-rpx80).
func TestDaemonComposeWriteForgeCredential(t *testing.T) {
	t.Parallel()
	r := newComposeRig(t)
	r.flags.forgeHost, r.flags.forgeTokenFile = composeForgeHost, composeForgeToken
	if err := runDaemonCompose(r.flags, r.deps); err != nil {
		t.Fatalf("runDaemonCompose: %v", err)
	}
	got, ok := r.disk[r.composePath()]
	if !ok {
		t.Fatalf("no compose file written at %s", r.composePath())
	}
	for _, want := range []string{
		`command: ["--forge-host", "` + composeForgeHost + `", "--forge-token-file", "` + templates.LiveForgeTokenPath + `"]`,
		`source: "` + composeForgeToken + `"`,
		`target: "` + templates.LiveForgeTokenPath + `"`,
		"read_only: true",
	} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("compose file does not carry %q:\n%s", want, got)
		}
	}
	if bytes.Contains(got, []byte("--scratch-town")) {
		t.Errorf("the live compose file asks for a scratch town:\n%s", got)
	}
}

// A pair the render refuses writes nothing and leaves the file the town had as
// it was. Each row is red if its check is removed: the render would accept the
// spec and the command would write.
func TestDaemonComposeWriteForgeCredentialRefusals(t *testing.T) {
	t.Parallel()
	old := []byte("services: {}\n")
	for _, tc := range []struct {
		name  string
		host  string
		token string
		want  string
	}{
		{"a host without a token file", composeForgeHost, "", "carries both or neither"},
		{"a token file without a host", "", composeForgeToken, "carries both or neither"},
		{"a relative token file", composeForgeHost, "forgejo/forgejo-landing.env", "is not absolute"},
		{"a host with a scheme", "http://" + composeForgeHost, composeForgeToken, "not a bare host:port"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newComposeRig(t)
			r.disk[r.composePath()] = old
			r.flags.forgeHost, r.flags.forgeTokenFile = tc.host, tc.token
			err := runDaemonCompose(r.flags, r.deps)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
			if len(r.wrote) != 0 {
				t.Errorf("a refusal wrote %v", r.wrote)
			}
			if got, ok := r.disk[r.composePath()]; !ok || !bytes.Equal(got, old) {
				t.Errorf("compose file = %q, want the old render %q kept", got, old)
			}
		})
	}
}

// `gt daemon compose write` parses both forge flags into the values the render
// receives.
func TestDaemonComposeForgeFlagsAreWired(t *testing.T) {
	t.Parallel()
	var got daemonComposeFlags
	var ran bool
	c := newDaemonComposeCmd(func(_ io.Writer, f daemonComposeFlags) error {
		got, ran = f, true
		return nil
	})
	c.SetArgs([]string{"write", "--image", "gt-daemon:0123456",
		"--forge-host", composeForgeHost, "--forge-token-file", composeForgeToken})
	c.SilenceUsage, c.SilenceErrors = true, true
	if err := c.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !ran || got.forgeHost != composeForgeHost || got.forgeTokenFile != composeForgeToken {
		t.Errorf("ran %v flags %+v, want write with the forge host %q and token file %q", ran, got, composeForgeHost, composeForgeToken)
	}
}
