package done

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/authorkind"
	"github.com/steveyegge/gastown/internal/config"
)

// A crew submission carries the session's agent identity, never the human's:
// the branch seat is crew/<agent> and the head commit's author and committer
// are the agent (gt-dvotr). A hand-run wizard or a copied git identity
// otherwise lands the operator's name on the work.
//
// The human is the town's overseer, the identity configuration records and
// the landing worker refuses a submission under (gt-2ge3z), so gt done reads
// that one record rather than naming a person in code.

// humanIdentity is the town's configured human: the overseer's name and
// email.
type humanIdentity struct {
	name  string
	email string
}

// loadHumanIdentity reads the human's identity from the town's configuration.
// A town whose identity cannot be read gets an error, never an empty identity
// that would let every branch and commit pass (gt-2ge3z).
func loadHumanIdentity(townRoot string) (humanIdentity, error) {
	path := config.OverseerConfigPath(townRoot)
	cfg, err := config.LoadOverseerConfig(path)
	if err != nil {
		return humanIdentity{}, fmt.Errorf(
			"refusing to submit: the town's human identity cannot be read from %s, so gt done cannot tell whether this branch or commit names the human: %w", path, err)
	}
	return humanIdentity{name: cfg.Name, email: cfg.Email}, nil
}

// names reports whether value names the human: case-insensitively equal to
// their configured name or to the local part of their email.
func (h humanIdentity) names(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if h.name != "" && strings.EqualFold(value, h.name) {
		return true
	}
	local := emailLocalPart(h.email)
	return local != "" && strings.EqualFold(value, local)
}

// emailLocalPart is the part of address before its "@", trimmed; "" when the
// address is empty or has no "@".
func emailLocalPart(address string) string {
	address = strings.TrimSpace(address)
	at := strings.IndexByte(address, '@')
	if at <= 0 {
		return ""
	}
	return strings.TrimSpace(address[:at])
}

// crewSeatAgent is the agent a crew/<agent>/<bead> branch names, and "" for a
// branch of another shape. A crew branch missing its bead still names its
// agent, so the segment after "crew/" is read with or without the "/" that
// follows it.
func crewSeatAgent(branch string) string {
	rest, ok := strings.CutPrefix(strings.TrimSpace(branch), authorkind.CrewPrefix)
	if !ok {
		return ""
	}
	seat, _, _ := strings.Cut(rest, "/")
	return strings.TrimSpace(seat)
}

// refuseHumanIdentity refuses a crew submission whose branch or head commit
// names the human, naming the field that matched and how to fix it.
func refuseHumanIdentity(r *doneRun, head string) error {
	human, err := loadHumanIdentity(r.townRoot)
	if err != nil {
		return err
	}
	if seat := crewSeatAgent(r.branch); seat != "" && human.names(seat) {
		return fmt.Errorf(
			"refusing to submit: the branch name %q names the human, %s, not an agent.\n"+
				"Every branch is crew/<agent>/<bead>, and <agent> is the session's own agent name.\n"+
				"Rename the branch to crew/<agent>/<bead>, push it, and re-run gt done.",
			r.branch, human.name)
	}
	authorship, err := r.deps.repo.CommitAuthorship(head)
	if err != nil {
		return fmt.Errorf("refusing to submit: the author and committer of head commit %s cannot be read: %w", ShortSHA(head), err)
	}
	// shown is the value the refusal prints; names is what the human
	// comparison reads, the local part for an email.
	var named []string
	for _, field := range []struct {
		name, shown, names string
	}{
		{"author name", authorship.AuthorName, authorship.AuthorName},
		{"author email", authorship.AuthorEmail, emailLocalPart(authorship.AuthorEmail)},
		{"committer name", authorship.CommitterName, authorship.CommitterName},
		{"committer email", authorship.CommitterEmail, emailLocalPart(authorship.CommitterEmail)},
	} {
		if human.names(field.names) {
			named = append(named, fmt.Sprintf("%s %q", field.name, field.shown))
		}
	}
	if len(named) == 0 {
		return nil
	}
	return fmt.Errorf(
		"refusing to submit: the head commit %s names the human, %s, in its %s.\n"+
			"Every author and committer is the session's agent identity, and gt done will not put the operator's name on landed work.\n"+
			"Amend the commit under your agent identity (set user.name and user.email to the agent's, then run git commit --amend --reset-author), and re-run gt done.",
		ShortSHA(head), human.name, strings.Join(named, ", "))
}
