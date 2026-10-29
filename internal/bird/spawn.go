package bird

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// cmuxSocketVar is the variable cmux sets in every surface it opens, so its
// presence is what says there is a workspace to open a tab in.
const cmuxSocketVar = "CMUX_SOCKET_PATH"

// gtBinary is the gt a spawned tab runs, read off PATH as the operator would
// have typed it so the line printed outside cmux runs as it stands.
const gtBinary = "gt"

// spawnBead is the bead a spawned bird runs: the name's own handoff bead, which
// /bird-spawn creates in the new session, so the start in the tab binds the
// name before that bead exists (gt-b58mk).
func spawnBead(name string) string {
	return "gt-" + name
}

// SpawnRequest is what gt bird spawn was asked to open.
type SpawnRequest struct {
	// Name is the bird name the caller asked for, empty to draw a free one.
	Name string
	// Mission is what the new session is asked to do: the words after the name,
	// which /bird-spawn reads there. Empty opens the tab with no prompt.
	Mission string
}

// SpawnResult is the session a spawn opened.
type SpawnResult struct {
	// Name is the bird name the tab runs under, and its title.
	Name string
	// Surface is the cmux surface the tab runs in.
	Surface string
	// Bead is the bead the session was started for.
	Bead string
}

// NotInCmuxError is a spawn with no workspace to open a tab in, carrying the
// gt bird start line to run in this terminal instead.
type NotInCmuxError struct {
	// Line is the gt bird start command, as the tab would have run it.
	Line string
}

// Error says what is missing and the line that stands in for the tab.
func (e *NotInCmuxError) Error() string {
	return "gt bird spawn: " + cmuxSocketVar + " is unset, so there is no cmux workspace to open a tab in; run this instead:\n" + e.Line
}

// Spawn opens a cmux tab running a bird reserved under its name, and reports
// the name, the surface and the bead the session runs (gt-b58mk).
func (r *Runner) Spawn(req SpawnRequest) (SpawnResult, error) {
	if req.Name != "" && !ValidName(req.Name) {
		return SpawnResult{}, &UsageError{Arg: "gt bird spawn " + req.Name, Wants: "a bird name matching " + nameShape.String()}
	}
	if strings.ContainsAny(req.Mission, "\r\n") {
		// The tab's shell takes a newline for the Enter that runs the line, so
		// the words after it would run as commands of their own.
		return SpawnResult{}, &UsageError{Arg: "the mission", Wants: "one line of text"}
	}
	if envGet(r.environ(), cmuxSocketVar) == "" {
		// No name is drawn here: a reservation nothing claims holds its name
		// against the very session the printed line starts.
		return SpawnResult{}, &NotInCmuxError{Line: shellLine(startArgv(req.Name, req.Mission))}
	}
	name, err := r.spawnName(req.Name)
	if err != nil {
		return SpawnResult{}, err
	}
	result, opened, err := r.openTab(name, req.Mission)
	if err != nil && !opened {
		// Nothing runs under the name, so its reservation must not hold it.
		if rbErr := r.Registry.ReleaseName(name); rbErr != nil {
			return SpawnResult{}, fmt.Errorf("%w; releasing the reservation on %s also failed: %v", err, name, rbErr)
		}
	}
	return result, err
}

// spawnName is the name the tab runs: the one the caller gave when nothing
// holds it, else a free one drawn from the pool. Either way the name is left
// reserved under the registry lock, so a second spawn running now gets another.
func (r *Runner) spawnName(given string) (string, error) {
	if given == "" {
		picked, err := r.Registry.Pick()
		if err != nil {
			return "", fmt.Errorf("gt bird spawn: drawing a name: %w", err)
		}
		if picked.Name == "" {
			return "", errors.New("gt bird spawn: every name in the pool is taken; gt bird list shows the live ones")
		}
		return picked.Name, nil
	}
	held, err := r.Registry.ReserveName(given)
	if err != nil {
		return "", fmt.Errorf("gt bird spawn: reserving %s: %w", given, err)
	}
	if held != "" {
		return "", fmt.Errorf("gt bird spawn: the name %s is taken (%s)", given, held)
	}
	return given, nil
}

