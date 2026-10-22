package polecat

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/lock"
	"github.com/steveyegge/gastown/internal/rig"
)

const (
	// DefaultPoolSize is the number of name slots in the pool.
	// Names are allocated when a polecat is first created. Polecat identity persists,
	// while clean completion retires the live session and cleanup owns remaining state.
	DefaultPoolSize = 50

	// DefaultTheme is the default theme for new rigs.
	DefaultTheme = "mad-max"

	// MaxThemeNames is the maximum number of names allowed in a custom theme file.
	// Prevents accidental theme bloat from large --from-file inputs.
	MaxThemeNames = 2000
)

// ReservedInfraAgentNames contains names reserved for infrastructure agents.
// These names must never be allocated to polecats.
var ReservedInfraAgentNames = map[string]bool{
	"witness":  true,
	"deacon":   true,
	"refinery": true,
	"crew":     true,
	"polecats": true,
}

// Built-in themes with themed polecat names.
var BuiltinThemes = map[string][]string{
	"mad-max": {
		"furiosa", "nux", "slit", "rictus", "dementus",
		"capable", "toast", "dag", "cheedo", "valkyrie",
		"keeper", "morsov", "ace", "warboy", "imperator",
		"organic", "coma", "splendid", "angharad", "max",
		"immortan", "bullet", "toecutter", "goose", "nightrider",
		"glory", "scrotus", "chumbucket", "corpus", "dinki",
		"prime", "vuvalini", "rockryder", "wretched", "buzzard",
		"gastown", "bullet-farmer", "citadel", "wasteland", "fury",
		"road-warrior", "interceptor", "blackfinger", "wraith", "witness",
		"chrome", "shiny", "mediocre", "guzzoline", "aqua-cola",
	},
	"minerals": {
		"obsidian", "quartz", "jasper", "onyx", "opal",
		"topaz", "garnet", "ruby", "amber", "jade",
		"pearl", "flint", "granite", "basalt", "marble",
		"shale", "slate", "pyrite", "mica", "agate",
		"malachite", "turquoise", "lapis", "emerald", "sapphire",
		"diamond", "amethyst", "citrine", "zircon", "peridot",
		"coral", "jet", "moonstone", "sunstone", "bloodstone",
		"rhodonite", "sodalite", "hematite", "magnetite", "calcite",
		"fluorite", "selenite", "kyanite", "labradorite", "amazonite",
		"chalcedony", "carnelian", "aventurine", "chrysoprase", "heliodor",
	},
	"wasteland": {
		"rust", "chrome", "nitro", "guzzle", "witness",
		"shiny", "fury", "thunder", "dust", "scavenger",
		"radrat", "ghoul", "mutant", "raider", "vault",
		"pipboy", "nuka", "brahmin", "deathclaw", "mirelurk",
		"synth", "institute", "enclave", "brotherhood", "minuteman",
		"railroad", "atom", "crater", "foundation", "refuge",
		"settler", "wanderer", "courier", "lone", "chosen",
		"tribal", "khan", "legion", "ncr", "ranger",
		"overseer", "sentinel", "paladin", "scribe", "initiate",
		"elder", "lancer", "knight", "squire", "proctor",
	},
}

