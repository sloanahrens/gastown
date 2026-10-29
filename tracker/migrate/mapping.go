package migrate

import (
	"encoding/json"
	"fmt"
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

// dottedParent is the parent a dotted child id names: X for X.N, the part
// before its last dot. It reports false for an id with no dot, nothing before
// the dot, or nothing after it.
func dottedParent(id string) (string, bool) {
	i := strings.LastIndex(id, ".")
	if i <= 0 || i == len(id)-1 {
		return "", false
	}
	return id[:i], true
}

// repairedDeps returns the dependency rows to import and the parent-child links
// it synthesizes. A dotted id X.N whose prefix X is an issue of the dataset
// gets one parent-child row to X when it has no dependency row at all: bd lists
// such a child under X, but the tracker's children-of reads only the dependency
// rows, so the two would disagree. An id that already has a row keeps exactly
// its own rows, so no row is ever replaced. A wisp child is repaired only when
// its parent is a wisp too; the data says which rows are wisps, so no wisp is
// left unjudged.
// A non-wisp child is repaired whatever its parent is, wisp or not.
func repairedDeps(ds *Dataset) (deps, synthesized []DepRow) {
	isWisp := map[string]bool{}
	for _, is := range ds.Issues {
		isWisp[is.ID] = is.Wisp
	}
	hasRow := map[string]bool{}
	for _, d := range ds.Deps {
		if d.DependsOn != "" {
			hasRow[d.IssueID] = true
		}
	}
	for _, is := range ds.Issues {
		parent, ok := dottedParent(is.ID)
		if !ok || hasRow[is.ID] {
			continue
		}
		parentWisp, known := isWisp[parent]
		if !known || (is.Wisp && !parentWisp) {
			continue
		}
		synthesized = append(synthesized, DepRow{IssueID: is.ID, DependsOn: parent, Type: "parent-child"})
	}
	return append(slices.Clone(ds.Deps), synthesized...), synthesized
}

// linkKey identifies a dependency row by its issue, its target and its type.
func linkKey(d DepRow) string { return d.IssueID + "\x1f" + d.DependsOn + "\x1f" + d.Type }

// repairProblems checks deps, the dependency rows an import writes, against the
// repair rule as repairedDeps states it. It reports each synthesized row the
// rule does not give, and each child the rule repairs that deps leaves
// unlinked. The rule is restated here rather than read from repairedDeps: a
// check that calls the function it checks would accept that function's wrong
// answer.
func repairProblems(ds *Dataset, deps []DepRow) []string {
	isWisp := map[string]bool{}
	for _, is := range ds.Issues {
		isWisp[is.ID] = is.Wisp
	}
	own := map[string]bool{}
	fromDataset := map[string]bool{}
	for _, d := range ds.Deps {
		if d.DependsOn != "" {
			own[d.IssueID] = true
		}
		fromDataset[linkKey(d)] = true
	}
	// eligible maps each child the rule repairs to the parent it links.
	eligible := map[string]string{}
	for _, is := range ds.Issues {
		parent, ok := dottedParent(is.ID)
		if !ok || own[is.ID] {
			continue
		}
		parentWisp, known := isWisp[parent]
		if !known || (is.Wisp && !parentWisp) {
			continue
		}
		eligible[is.ID] = parent
	}
	var out []string
	have := map[string]bool{}
	for _, d := range deps {
		have[linkKey(d)] = true
		if fromDataset[linkKey(d)] {
			continue
		}
		if want, ok := eligible[d.IssueID]; !ok || want != d.DependsOn || d.Type != "parent-child" {
			out = append(out, fmt.Sprintf("repair links %s -> %s (%s), which the rule does not give it", d.IssueID, d.DependsOn, d.Type))
		}
	}
	for _, is := range ds.Issues {
		if parent, ok := eligible[is.ID]; ok && !have[linkKey(DepRow{IssueID: is.ID, DependsOn: parent, Type: "parent-child"})] {
			out = append(out, fmt.Sprintf("repair of %s -> %s is missing from the import", is.ID, parent))
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

// primaryRig is the rig a Dolt database belongs to: its issue_prefix setting,
// else the only rig its ids use, else "".
func primaryRig(ds *Dataset) string {
	if p := ds.Config["issue_prefix"]; p != "" {
		return p
	}
	if rigs := rigsOf(idsOf(ds)); len(rigs) == 1 {
		return rigs[0]
	}
	return ""
}

// configKey namespaces a rig's configuration so several rigs can share the
// tracker's one config table: "rig.gt.issue_prefix". With no rig it is as is.
func configKey(rig, key string) string {
	if rig == "" {
		return key
	}
	return "rig." + rig + "." + key
}

// idsOf returns the ids of the dataset's issues.
func idsOf(ds *Dataset) []string {
	ids := make([]string, len(ds.Issues))
	for i, is := range ds.Issues {
		ids[i] = is.ID
	}
	return ids
}

// rigOfID is the rig prefix of an id, the part before its first dash; the
// importer sets issues.rig from it, and Verify scopes events and counters by
// it in SQL as split_part(id, '-', 1), so all three agree by construction.
func rigOfID(id string) (string, bool) {
	rig, _, ok := strings.Cut(id, "-")
	return rig, ok && rig != ""
}

// inRigs reports whether id's rig prefix is one of rigs.
func inRigs(id string, rigs []string) bool {
	rig, ok := rigOfID(id)
	return ok && slices.Contains(rigs, rig)
}

// eventsInRigs counts the events whose issue id belongs to one of rigs: the
// scope Verify can check in a tracker that holds other rigs too.
func eventsInRigs(events []EventRow, rigs []string) int {
	n := 0
	for _, e := range events {
		if inRigs(e.IssueID, rigs) {
			n++
		}
	}
	return n
}
