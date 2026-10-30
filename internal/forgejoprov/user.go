package forgejoprov

import (
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
)

// passwordAlphabet is what a generated password may hold: the letters and digits
// the script's tr -dc kept.
const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// passwordLength is the length of a generated password, as the script cut it.
const passwordLength = 32

// unbiasedByteLimit is the first byte value the alphabet cannot take evenly: 248
// is 62 four times over, so a byte at or above it is drawn again rather than
// folded onto the first characters.
const unbiasedByteLimit = 256 - 256%len(passwordAlphabet)

// Provisioner creates the bot accounts of a run through Client.
type Provisioner struct {
	// Client is the Forgejo API the run writes through; its DryRun and Log apply.
	Client *Client
	// BotPrefix is the username prefix every bot carries: a role's bot is
	// BotPrefix+role.
	BotPrefix string
	// BotEmailDomain is the domain of a new bot's email.
	BotEmailDomain string
	// Random is the source of a new bot's password; nil means crypto/rand.
	Random io.Reader
	// TokenName names each minted token.
	TokenName string
	// WebURL is the web root written to the token files.
	WebURL string
	// ConfigDir is where the role token files go.
	ConfigDir string
	// Rotate mints a new token where a token file exists, revoking the old one.
	Rotate bool
	// ViewerRequested says the run provisions the viewer role itself, so its role
	// loop grants the viewer's repository access and EnsureViewerAccess does nothing.
	ViewerRequested bool
	// Files is the filesystem the token files are written through; nil means
	// the real one.
	Files Files
}

// EnsureUser creates the bot account of role when it is missing and leaves an
// existing one alone, so a second run writes nothing.
func (p *Provisioner) EnsureUser(role string) error {
	user := p.BotPrefix + role
	path := "/users/" + user
	status, resp, err := p.Client.Do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
		p.logf("user %s exists", user)
		return nil
	case http.StatusNotFound:
	default:
		return p.Client.Unexpected(http.MethodGet, path, status, resp)
	}
	// A dry run draws no password: it reports the create and sends none.
	if p.Client.DryRun {
		p.logf("would create user %s", user)
		return nil
	}
	password, err := randomPassword(p.random())
	if err != nil {
		return err
	}
	// The bots authenticate by token, so nothing reads this password back.
	body := map[string]any{
		"username":             user,
		"email":                user + "@" + p.BotEmailDomain,
		"password":             password,
		"must_change_password": false,
		"send_notify":          false,
	}
	status, resp, err = p.Client.Do(http.MethodPost, "/admin/users", body)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusCreated:
		p.logf("created user %s", user)
	// A concurrent provisioner may have created the account between the GET and
	// the POST.
	case http.StatusUnprocessableEntity:
		p.logf("user %s exists", user)
	default:
		return p.Client.Unexpected(http.MethodPost, "/admin/users", status, resp)
	}
	return nil
}

// random returns the password source: Random when the run set one, else
// crypto/rand.
func (p *Provisioner) random() io.Reader {
	if p.Random != nil {
		return p.Random
	}
	return rand.Reader
}

// logf sends one progress line to the client's Log, and drops it when none is set.
func (p *Provisioner) logf(format string, args ...any) {
	if p.Client.Log != nil {
		p.Client.Log(fmt.Sprintf(format, args...))
	}
}

// randomPassword draws passwordLength characters of passwordAlphabet from src.
// A byte the alphabet cannot take evenly is drawn again, so every character is
// equally likely.
func randomPassword(src io.Reader) (string, error) {
	out := make([]byte, 0, passwordLength)
	buf := make([]byte, passwordLength)
	for len(out) < passwordLength {
		if _, err := io.ReadFull(src, buf); err != nil {
			return "", fmt.Errorf("read random password: %w", err)
		}
		for _, b := range buf {
			if int(b) >= unbiasedByteLimit {
				continue
			}
			out = append(out, passwordAlphabet[int(b)%len(passwordAlphabet)])
			if len(out) == passwordLength {
				break
			}
		}
	}
	return string(out), nil
}