// NamePool manages a bounded pool of reusable polecat names.
// Names are allocated once per polecat and persist across assignments in the
// persistent polecat model (gt-4ac). A name slot is only freed when a polecat
// is explicitly nuked.
//
// Names are drawn from a themed pool (mad-max by default).
// When the pool is exhausted, overflow names use N format (just numbers).
// The rig prefix is added by SessionName to create session names like "gt-<rig>-N".
type NamePool struct {
	mu sync.RWMutex

	// RigName is the rig this pool belongs to.
	RigName string `json:"rig_name"`

	// Theme is the current theme name (e.g., "mad-max", "minerals", or a custom theme).
	Theme string `json:"theme"`

	// CustomNames allows overriding the built-in theme names.
	CustomNames []string `json:"custom_names,omitempty"`

	// InUse tracks which pool names are currently in use.
	// Key is the name itself, value is true if in use.
	// ZFC: This is transient state derived from filesystem via Reconcile().
	// Never persist - always discover from existing polecat directories.
	InUse map[string]bool `json:"-"`

	// OverflowNext is the next overflow sequence number.
	// Starts at MaxSize+1 and increments.
	OverflowNext int `json:"overflow_next"`

	// MaxSize is the maximum number of themed names before overflow.
	MaxSize int `json:"max_size"`

	// Bindings maps a name to the model its slot was first allocated to. The
	// binding outlives the polecat: a freed name comes back only to a seat of
	// its own model, which is what keeps a rig's names meaningful per model
	// (gt-m9dty). A name with no binding either predates the rule or has not
	// been allocated with a model yet. While it is in use it stays unbound,
	// because reconcile never invents a binding from a last-slung agent bead;
	// once released it is anyone's, and the first allocation that names a
	// model binds it for good.
	Bindings map[string]string `json:"bindings,omitempty"`

	// SeatNames maps a normalized agent name to the names its seat lists.
	// Configuration from polecat_pool.seats, never persisted (SetSeatNames).
	SeatNames map[string][]string `json:"-"`

	// stateFile is the path to persist pool state.
	stateFile string

	// townRoot is the town root directory, used to resolve custom theme files.
	townRoot string
}

// NewNamePool creates a new name pool for a rig.
func NewNamePool(rigPath, rigName string) *NamePool {
	return &NamePool{
		RigName:      rigName,
		Theme:        ThemeForRig(rigName),
		InUse:        make(map[string]bool),
		Bindings:     make(map[string]string),
		OverflowNext: DefaultPoolSize + 1,
		MaxSize:      DefaultPoolSize,
		stateFile:    filepath.Join(rigPath, ".runtime", "namepool-state.json"),
	}
}

// NewNamePoolWithConfig creates a name pool with specific configuration.
func NewNamePoolWithConfig(rigPath, rigName, theme string, customNames []string, maxSize int) *NamePool {
	if theme == "" {
		theme = DefaultTheme
	}
	if maxSize <= 0 {
		maxSize = DefaultPoolSize
	}

	return &NamePool{
		RigName:      rigName,
		Theme:        theme,
		CustomNames:  customNames,
		InUse:        make(map[string]bool),
		Bindings:     make(map[string]string),
		OverflowNext: maxSize + 1,
		MaxSize:      maxSize,
		stateFile:    filepath.Join(rigPath, ".runtime", "namepool-state.json"),
	}
}

// SetTownRoot sets the town root for custom theme resolution.
func (p *NamePool) SetTownRoot(townRoot string) {
	p.townRoot = townRoot
}

// getNames returns the list of names to use for the pool.
// Reserved infrastructure agent names are filtered out.
func (p *NamePool) getNames() []string {
	var names []string

	// Custom names take precedence
	if len(p.CustomNames) > 0 {
		names = p.CustomNames
	} else if themeNames, ok := BuiltinThemes[p.Theme]; ok {
		// Look up built-in theme
		names = themeNames
	} else if p.townRoot != "" {
		// Try resolving as a custom theme file
		if resolved, err := ResolveThemeNames(p.townRoot, p.Theme); err == nil {
			names = resolved
		} else {
			fmt.Fprintf(os.Stderr, "Warning: namepool theme %q not found (not built-in, no custom theme file); using default\n", p.Theme)
			names = BuiltinThemes[DefaultTheme]
		}
	} else {
		// Fall back to default theme
		names = BuiltinThemes[DefaultTheme]
	}

	// Filter out reserved infrastructure agent names
	return filterReservedNames(names)
}

// filterReservedNames removes reserved infrastructure agent names from a name list.
func filterReservedNames(names []string) []string {
	filtered := make([]string, 0, len(names))
	for _, name := range names {
		if !ReservedInfraAgentNames[name] {
			filtered = append(filtered, name)
		}
	}
	return filtered
}

