package gitfake

import (
	"path/filepath"
	"testing"
)

// TestRemoteBranchTip reads a rig's remote branch tip with no process: the
// branch's tip, and an empty string for a branch the remote does not have.
func TestRemoteBranchTip(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, New())
	g := fx.env.Open(fx.clone)
	if tip, err := g.RemoteBranchTip("origin", fixtureBranch); err != nil || tip != fx.head {
		t.Errorf("RemoteBranchTip(origin, %s) = %q, %v; want %s", fixtureBranch, tip, err, fx.head)
	}
	if tip, err := g.RemoteBranchTip("origin", "gone"); err != nil || tip != "" {
		t.Errorf("RemoteBranchTip of a missing branch = %q, %v; want empty", tip, err)
	}
}

// TestRemoteBranchTipUnknownRemote refuses what does not resolve: both reads
// error, never answering an empty tip a caller could take for a missing
// branch. A push URL at a path that holds no repository is refused too.
func TestRemoteBranchTipUnknownRemote(t *testing.T) {
	t.Parallel()
	fx := newFixture(t, New())
	g := fx.env.Open(fx.clone)
	for _, read := range []struct {
		name string
		tip  func(string, string) (string, error)
	}{
		{"RemoteBranchTip", g.RemoteBranchTip},
		{"PushRemoteBranchTip", g.PushRemoteBranchTip},
	} {
		tip, err := read.tip("nope", fixtureBranch)
		if err == nil {
			t.Errorf("%s on an unknown remote succeeded; want an error", read.name)
		}
		if tip != "" {
			t.Errorf("%s on an unknown remote = %q; want empty", read.name, tip)
		}
	}
	if err := g.ConfigurePushURL("origin", filepath.Join(fx.root, "gone.git")); err != nil {
		t.Fatalf("ConfigurePushURL: %v", err)
	}
	if tip, err := g.PushRemoteBranchTip("origin", fixtureBranch); err == nil || tip != "" {
		t.Errorf("PushRemoteBranchTip at a missing push URL = %q, %v; want an error and empty", tip, err)
	}
}
