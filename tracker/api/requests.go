package api

// Request and response bodies of the /v1 routes. A route with no body type
// here uses ListFilter, ReadyFilter, CreateParams, Issue or IssuePage directly.

// ShowManyRequest asks for several issues by id.
type ShowManyRequest struct {
	IDs []string `json:"ids"`
}

// ShowManyResponse holds the issues that exist among the ids asked for.
type ShowManyResponse struct {
	Issues map[string]Issue `json:"issues"`
}

// UpdateRequest is a patch applied while a guard holds.
type UpdateRequest struct {
	Patch UpdatePatch `json:"patch"`
	Guard Guard       `json:"guard"`
}

// CloseRequest closes several issues together.
type CloseRequest struct {
	IDs    []string `json:"ids"`
	Reason string   `json:"reason,omitempty"`
	Force  bool     `json:"force,omitempty"`
}

// DeleteRequest deletes several issues together.
type DeleteRequest struct {
	IDs []string `json:"ids"`
}

// CommentRequest adds a comment. Author defaults to the caller's actor; a
// caller sets it to write on someone else's behalf.
type CommentRequest struct {
	Text   string `json:"text"`
	Author string `json:"author,omitempty"`
}

// CommentsResponse lists an issue's comments, oldest first.
type CommentsResponse struct {
	Comments []Comment `json:"comments"`
}

// DependencyRequest links the issue in the path to DependsOn.
type DependencyRequest struct {
	DependsOn string `json:"depends_on"`
	Type      string `json:"type"`
}

// DependenciesResponse lists what an issue depends on.
type DependenciesResponse struct {
	Dependencies []IssueDep `json:"dependencies"`
}

// ChildrenOfRequest asks for the direct children of several parents.
type ChildrenOfRequest struct {
	Parents []string `json:"parents"`
}

// ChildrenOfResponse maps each parent that has children to them.
type ChildrenOfResponse struct {
	Children map[string][]Issue `json:"children"`
}

// DispatchBoardRequest asks for everything one dispatch tick reads about the
// work board: per rig the ready issues, the issues a seat holds and the
// children of the ready issues, plus the status of a few named issues.
type DispatchBoardRequest struct {
	// Rigs are the rig prefixes to read. A prefix the tracker does not know
	// is reported in Errors, not failed.
	Rigs []string `json:"rigs"`
	// ExcludeLabels and ExcludeTypes narrow the ready issues, as in ReadyFilter.
	ExcludeLabels []string `json:"exclude_labels,omitempty"`
	ExcludeTypes  []string `json:"exclude_types,omitempty"`
	// HeldStatuses are the statuses that mean a seat holds an issue.
	HeldStatuses []string `json:"held_statuses,omitempty"`
	// ProbeIDs are issues whose status is wanted (revert beads).
	ProbeIDs []string `json:"probe_ids,omitempty"`
}

// HeldBead is an assigned issue in one of the held statuses.
type HeldBead struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Assignee    string `json:"assignee"`
	Description string `json:"description"`
}

// ChildStatus is a direct child of a ready issue.
type ChildStatus struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// RigBoard is one rig's part of the dispatch board. Ready, Held and Children
// are never null.
type RigBoard struct {
	Ready    []Issue                  `json:"ready"`
	Held     []HeldBead               `json:"held"`
	Children map[string][]ChildStatus `json:"children"`
}

// Probe is the status of one probed issue; Found is false when it does not exist.
type Probe struct {
	Found  bool   `json:"found"`
	Status string `json:"status,omitempty"`
}

// DispatchBoardResponse is one consistent snapshot of the board. Rigs holds
// the rigs that were read; Errors says why each other requested rig was not.
type DispatchBoardResponse struct {
	Rigs   map[string]RigBoard `json:"rigs"`
	Probes map[string]Probe    `json:"probes"`
	Errors map[string]string   `json:"errors"`
}

// Stats counts a rig's durable issues (wisps are not counted) by status.
type Stats struct {
	Total    int            `json:"total"`
	ByStatus map[string]int `json:"by_status"`
}

// ConfigValue is a configuration key's value; Set is false for a key that has
// none, whose Value is then empty.
type ConfigValue struct {
	Value string `json:"value"`
	Set   bool   `json:"set"`
}

// ConfigSetRequest sets a configuration key.
type ConfigSetRequest struct {
	Value string `json:"value"`
}

// Limits on one graph.
const (
	MaxGraphNodes = 500
	MaxGraphEdges = 2000
)

// GraphNode is one issue of a graph. Key is the caller's own name for it,
// unique within the request; edges refer to it by that name. The embedded
// CreateParams is the issue; its ID, Parent, Rig and Actor must be empty (the
// server mints the id, edges set parents, the request names the rig and the
// token names the actor).
type GraphNode struct {
	Key string `json:"key"`
	CreateParams
}

// GraphEdge links two issues of a graph, or an issue of the graph to one that
// already exists. From depends on To, with the relation Type ("blocks",
// "parent-child" with From the child, ...). From and To are node keys; one
// that is not a key names an existing issue (or external:<rig>:<id>).
type GraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type"`
}

// GraphRequest creates a set of issues and the links between them, all or
// none: a molecule with its steps, or any batch of related work.
type GraphRequest struct {
	Rig   string      `json:"rig"`
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges,omitempty"`
}

// GraphResult maps each node key to the id of the issue created for it.
type GraphResult struct {
	IDs map[string]string `json:"ids"`
}