// Load loads the pool state from disk.
func (p *NamePool) Load() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	data, err := os.ReadFile(p.stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			// Initialize with empty state
			p.InUse = make(map[string]bool)
			p.Bindings = make(map[string]string)
			p.OverflowNext = p.MaxSize + 1
			return nil
		}
		return err
	}

	// Load only runtime state - Theme and CustomNames come from settings/config.json.
	// ZFC: InUse is NEVER loaded from disk - it's transient state derived
	// from filesystem via Reconcile(). Always start with empty map.
	var loaded namePoolState
	if err := json.Unmarshal(data, &loaded); err != nil {
		return err
	}

	p.InUse = make(map[string]bool)
	p.Bindings = loaded.Bindings
	if p.Bindings == nil {
		p.Bindings = make(map[string]string)
	}

	p.OverflowNext = loaded.OverflowNext
	if p.OverflowNext < p.MaxSize+1 {
		p.OverflowNext = p.MaxSize + 1
	}
	if loaded.MaxSize > 0 {
		p.MaxSize = loaded.MaxSize
	}

	return nil
}

// namePoolState is the subset of NamePool that is persisted to the state file.
// Only runtime state is saved, not configuration (Theme, CustomNames come from settings).
type namePoolState struct {
	RigName      string            `json:"rig_name"`
	OverflowNext int               `json:"overflow_next"`
	MaxSize      int               `json:"max_size"`
	Bindings     map[string]string `json:"bindings,omitempty"`
}

// Save persists the pool state to disk using atomic write.
// Only runtime state (OverflowNext, MaxSize, model bindings) is saved -
// configuration like Theme and CustomNames come from settings/config.json and
// are not persisted here.
func (p *NamePool) Save() error {
	p.mu.RLock()
	defer p.mu.RUnlock()

	dir := filepath.Dir(p.stateFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	// Only save runtime state, not configuration
	state := namePoolState{
		RigName:      p.RigName,
		OverflowNext: p.OverflowNext,
		MaxSize:      p.MaxSize,
		Bindings:     p.Bindings,
	}

	return atomicfile.WriteJSON(p.stateFile, state)
}

// Allocate returns an unbound name from the pool: one no model has claimed.
// Seats whose model is known allocate with AllocateFor so the name is bound
// to that model.
func (p *NamePool) Allocate() (string, error) {
	return p.AllocateFor("")
}

// AllocateFor returns a name for a seat on model, binding the name to it.
// A name bound to another model is never handed out, and an unbound name is
// bound by being allocated. Names are taken in theme order; overflow names
// back the pool when no eligible themed name is free.
func (p *NamePool) AllocateFor(model string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	model = normalizeModel(model)

	// A seat with a names list draws from it first. A seat with none, or whose
	// list is spent, falls through to the themed default below.
	for _, name := range p.SeatNames[model] {
		if p.InUse[name] || !p.eligibleForLocked(name, model) {
			continue
		}
		p.InUse[name] = true
		p.Bindings[name] = model
		return name, nil
	}

	names := p.getNames()

	// Try to find first available name from the theme
	for i := 0; i < len(names) && i < p.MaxSize; i++ {
		name := names[i]
		if p.InUse[name] || !p.eligibleForLocked(name, model) {
			continue
		}
		p.InUse[name] = true
		if model != "" {
			p.Bindings[name] = model
		}
		return name, nil
	}

	// Pool exhausted, use overflow naming. An overflow name is drawn once and
	// never handed out again (Release is a no-op for it), so it is not bound:
	// a binding could only grow the state file, never protect the name
	// (gt-m9dty).
	name := p.formatOverflowName(p.OverflowNext)
	p.OverflowNext++
	return name, nil
}

// eligibleForLocked reports whether the seat on model may take name. A bound
// name belongs to its model alone, and that binding outranks a seat list: a
// name bound before the list named it stays with the model it was bound to. An
// unbound name is anyone's unless another seat lists it.
func (p *NamePool) eligibleForLocked(name, model string) bool {
	if bound := p.Bindings[name]; bound != "" {
		return model != "" && sameModel(bound, model)
	}
	return !p.reservedForOtherLocked(name, model)
}

// reservedForOtherLocked reports whether a seat other than model lists name.
func (p *NamePool) reservedForOtherLocked(name, model string) bool {
	for agent, names := range p.SeatNames {
		if agent == model {
			continue
		}
		for _, n := range names {
			if n == name {
				return true
			}
		}
	}
	return false
}

// SetSeatNames sets the names each seat lists, keyed by agent. A listed name
// is allocated only to its seat's agent. It is configuration, read from the
// seat map and not persisted; an agent absent from the map draws from the
// themed default.
func (p *NamePool) SetSeatNames(seatNames map[string][]string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.SeatNames = make(map[string][]string, len(seatNames))
	for agent, names := range seatNames {
		if agent = normalizeModel(agent); agent != "" && len(names) > 0 {
			p.SeatNames[agent] = append([]string(nil), names...)
		}
	}
}

// Bind records model as the model of name. It reports an error when the name
// is already bound to another model: a name keeps the model its slot was
// first allocated to.
func (p *NamePool) Bind(name, model string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bindLocked(name, model)
}

func (p *NamePool) bindLocked(name, model string) error {
	model = normalizeModel(model)
	if name == "" || model == "" {
		return nil
	}
	if bound := p.Bindings[name]; bound != "" && !sameModel(bound, model) {
		return fmt.Errorf("name %q is bound to %s, not %s", name, bound, model)
	}
	if p.Bindings[name] == "" && p.reservedForOtherLocked(name, model) {
		return fmt.Errorf("name %q is listed by another seat, not %s", name, model)
	}
	p.Bindings[name] = model
	return nil
}

// ModelOf returns the model name is bound to, or "" when it has none.
func (p *NamePool) ModelOf(name string) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.Bindings[name]
}

