// Package clusterapproval holds the wire formats of a deployer cluster apply:
// the plan, the approval record the broker writes and the proposal the
// operator approves, so the broker, the runner and the apply share one
// definition (gt-esk22l). It reads no file and opens no network.
package clusterapproval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// The two clusters a plan names; there is one plan and one approval per
// cluster (operator decision 2026-11-18).
const (
	ClusterControl = "control"
	ClusterGTSbx   = "gt-sbx"
)

// MaxValidity is the longest window a record may name between approved_at and
// expires_at; ParseRecord refuses a longer one.
const MaxValidity = 24 * time.Hour

// ActApplyPlan is the act kind a record approving an apply of a plan carries.
const ActApplyPlan = "apply-plan"

// proposalIDChars is how much of the proposal's SHA-256 its id carries.
const proposalIDChars = 12

// recordKeys is the exact key set the broker writes (gt-esk22l).
var recordKeys = map[string]bool{
	"act":         true,
	"cluster":     true,
	"plan_sha256": true,
	"proposal_id": true,
	"approved_at": true,
	"expires_at":  true,
}

// planPhases names the phases a plan may carry, in the only order it may carry
// them (operator decision 2026-11-18).
var planPhases = []string{"policy", "workloads"}

// Plan is one rendered and hashed apply: the cluster it targets, the SHA-256
// of its rendered files in apply order, its phases in apply order and the live
// resourceVersion of each object, keyed by <kind>/<namespace>/<name>.
type Plan struct {
	Cluster  string
	SHA256   string
	Phases   []string
	Versions map[string]string
}

// ValidCluster reports whether name is one of the two clusters a plan names.
func ValidCluster(name string) bool {
	return name == ClusterControl || name == ClusterGTSbx
}

// Record is the broker's approval of a plan: the act and cluster approved, the
// full plan SHA-256 and the proposal id the operator was shown, and the window
// the approval is good for.
type Record struct {
	Act        string
	Cluster    string
	PlanSHA256 string
	ProposalID string
	ApprovedAt time.Time
	ExpiresAt  time.Time
}

// ParseRecord decodes the approval record the broker writes and refuses it
// unless every key is exact, the cluster, plan hash, act and proposal id are
// present and valid, both times are in UTC and the window is longer than zero
// and no longer than MaxValidity. It reads no clock: the caller decides
// whether the window covers its moment.
func ParseRecord(data []byte) (Record, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := readDelim(dec, '{'); err != nil {
		return Record{}, fmt.Errorf("clusterapproval: the approval record is not one JSON object: %w", err)
	}
	// encoding/json matches a struct's keys case-insensitively and keeps the
	// last of duplicate keys, so the parser reads the object token by token
	// and compares each key exactly (gt-esk22l).
	values := make(map[string]json.RawMessage, len(recordKeys))
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return Record{}, fmt.Errorf("clusterapproval: read a key of the approval record: %w", err)
		}
		key, ok := tok.(string)
		if !ok {
			return Record{}, fmt.Errorf("clusterapproval: the approval record has the non-string key %v", tok)
		}
		if !recordKeys[key] {
			return Record{}, fmt.Errorf("clusterapproval: the record has the unknown key %q", key)
		}
		if _, duplicate := values[key]; duplicate {
			return Record{}, fmt.Errorf("clusterapproval: the record names the key %q twice", key)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return Record{}, fmt.Errorf("clusterapproval: read the value of %q: %w", key, err)
		}
		values[key] = value
	}
	if err := readDelim(dec, '}'); err != nil {
		return Record{}, fmt.Errorf("clusterapproval: the approval record does not close: %w", err)
	}
	if tok, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return Record{}, fmt.Errorf("clusterapproval: trailing data after the approval record: %w", err)
		}
		return Record{}, fmt.Errorf("clusterapproval: trailing data after the approval record: %v", tok)
	}

	act, err := recordString(values, "act")
	if err != nil {
		return Record{}, err
	}
	if act == "" {
		return Record{}, errors.New("clusterapproval: the record has no act")
	}
	cluster, err := recordString(values, "cluster")
	if err != nil {
		return Record{}, err
	}
	if !ValidCluster(cluster) {
		return Record{}, fmt.Errorf("clusterapproval: the record's cluster is %q, not %q or %q", cluster, ClusterControl, ClusterGTSbx)
	}
	planSHA256, err := recordString(values, "plan_sha256")
	if err != nil {
		return Record{}, err
	}
	if !isPlanSHA256(planSHA256) {
		return Record{}, fmt.Errorf("clusterapproval: the record's plan_sha256 is %q, not 64 lower-case hex characters", planSHA256)
	}
	proposalID, err := recordString(values, "proposal_id")
	if err != nil {
		return Record{}, err
	}
	if proposalID == "" {
		return Record{}, errors.New("clusterapproval: the record has no proposal_id")
	}
	approvedAt, err := recordTime(values, "approved_at")
	if err != nil {
		return Record{}, err
	}
	expiresAt, err := recordTime(values, "expires_at")
	if err != nil {
		return Record{}, err
	}
	if !expiresAt.After(approvedAt) {
		return Record{}, fmt.Errorf("clusterapproval: the record's expires_at %s is not after approved_at %s", expiresAt.Format(time.RFC3339), approvedAt.Format(time.RFC3339))
	}
	if expiresAt.After(approvedAt.Add(MaxValidity)) {
		return Record{}, fmt.Errorf("clusterapproval: the record is valid for more than %s", MaxValidity)
	}
	return Record{
		Act:        act,
		Cluster:    cluster,
		PlanSHA256: planSHA256,
		ProposalID: proposalID,
		ApprovedAt: approvedAt,
		ExpiresAt:  expiresAt,
	}, nil
}

