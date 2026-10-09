package cmd

import (
	"reflect"
	"testing"
)

// The guard matchers used to assume a fixed argv shape: `<shell> -c`, `git
// push` with nothing between them, `bd close` with nothing between them
// (gt-kocid). Each spelling below is the same command with a different argv,
// and the guard must see through all of them.

func TestShellCFlagVariantsAreCheckedAsNestedCommands(t *testing.T) {
	t.Parallel()
	blocked := []string{
		`bash -lc 'git reset --hard'`,
		`sh -ec 'git clean -fd'`,
		`zsh -ic 'git reset --hard HEAD~1'`,
		`bash -x -c 'git reset --hard'`,
		`bash --norc -c 'git reset --hard'`,
		`/bin/bash -c 'git reset --hard'`,
		`/usr/bin/env bash -lc 'git reset --hard'`,
		`bash -lc 'while true; do gt done; done'`,
		`/bin/sh -c 'git push --force origin main'`,
	}
	for _, command := range blocked {
		if reason, _ := evaluateDangerousCommand(command, 0, noTownSession); reason == "" {
			t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked", command)
		}
	}
	allowed := []string{
		`bash -lc 'echo hello'`,
		`bash ./run.sh -c 'git reset --hard'`, // -c belongs to the script, not to bash
		`bash -l`,
	}
	for _, command := range allowed {
		if reason, _ := evaluateDangerousCommand(command, 0, noTownSession); reason != "" {
			t.Errorf("evaluateDangerousCommand(%q) blocked (%q), want allowed", command, reason)
		}
	}
}

func TestGitPushForceVariants(t *testing.T) {
	t.Parallel()
	blocked := []string{
		`/usr/bin/git push --force origin x`,
		`git -c k=v push -f`,
		`git -C repo push --force`,
		`git push -fu origin x`,
		`git push -uf origin x`,
		`git push origin +main`,
		`git push origin +HEAD:feature`,
		`echo ok && git push --force`,
	}
	for _, command := range blocked {
		if reason, _ := evaluateDangerousCommand(command, 0, noTownSession); reason == "" {
			t.Errorf("evaluateDangerousCommand(%q) allowed, want blocked as a force push", command)
		}
	}
	allowed := []string{
		`git push origin x`,
		`git push --force-with-lease origin x`,
		`git push --force-if-includes origin x`,
		`git -C repo push origin x`,
		`git status -f`,
		`git commit -m "a +b"`,
	}
	for _, command := range allowed {
		if reason, _ := evaluateDangerousCommand(command, 0, noTownSession); reason != "" {
			t.Errorf("evaluateDangerousCommand(%q) blocked (%q), want allowed", command, reason)
		}
	}
}

func TestParseBdCloseSegmentSkipsGlobalFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		segment []string
		wantIDs []string
		wantOK  bool
	}{
		{"plain", []string{"bd", "close", "gt-x"}, []string{"gt-x"}, true},
		{"json before close", []string{"bd", "--json", "close", "gt-x"}, []string{"gt-x"}, true},
		{"short flag before close", []string{"bd", "-q", "close", "gt-x"}, []string{"gt-x"}, true},
		{"value flag before close", []string{"bd", "--db", "/p/x.db", "close", "gt-x"}, []string{"gt-x"}, true},
		{"value flag equals form", []string{"bd", "--actor=me", "close", "gt-x"}, []string{"gt-x"}, true},
		{"path to bd", []string{"/usr/local/bin/bd", "-v", "close", "gt-x", "-r", "done"}, []string{"gt-x"}, true},
		{"other subcommand", []string{"bd", "--json", "show", "gt-x"}, nil, false},
		{"only flags", []string{"bd", "--json"}, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseBdCloseSegment(c.segment)
			if ok != c.wantOK {
				t.Fatalf("parseBdCloseSegment(%q) ok = %v, want %v", c.segment, ok, c.wantOK)
			}
			if ok && !reflect.DeepEqual(got.IDs, c.wantIDs) {
				t.Errorf("IDs = %q, want %q", got.IDs, c.wantIDs)
			}
		})
	}
}
