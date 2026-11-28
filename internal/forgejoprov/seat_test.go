package forgejoprov

import (
	"net/http"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/forgejotest"
)

// seatRepos are the two repositories the seat tests provision.
var seatRepos = []string{"acme/one", "acme/two"}

// TestProvisionSeatCreatesAccountTokenAndGrants checks one call creates the account,
// writes the mode-600 token file with the polecat scopes and grants write on both
// repositories, with no protection or viewer request and no second-call write.
func TestProvisionSeatCreatesAccountTokenAndGrants(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	for _, repo := range seatRepos {
		f.AddRepo(repo)
	}
	files := newMemFiles()
	p := tokenProvisioner(f, files, false, false, new([]string))
	if err := p.ProvisionSeat("furiosa", seatRepos); err != nil {
		t.Fatalf("ProvisionSeat: %v", masked(errString(err)))
	}
	user, path := DefaultBotPrefix+"seat-furiosa", tokenDir+"/forgejo-seat-furiosa.env"
	compareSecretText(t, "token file", string(files.files[path]), tokenFileText(user, f.Token(user, DefaultTokenName)))
	if mode := files.modes[path]; mode != tokenFileMode {
		t.Errorf("token file mode = %v, want %v", mode, tokenFileMode)
	}
	want, ok := RoleScopes("polecat")
	if !ok {
		t.Fatal("RoleScopes(polecat) reports no such role")
	}
	if scopes, ok := fakeTokenScopes(f, user, DefaultTokenName); !ok || strings.Join(scopes, ",") != strings.Join(want, ",") {
		t.Errorf("seat token scopes = %v (found %v), want the polecat role's", scopes, ok)
	}
	for _, repo := range seatRepos {
		if perm := f.Permission(repo, user); perm != permissionWrite {
			t.Errorf("%s holds %q on %s, want %q", user, perm, repo, permissionWrite)
		}
	}
	if calls := strings.Join(sentCalls(f), "\n"); strings.Contains(calls, "branch_protections") || strings.Contains(calls, "viewer") {
		t.Errorf("the seat run sent a protection or viewer request: %s", calls)
	}
	if len(files.files) != 1 {
		t.Errorf("the seat run wrote %d files, want only its token file", len(files.files))
	}
	f.ClearCalls()
	if err := p.ProvisionSeat("furiosa", seatRepos); err != nil || len(f.Writes()) != 0 {
		t.Errorf("second call: err = %v, %d writes; want nil and none", masked(errString(err)), len(f.Writes()))
	}
}

// TestProvisionSeatAccountFailureNamesTheSeat checks a failed account step stops the
// run with an error naming the seat bot.
func TestProvisionSeatAccountFailureNamesTheSeat(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	rt := failingRoute{next: f, match: "/users/bot-seat-furiosa", method: http.MethodGet, status: http.StatusInternalServerError}
	err := tokenProvisioner(rt, newMemFiles(), false, false, new([]string)).ProvisionSeat("furiosa", seatRepos)
	if err == nil || !strings.Contains(err.Error(), "seat bot bot-seat-furiosa") {
		t.Fatalf("err = %v, want the account failure naming the seat bot", masked(errString(err)))
	}
}

// TestProvisionSeatRefusedGrantNamesTheSeatAndRepo checks a failed grant stops the
// run with an error naming the seat bot and the repository, and prints no token.
func TestProvisionSeatRefusedGrantNamesTheSeatAndRepo(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	for _, repo := range seatRepos {
		f.AddRepo(repo)
	}
	var logs []string
	rt := failingRoute{next: f, match: "/repos/" + seatRepos[1] + "/collaborators/", method: http.MethodPut, status: http.StatusInternalServerError}
	err := tokenProvisioner(rt, newMemFiles(), false, false, &logs).ProvisionSeat("furiosa", seatRepos)
	if err == nil {
		t.Fatal("ProvisionSeat = nil, want the refused grant's error")
	}
	if msg := err.Error(); !strings.Contains(msg, "bot-seat-furiosa") || !strings.Contains(msg, seatRepos[1]) {
		t.Errorf("error %q does not name the seat bot and the repository", msg)
	}
	secrets := []namedSecret{{"admin token", fixtureToken}, {"minted token", f.Token("bot-seat-furiosa", DefaultTokenName)}}
	if report := leakReport(strings.Join(logs, "\n")+err.Error(), secrets); report != "" {
		t.Fatal(report)
	}
}

