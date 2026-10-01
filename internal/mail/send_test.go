package mail

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// sendTown writes a town root with a town beads database and a workspace for
// every address in agents, returning the root and the sender's workspace.
func sendTown(t *testing.T, agents ...string) (root, senderDir string) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "town")
	senderDir = filepath.Join(root, "barnaby", "crew", "tom")
	dirs := []string{
		filepath.Join(root, ".beads"),
		filepath.Join(root, "mayor"),
		senderDir,
	}
	for _, agent := range agents {
		dirs = append(dirs, filepath.Join(root, filepath.FromSlash(agent)))
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	files := map[string]string{
		filepath.Join(root, "mayor", "town.json"):                     `{"name":"test"}`,
		filepath.Join(root, ".beads", "beads.db"):                     "",
		filepath.Join(root, ".beads", ".gt-types-configured"):         beads.TypeConfigSentinelValue() + "\n",
		filepath.Join(root, "barnaby", "crew", "tom", "polecat.json"): `{"name":"tom"}`,
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return root, senderDir
}

// sendBd answers the town's reads, accepts every create and serves the
// message bead in show, which is what a reply's thread lookup reads.
func sendBd(show string) *bdScript {
	return &bdScript{answer: func(c bdCall) (string, string, int) {
		args := c.Args
		switch {
		case args[0] == "config" || args[0] == "init" || args[0] == "types":
			return "", "", 0
		case args[0] == "list":
			return "[]\n", "", 0
		case len(args) >= 3 && args[0] == "mol" && args[1] == "wisp" && args[2] == "list":
			return "[]\n", "", 0
		case args[0] == "show":
			return show, "", 0
		case args[0] == "create":
			return "hq-testmail-1\n", "", 0
		}
		return "", "unsupported bd args: " + strings.Join(args, " "), 1
	}}
}

// sendRouter is a router in a town whose agents have workspaces and whose bd
// answers bd.
func sendRouter(t *testing.T, bd *bdScript, agents ...string) *Router {
	t.Helper()
	_, senderDir := sendTown(t, agents...)
	r := NewRouter(senderDir, testPrefixRegistry())
	r.bd = bd.run
	r.town = noTownBeads{}
	return r
}

// createdTo returns the assignees of every bd create the router ran.
func createdTo(bd *bdScript) []string {
	var out []string
	for _, c := range bd.recorded() {
		if c.Args[0] != "create" {
			continue
		}
		for i, arg := range c.Args {
			if arg == "--assignee" && i+1 < len(c.Args) {
				out = append(out, c.Args[i+1])
			}
		}
	}
	return out
}

// TestSendMessageFansOutOneCopyPerRecipient: every resolved recipient gets a
// copy of their own, and the receipt names them.
func TestSendMessageFansOutOneCopyPerRecipient(t *testing.T) {
	t.Parallel()
	bd := sendBd("")
	r := sendRouter(t, bd, "barnaby/crew/troy", "barnaby/crew/alice")
	r.resolve = func(string) ([]Recipient, error) {
		return []Recipient{
			{Address: "barnaby/crew/troy", Type: RecipientAgent},
			{Address: "barnaby/crew/alice", Type: RecipientAgent},
		}, nil
	}

	receipt, err := r.SendMessage(SendRequest{
		From: "convoy/hq-cv-1", To: "barnaby/", Subject: "Convoy landed", Body: "All closed.",
		Priority: PriorityNormal, Type: TypeNotification, Wisp: true, SuppressNotify: true,
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if want := "barnaby/troy,barnaby/alice"; strings.Join(createdTo(bd), ",") != want {
		t.Errorf("delivered to %v, want %s", createdTo(bd), want)
	}
	if want := "barnaby/crew/troy,barnaby/crew/alice"; strings.Join(receipt.Recipients, ",") != want {
		t.Errorf("receipt recipients = %v, want %s", receipt.Recipients, want)
	}
	if len(receipt.Failed) != 0 {
		t.Errorf("receipt failures = %v, want none", receipt.Failed)
	}
	if receipt.Message == nil || !receipt.Message.SuppressNotify {
		t.Error("the message did not carry --no-notify onto the router")
	}
	if receipt.Message.From != "convoy/hq-cv-1" {
		t.Errorf("message sender = %q, want convoy/hq-cv-1", receipt.Message.From)
	}
	if receipt.Message.Wisp != true {
		t.Error("the message was not sent as a wisp")
	}
	if receipt.Message.Priority != PriorityNormal {
		t.Errorf("message priority = %v, want normal", receipt.Message.Priority)
	}
	if receipt.Message.Type != TypeNotification {
		t.Errorf("message type = %v, want notification", receipt.Message.Type)
	}
	// The fields the request carries have to reach the delivered copy, not
	// just the receipt.
	for _, argv := range bd.argvs() {
		if !strings.HasPrefix(argv, "create ") {
			continue
		}
		if !strings.Contains(argv, "--priority 2") || !strings.Contains(argv, "--ephemeral") {
			t.Errorf("delivered copy is not a normal-priority wisp: %s", argv)
		}
		if !strings.Contains(argv, "Convoy landed") {
			t.Errorf("delivered copy lost the subject: %s", argv)
		}
	}
}

// TestSendMessageRejectedAddressDoesNotFallBackToLegacyRouting: an address the
// resolver rejects is definitively bad, so nothing is delivered — the fallback
// would have written it to a dead inbox (gt#2038).
func TestSendMessageRejectedAddressDoesNotFallBackToLegacyRouting(t *testing.T) {
	t.Parallel()
	bd := sendBd("")
	r := sendRouter(t, bd)
	r.resolve = func(address string) ([]Recipient, error) {
		return nil, ErrUnknownRecipient
	}

	_, err := r.SendMessage(SendRequest{From: "mayor/", To: "nowhere/", Subject: "s", Body: "b"})
	if !errors.Is(err, ErrUnknownRecipient) {
		t.Fatalf("SendMessage error = %v, want ErrUnknownRecipient", err)
	}
	if got := createdTo(bd); len(got) != 0 {
		t.Errorf("a rejected address was delivered anyway: %v", got)
	}
}

// TestSendMessageResolverFailureFallsBackToLegacyRouting: a resolver that
// failed for any other reason — beads unreachable, say — is not a verdict on
// the address, so the message goes the legacy way.
func TestSendMessageResolverFailureFallsBackToLegacyRouting(t *testing.T) {
	t.Parallel()
	bd := sendBd("")
	r := sendRouter(t, bd, "barnaby/crew/troy")
	r.resolve = func(string) ([]Recipient, error) {
		return nil, errors.New("beads unreachable")
	}

	receipt, err := r.SendMessage(SendRequest{
		From: "convoy/hq-cv-2", To: "barnaby/crew/troy", Subject: "Convoy landed", Body: "All closed.",
		Priority: PriorityNormal, Type: TypeNotification, Wisp: true, SuppressNotify: true,
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if want := "barnaby/troy"; strings.Join(createdTo(bd), ",") != want {
		t.Errorf("delivered to %v, want the legacy route %s", createdTo(bd), want)
	}
	if want := "barnaby/crew/troy"; strings.Join(receipt.Recipients, ",") != want {
		t.Errorf("receipt recipients = %v, want %s", receipt.Recipients, want)
	}
}

// TestSendMessageReplyInheritsTheThread: a reply reads the original's thread
// from the sender's own mailbox, so both land in one conversation.
func TestSendMessageReplyInheritsTheThread(t *testing.T) {
	t.Parallel()
	original := `[{"id":"hq-msg-1","title":"Convoy landed","description":"All closed.",` +
		`"assignee":"barnaby/tom","status":"open","priority":2,` +
		`"labels":["gt:message","thread:thread-original"]}]`
	bd := sendBd(original)
	r := sendRouter(t, bd, "barnaby/crew/troy")
	r.resolve = func(string) ([]Recipient, error) {
		return []Recipient{{Address: "barnaby/crew/troy", Type: RecipientAgent}}, nil
	}

	receipt, err := r.SendMessage(SendRequest{
		From: "barnaby/crew/tom", To: "barnaby/crew/troy", Subject: "Re: Convoy landed", Body: "Thanks.",
		Priority: PriorityNormal, Type: TypeNotification, Wisp: true, SuppressNotify: true,
		ReplyTo: "hq-msg-1",
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got := receipt.Message.ThreadID; got != "thread-original" {
		t.Errorf("reply thread = %q, want thread-original", got)
	}
	if got := receipt.Message.Type; got != TypeReply {
		t.Errorf("reply type = %q, want %q", got, TypeReply)
	}
	if !strings.Contains(strings.Join(bd.argvs(), "\n"), "thread:thread-original") {
		t.Error("the delivered copy did not carry the original's thread")
	}
}