// normalizeModel is the form bindings are stored and compared in, so a model
// named with stray case or space cannot masquerade as a second model.
func normalizeModel(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

// sameModel reports whether a and b name the same model. Case and surrounding
// space are ignored, matching the form bindings are stored in.
func sameModel(a, b string) bool {
	return normalizeModel(a) == normalizeModel(b)
}

// Release returns a name slot to the available pool.
// Called when a polecat is nuked - the name becomes available for new polecats.
// NOTE: This releases the NAME, not the polecat. The polecat is gone (nuked).
// The model binding is kept: the freed name remains its model's.
// For overflow names, this is a no-op (they are not reusable).
func (p *NamePool) Release(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Check if it's a themed name
	if p.isThemedName(name) {
		delete(p.InUse, name)
	}
	// Overflow names are not reusable, so we don't track them
}

// isThemedName checks if a name is in the theme pool or listed by a seat.
func (p *NamePool) isThemedName(name string) bool {
	names := p.getNames()
	for _, n := range names {
		if n == name {
			return true
		}
	}
	for _, seatNames := range p.SeatNames {
		for _, n := range seatNames {
			if n == name {
				return true
			}
		}
	}
	return false
}

// IsPoolName returns true if the name is a pool name (themed or numbered).
func (p *NamePool) IsPoolName(name string) bool {
	return p.isThemedName(name)
}

// ActiveCount returns the number of names currently in use from the pool.
func (p *NamePool) ActiveCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.InUse)
}

// ActiveNames returns a sorted list of names currently in use from the pool.
func (p *NamePool) ActiveNames() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	var names []string
	for name := range p.InUse {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// IsInUse reports whether name is currently marked in use.
func (p *NamePool) IsInUse(name string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.InUse[name]
}

// MarkInUse marks a name as in use (for reconciling with existing polecats).
func (p *NamePool) MarkInUse(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.isThemedName(name) {
		p.InUse[name] = true
	}
}

// Reconcile updates the pool state based on existing polecat directories.
// This should be called on startup to sync pool state with reality.
func (p *NamePool) Reconcile(existingPolecats []string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Clear current state
	p.InUse = make(map[string]bool)

	// Mark all existing polecats as in use
	for _, name := range existingPolecats {
		if p.isThemedName(name) {
			p.InUse[name] = true
			continue
		}
		if seq, err := strconv.Atoi(name); err == nil && seq >= p.OverflowNext {
			p.OverflowNext = seq + 1
		}
	}
}

// formatOverflowName formats an overflow sequence number as a name.
// Returns just the number (e.g., "51") since SessionName will add the rig prefix.
// This prevents double-prefix bugs like "gt-gastown_manager-gastown_manager-51".
func (p *NamePool) formatOverflowName(seq int) string {
	return fmt.Sprintf("%d", seq)
}

// GetTheme returns the current theme name.
func (p *NamePool) GetTheme() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.Theme
}

