# Installing Gas Town

Complete setup guide for Gas Town multi-agent orchestrator.

For the shortest path, use `brew install gastown` on macOS or the Docker setup in [docker.md](docker.md). Homebrew installs `gt`, `bd`, and `dolt` together. Docker supplies the runtime tools inside the container. The native/source paths below are for hosts where you install and run `gt` directly.

## Prerequisites

### Required

Native source installs require these host tools. Homebrew and Docker installs provide some of them for you, as noted in the platform sections below. Docker installs only require Docker Compose on the host; the container supplies Go, Dolt, `bd`, tmux, and CLI utilities.

| Tool | Version | Check | Install |
|------|---------|-------|---------|
| **Go** | 1.26.2+ | `go version` | See [golang.org](https://go.dev/doc/install) |
| **Git** | 2.20+ | `git --version` | See below |
| **ICU4C dev headers** | varies | `pkg-config --modversion icu-uc`, `dpkg -l libicu-dev`, `rpm -q libicu-devel`, or `brew --prefix icu4c` | Source builds need Debian/Ubuntu `libicu-dev`, Fedora/RHEL `libicu-devel` with `pkgconf-pkg-config`, macOS `icu4c`, or native Windows MSYS2 ICU/toolchain/pkg-config packages |
| **Dolt** | >= 2.0.7 | `dolt version` | macOS: `brew install dolt`; other platforms: see [dolthub/dolt](https://github.com/dolthub/dolt?tab=readme-ov-file#installation) |
| **Beads** | >= 0.57.0 | `bd version` | Installed by `brew install gastown`, or from source with `go install github.com/steveyegge/beads/cmd/bd@latest` |
| **Docker Compose** | v2+ | `docker compose version` | Docker setup only. Install Docker Desktop or Docker Engine with the Compose plugin. |

### Optional (for Full Stack Mode)

| Tool | Version | Check | Install |
|------|---------|-------|---------|
| **tmux** | 3.0+ | `tmux -V` | See below |
| **Claude Code** (the only runtime) | >= 2.0.20 | `claude --version` | See [claude.ai/claude-code](https://claude.ai/claude-code) |

## Installing Prerequisites

### macOS

Use Homebrew for the normal macOS install. It installs `gt`, `bd`, and `dolt` together.

```bash
# Install Homebrew if needed
/bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"

# Recommended install
brew install gastown

# Optional: source builds also need Go, Dolt, and ICU4C
brew install go dolt icu4c

# Optional: Docker setup only
# Install Docker Desktop or another Docker Engine with Compose v2.

# Optional (for full stack mode)
brew install tmux
```

### Linux (Debian/Ubuntu)

```bash
# Required
sudo apt update
sudo apt install -y git libicu-dev

# Install Go (apt version may be outdated, use official installer)
wget https://go.dev/dl/go1.26.2.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.26.2.linux-amd64.tar.gz
echo 'export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH' >> ~/.bashrc
source ~/.bashrc

# Install Dolt: see https://github.com/dolthub/dolt?tab=readme-ov-file#installation

# Docker setup only: install Docker Engine with the Compose plugin.

# Optional (for full stack mode)
sudo apt install -y tmux
```

### Linux (Fedora/RHEL)

```bash
# Required
sudo dnf install -y git libicu-devel pkgconf-pkg-config
# Install Go 1.26.2+ from your distro if available, otherwise use the official Go installer.
# Install Dolt: see https://github.com/dolthub/dolt?tab=readme-ov-file#installation
# Docker setup only: install Docker Engine with the Compose plugin.

# Optional
sudo dnf install -y tmux
```

### Windows

Install Go and Dolt first, then install `gt` and `bd` with Go. The binaries land in `%USERPROFILE%\go\bin`; put that directory before older `gt` or `bd` install locations on `PATH`, then open a new shell. For Docker setup, install Docker Desktop with Compose support.

Native Windows source builds that compile the ICU-backed query layer need an MSYS2 UCRT64 or MinGW64 shell with matching `icu`, `toolchain`, and `pkg-config` packages. The repository's Windows CI uses `pacboy -S icu:p toolchain:p pkg-config:p` before running Go commands; plain PowerShell/MSVC is not enough for that CGO build.

```powershell
go install github.com/steveyegge/gastown/cmd/gt@latest
go install github.com/steveyegge/beads/cmd/bd@latest
```

Use WSL or another Linux environment for tmux-backed workflows. Native Windows shells are best suited to minimal CLI-only use.

### Verify Prerequisites

```bash
# Check all prerequisites
go version        # Should show go1.26.2 or higher
git --version     # Should show 2.20 or higher
dolt version      # Should show 2.0.7 or higher
tmux -V           # (Optional) Should show 3.0 or higher
```

## Installing Gas Town

### Step 1: Install the Binaries

If you used `brew install gastown`, the binaries are already installed. Verify them:

```bash
gt version
bd version
dolt version
```

On Linux and Windows, install `gt` and `bd` with Go after installing Dolt separately:

```bash
go install github.com/steveyegge/gastown/cmd/gt@latest
go install github.com/steveyegge/beads/cmd/bd@latest
```

Homebrew installs the runtime dependencies declared by the core formula. The
`gastownhall/gastown` tap is reserved for emergency updates. If you build from
source instead, install `dolt` and ICU4C first, install `bd` with Go, and ensure both
`~/.local/bin` and `$GOPATH/bin` (usually `~/go/bin`) appear before older
install locations. On macOS, do not install `gt` with `go install`:
unsigned binaries may be killed by the OS. Clone the repository and use
`make install-local` instead: it builds the checkout and swaps the binary into
`~/.local/bin` atomically. It is for this first install only; once a town
exists, update with `make install` (see [Updating](#updating)).

```bash
brew install dolt icu4c
go install github.com/steveyegge/beads/cmd/bd@latest
export PATH="$HOME/.local/bin:$HOME/go/bin:$PATH"
git clone https://github.com/steveyegge/gastown.git
cd gastown
make install-local
```

### Step 2: Create Your Workspace

Run these workspace steps on macOS, Linux, or WSL. Native Windows shells are minimal CLI-only environments; use WSL for `--shell`, `gt up`, tmux-backed roles, and Mayor sessions.

```bash
# Set identity before --git so the initial HQ commit and Dolt config are valid
git config --global user.name "Your Name"
git config --global user.email "you@example.com"

# Create a Gas Town workspace (HQ)
gt install ~/gt --shell --git

# This creates:
#   ~/gt/
#   ├── CLAUDE.md          # Identity anchor (run gt prime)
#   ├── mayor/             # Mayor config and state
#   ├── rigs/              # Project containers (initially empty)
#   └── .beads/            # Town-level issue tracking
```

### Step 3: Add a Project (Rig)

```bash
# Add your first project
gt rig add myproject https://github.com/you/repo.git

# This clones the repo and sets up:
#   ~/gt/myproject/
#   ├── .beads/            # Project issue tracking
#   ├── mayor/rig/         # Mayor's clone (canonical)
#   ├── crew/              # Human workspaces
#   └── polecats/          # Worker clones (created on demand)
```

### Step 4: Verify Installation

```bash
cd ~/gt

gt up                  # Start all services. Use gt down or gt shutdown for stopping. 

gt doctor              # Read-only health checks
gt doctor fix <check>  # Repair one check doctor reported
gt status              # Show workspace status
```

### Step 5: Configure Agents (Optional)

Every agent runs the Claude Code CLI (or a wrapper that execs it). The built-in
presets are `claude` and `groq-compound`; custom agent aliases add models,
flags or another Anthropic-compatible backend.

```bash
# List available agents
gt config agent list

# Create an alias (aliases can encode model/thinking flags)
gt config agent set claude-haiku "claude --model haiku --dangerously-skip-permissions"

# Set the town default agent (used when a rig doesn't specify one)
gt config default-agent claude-haiku
```

You can also override the agent per command without changing defaults:

```bash
gt mayor start --agent claude-haiku
gt sling gt-abc12 myproject --agent claude-haiku
```

## Minimal Mode vs Full Stack Mode

Gas Town supports two operational modes:

### Minimal Mode (No Daemon)

Run individual runtime instances manually. Gas Town only tracks state.

```bash
# Assign work
gt sling gt-abc12 myproject

# Run runtime manually
cd ~/gt/myproject/polecats/<worker>
claude --resume          # Claude Code

# Check progress
bd show gt-abc12
```

**When to use**: Testing, simple workflows, or when you prefer manual control.

### Full Stack Mode (With Daemon)

Agents run in tmux sessions. Daemon manages lifecycle automatically.

```bash
# Start the daemon
gt daemon start

# Assign work (workers spawn automatically)
gt sling gt-abc12 gt-def34 myproject

# Check dispatch
gt scheduler status

# Attach to any agent session
gt mayor attach
```

**When to use**: Production workflows with multiple concurrent agents.

#### Auto-Restart on Crash/Boot (Optional)

To have the daemon auto-restart if it crashes and start automatically on
login/boot, hand it off to the OS supervisor (launchd on macOS, systemd on
Linux):

```bash
# If a manually-started daemon is running, stop it first — enable-supervisor
# refuses while daemon.lock is held, to avoid a launchd/systemd respawn loop
# racing the manual daemon for the lock.
gt daemon stop

gt daemon enable-supervisor
```

Host-specific environment variables the daemon needs (e.g. `SDKROOT` on
machines with a custom CLT/SDK setup) can be supplied via
`settings/daemon.env` — see [Reference](reference.md#daemon-environment-settingsdaemonenv).

`gt daemon status` reports what the supervisor is doing, not just that it is
installed: `Supervised: launchd` means the job is running the daemon holding
daemon.lock, and `DETACHED` with a spawn count and last exit code means the job
is not — the daemon is running outside it while launchd respawns and loses the
lock every `ThrottleInterval`. Stopping a supervised daemon goes through the
supervisor: on launchd the job is left unloaded and `gt daemon start` bootstraps
it again, while `systemctl --user stop` leaves the systemd unit loaded but
inactive and `gt daemon start` restarts it. A stop that cannot establish the
job's state exits non-zero and names the uncertainty rather than reporting a
stop it could not confirm.

The job file also records how long the daemon is given to stop before launchd
SIGKILLs it, and that value comes from the `gt` that wrote the file. `gt daemon
start`, `gt daemon restart` and `gt up` therefore rewrite a file installed by
an older `gt` and reload the job from it, so expect the daemon to restart once
at the first of those commands after an upgrade. Only that value is repaired
this way; re-provision anything else with `gt daemon enable-supervisor`.

### Choosing Roles

Gas Town is modular. Enable only what you need:

| Configuration | Roles | Use Case |
|--------------|-------|----------|
| **Polecats only** | Workers | Manual spawning |
| **+ Daemon** | + Supervisor, landing worker | Dead-session restart, gated landing on main |
| **+ Mayor** | + Coordinator | Cross-project coordination |

## Troubleshooting

### `gt: command not found`

The Gas Town binary directory is not in PATH. Homebrew usually handles this for
Homebrew installs. Source installs place `gt` in `~/.local/bin`:

```bash
# Add to your shell config (~/.bashrc, ~/.zshrc)
export PATH="$HOME/.local/bin:$PATH"
source ~/.bashrc  # or restart terminal
```

If you also installed Beads with Go, keep `$HOME/go/bin` in PATH for `bd`.

### `bd: command not found`

Beads CLI not installed:

```bash
go install github.com/steveyegge/beads/cmd/bd@latest
```

### `gt doctor` shows errors

`gt doctor` is read-only. Repair the checks it names one at a time:

```bash
gt doctor fix <check>
```

For persistent issues, check specific errors:

```bash
gt doctor --verbose
```

### Daemon not starting

Check if tmux is installed and working:

```bash
tmux -V                    # Should show version
tmux new-session -d -s test && tmux kill-session -t test  # Quick test
```

### Git authentication issues

Ensure SSH keys or credentials are configured:

```bash
# Test SSH access
ssh -T git@github.com

# Or configure credential helper
git config --global credential.helper cache
```

### Beads issues

If experiencing beads problems:

```bash
cd ~/gt/myproject/mayor/rig
bd status                  # Check database health
bd doctor                  # Run beads health check
```

## Updating

Update Gas Town through the same channel you used to install it. For the
recommended Homebrew install:

```bash
brew update
brew upgrade gastown
command -v gt              # Should be Homebrew's gt, e.g. /opt/homebrew/bin/gt
gt version
gt doctor                  # Post-update health checks (read-only)
```

If you installed from source and a town is running, `make install` is the one
way to update `gt`, whether you are a human, a crew session or an automation.
Run it from any gastown clone in the town, with no arguments. It runs
`scripts/install-gt.sh`, which installs the latest `origin/main`. `SHA=<ref>`
picks another merged commit. The script works under a lock. It fast-forwards
`<town>/gastown/mayor/rig`, builds there, and swaps the binary atomically. A
smoke check follows, and if it fails the script rolls back to `gt.prev`. It
then syncs formulas and plugins; plugin sync leaves runtime-only plugin edits
untouched. Finally it writes `daemon/restart-pending.json`, and the daemon
restarts itself once idle. It never kills the daemon. Exit codes: 0 installed
or already current, 1 failed and rolled back, 2 refused (mayor/rig dirty,
diverged or not forward), 3 busy (retry). The daemon's `rebuild_gt` job calls
the same script as a backstop, whenever the installed binary falls behind main
and the town is quiet. Do not copy binaries by hand, run `go install`, or
use `make install-local` in a town: none of them restarts the daemon or rolls
back.

```bash
make install               # from any gastown clone in the town
gt version                 # the installed commit
gt doctor                  # read-only checks after the update
```

If you maintain Beads separately from Homebrew, update `bd` from its own source:

```bash
go install github.com/steveyegge/beads/cmd/bd@latest
```

Run the `command -v gt` and `gt version` checks before `gt doctor` so a
stale shadow binary does not run the checks first.

If `command -v gt` points at a different install channel than the one you just
updated, fix your PATH before continuing.

## Uninstalling

Remove the installed binaries from a gastown clone:

```bash
make uninstall             # the directory comes from INSTALL_DIR
```

`make uninstall` runs `scripts/uninstall-gt.sh`, which removes `<INSTALL_DIR>/gt`
and `<INSTALL_DIR>/bd` and nothing else. The directory is named, not looked up:
`command -v` answers what runs if you type `gt`, and in a running town that is
the live install every agent on the host runs from — which is how the recipe
this replaces resolved to the town's own binaries and deleted them (gt-acdfp).
The script also clears the OS-immutable flag `make install` leaves on the binary
(gt-vya0s) before removing it.

Handed the live install of a town — `$HOME/.local/bin` with a town at or above
the clone — the script refuses, exits 2 and names the town. Removing the
binaries a running town runs from is a decommission, not a test, and it has to
be asked for: `FORCE=1`, after `gt daemon stop`, is how you ask. To exercise an
uninstall without risking the install the town is using, name a scratch
directory:

```bash
make uninstall INSTALL_DIR="$(mktemp -d)"
```

The workspace is a separate act, and `make uninstall` will not perform it:

```bash
rm -rf ~/gt                # CAUTION: every rig, worktree and bead in the town
```

## Next Steps

After installation:

1. **Read the README** - Core concepts and workflows
2. **Try a simple workflow** - `bd create "Test task"` then `gt sling <bead-id> <rig>`
3. **Explore docs** - `docs/reference.md` for command reference
4. **Run doctor regularly** - `gt doctor` catches problems early
