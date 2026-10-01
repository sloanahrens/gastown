package convoy

import (
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/mail"
)

// nudgeCall is one notice nudge: its target, message and sender.
type nudgeCall struct {
	Target  string
	Message string
	Sender  string
}

// noticeScript is a Town's notice seams: it records every mail and nudge a
// convoy sent and fails none. onMail runs after each mail is recorded, which
// is how the notice order tests place a send among the store's calls.
type noticeScript struct {
	mu     sync.Mutex
	mails  []mail.SendRequest
	nudges []nudgeCall
	onMail func()
}

func (s *noticeScript) mail(req mail.SendRequest) error {
	s.mu.Lock()
	s.mails = append(s.mails, req)
	onMail := s.onMail
	s.mu.Unlock()
	if onMail != nil {
		onMail()
	}
	return nil
}

func (s *noticeScript) nudge(target, message, sender string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nudges = append(s.nudges, nudgeCall{Target: target, Message: message, Sender: sender})
	return nil
}

func (s *noticeScript) mailsSent() []mail.SendRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mail.SendRequest(nil), s.mails...)
}

func (s *noticeScript) nudgesSent() []nudgeCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]nudgeCall(nil), s.nudges...)
}

// testTown is a Town at root whose every store, the routed issue lookup
// included, is db, and whose notices go to notices. A nil notices leaves the
// town's real mail and nudge delivery in place.
func testTown(root string, db Store, notices *noticeScript) Town {
	t := Town{Root: root, Open: func(string) Store { return db }, Issues: db}
	if notices != nil {
		t.Mail, t.Nudge = notices.mail, notices.nudge
	}
	return t
}

// townDB is an empty fake town database (prefix hq).
func townDB() *beadsfake.Fake { return beadsfake.New(beadsfake.WithPrefix("hq")) }

// seedConvoy stores an open gt:convoy convoy and the issues it tracks. A
// tracked issue that is not already in db is seeded as given.
func seedConvoy(t *testing.T, db *beadsfake.Fake, convoy beads.Issue, tracked ...beads.Issue) {
	t.Helper()
	if convoy.Type == "" {
		convoy.Type = "convoy"
	}
	convoy.Labels = append(convoy.Labels, ConvoyLabel)
	db.Seed(convoy)
	for _, is := range tracked {
		if _, err := db.Show(is.ID); err != nil {
			db.Seed(is)
		}
		if err := db.AddTypedDependency(convoy.ID, is.ID, "tracks"); err != nil {
			t.Fatalf("tracks %s -> %s: %v", convoy.ID, is.ID, err)
		}
	}
}

// rawDepsAnswer scripts db's bd sql answer for the tracked-edge query of
// each convoy in tracks (convoy ID -> raw depends_on_id values); any other
// convoy has none.
func rawDepsAnswer(db *beadsfake.Fake, tracks map[string][]string) {
	db.OnSQL(func(query string) ([][]string, error) {
		rows := [][]string{{"depends_on_id"}}
		for convoyID, targets := range tracks {
			if strings.Contains(query, "issue_id = '"+convoyID+"'") {
				for _, id := range targets {
					rows = append(rows, []string{id})
				}
			}
		}
		return rows, nil
	})
}