// SetTheme sets the theme and resets the pool.
// Existing in-use names are preserved if they exist in the new theme.
// Supports both built-in themes and custom theme files in settings/themes/.
func (p *NamePool) SetTheme(theme string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	var newNames []string
	if names, ok := BuiltinThemes[theme]; ok {
		newNames = names
	} else if p.townRoot != "" {
		resolved, err := ResolveThemeNames(p.townRoot, theme)
		if err != nil {
			return err
		}
		newNames = resolved
	} else {
		return fmt.Errorf("unknown theme: %s (built-in themes: %s)", theme, strings.Join(ListThemes(), ", "))
	}

	// Preserve names that exist in both themes
	newInUse := make(map[string]bool)
	for name := range p.InUse {
		for _, n := range newNames {
			if n == name {
				newInUse[name] = true
				break
			}
		}
	}

	p.Theme = theme
	p.InUse = newInUse
	p.CustomNames = nil
	return nil
}

// ListThemes returns the list of available built-in themes.
func ListThemes() []string {
	themes := make([]string, 0, len(BuiltinThemes))
	for theme := range BuiltinThemes {
		themes = append(themes, theme)
	}
	sort.Strings(themes)
	return themes
}

// ThemeForRig returns a deterministic theme for a rig based on its name.
// This provides variety across rigs without requiring manual configuration.
func ThemeForRig(rigName string) string {
	themes := ListThemes()
	if len(themes) == 0 {
		return DefaultTheme
	}
	// Hash using prime multiplier for better distribution
	var hash uint32
	for _, b := range []byte(rigName) {
		hash = hash*31 + uint32(b)
	}
	return themes[hash%uint32(len(themes))] //nolint:gosec // len(themes) is small constant
}

// ThemeForRigAvoiding picks a theme for rigName that is not already in usedThemes.
// This ensures polecat names are unique across rigs by giving each rig a different theme.
// If all built-in themes are taken, falls back to the hash-based ThemeForRig result.
func ThemeForRigAvoiding(rigName string, usedThemes []string) string {
	themes := ListThemes()
	if len(themes) == 0 {
		return DefaultTheme
	}

	used := make(map[string]bool, len(usedThemes))
	for _, t := range usedThemes {
		used[t] = true
	}

	// Try to find an unused theme
	var available []string
	for _, t := range themes {
		if !used[t] {
			available = append(available, t)
		}
	}

	if len(available) == 0 {
		// All built-in themes taken — fall back to hash-based selection
		return ThemeForRig(rigName)
	}

	if len(available) == 1 {
		return available[0]
	}

	// Deterministic pick from available themes using rig name hash
	var hash uint32
	for _, b := range []byte(rigName) {
		hash = hash*31 + uint32(b)
	}
	return available[hash%uint32(len(available))] //nolint:gosec // len(available) is small
}

// GetThemeNames returns the names in a specific built-in theme.
// For custom themes, use ResolveThemeNames instead.
func GetThemeNames(theme string) ([]string, error) {
	if names, ok := BuiltinThemes[theme]; ok {
		return names, nil
	}
	return nil, fmt.Errorf("unknown theme: %s", theme)
}

// ThemeInfo describes a theme (built-in or custom).
type ThemeInfo struct {
	Name     string
	IsCustom bool
	Count    int
}

// ListAllThemes returns a sorted list of all built-in and custom themes.
func ListAllThemes(townRoot string) []ThemeInfo {
	var themes []ThemeInfo

	// Add built-in themes
	for name, names := range BuiltinThemes {
		themes = append(themes, ThemeInfo{
			Name:  name,
			Count: len(filterReservedNames(names)),
		})
	}

	// Add custom themes from settings/themes/
	if townRoot != "" {
		themesDir := filepath.Join(townRoot, "settings", "themes")
		entries, err := os.ReadDir(themesDir)
		if err == nil {
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
					continue
				}
				name := strings.TrimSuffix(e.Name(), ".txt")
				// Skip if it collides with a built-in
				if IsBuiltinTheme(name) {
					continue
				}
				names, err := ParseThemeFile(filepath.Join(themesDir, e.Name()))
				if err != nil {
					continue
				}
				themes = append(themes, ThemeInfo{
					Name:     name,
					IsCustom: true,
					Count:    len(names),
				})
			}
		}
	}

	sort.Slice(themes, func(i, j int) bool {
		return themes[i].Name < themes[j].Name
	})
	return themes
}

