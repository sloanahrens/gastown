package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
)

// Memories live in two kv namespaces: gt.<type>.<key>, which `gt remember`
// writes, and the older memory.<key> that `bd remember` writes. Both corpora
// are live, so every reader has to accept both prefixes (gt-o51s).
const (
	memoryKeyPrefix       = "gt."
	memoryLegacyKeyPrefix = "memory."
)

// isMemoryKey reports whether a beads kv key holds a memory, in either the gt.*
// or the legacy memory.* namespace.
func isMemoryKey(key string) bool {
	return strings.HasPrefix(key, memoryKeyPrefix) || strings.HasPrefix(key, memoryLegacyKeyPrefix)
}

// validMemoryTypes are the recognized memory type categories.
// Typed memories are stored as gt.<type>.<key> in the kv store.
// Legacy untyped memories (gt.<key>) are treated as "general".
var validMemoryTypes = map[string]string{
	"feedback":  "Guidance or corrections from users — behavioral rules for future work",
	"project":   "Ongoing work context, goals, deadlines, decisions",
	"user":      "Info about the user's role, preferences, expertise",
	"reference": "Pointers to external resources (URLs, tools, dashboards)",
	"general":   "Uncategorized memories (default)",
}

// memoryTypeOrder defines the injection priority during gt prime.
// Feedback first (behavioral corrections), then user context, then the rest.
var memoryTypeOrder = []string{"feedback", "user", "project", "reference", "general"}

var rememberKey string
var rememberType string
var rememberWith []string

func init() {
	rememberCmd.Flags().StringVar(&rememberKey, "key", "", "Explicit key slug (default: auto-generated from content)")
	rememberCmd.Flags().StringVar(&rememberType, "type", "", "Memory type: feedback, project, user, reference (default: general)")
	rememberCmd.Flags().StringSliceVar(&rememberWith, "with", nil, "Co-authors for a jointly-established memory (e.g. --with gastown/witness)")
	rememberCmd.GroupID = GroupWork
	rootCmd.AddCommand(rememberCmd)
}

var rememberCmd = &cobra.Command{
	Use:   `remember "insight"`,
	Short: "Store a persistent memory",
	Long: `Store a persistent memory in the beads key-value store.

Memories persist across sessions, and gt prime renders the memory index only
for mayor and crew — the roles that carry context from one session into the
next. Every role stores memories the same way, but no other role sees them
injected at prime time; read them back with gt memories. This replaces
filesystem-based MEMORY.md with bead-backed storage.

The key is auto-generated from the content if not specified.
Use --key to provide an explicit slug for easy retrieval.

Memory types help organize memories and prioritize injection:
  feedback   Guidance or corrections from users
  project    Ongoing work context, goals, deadlines
  user       Info about the user's role and preferences
  reference  Pointers to external resources

A memory is stamped with the identity that actually ran this command (from
BD_ACTOR, falling back to GT_ROLE) — not free text you type, so the
attribution cannot drift from who really established it (gt-04h). For a
memory two or more agents established together, name the others with --with
instead of writing "X and I" into the body; the tag will otherwise carry only
your own identity even though the content reads as joint.

Examples:
  gt remember "Refinery uses worktree, cannot checkout main"
  gt remember --type feedback "Don't mock the database in integration tests"
  gt remember --type user --key senior-go-dev "User has 10 years Go experience"
  gt remember --key refinery-worktree "Refinery uses worktree, cannot checkout main"
  gt remember --with gastown/witness "Converged on the source-read citation requirement"`,
	Args: cobra.ExactArgs(1),
	RunE: runRemember,
}

func runRemember(cmd *cobra.Command, args []string) error {
	content := args[0]
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("memory content cannot be empty")
	}

	// Validate --type if provided
	memType := strings.ToLower(strings.TrimSpace(rememberType))
	if memType != "" {
		if _, ok := validMemoryTypes[memType]; !ok {
			return fmt.Errorf("invalid memory type %q — valid types: feedback, project, user, reference", memType)
		}
	}
	if memType == "" {
		memType = "general"
	}

	key := rememberKey
	if key == "" {
		key = autoKey(content)
	}

	// Sanitize key: lowercase, hyphens instead of spaces, strip dots
	key = sanitizeKey(key)

	fullKey := memoryKeyPrefix + memType + "." + key

	// Check if key already exists
	existing, _ := bdKvGet(fullKey)
	verb := "Stored"
	if existing != "" {
		verb = "Updated"
	}

	author := resolveMemoryAuthor()
	if len(rememberWith) == 0 {
		if role := detectUnattributedJointVoice(content); role != "" {
			style.PrintWarning("this reads as joint with %s (%q) but no --with was given — the stored attribution will name only %s. Re-run with --with %s/<name> to record joint authorship.",
				role, role+" and I", orElse(author, "no one"), role)
		}
	}

	stored := content + attributionSuffix(author, rememberWith)

	if err := bdKvSet(fullKey, stored); err != nil {
		return fmt.Errorf("storing memory: %w", err)
	}

	displayKey := key
	if memType != "general" {
		displayKey = memType + "/" + key
	}
	fmt.Printf("%s %s memory: %s\n", style.Success.Render("✓"), verb, style.Bold.Render(displayKey))
	return nil
}

// resolveMemoryAuthor returns the canonical actor identity for the current
// process: BD_ACTOR, the same identity gt done and gt dolt already trust,
// falling back to GT_ROLE. Returns "" for a human at a plain terminal
// (neither set) so the caller can skip stamping instead of fabricating an
// identity.
func resolveMemoryAuthor() string {
	if actor := strings.TrimSpace(os.Getenv("BD_ACTOR")); actor != "" {
		return actor
	}
	return strings.TrimSpace(os.Getenv("GT_ROLE"))
}