// openTab opens the tab the bird runs in and titles it with the name; opened
// says whether a tab exists, which decides whether the name may go back to the
// pool when the rest fails.
func (r *Runner) openTab(name, mission string) (result SpawnResult, opened bool, err error) {
	text := shellLine(startArgv(name, mission))
	out, err := r.cmux([]string{"cmux", "new-surface", "--type", "terminal", "--command", text, "--json"})
	if err != nil {
		return SpawnResult{}, false, fmt.Errorf("gt bird spawn: opening the tab: %w", err)
	}
	surface, err := cmuxSurface(out)
	if err != nil {
		return SpawnResult{}, true, fmt.Errorf("gt bird spawn: the tab for %s is open but the surface it runs in is unknown: %w", name, err)
	}
	// The tab's title is cmux's own, so the terminal titles the session sets
	// cannot take the bird's name off it.
	if _, err := r.cmux([]string{"cmux", "rename-tab", "--tab", surface, name}); err != nil {
		return SpawnResult{}, true, fmt.Errorf("gt bird spawn: the tab %s runs %s but titling it failed: %w", surface, name, err)
	}
	return SpawnResult{Name: name, Surface: surface, Bead: spawnBead(name)}, true, nil
}

// startArgv is the gt bird start line the tab runs: the bird under name, bound
// to its own bead, asked for /bird-spawn and the mission. A name the caller did
// not give leaves both flags off, so the session draws its own.
func startArgv(name, mission string) []string {
	argv := []string{gtBinary, "bird", "start"}
	if name != "" {
		argv = append(argv, "--name", name, "--bead", spawnBead(name))
	}
	if mission != "" {
		argv = append(argv, "/bird-spawn "+mission)
	}
	return argv
}

// shellSafe is every character the shell reads as itself inside a word.
const shellSafe = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789@%+=:,./-_"

// shellLine joins argv into the one line the tab's shell reads, quoting the
// words it would otherwise take for more than themselves. It is the only place
// the command text is built: a mission holding a quote, a backtick or a $( )
// reaches the session as the plain text it is, in one argument (gt-b58mk).
func shellLine(argv []string) string {
	words := make([]string, len(argv))
	for i, word := range argv {
		words[i] = shellWord(word)
	}
	return strings.Join(words, " ")
}

// shellWord returns word as the shell reads it back unchanged: bare when every
// character is an ordinary one, else in single quotes, where the only character
// that still needs care is the quote itself.
func shellWord(word string) string {
	ordinary := word != "" && !strings.ContainsFunc(word, func(r rune) bool {
		return !strings.ContainsRune(shellSafe, r)
	})
	if ordinary {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// cmuxAnswer is the part of new-surface's JSON answer that names the surface it
// created. cmux has spelled the key more than one way across versions, so every
// spelling read here is accepted and the first non-empty one is the surface.
type cmuxAnswer struct {
	SurfaceRef string `json:"surface_ref"`
	SurfaceID  string `json:"surface_id"`
	Ref        string `json:"ref"`
	ID         string `json:"id"`
	Surface    struct {
		Ref string `json:"ref"`
		ID  string `json:"id"`
	} `json:"surface"`
}

// cmuxSurface returns the surface new-surface created, failing rather than
// guessing when its answer names none.
func cmuxSurface(out []byte) (string, error) {
	var a cmuxAnswer
	if err := json.Unmarshal(out, &a); err != nil {
		return "", errors.New("cmux's answer is not the JSON new-surface reports a surface in")
	}
	for _, ref := range []string{a.SurfaceRef, a.SurfaceID, a.Surface.Ref, a.Surface.ID, a.Ref, a.ID} {
		if ref != "" {
			return ref, nil
		}
	}
	return "", errors.New("cmux's answer names no surface")
}

// runCmux runs one cmux command line with this process's environment and
// returns what it printed, leaving its complaints on the operator's terminal.
func runCmux(argv []string) ([]byte, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", argv[0], err)
	}
	return out, nil
}
