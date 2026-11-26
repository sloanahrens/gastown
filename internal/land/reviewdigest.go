package land

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
)

// ExpectedReviewFlags is the sorted flag names a complete om review run carries
// for in and bar, as flagNames gives them for reviewOnce's argv: -C, --base,
// --head and --out on every run, --bead when the review names its work bead,
// --threshold when bar > 0, --round when Round > 0, --prior-findings when the
// input has the previous round's findings, --criteria-file when it carries the
// bead's acceptance text, --context-file when it carries the accepted-risk
// record, --bead-file when it carries the work-item file, --policy-file when it
// carries the town-policy block, and --rounds-file when it carries the rounds
// ledger. Whether the installed om takes a flag is a run-time question its help
// probe answers, so a run whose probe refuses --threshold, --round,
// --criteria-file, --context-file, --bead-file, --policy-file or --rounds-file
// carries fewer flags, and Verdict.Flags records the set it ran with. Any
// mismatch against the list a stored review ran with is a reuse miss, not a
// near match: the key's InputDigest covers these flags
// (gt-0lys4u), and ReuseMiss compares every key field for equality, so a flag
// over-claimed here costs a review and never reuses a stale verdict.
func ExpectedReviewFlags(in ReviewInput, bar float64) []string {
	flags := []string{"-C", "--base", "--head", "--out"}
	if in.BeadID != "" {
		flags = append(flags, "--bead")
	}
	if bar > 0 {
		flags = append(flags, "--threshold")
	}
	if in.Round > 0 {
		flags = append(flags, "--round")
	}
	if len(in.PriorFindings) > 0 {
		flags = append(flags, "--prior-findings")
	}
	if in.CriteriaText != "" {
		flags = append(flags, "--criteria-file")
	}
	if in.AcceptedRisksText != "" {
		flags = append(flags, "--context-file")
	}
	if in.BriefText != "" {
		flags = append(flags, "--bead-file")
	}
	if in.PolicyText != "" {
		flags = append(flags, "--policy-file")
	}
	if in.RoundsText != "" {
		flags = append(flags, "--rounds-file")
	}
	slices.Sort(flags)
	return flags
}

// reviewDigestExcluded names the ReviewInput fields the input digest leaves
// out: the checkout and the range om runs in, the repository a caller reads om's
// config from, and the log sink are not given to om on its command line, so a
// verdict made in one checkout or at one base stays reusable in another
// (gt-0cdbz6).
var reviewDigestExcluded = []string{"Dir", "Base", "Head", "Repo", "Logf"}

// ReviewInputDigest is the sha256 of one review's inputs as om is given them:
// the exported ReviewInput fields reviewDigestExcluded does not name, the flag
// names, the .om.json text read at base, the context_file text it names and the
// target branch (gt-0cdbz6).
func ReviewInputDigest(in ReviewInput, flags []string, omRepoConfig, contextFile, target string) (string, error) {
	return reviewDigestOf(in, map[string]any{
		"flags":        flags,
		"omRepoConfig": omRepoConfig,
		"contextFile":  contextFile,
		"target":       target,
	})
}

// reviewDigestOf is the digest of any struct's exported fields outside
// reviewDigestExcluded, with extra's entries added, as canonical JSON: the
// object's keys are sorted and PriorFindings is encoded as its JSON, so equal
// inputs always digest equal. It takes any value so a test can digest a struct
// with a field ReviewInput does not have (gt-0cdbz6). A value that is not a
// struct, an extra name that is also a field, and a field JSON cannot encode
// are errors, never a partial digest.
func reviewDigestOf(v any, extra map[string]any) (string, error) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Struct {
		return "", fmt.Errorf("review input digest: %T is not a struct", v)
	}
	excluded := make(map[string]bool, len(reviewDigestExcluded))
	for _, name := range reviewDigestExcluded {
		excluded[name] = true
	}
	fields := make(map[string]any, rv.NumField()+len(extra))
	for i := range rv.NumField() {
		f := rv.Type().Field(i)
		if f.PkgPath != "" || excluded[f.Name] {
			continue
		}
		if _, dup := extra[f.Name]; dup {
			return "", fmt.Errorf("review input digest: %s is a field and an extra input", f.Name)
		}
		fields[f.Name] = rv.Field(i).Interface()
	}
	maps.Copy(fields, extra)
	data, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("review input digest: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
