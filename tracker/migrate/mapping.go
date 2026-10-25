package migrate

import (
	"encoding/json"
	"slices"
	"strings"
)

// depTarget is the first non-empty of the three target columns, in the order
// issue, wisp, external. The external column holds a bare id of another rig,
// which stays a bare id: it resolves on its own once that rig is imported.
func depTarget(issue, wisp, external string) string {
	for _, s := range []string{issue, wisp, external} {
		if s != "" {
			return s
		}
	}
	return ""
}

// parentsOf maps each child to its parent from the parent-child links.
func parentsOf(deps []DepRow) map[string]string {
	out := map[string]string{}
	for _, d := range deps {
		if d.Type == "parent-child" && d.DependsOn != "" {
			out[d.IssueID] = d.DependsOn
		}
	}
	return out
}

// splitConfig separates memories (keys under "kv.") from rig configuration.
func splitConfig(all map[string]string) (cfg, kv map[string]string) {
	cfg, kv = map[string]string{}, map[string]string{}
	for k, v := range all {
		if strings.HasPrefix(k, "kv.") {
			kv[k] = v
		} else {
			cfg[k] = v
		}
	}
	return cfg, kv
}

// rigsOf returns the sorted, de-duplicated rig prefixes of ids; an id with no
// prefix is skipped.
func rigsOf(ids []string) []string {
	seen := map[string]bool{}
	for _, id := range ids {
		if rig, _, ok := strings.Cut(id, "-"); ok && rig != "" {
			seen[rig] = true
		}
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	slices.Sort(out)
	return out
}

// legacyMetadata is the JSON stored in issues.metadata: {"legacy": {...}} with
// only the columns that hold a value. Zero values (empty string, false, 0) are
// dropped so a row carries nothing it never had.
func legacyMetadata(cols map[string]any) []byte {
	kept := map[string]any{}
	for k, v := range cols {
		switch x := v.(type) {
		case nil:
		case string:
			if x != "" {
				kept[k] = x
			}
		case bool:
			if x {
				kept[k] = x
			}
		case int64:
			if x != 0 {
				kept[k] = x
			}
		case float64:
			if x != 0 {
				kept[k] = x
			}
		default:
			kept[k] = x
		}
	}
	if len(kept) == 0 {
		return []byte("{}")
	}
	b, _ := json.Marshal(map[string]any{"legacy": kept}) // plain maps; cannot fail
	return b
}