// attributionSuffix renders a machine-generated attribution line to append to
// a stored memory's content. Appended at the end, not the start, so it never
// displaces the first sentence memorySummary uses as the prime-time preview.
//
// This is deliberately not free text the caller can compose: it is built from
// the actor identity the process was invoked with (resolveMemoryAuthor) plus
// any --with co-authors, so a memory's tag cannot silently drift from who ran
// the command (gt-04h). Returns "" when author is empty, so an unattributable
// write is stored as-is rather than stamped with a blank tag.
func attributionSuffix(author string, coAuthors []string) string {
	if author == "" {
		return ""
	}
	who := author
	for _, c := range coAuthors {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		who += " + " + c
	}
	return fmt.Sprintf("\n\n[recorded by: %s @ %s]", who, time.Now().UTC().Format("2006-01-02"))
}

// jointVoiceRoles are the role names whose bare mention immediately before
// "and I" signals the writer is narrating a joint action without recording
// the second party via --with. Matches the exact failure gt-04h documents: a
// memory tagged to one agent whose body reads "refinery and I ran this
// independently and converged."
var jointVoiceRoles = []string{"mayor", "deacon", "witness", "refinery", "crew", "polecat"}

var jointVoicePattern = regexp.MustCompile(`(?i)\b(` + strings.Join(jointVoiceRoles, "|") + `)\b\s+and\s+i\b`)

// detectUnattributedJointVoice reports the first role name found written in
// first-person-joint voice ("<role> and I") in content, or "" if none is
// found. A cheap, narrow signal deliberately modeled on gt-3t2's rejected
// title guard: unlike that guard, this one matches a fixed, closed vocabulary
// of role names rather than trying to infer intent, which is what kept that
// guard's precision at ~25%.
func detectUnattributedJointVoice(content string) string {
	m := jointVoicePattern.FindStringSubmatch(content)
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1])
}

// orElse returns s if non-empty, otherwise fallback.
func orElse(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// parseMemoryKey extracts the type and short key from a full kv key.
// Handles both typed keys (gt.<type>.<key> or memory.<type>.<key>) and legacy
// untyped keys (gt.<key> or memory.<key>).
func parseMemoryKey(kvKey string) (memType, shortKey string) {
	prefix := memoryLegacyKeyPrefix
	if strings.HasPrefix(kvKey, memoryKeyPrefix) {
		prefix = memoryKeyPrefix
	}

	rest := strings.TrimPrefix(kvKey, prefix)
	if rest == "" {
		return "general", ""
	}

	// Check if first segment is a known type
	if dotIdx := strings.Index(rest, "."); dotIdx > 0 {
		candidate := rest[:dotIdx]
		if _, ok := validMemoryTypes[candidate]; ok {
			return candidate, rest[dotIdx+1:]
		}
	}

	// Legacy untyped memory
	return "general", rest
}

// autoKey generates a short key from content using first few meaningful words.
func autoKey(content string) string {
	// Take first ~5 words, lowercase, hyphenate
	words := strings.Fields(strings.ToLower(content))
	if len(words) > 5 {
		words = words[:5]
	}

	// Strip non-alphanumeric chars from each word
	var clean []string
	for _, w := range words {
		w = strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				return r
			}
			return -1
		}, w)
		if w != "" {
			clean = append(clean, w)
		}
	}

	if len(clean) == 0 {
		// Fallback to hash
		h := sha256.Sum256([]byte(content))
		return hex.EncodeToString(h[:4])
	}

	slug := strings.Join(clean, "-")
	// Cap length
	if len(slug) > 40 {
		slug = slug[:40]
	}
	return slug
}

// sanitizeKey normalizes a key slug.
func sanitizeKey(key string) string {
	key = strings.ToLower(key)
	key = strings.ReplaceAll(key, " ", "-")
	key = strings.ReplaceAll(key, ".", "-")

	// Strip anything that isn't alphanumeric or hyphen
	key = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return -1
	}, key)

	// Collapse multiple hyphens
	for strings.Contains(key, "--") {
		key = strings.ReplaceAll(key, "--", "-")
	}
	key = strings.Trim(key, "-")

	return key
}

// bdKvSet calls bd kv set <key> <value>.
func bdKvSet(key, value string) error {
	cmd := beads.CommandWithEnv("", nil, "kv", "set", key, value)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// bdKvGet calls bd kv get <key> and returns the value.
func bdKvGet(key string) (string, error) {
	cmd := beads.CommandWithEnv("", nil, "kv", "get", key)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// bdKvClear calls bd kv clear <key>.
func bdKvClear(key string) error {
	cmd := beads.CommandWithEnv("", nil, "kv", "clear", key)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// parseBdKvListJSON parses bd kv list --json output into displayable string values.
func parseBdKvListJSON(data []byte) (map[string]string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing kv list: %w", err)
	}

	kvs := make(map[string]string, len(raw))
	for k, v := range raw {
		var s *string
		if err := json.Unmarshal(v, &s); err == nil {
			if s != nil {
				kvs[k] = *s
			}
			continue
		}

		if !isMemoryKey(k) {
			continue
		}

		// Keep non-string memory values visible without promoting bd metadata.
		var compact bytes.Buffer
		if err := json.Compact(&compact, v); err != nil {
			continue
		}
		kvs[k] = compact.String()
	}
	return kvs, nil
}

// bdKvListJSON calls bd kv list --json and returns the parsed string values.
func bdKvListJSON() (map[string]string, error) {
	cmd := beads.CommandWithEnv("", nil, "kv", "list", "--json")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	return parseBdKvListJSON(out)
}
