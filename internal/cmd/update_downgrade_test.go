package cmd

import (
	"errors"
	"strings"
	"testing"
)

// downgradeCase is one call; an empty wantErr means the call must pass.
type downgradeCase struct {
	name          string
	running       string
	target        string
	allowRollback bool
	isAncestor    func(a, b string) (bool, error)
	wantErr       string
}

func always(ok bool, err error) func(a, b string) (bool, error) {
	return func(string, string) (bool, error) { return ok, err }
}

func runDowngradeCases(t *testing.T, cases []downgradeCase) {
	t.Helper()
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := refuseDowngrade(tt.running, tt.target, tt.allowRollback, tt.isAncestor)
			if (tt.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("refuseDowngrade = %v, want refusal containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestRefuseDowngrade: the same commit, a descendant and an allowed rollback pass;
// an ancestor is refused, naming both commits.
func TestRefuseDowngrade(t *testing.T) {
	t.Parallel()
	const running, target = "aaa111", "bbb222"
	runDowngradeCases(t, []downgradeCase{
		{name: "the same commit passes", running: running, target: running, isAncestor: always(false, nil)},
		{name: "a descendant passes", running: running, target: target, isAncestor: always(true, nil)},
		{name: "an ancestor is refused naming both", running: running, target: target, isAncestor: always(false, nil), wantErr: "refusing downgrade of aaa111 to bbb222"},
		{name: "an ancestor passes with allowRollback", running: running, target: target, allowRollback: true, isAncestor: always(false, nil)},
	})
}

// TestRefuseDowngradeRefuses: an unknown commit, an unwired reader and a failed
// ancestry read are each refused, even when the read reports an ancestor.
func TestRefuseDowngradeRefuses(t *testing.T) {
	t.Parallel()
	const running, target = "aaa111", "bbb222"
	runDowngradeCases(t, []downgradeCase{
		{name: "an empty running commit is refused", target: target, isAncestor: always(true, nil), wantErr: "the running commit is unknown"},
		{name: "an empty running commit is refused with allowRollback", target: target, allowRollback: true, wantErr: "the running commit is unknown"},
		{name: "an empty target is refused", running: running, isAncestor: always(true, nil), wantErr: "the target commit is empty"},
		{name: "an unwired ancestry reader is refused", running: running, target: target, wantErr: "no ancestry reader is wired"},
		{name: "a read error is refused", running: running, target: target, isAncestor: always(true, errors.New("git merge-base failed")), wantErr: "git merge-base failed"},
	})
}
