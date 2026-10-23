package session

import (
	"os"
	"sync"

	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// sandboxRouting makes every tmux client in the process reach sandboxed
// polecats: the router answers a routed client for any session the sandbox
// registry lists, and the extra-sessions hook lists them. It is installed
// once per process (init below); the town root is found lazily, so a
// process that is not in a town never pays for it.
type sandboxRouting struct {
	mu       sync.Mutex
	townRoot string
	resolved bool
	clients  map[string]*tmux.Tmux // session -> routed client, built once
	findTown func() string
}

var routing = &sandboxRouting{clients: map[string]*tmux.Tmux{}, findTown: defaultFindTown}

func init() {
	tmux.SetSessionRouter(routing.route)
	tmux.SetExtraSessions(routing.sessions)
}

// defaultFindTown is GT_TOWN_ROOT (what the daemon and every town session
// export), else the workspace walk from the working directory.
func defaultFindTown() string {
	if r := os.Getenv("GT_TOWN_ROOT"); r != "" {
		return r
	}
	r, _ := workspace.FindFromCwd()
	return r
}

func (s *sandboxRouting) town() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.resolved {
		s.townRoot = s.findTown()
		s.resolved = true
	}
	return s.townRoot
}

// route returns the routed client for session when the registry lists it.
func (s *sandboxRouting) route(session string) *tmux.Tmux {
	town := s.town()
	if town == "" {
		return nil
	}
	rec, ok, err := ReadSandboxRecord(town, session)
	if err != nil || !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.clients[session]; ok {
		return c
	}
	c := tmux.NewTmuxWithExecPrefix(SandboxTmuxSocket, SandboxExecPrefix(rec.Gateway, session))
	s.clients[session] = c
	return c
}

func (s *sandboxRouting) sessions() []string {
	town := s.town()
	if town == "" {
		return nil
	}
	return ListSandboxSessions(town)
}

// forget drops the cached client for session (after its sandbox is deleted).
func (s *sandboxRouting) forget(session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, session)
}

// ResetSandboxRoutingForTest points routing at townRoot and clears its
// cache; tests use it instead of the process environment.
func ResetSandboxRoutingForTest(townRoot string) {
	routing.mu.Lock()
	defer routing.mu.Unlock()
	routing.townRoot = townRoot
	routing.resolved = true
	routing.clients = map[string]*tmux.Tmux{}
}

// RoutedTmuxFor returns the tmux client that drives session: the routed
// client when the registry lists it, else host. Callers that already hold a
// client need not call this (the router applies inside every client); it
// exists for code that wants the sandbox client explicitly, such as the
// openshell backend's Tmux().
func RoutedTmuxFor(session string, host *tmux.Tmux) *tmux.Tmux {
	if c := routing.route(session); c != nil {
		return c
	}
	return host
}
