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

// TestProvisionSeatDryRunSendsNoWrite checks a dry run reports the seat's account
// and token and sends no write or file.
func TestProvisionSeatDryRunSendsNoWrite(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	for _, repo := range seatRepos {
		f.AddRepo(repo)
	}
	files := newMemFiles()
	var logs []string
	p := tokenProvisioner(f, files, false, true, &logs)
	if err := p.ProvisionSeat("furiosa", seatRepos); err != nil {
		t.Fatalf("ProvisionSeat dry run: %v", masked(errString(err)))
	}
	if writes := f.Writes(); len(writes) != 0 {
		t.Errorf("the dry run sent %d writes: %v", len(writes), writes)
	}
	if len(files.files) != 0 {
		t.Errorf("the dry run wrote %d files, want none", len(files.files))
	}
	want := "would mint a '" + DefaultTokenName + "' token for " + DefaultBotPrefix + "seat-furiosa"
	if joined := strings.Join(logs, "\n"); !strings.Contains(joined, want) {
		t.Errorf("logs lack %q: %v", want, masked(joined))
	}
}

// TestProvisionSeatSkipTokensWritesNoToken checks --skip-tokens leaves an existing
// seat account without minting or writing a token, and still grants the repos.
func TestProvisionSeatSkipTokensWritesNoToken(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	for _, repo := range seatRepos {
		f.AddRepo(repo)
	}
	withBot(t, f, "seat-furiosa")
	files := newMemFiles()
	p := tokenProvisioner(f, files, false, false, new([]string))
	p.SkipTokens = true
	if err := p.ProvisionSeat("furiosa", seatRepos); err != nil {
		t.Fatalf("ProvisionSeat --skip-tokens: %v", masked(errString(err)))
	}
	user := DefaultBotPrefix + "seat-furiosa"
	if _, ok := fakeTokenScopes(f, user, DefaultTokenName); ok {
		t.Errorf("--skip-tokens minted a token for %s", user)
	}
	if _, ok := files.files[tokenDir+"/forgejo-seat-furiosa.env"]; ok {
		t.Errorf("--skip-tokens wrote the token file")
	}
	for _, repo := range seatRepos {
		if perm := f.Permission(repo, user); perm != permissionWrite {
			t.Errorf("%s holds %q on %s, want %q", user, perm, repo, permissionWrite)
		}
	}
}

// TestProvisionSeatRotateReplacesTheToken checks --rotate on a seat mints a new
// token and replaces the file, as the same flag does for a role.
func TestProvisionSeatRotateReplacesTheToken(t *testing.T) {
	t.Parallel()
	f := forgejotest.New()
	for _, repo := range seatRepos {
		f.AddRepo(repo)
	}
	files := newMemFiles()
	p := tokenProvisioner(f, files, false, false, new([]string))
	if err := p.ProvisionSeat("furiosa", seatRepos); err != nil {
		t.Fatalf("first run: %v", masked(errString(err)))
	}
	user, path := DefaultBotPrefix+"seat-furiosa", tokenDir+"/forgejo-seat-furiosa.env"
	first := f.Token(user, DefaultTokenName)
	p.Rotate = true
	if err := p.ProvisionSeat("furiosa", seatRepos); err != nil {
		t.Fatalf("rotate: %v", masked(errString(err)))
	}
	second := f.Token(user, DefaultTokenName)
	if second == "" || second == first {
		t.Fatal("rotation kept the token, want a newly minted one")
	}
	if got := string(files.files[path]); got != tokenFileText(user, second) {
		t.Errorf("the token file does not hold the rotated token: %d characters", len(got))
	}
}