// readDelim reads the stream's next token and reports it as want.
func readDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != want {
		return fmt.Errorf("got %v, want %v", tok, want)
	}
	return nil
}

// recordString reads values[key] as a JSON string; an absent key reads as "".
func recordString(values map[string]json.RawMessage, key string) (string, error) {
	var s string
	if raw, present := values[key]; present {
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", fmt.Errorf("clusterapproval: the record's %s is not a JSON string: %w", key, err)
		}
	}
	return s, nil
}

// recordTime reads values[key] as an RFC 3339 time and refuses an absent,
// zero or non-UTC one.
func recordTime(values map[string]json.RawMessage, key string) (time.Time, error) {
	var at time.Time
	if raw, present := values[key]; present {
		if err := json.Unmarshal(raw, &at); err != nil {
			return time.Time{}, fmt.Errorf("clusterapproval: the record's %s is not an RFC 3339 time: %w", key, err)
		}
	}
	if at.IsZero() {
		return time.Time{}, fmt.Errorf("clusterapproval: the record has no %s", key)
	}
	if at.Location() != time.UTC {
		return time.Time{}, fmt.Errorf("clusterapproval: the record's %s is not in UTC", key)
	}
	return at, nil
}

// isPlanSHA256 reports whether s is 64 lower-case hex characters, the form of
// the plan hash the record must carry so it compares equal to Plan.SHA256.
func isPlanSHA256(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for _, c := range s {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

// proposalJSON is the proposal's wire form. The field order fixes the byte
// order, and encoding/json writes a map's keys sorted, so one plan content has
// one encoding whatever order its Versions map was filled in.
type proposalJSON struct {
	Act        string            `json:"act"`
	Cluster    string            `json:"cluster"`
	PlanSHA256 string            `json:"plan_sha256"`
	Phases     []string          `json:"phases"`
	Versions   map[string]string `json:"versions"`
}

// NewProposal returns p's canonical proposal JSON and its id, the first 12 hex
// characters of the sha256 of those bytes, and refuses an invalid plan with no
// bytes and no id.
func NewProposal(p Plan) ([]byte, string, error) {
	if err := validatePlan(p); err != nil {
		return nil, "", err
	}
	versions := p.Versions
	if versions == nil {
		versions = map[string]string{}
	}
	body, err := json.Marshal(proposalJSON{
		Act:        ActApplyPlan,
		Cluster:    p.Cluster,
		PlanSHA256: p.SHA256,
		Phases:     p.Phases,
		Versions:   versions,
	})
	if err != nil {
		return nil, "", fmt.Errorf("clusterapproval: encode the proposal: %w", err)
	}
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:])[:proposalIDChars], nil
}

// validatePlan reports why p cannot become a proposal.
func validatePlan(p Plan) error {
	if !ValidCluster(p.Cluster) {
		return fmt.Errorf("clusterapproval: the plan's cluster is %q, not %q or %q", p.Cluster, ClusterControl, ClusterGTSbx)
	}
	if !isPlanSHA256(p.SHA256) {
		return fmt.Errorf("clusterapproval: the plan's SHA256 is %q, not 64 lower-case hex characters", p.SHA256)
	}
	if len(p.Phases) == 0 {
		return errors.New("clusterapproval: the plan has no phases")
	}
	next := 0
	for _, phase := range p.Phases {
		for next < len(planPhases) && planPhases[next] != phase {
			next++
		}
		if next == len(planPhases) {
			return fmt.Errorf("clusterapproval: the plan's phases %q are not drawn once each from %q in that order", p.Phases, planPhases)
		}
		next++
	}
	return nil
}
