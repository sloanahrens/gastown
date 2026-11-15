package hygiene

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/manifestdata"
)

// scanExempting scans one file through Scan with the options a guard passes, so
// the exemption is exercised where the scan applies it rather than through a
// helper it calls.
func scanExempting(t *testing.T, fsys fstest.MapFS, opts ...ScanOption) []Finding {
	t.Helper()
	findings, _, err := Scan(fsys, sortedPaths(fsys), DefaultRules(), shippedBlocked(t), nil, opts...)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return findings
}

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
			exempt := ExemptIdentities(owner, root)
			assertFindings(t, scanExempting(t, singleFile("docs/note.md", tc.line+"\n"), exempt), want...)
		})
	}
}

// TestValidateAllowRefusesARowForAnExemptedLine: a row written for a line the
// scan exempts would excuse nothing, so validation run with the same exemption
// refuses it and the allow file cannot spend a row on an identity.
func TestValidateAllowRefusesARowForAnExemptedLine(t *testing.T) {
	t.Parallel()
	line := "mountPath: " + manifestdata.PodTownRoot
	fsys := singleFile("docs/note.md", line+"\n")
	row := Allow{Path: "docs/note.md", Rule: RuleHomePath, Line: HashLine(line), Reason: "the pod mount"}
	if err := ValidateAllow(fsys, sortedPaths(fsys), DefaultRules(), shippedBlocked(t), []Allow{row}, ExemptIdentities(config.DefaultForgeOwner, manifestdata.PodTownRoot)); err == nil {
		t.Fatal("ValidateAllow accepted a row for a line the scan exempts")
	}
	if err := ValidateAllow(fsys, sortedPaths(fsys), DefaultRules(), shippedBlocked(t), []Allow{row}); err != nil {
		t.Fatalf("ValidateAllow with no exemption refused the row: %v", err)
	}
}

// TestAnEmptyIdentityExemptsNothing keeps a lost setting from excusing every
// line: an identity the caller leaves empty is not in the line, so the scan
// exempts nothing and the line stays a finding. Each row runs through Scan, the
// way the guard runs it, so an exemption applied to an empty identity turns it
// red.
func TestAnEmptyIdentityExemptsNothing(t *testing.T) {
	t.Parallel()
	owner := config.DefaultForgeOwner
	root := manifestdata.PodTownRoot
	urlLine := "origin: http://forgejo:3000/" + owner + "/gastown.git"
	rootLine := "mountPath: " + root
	tests := []struct {
		name string
		line string
		opts []ScanOption
		want []string // the rules that must fire on the line, in order
	}{
		{
			name: "no identity named",
			line: urlLine,
			want: []string{RuleBlockedWord},
		},
		{
			name: "an empty forge owner",
			line: urlLine,
			opts: []ScanOption{ExemptIdentities("", root)},
			want: []string{RuleBlockedWord},
		},
		{
			name: "an empty town root",
			line: rootLine,
			opts: []ScanOption{ExemptIdentities(owner, "")},
			want: []string{RuleHomePath, RuleBlockedWord},
		},
		{
			name: "both identities empty",
			line: rootLine,
			opts: []ScanOption{ExemptIdentities("", "")},
			want: []string{RuleHomePath, RuleBlockedWord},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			want := make([]Finding, 0, len(tc.want))
			for _, rule := range tc.want {
				want = append(want, Finding{Path: "docs/note.md", Rule: rule, Line: 1, LineHash: HashLine(tc.line)})
			}
			assertFindings(t, scanExempting(t, singleFile("docs/note.md", tc.line+"\n"), tc.opts...), want...)
		})
	}
}