// IsBuiltinTheme returns true if the theme name matches a built-in theme.
func IsBuiltinTheme(theme string) bool {
	_, ok := BuiltinThemes[theme]
	return ok
}

// validPoolNameRe matches lowercase alphanumeric names with hyphens.
var validPoolNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ValidatePoolName validates a polecat name for use in a theme.
// Names must be >3 chars, lowercase alphanumeric with hyphens, and not reserved.
func ValidatePoolName(name string) error {
	if len(name) <= 3 {
		return fmt.Errorf("name %q too short (must be >3 characters)", name)
	}
	if !validPoolNameRe.MatchString(name) {
		return fmt.Errorf("name %q invalid (must be lowercase alphanumeric with hyphens, starting with a letter)", name)
	}
	if ReservedInfraAgentNames[name] {
		return fmt.Errorf("name %q is reserved for infrastructure agents", name)
	}
	return nil
}

// ParseThemeFile reads a custom theme file (one name per line).
// Lines starting with # are comments. Blank lines are skipped.
// Names are lowercased and deduplicated. Names <=3 chars are rejected.
// Reserved names are filtered with a stderr warning.
func ParseThemeFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	seen := make(map[string]bool)
	var names []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := strings.ToLower(line)
		if len(name) <= 3 {
			fmt.Fprintf(os.Stderr, "warning: skipping name %q (must be >3 characters)\n", name)
			continue
		}
		if !validPoolNameRe.MatchString(name) {
			fmt.Fprintf(os.Stderr, "warning: skipping invalid name %q (must be lowercase alphanumeric with hyphens)\n", name)
			continue
		}
		if ReservedInfraAgentNames[name] {
			fmt.Fprintf(os.Stderr, "warning: skipping reserved name %q\n", name)
			continue
		}
		if seen[name] {
			continue
		}
		if len(names) >= MaxThemeNames {
			return nil, fmt.Errorf("theme file %s exceeds maximum of %d names", filepath.Base(path), MaxThemeNames)
		}
		seen[name] = true
		names = append(names, name)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no valid names in theme file %s", filepath.Base(path))
	}
	return names, nil
}

// ResolveThemeNames resolves a theme name to its list of names.
// Checks built-in themes first, then looks for a custom theme file
// at <townRoot>/settings/themes/<theme>.txt.
func ResolveThemeNames(townRoot, theme string) ([]string, error) {
	if names, ok := BuiltinThemes[theme]; ok {
		return names, nil
	}
	path := filepath.Join(townRoot, "settings", "themes", theme+".txt")
	return ParseThemeFile(path)
}

// namepoolNamesWin reports whether a rig settings' namepool block draws its
// polecat names from its explicit list rather than its style: the list
// overrides the style (config.NamepoolConfig), so a style set beside it is
// ignored. newManager and EffectiveTheme both ask this, so the pool and the
// theme the dashboard reports cannot pick different sources.
func namepoolNamesWin(np *config.NamepoolConfig) bool {
	return len(np.Names) > 0
}

// EffectiveTheme returns the name theme a rig's polecats are drawn from: an
// explicit namepool name list (which overrides the rig settings' namepool
// style), then that style, then the rig
// config's polecat_names, and ThemeForRig's hash when none of the three is
// set; a rig drawing from an explicit list has no theme to name and reports "".
//
// The dashboard labels a rig with this, so the precedence sits beside the pool
// the manager builds (newManager) rather than in a second copy that can drift.
func EffectiveTheme(rigPath, rigName string) string {
	settings, err := config.LoadRigSettings(filepath.Join(rigPath, "settings", "config.json"))
	if err == nil && settings.Namepool != nil {
		if namepoolNamesWin(settings.Namepool) {
			return ""
		}
		if settings.Namepool.Style != "" {
			return settings.Namepool.Style
		}
		// An empty namepool block is the pool's own default theme, not the
		// rig-name hash (NewNamePoolWithConfig).
		return DefaultTheme
	}
	// A rig config that does not decode reads as unconfigured, the fallback the
	// pool takes too (newManager warns once per rig; a reader polling it would
	// only repeat that warning).
	rigCfg, _ := rig.LoadRigConfigIfPresent(rigPath)
	if rigCfg != nil && len(rigCfg.PolecatNames) > 0 {
		return ""
	}
	return ThemeForRig(rigName)
}