// TestProvisionSeatRefusesABadName checks a name outside the seat alphabet is refused
// before any request or file write.
func TestProvisionSeatRefusesABadName(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	f.AddRepo(seatRepos[0])
	files := newMemFiles()
	p := tokenProvisioner(f, files, false, false, new([]string))
	for _, name := range []string{"Fur/iosa", "Furiosa", "fur_iosa", "fur.iosa", "fur iosa", ""} {
		if err := p.ProvisionSeat(name, seatRepos[:1]); err == nil {
			t.Errorf("ProvisionSeat(%q) = nil, want the name refused", name)
		}
	}
	if calls := sentCalls(f); len(calls) != 0 {
		t.Errorf("a request was sent though every name was refused: %v", calls)
	}
	if len(files.ops) != 0 {
		t.Errorf("a file was written though every name was refused")
	}
}

// TestProvisionSeatAcceptsOnlyTheSeatNameForm checks a name openshellprov would
// refuse is refused here before any request or file write, and the names both
// ends share are provisioned.
func TestProvisionSeatAcceptsOnlyTheSeatNameForm(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		wantErr bool
	}{
		{"-a", true},
		{"a-", true},
		{"a--b", true},
		{strings.Repeat("a", maxSeatNameLength+1), true},
		{"bullet-farmer", false},
		{strings.Repeat("a", maxSeatNameLength), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := forgejotest.New()
			f.AddRepo(seatRepos[0])
			files := newMemFiles()
			p := tokenProvisioner(f, files, false, false, new([]string))
			err := p.ProvisionSeat(tc.name, seatRepos[:1])
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("ProvisionSeat(%q): %v", tc.name, masked(errString(err)))
				}
				return
			}
			if err == nil {
				t.Fatalf("ProvisionSeat(%q) = nil, want the name refused", tc.name)
			}
			if calls := sentCalls(f); len(calls) != 0 {
				t.Errorf("a request was sent though the name was refused: %v", calls)
			}
			if len(files.ops) != 0 {
				t.Error("a file was written though the name was refused")
			}
		})
	}
}

// TestSeatRunProvisionsEachSeatOnEveryRepo: two --seat flags and one --repo create
// both seat bots, their token files and write access, and send no request about a
// role bot, a branch protection or the viewer.
func TestSeatRunProvisionsEachSeatOnEveryRepo(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	f.AddRepo(protectionRepo)
	files := newMemFiles()
	res := runProvisioning(t, f, files, nil, "--seat", "alpha", "--seat", "beta", "--repo", protectionRepo)
	if res.code != ExitOK {
		t.Fatalf("run = %d, want %d (logs: %s)", res.code, ExitOK, res.shown())
	}
	for _, name := range []string{"alpha", "beta"} {
		user, path := DefaultBotPrefix+"seat-"+name, fixtureTokenFile("seat-"+name)
		if perm := f.Permission(protectionRepo, user); perm != permissionWrite {
			t.Errorf("%s holds %q on %s, want %q", user, perm, protectionRepo, permissionWrite)
		}
		if _, ok := fakeTokenScopes(f, user, DefaultTokenName); !ok {
			t.Errorf("the fake holds no %s token for %s", DefaultTokenName, user)
		}
		if _, ok := files.files[path]; !ok {
			t.Errorf("the run wrote no token file %s", path)
		}
	}
	calls := strings.Join(sentCalls(f), "\n")
	for _, forbidden := range []string{"branch_protections", "bot-polecat", "bot-landing", "bot-registry", "bot-viewer"} {
		if strings.Contains(calls, forbidden) {
			t.Errorf("the seat run sent a request about %s: %s", forbidden, calls)
		}
	}
	if got := tokenFileCount(files); got != 2 {
		t.Errorf("the seat run wrote %d token files, want the two seats'", got)
	}
}

// TestSeatRunStopsAtTheFirstRefusedSeat: a refused grant for the first seat exits
// non-zero and sends nothing for the second.
func TestSeatRunStopsAtTheFirstRefusedSeat(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	f.AddRepo(protectionRepo)
	files := newMemFiles()
	rt := failingRoute{next: f, match: "/collaborators/" + DefaultBotPrefix + "seat-alpha", method: http.MethodPut, status: http.StatusInternalServerError}
	res := runProvisioning(t, rt, files, nil, "--seat", "alpha", "--seat", "beta", "--repo", protectionRepo)
	if res.code != ExitFailure {
		t.Fatalf("run = %d, want %d (logs: %s)", res.code, ExitFailure, res.shown())
	}
	if calls := strings.Join(sentCalls(f), "\n"); strings.Contains(calls, DefaultBotPrefix+"seat-beta") {
		t.Errorf("the run reached the second seat after the first failed: %s", calls)
	}
	if _, ok := files.files[fixtureTokenFile("seat-beta")]; ok {
		t.Error("the run wrote the second seat's token file, want none")
	}
}

