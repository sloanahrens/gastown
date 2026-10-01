package convoy

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/nudge/deliver"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Town is the town a convoy operation runs against.
type Town struct {
	// Root is the town root; bd runs from it.
	Root string
	// Env is the base environment for the bd children; nil is
	// os.Environ(). The daemon sets it to its routing environment.
	Env []string
	// Out receives the progress lines a command prints; nil discards them.
	Out io.Writer
	// Warn receives warnings; nil discards them.
	Warn io.Writer

	// Open opens the issue store at dir (the town root or a rig directory);
	// nil opens bd pinned to dir's database, starting from Env. Unit tests
	// answer from beadsfake databases.
	Open func(dir string) Store
	// Issues answers the tracked-issue lookups, which bd routes to each
	// ID's rig by prefix; nil is bd at the town root.
	Issues beads.Client
	// Prefixes maps rigs to the session prefixes the stranded scan probes
	// assignees' sessions under; nil gives every rig session.DefaultPrefix.
	Prefixes *session.PrefixRegistry
	// Mail sends one notice message; nil sends it in process through
	// internal/mail.
	Mail func(req mail.SendRequest) error
	// Nudge delivers one notice nudge from sender to target; nil delivers it
	// in process through internal/nudge/deliver.
	Nudge func(target, message, sender string) error
}

// Store is the issue store surface the convoy operations use: the shared
// Client plus the JSONL export and the raw dependency query.
// *beads.Beads and beadsfake.Fake implement it.
type Store interface {
	beads.Client
	Export(path string) error
	SQLCSV(query beadsql.Query) ([][]string, error)
}

// noticeMail sends one notice message: `gt mail send <addr> -s <subject> -m
// <body> --from <from> --no-notify` at the town root. The router's own
// notification to the recipient is suppressed because the nudge watchers get
// a nudge of their own.
func (t Town) noticeMail(addr, subject, body, from string) error {
	req := mail.SendRequest{
		From:           from,
		To:             addr,
		Subject:        subject,
		Body:           body,
		Priority:       mail.PriorityNormal,
		Type:           mail.TypeNotification,
		Wisp:           true,
		SuppressNotify: true,
	}
	if t.Mail != nil {
		return t.Mail(req)
	}
	root := t.rootDir()
	router := mail.NewRouterWithTownRoot(root, root, t.Prefixes)
	_, err := router.SendMessage(req)
	return err
}

// noticeNudge delivers one notice nudge: `gt nudge <target> -m <message>` with
// the sender attributed explicitly, so no GT_ROLE has to be exported for a
// child process to read.
func (t Town) noticeNudge(target, message, sender string) error {
	if t.Nudge != nil {
		return t.Nudge(target, message, sender)
	}
	town := &deliver.Town{Delivery: deliver.New(tmux.NewTmux(), t.rootDir())}
	return town.Nudge(context.Background(), target, message, sender)
}

// StdTown is a Town whose output goes to the terminal: what the CLI uses.
func StdTown(root string) Town {
	return Town{Root: root, Out: os.Stdout, Warn: os.Stderr}
}

// store opens the issue store at dir.
func (t Town) store(dir string) Store {
	if t.Open != nil {
		return t.Open(dir)
	}
	return beads.NewPinned(beads.ResolveBeadsDir(dir), beads.WithEnv(t.Env))
}

func (t Town) outWriter() io.Writer {
	if t.Out == nil {
		return io.Discard
	}
	return t.Out
}

func (t Town) warnWriter() io.Writer {
	if t.Warn == nil {
		return io.Discard
	}
	return t.Warn
}

func (t Town) printf(format string, args ...any) {
	fmt.Fprintf(t.outWriter(), format, args...)
}

func (t Town) warnf(format string, args ...any) {
	style.FprintWarning(t.warnWriter(), format, args...)
}

// rootDir returns the town root, accepting the town's .beads directory too.
func (t Town) rootDir() string {
	if filepath.Base(filepath.Clean(t.Root)) == ".beads" {
		return filepath.Dir(filepath.Clean(t.Root))
	}
	return t.Root
}