// SaveCustomTheme writes a custom theme file to settings/themes/<name>.txt.
// Uses file locking (flock) to prevent concurrent writes from losing data.
func SaveCustomTheme(townRoot, name string, names []string) error {
	if IsBuiltinTheme(name) {
		return fmt.Errorf("cannot create custom theme %q: conflicts with built-in theme", name)
	}
	themesDir := filepath.Join(townRoot, "settings", "themes")
	if err := os.MkdirAll(themesDir, 0755); err != nil {
		return fmt.Errorf("creating themes directory: %w", err)
	}

	themePath := filepath.Join(themesDir, name+".txt")
	lockPath := themePath + ".lock"

	// Acquire advisory file lock to prevent concurrent writes
	unlock, err := lock.FlockAcquire(lockPath)
	if err != nil {
		return fmt.Errorf("acquiring theme file lock: %w", err)
	}
	defer unlock()

	var sb strings.Builder
	sb.WriteString("# Custom theme: " + name + "\n")
	for _, n := range names {
		sb.WriteString(n + "\n")
	}
	return os.WriteFile(themePath, []byte(sb.String()), 0644)
}

// AppendToCustomTheme atomically adds a name to a custom theme file.
// Holds a file lock across the read-check-write cycle to prevent TOCTOU races.
// Returns (alreadyExists, error).
func AppendToCustomTheme(townRoot, theme, name string) (bool, error) {
	themesDir := filepath.Join(townRoot, "settings", "themes")
	themePath := filepath.Join(themesDir, theme+".txt")
	lockPath := themePath + ".lock"

	// Acquire lock for the entire read-modify-write
	unlock, err := lock.FlockAcquire(lockPath)
	if err != nil {
		return false, fmt.Errorf("acquiring theme file lock: %w", err)
	}
	defer unlock()

	// Read current names
	existing, err := ParseThemeFile(themePath)
	if err != nil {
		return false, fmt.Errorf("reading theme %q: %w", theme, err)
	}

	// Check for duplicate
	for _, n := range existing {
		if n == name {
			return true, nil
		}
	}

	// Append and write
	existing = append(existing, name)
	var sb strings.Builder
	sb.WriteString("# Custom theme: " + theme + "\n")
	for _, n := range existing {
		sb.WriteString(n + "\n")
	}
	if err := os.WriteFile(themePath, []byte(sb.String()), 0644); err != nil {
		return false, err
	}
	return false, nil
}

// DeleteCustomTheme removes a custom theme file.
func DeleteCustomTheme(townRoot, name string) error {
	if IsBuiltinTheme(name) {
		return fmt.Errorf("cannot delete built-in theme %q", name)
	}
	path := filepath.Join(townRoot, "settings", "themes", name+".txt")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("custom theme %q not found", name)
	}
	return os.Remove(path)
}

// FindRigsUsingTheme checks all rigs in a town and returns the names of any
// rigs whose namepool style matches the given theme name.
func FindRigsUsingTheme(townRoot, theme string) []string {
	registry, err := config.LoadRigsConfig(filepath.Join(townRoot, "mayor", "rigs.json"))
	if err != nil {
		return nil
	}

	var using []string
	for rigName := range registry.Rigs {
		// No settings file, or one the kernel rejects, is a rig that reports
		// no style: skip it rather than count it as using the theme.
		settings, err := config.LoadRigSettings(config.RigSettingsPath(filepath.Join(townRoot, rigName)))
		if err != nil {
			continue
		}
		if settings.Namepool != nil && settings.Namepool.Style == theme {
			using = append(using, rigName)
		}
	}
	sort.Strings(using)
	return using
}

// AddCustomName adds a custom name to the pool.
func (p *NamePool) AddCustomName(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Check if already in custom names
	for _, n := range p.CustomNames {
		if n == name {
			return
		}
	}
	p.CustomNames = append(p.CustomNames, name)
}

// Reset clears the pool state, releasing all names and their model bindings.
func (p *NamePool) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.InUse = make(map[string]bool)
	p.Bindings = make(map[string]string)
	p.OverflowNext = p.MaxSize + 1
}
