package session

import (
	"context"
	"fmt"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/liveness"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Backend names: a seat's polecats are hosted one of these ways.
const (
	BackendTmux      = "tmux"      // a tmux session on the host (the default)
	BackendOpenShell = "openshell" // a tmux session inside an OpenShell sandbox
)

// Profile is the sandbox profile a seat points at: config.SandboxProfile's
// fields, converted by the caller (cmd.sessionBackendForAgent) so this
// package does not depend on the settings loader.
type Profile struct {
	Name             string   // the sandboxes.<name> key
	Gateway          string   // openshell gateway name; "" = the CLI's active gateway
	Image            string   // OCI reference
	Policy           string   // policy file path, relative to the rig repo root
	Providers        []string // provider instance names attached at create
	CPU, Memory      string   // k8s quantities, "" = driver default
	ControlPlaneHost string   // how the sandbox reaches Dolt, and Forgejo unless ForgejoHost is set; "" = host.docker.internal
	ForgejoHost      string   // how the sandbox reaches Forgejo when it differs from Dolt; "" = ControlPlaneHost
	CacheVolume      string   // named volume for /sandbox/.cache; "{rig}" expands; "" = none
	Driver           string   // the gateway's compute driver: DriverDocker or DriverKubernetes; "" means docker
	// KubernetesCacheClaim is the shared claim a kubernetes driver mounts at
	// /sandbox/.cache; "" means no cache mount (gt-zl804).
	KubernetesCacheClaim string

	// Kubernetes pod placement and sizing; only a kubernetes driver sends
	// them (as driver config). Nil and empty mean the gateway's defaults.
	NodeSelector  map[string]string
	Tolerations   []config.SandboxToleration
	PriorityClass string
	RuntimeClass  string
	Requests      *config.SandboxResources
	Limits        *config.SandboxResources
}

// Profile.Driver values, the config package's constants: a kubernetes gateway
// has no docker volumes, so only a docker (or unset) driver gets volume work.
const (
	DriverDocker     = config.SandboxDriverDocker
	DriverKubernetes = config.SandboxDriverKubernetes
)

// SpawnSpec is what a backend needs to host one polecat's session.
type SpawnSpec struct {
	TownRoot    string
	Rig         string
	Polecat     string
	Session     string // the tmux session name, gt-<rig>-<polecat>
	Branch      string // the polecat branch, created on the host before Prepare
	RepoURL     string // the sandbox-side clone URL (Forgejo through the profile's Forgejo host)
	HostWorkDir string // the host worktree; the tmux backend's WorkDir
	// WorkBeadID is the bead the polecat was slung, "" when it was not slung
	// one. The spawn stamps it with the stored prompt's hash, joining a
	// landing's rejection to the prompt version its author had (gt-2lu00).
	WorkBeadID string
	Profile    Profile
	// Env is the startup environment the backend must set on the sandbox
	// (the GT_* identity, the Dolt endpoint, the agent preset's non-secret
	// vars). The tmux backend ignores it: StartSession sets the same vars.
	Env map[string]string
	// Trace times Prepare's phases for the spawn's log line. Nil times nothing.
	Trace *SandboxTrace
}

// Prepared is what Prepare hands back to the session start.
type Prepared struct {
	// WorkDir is where the session's working directory points: the host
	// worktree for tmux, the in-sandbox clone for openshell.
	WorkDir string
	// Env overrides the agent startup command's environment (applied last,
	// an empty value drops the key): the sandbox's GT_TOWN_ROOT, GT_ROOT,
	// BD_ACTOR, GT_SESSION_BACKEND, the Dolt endpoint, and the removal of
	// host-only paths and of any credential the provider injects instead.
	// Nil for tmux.
	Env map[string]string
	// OmitSecretPrefix drops the shell prefix that reads file-backed secrets
	// (settings/daemon.env) into the agent's environment: true when the
	// backend's credentials come from elsewhere (a provider), false for tmux.
	OmitSecretPrefix bool
}

// Backend hosts polecat sessions. The tmux backend is a pass-through to
// today's behavior; the openshell backend builds a sandbox first and then
// drives the tmux server inside it through a routed client (tmux/router.go).
//
// Every method takes the session name the rest of gastown already uses, so
// the daemon, gt sling and gt done call the same thing for both backends.
type Backend interface {
	// Name is BackendTmux or BackendOpenShell.
	Name() string
	// Tmux is the client the session's tmux commands run through. For the
	// tmux backend it is the shared host client; for openshell it is the
	// routed client for this session.
	Tmux() *tmux.Tmux
	// Prepare makes the place the session will run in. The tmux backend
	// returns the host worktree; openshell creates and populates a sandbox.
	Prepare(ctx context.Context, spec SpawnSpec) (*Prepared, error)
	// SessionExists reports whether the session is there to be driven: a
	// host tmux session, or a running sandbox with its tmux session.
	SessionExists(session string) (bool, error)
	// ProgressSample runs the liveness assessment where the session lives.
	ProgressSample(session string, in liveness.Input) liveness.Result
	// Stop ends the session's processes but keeps what Prepare made.
	Stop(session string) error
	// Delete removes what Prepare made. Idempotent.
	Delete(session string) error
}

// NewOpenShellBackend is set by the openshell implementation when it is
// linked in; nil means openshell seats cannot spawn. See sandbox_hooks.go
// for why these registrations are package variables.
var NewOpenShellBackend func(townRoot string, p Profile) (Backend, error)

// Resolve picks the backend for a seat. backend is the seat's backend name
// ("" means tmux); p is the seat's sandbox profile (zero for tmux); host is
// the process's tmux client.
func Resolve(seat, backend string, p Profile, townRoot string, host *tmux.Tmux) (Backend, error) {
	return ResolveWith(NewOpenShellBackend, seat, backend, p, townRoot, host)
}

// ResolveWith is Resolve with the openshell constructor given (nil = not
// linked); Resolve passes NewOpenShellBackend.
func ResolveWith(newOpenShell func(townRoot string, p Profile) (Backend, error), seat, backend string, p Profile, townRoot string, host *tmux.Tmux) (Backend, error) {
	switch backend {
	case "", BackendTmux:
		return NewTmuxBackend(host), nil
	case BackendOpenShell:
		if newOpenShell == nil {
			return nil, fmt.Errorf("seat %s: backend openshell is not linked into this binary", seat)
		}
		if p.Image == "" || p.Policy == "" {
			return nil, fmt.Errorf("seat %s: sandbox profile %q needs image and policy", seat, p.Name)
		}
		return newOpenShell(townRoot, p)
	default:
		return nil, fmt.Errorf("seat %s: unknown session backend %q (want tmux or openshell)", seat, backend)
	}
}

// tmuxBackend is today's behavior behind the interface.
type tmuxBackend struct{ t *tmux.Tmux }

// NewTmuxBackend hosts sessions on the process's tmux server through t.
func NewTmuxBackend(t *tmux.Tmux) Backend { return &tmuxBackend{t: t} }

func (b *tmuxBackend) Name() string     { return BackendTmux }
func (b *tmuxBackend) Tmux() *tmux.Tmux { return b.t }

func (b *tmuxBackend) Prepare(_ context.Context, spec SpawnSpec) (*Prepared, error) {
	return &Prepared{WorkDir: spec.HostWorkDir}, nil
}

func (b *tmuxBackend) SessionExists(session string) (bool, error) {
	if b.t == nil {
		return false, nil
	}
	return b.t.HasSession(session)
}

func (b *tmuxBackend) ProgressSample(_ string, in liveness.Input) liveness.Result {
	return liveness.Assess(b.t, in)
}

func (b *tmuxBackend) Stop(session string) error {
	if b.t == nil {
		return nil
	}
	return b.t.KillSessionWithProcesses(session)
}

func (b *tmuxBackend) Delete(session string) error { return b.Stop(session) }
