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
