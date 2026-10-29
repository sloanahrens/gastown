package api

import (
	"encoding/json"
	"time"
)

// ExportRequest asks for every issue of one rig, wisps included, as JSONL.
type ExportRequest struct {
	Rig string `json:"rig"`
}

// The record types of an export stream. The stream carries the issues first,
// then their labels, dependencies and comments. An error record ends a stream
// that failed after its first line, because the status was already sent.
const (
	RecordIssue      = "issue"
	RecordLabel      = "label"
	RecordDependency = "dependency"
	RecordComment    = "comment"
	RecordError      = "error"
)

// ExportRecord is one line of an export stream. Type names the record and the
// one field that carries it; every other field is absent.
type ExportRecord struct {
	Type       string            `json:"type"`
	Issue      *Issue            `json:"issue,omitempty"`
	Label      *ExportLabel      `json:"label,omitempty"`
	Dependency *ExportDependency `json:"dependency,omitempty"`
	Comment    *Comment          `json:"comment,omitempty"`
	Error      *Error            `json:"error,omitempty"`
}

// ExportLabel is one label of an issue.
type ExportLabel struct {
	IssueID string `json:"issue_id"`
	Label   string `json:"label"`
}

// ExportDependency is one stored link from an issue to what it depends on.
// DependsOn may be an external:<rig>:<id> reference. Metadata is the link's
// JSON as stored, absent when it has none.
type ExportDependency struct {
	IssueID   string          `json:"issue_id"`
	DependsOn string          `json:"depends_on"`
	Type      string          `json:"type"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	ThreadID  string          `json:"thread_id,omitempty"`
	CreatedBy string          `json:"created_by,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}
