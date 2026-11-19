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

// recordJSON is Record's wire form: the keys the broker writes and the
// deployer reads.
type recordJSON struct {
	Act        string    `json:"act"`
	Cluster    string    `json:"cluster"`
	PlanSHA256 string    `json:"plan_sha256"`
	ProposalID string    `json:"proposal_id"`
	ApprovedAt time.Time `json:"approved_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// ParseRecord decodes an approval record written by the broker. It refuses an
// unknown key, a cluster that is missing or not one of the two names, a plan
// SHA-256 that is not 64 lower-case hex characters, an approved_at or
// expires_at that is missing or not in UTC, and an expires_at more than
// MaxValidity after approved_at.
//
// The parser is clock-free: it checks how long the window is, never whether it
// covers a given moment, and it leaves the act's value to the caller.
func ParseRecord(data []byte) (Record, error) {
	var wire recordJSON
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return Record{}, fmt.Errorf("clusterapproval: decode the approval record: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return Record{}, errors.New("clusterapproval: trailing data after the approval record")
	}
	if !ValidCluster(wire.Cluster) {
		return Record{}, fmt.Errorf("clusterapproval: the record's cluster is %q, not %q or %q", wire.Cluster, ClusterControl, ClusterGTSbx)
	}
	if !isPlanSHA256(wire.PlanSHA256) {
		return Record{}, fmt.Errorf("clusterapproval: the record's plan_sha256 is %q, not 64 lower-case hex characters", wire.PlanSHA256)
	}
	for _, field := range []struct {
		key string
		at  time.Time
	}{
		{"approved_at", wire.ApprovedAt},
		{"expires_at", wire.ExpiresAt},
	} {
		if field.at.IsZero() {
			return Record{}, fmt.Errorf("clusterapproval: the record has no %s", field.key)
		}
		if field.at.Location() != time.UTC {
			return Record{}, fmt.Errorf("clusterapproval: the record's %s is not in UTC", field.key)
		}
	}
	if wire.ExpiresAt.After(wire.ApprovedAt.Add(MaxValidity)) {
		return Record{}, fmt.Errorf("clusterapproval: the record is valid for more than %s", MaxValidity)
	}
	return Record{
		Act:        wire.Act,
		Cluster:    wire.Cluster,
		PlanSHA256: wire.PlanSHA256,
		ProposalID: wire.ProposalID,
		ApprovedAt: wire.ApprovedAt,
		ExpiresAt:  wire.ExpiresAt,
	}, nil
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

// NewProposal returns the canonical proposal JSON for p and its id, the first
// 12 hex characters of the sha256 of those bytes: what the operator is shown
// before the password command. Phases and Versions render as [] and {} when
// empty, never null, so the same plan content has one encoding.
func NewProposal(p Plan) ([]byte, string) {
	phases := p.Phases
	if phases == nil {
		phases = []string{}
	}
	versions := p.Versions
	if versions == nil {
		versions = map[string]string{}
	}
	proposal := proposalJSON{
		Act:        ActApplyPlan,
		Cluster:    p.Cluster,
		PlanSHA256: p.SHA256,
		Phases:     phases,
		Versions:   versions,
	}
	body, _ := json.Marshal(proposal) //nolint:errcheck // the proposal holds only strings, a string slice and a string map, so its JSON cannot fail to encode
	sum := sha256.Sum256(body)
	return body, hex.EncodeToString(sum[:])[:proposalIDChars]
}
