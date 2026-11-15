package hygiene

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/manifestdata"
)

// TestInfrastructureIdentityIsExemptOnlyInItsOwnPosition checks the two
// infrastructure identities against the shipped rules and blocklist: each is not
// a finding where the town writes it, and every other occurrence of either name
// or path is still a finding.
func TestInfrastructureIdentityIsExemptOnlyInItsOwnPosition(t *testing.T) {
	t.Parallel()
	owner := config.DefaultForgeOwner
	root := manifestdata.PodTownRoot
	// The configured root's segments, so the cases below the home root and
	// outside the town root need no name of their own.
	segments := strings.Split(strings.TrimPrefix(root, "/"), "/")
	account := segments[1]
	// A home path and a private temp path that the exemption must leave alone,
	// joined from pieces so this file adds no finding of its own.
	janeHome := planted("/Users/", "jane", "/notes.txt")
	tempPath := planted("/private/", "tmp", "/note.txt")

	cases := []struct {
		name string
		line string
		want []string // the rules that must fire on the line, in order
	}{
		{
			name: "the forge owner in its URL position",
			line: "origin: http://forgejo:3000/" + owner + "/gastown.git",
		},
		{
			name: "the pod town root",
			line: "mountPath: " + root,
		},
		{
			name: "a path under the pod town root",
			line: "update: " + root + "/.runtime/update/images.yaml",
		},
		{
			name: "both identities on one line",
			line: "mount " + root + " from http://forgejo:3000/" + owner + "/gastown.git",
		},
		{
			name: "the forge owner in prose",
			line: "the " + owner + " decision stands",
			want: []string{RuleBlockedWord},
		},
		{
			name: "the forge owner in a repository path",
			line: "module " + owner + "/devops",
			want: []string{RuleBlockedWord},
		},
		{
			name: "the forge owner under another URL host",
			line: "clone http://forge.example:3000/" + owner + "/gastown.git",
			want: []string{RuleBlockedWord},
		},
		{
			name: "the forge owner in a longer owner name",
			line: "clone http://forgejo:3000/" + owner + "-dev/gastown.git",
			want: []string{RuleBlockedWord},
		},
		{
			name: "the town root's sibling path",
			line: "backups: " + root + "-backups/dolt",
			want: []string{RuleHomePath, RuleBlockedWord},
		},
		{
			name: "the account's home outside the town root",
			line: "notes: /Users/" + account + "/notes.txt",
			want: []string{RuleHomePath, RuleBlockedWord},
		},
		{
			name: "the town root below another home",
			line: "old: /home/" + account + "/" + segments[2],
			want: []string{RuleHomePath, RuleBlockedWord},
		},
		{
			name: "another account's home beside the town root",
			line: "roots: " + root + " and " + janeHome,
			want: []string{RuleHomePath},
		},
		{
			name: "a temp path beside the town root",
			line: "roots: " + root + " and " + tempPath,
			want: []string{RulePrivateTemp},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := make([]Finding, 0, len(tc.want))
			for _, rule := range tc.want {
				want = append(want, Finding{Path: "docs/note.md", Rule: rule, Line: 1, LineHash: HashLine(tc.line)})
			}
			assertFindings(t, scanFS(t, singleFile("docs/note.md", tc.line+"\n"), nil), want...)
		})
	}
}

// TestAnEmptyIdentityPathReplacesNothing keeps a lost setting from excusing every
// line: an identity whose value is empty is not in the line, so it exempts
// nothing.
func TestAnEmptyIdentityPathReplacesNothing(t *testing.T) {
	t.Parallel()
	line := "home: " + planted("/Users/", "jane", "/notes.txt")
	if got := replaceWholePath(line, "", townRootPlaceholder); got != line {
		t.Fatalf("replaceWholePath with an empty path returned %q, want the line unchanged", got)
	}
}