// TestEnsureTokenRefusesASeatPrefixedRoleOutsideTheSeatGrammar checks the seat
// scopes go only to the role ProvisionSeat builds: any other role is unknown, so
// a string that merely starts with "seat-" cannot mint a polecat-scoped token.
func TestEnsureTokenRefusesASeatPrefixedRoleOutsideTheSeatGrammar(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	files := newMemFiles()
	p := tokenProvisioner(f, files, false, false, new([]string))
	for _, role := range []string{"seat-Furiosa", "seat-fur_iosa", "seat-fur.iosa", "seat-fur/iosa", "seat-", "seat"} {
		if err := p.EnsureToken(role); err == nil {
			t.Errorf("EnsureToken(%q) = nil, want the unknown role refused", role)
		}
	}
	if calls := sentCalls(f); len(calls) != 0 {
		t.Errorf("a request was sent for a refused role: %v", calls)
	}
	if len(files.ops) != 0 {
		t.Errorf("a file was written for a refused role")
	}
}

// TestEnsureTokenRefusesASeatNameProvisionSeatRefuses checks a seat-prefixed role
// whose name ProvisionSeat refuses is an unknown role for EnsureToken too, so
// neither check is looser than the other: no token is minted and no file written.
func TestEnsureTokenRefusesASeatNameProvisionSeatRefuses(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	files := newMemFiles()
	p := tokenProvisioner(f, files, false, false, new([]string))
	for _, role := range []string{"seat-a--b", "seat-a-", "seat--a", seatRolePrefix + strings.Repeat("a", maxSeatNameLength+1)} {
		err := p.EnsureToken(role)
		if err == nil || !strings.Contains(err.Error(), "unknown role") {
			t.Errorf("EnsureToken(%q) = %v, want the unknown role error", role, masked(errString(err)))
		}
	}
	if calls := sentCalls(f); len(calls) != 0 {
		t.Errorf("a request was sent for a refused role: %v", calls)
	}
	if len(files.ops) != 0 {
		t.Errorf("a file was written for a refused role")
	}
}

// TestProvisionSeatHonoursTheRunFlags checks --dry-run sends no write or file,
// --skip-tokens mints no token, and --rotate replaces the token the first run
// minted, as each flag does for a role.
func TestProvisionSeatHonoursTheRunFlags(t *testing.T) {
	t.Parallel()
	const user = DefaultBotPrefix + "seat-furiosa"
	path := tokenDir + "/forgejo-seat-furiosa.env"
	cases := []struct {
		name                 string
		dryRun, skip, rotate bool
		check                func(t *testing.T, f *forgejotest.Fake, files *memFiles, first string)
	}{
		{"dry run", true, false, false, func(t *testing.T, f *forgejotest.Fake, files *memFiles, _ string) {
			if w := f.Writes(); len(w) != 0 || len(files.files) != 0 {
				t.Errorf("the dry run sent %d writes and wrote %d files, want none", len(w), len(files.files))
			}
		}},
		{"skip tokens", false, true, false, func(t *testing.T, f *forgejotest.Fake, files *memFiles, _ string) {
			if _, ok := fakeTokenScopes(f, user, DefaultTokenName); ok {
				t.Errorf("--skip-tokens minted a token for %s", user)
			}
			if _, ok := files.files[path]; ok {
				t.Error("--skip-tokens wrote the token file")
			}
		}},
		{"rotate", false, false, true, func(t *testing.T, f *forgejotest.Fake, files *memFiles, first string) {
			second := f.Token(user, DefaultTokenName)
			if second == "" || second == first {
				t.Fatal("rotation kept the token, want a newly minted one")
			}
			if got := string(files.files[path]); got != tokenFileText(user, second) {
				t.Errorf("the token file does not hold the rotated token: %d bytes", len(got))
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := forgejotest.New()
			for _, repo := range seatRepos {
				f.AddRepo(repo)
			}
			withBot(t, f, "seat-furiosa")
			// The account seed is a write; only the run under test is counted.
			f.ClearCalls()
			files := newMemFiles()
			p := tokenProvisioner(f, files, false, tc.dryRun, new([]string))
			p.SkipTokens = tc.skip
			first := ""
			if tc.rotate {
				if err := p.ProvisionSeat("furiosa", seatRepos); err != nil {
					t.Fatalf("first run: %v", masked(errString(err)))
				}
				first, p.Rotate = f.Token(user, DefaultTokenName), true
			}
			if err := p.ProvisionSeat("furiosa", seatRepos); err != nil {
				t.Fatalf("ProvisionSeat: %v", masked(errString(err)))
			}
			tc.check(t, f, files, first)
		})
	}
}
