package doctor

import (
	"errors"
	"sync"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// doctorTown answers the bead stores checks open (CheckContext.openBeads),
// one beadsfake database per site directory, and records every site opened
// and every agent or rig bead created. A directory never seeded is an empty
// database, kept so later reads see what a fix wrote.
type doctorTown struct {
	mu  sync.Mutex
	dbs map[string]*beadsfake.Fake
	// sql answers the bd sql reads checks make through CheckContext.bd;
	// nil fails them as unscripted.
	sql     func(query string) ([][]string, error)
	created []string // "<id> dir=<workDir>"
	updated []string // "<id> dir=<workDir>"
}

func newDoctorTown() *doctorTown { return &doctorTown{dbs: map[string]*beadsfake.Fake{}} }

// db is dir's database.
func (d *doctorTown) db(dir string) *beadsfake.Fake {
	d.mu.Lock()
	defer d.mu.Unlock()
	db, ok := d.dbs[dir]
	if !ok {
		db = beadsfake.New()
		d.dbs[dir] = db
	}
	return db
}

func (d *doctorTown) open(site beadsSite) doctorBeads {
	return doctorDB{Fake: d.db(site.workDir), town: d, dir: site.workDir}
}

// ctx is a CheckContext for townRoot (scoped to rigName when set) whose
// bead stores d answers.
func (d *doctorTown) ctx(townRoot, rigName string) *CheckContext {
	return &CheckContext{TownRoot: townRoot, RigName: rigName, openBeads: d.open, openBD: d.openBD}
}

func (d *doctorTown) openBD(string, []string) bdCLI {
	f := beadsfake.New()
	if d.sql != nil {
		f.OnSQL(d.sql)
	}
	return f
}

func (d *doctorTown) note(list *[]string, id, dir string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	*list = append(*list, id+" dir="+dir)
}

// creates and updates are the recorded agent-bead writes, in order.
func (d *doctorTown) creates() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.created...)
}

func (d *doctorTown) updates() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.updated...)
}

// doctorDB is one database as checks see it: the fake, plus the
// *beads.Beads helpers checks call, each reduced to the Client operations
// it performs.
type doctorDB struct {
	*beadsfake.Fake
	town *doctorTown
	dir  string
}

func (db doctorDB) Update(id string, opts beads.UpdateOptions) error {
	db.town.note(&db.town.updated, id, db.dir)
	return db.Fake.Update(id, opts)
}

func (db doctorDB) issueMap(issues []*beads.Issue, err error) (map[string]*beads.Issue, error) {
	if err != nil {
		return nil, err
	}
	out := make(map[string]*beads.Issue, len(issues))
	for _, is := range issues {
		out[is.ID] = is
	}
	return out, nil
}

func (db doctorDB) ListAgentBeads() (map[string]*beads.Issue, error) {
	issues, err := db.List(beads.ListOptions{Label: "gt:agent", Priority: -1, IncludeInfra: true})
	if err != nil {
		return nil, err
	}
	wisps, _ := db.ListAgentBeadsFromWisps()
	out, _ := db.issueMap(issues, nil)
	for id, w := range wisps {
		if _, ok := out[id]; !ok {
			out[id] = w
		}
	}
	return out, nil
}

func (db doctorDB) ListAgentBeadsFromWisps() (map[string]*beads.Issue, error) {
	return db.issueMap(db.List(beads.ListOptions{Label: "gt:agent", Status: "all", Priority: -1, Ephemeral: true}))
}

func (db doctorDB) ListWispIDs() (map[string]bool, error) {
	wisps, err := db.List(beads.ListOptions{Status: "all", Priority: -1, Ephemeral: true})
	if err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(wisps))
	for _, w := range wisps {
		ids[w.ID] = true
	}
	return ids, nil
}

func (db doctorDB) CreateAgentBead(id, title string, fields *beads.AgentFields) (*beads.Issue, error) {
	db.town.note(&db.town.created, id, db.dir)
	return db.Create(beads.CreateOptions{ID: id, Title: title, Description: beads.FormatAgentDescription(title, fields), Labels: []string{"gt:agent"}, Priority: -1})
}

func (db doctorDB) DetachMolecule(id string) (*beads.Issue, error) {
	issue, err := db.Show(id)
	if err != nil {
		return nil, err
	}
	desc := beads.SetAttachmentFields(issue, nil)
	if err := db.Fake.Update(id, beads.UpdateOptions{Description: &desc}); err != nil {
		return nil, err
	}
	return db.Show(id)
}

func (db doctorDB) EnsureRigBead(name string, fields *beads.RigFields) (*beads.Issue, error) {
	if existing, err := db.Show(rigBeadIDFor(name, fields)); err == nil {
		return existing, nil
	}
	return db.CreateRigBead(name, fields)
}

func (db doctorDB) CreateRigBead(name string, fields *beads.RigFields) (*beads.Issue, error) {
	id := rigBeadIDFor(name, fields)
	db.town.note(&db.town.created, id, db.dir)
	return db.Create(beads.CreateOptions{ID: id, Title: name, Description: beads.FormatRigDescription(name, fields), Labels: []string{"gt:rig"}, Priority: -1})
}

func (db doctorDB) DemoteToWisp(id string) error {
	return errors.New("doctorDB: DemoteToWisp not modeled")
}

func (db doctorDB) ReopenUnassigned(id string) error {
	open, none := "open", ""
	return db.Fake.Update(id, beads.UpdateOptions{Status: &open, Assignee: &none})
}

// rigBeadIDFor is the rig identity bead ID *beads.Beads derives.
func rigBeadIDFor(name string, fields *beads.RigFields) string {
	prefix := "gt"
	if fields != nil && fields.Prefix != "" {
		prefix = fields.Prefix
	}
	return beads.RigBeadIDWithPrefix(prefix, name)
}

// errNoDatabase is what bd reports in a directory with no beads database.
var errNoDatabase = errors.New("bd: Error: no beads database found")

// noBeadsDB is a store where bd finds no database: every call fails.
type noBeadsDB struct{}

func (noBeadsDB) Show(string) (*beads.Issue, error)                      { return nil, errNoDatabase }
func (noBeadsDB) ShowMultiple([]string) (map[string]*beads.Issue, error) { return nil, errNoDatabase }
func (noBeadsDB) List(beads.ListOptions) ([]*beads.Issue, error)         { return nil, errNoDatabase }
func (noBeadsDB) ListByAssignee(string) ([]*beads.Issue, error)          { return nil, errNoDatabase }
func (noBeadsDB) GetAssignedIssue(string) (*beads.Issue, error)          { return nil, errNoDatabase }
func (noBeadsDB) ListIssueStatuses(...beads.IssueStatus) ([]*beads.Issue, error) {
	return nil, errNoDatabase
}
func (noBeadsDB) ListAssignedIssueStatuses(string, ...beads.IssueStatus) ([]*beads.Issue, error) {
	return nil, errNoDatabase
}
func (noBeadsDB) Ready() ([]*beads.Issue, error)                   { return nil, errNoDatabase }
func (noBeadsDB) ReadyAll() ([]*beads.Issue, error)                { return nil, errNoDatabase }
func (noBeadsDB) Children(string) ([]*beads.Issue, error)          { return nil, errNoDatabase }
func (noBeadsDB) Comments(string) ([]beads.Comment, error)         { return nil, errNoDatabase }
func (noBeadsDB) Create(beads.CreateOptions) (*beads.Issue, error) { return nil, errNoDatabase }
func (noBeadsDB) Update(string, beads.UpdateOptions) error         { return errNoDatabase }
func (noBeadsDB) Close(...string) error                            { return errNoDatabase }
func (noBeadsDB) CloseWithReason(string, ...string) error          { return errNoDatabase }
func (noBeadsDB) ForceCloseWithReason(string, ...string) error     { return errNoDatabase }
func (noBeadsDB) DeleteIssues(...string) error                     { return errNoDatabase }
func (noBeadsDB) Release(string) error                             { return errNoDatabase }
func (noBeadsDB) ReleaseWithReason(string, string) error           { return errNoDatabase }
func (noBeadsDB) AddComment(string, string) error                  { return errNoDatabase }
func (noBeadsDB) AddCommentAs(string, string, string) error        { return errNoDatabase }
func (noBeadsDB) AddDependency(string, string) error               { return errNoDatabase }
func (noBeadsDB) AddTypedDependency(string, string, string) error  { return errNoDatabase }
func (noBeadsDB) DepList(string, string) ([]beads.IssueDep, error) { return nil, errNoDatabase }
func (noBeadsDB) RemoveDependency(string, string) error            { return errNoDatabase }
func (noBeadsDB) AppendNotes(string, string) error                 { return errNoDatabase }
func (noBeadsDB) ReleaseIfAssignee(string, string) (bool, error)   { return false, errNoDatabase }
func (noBeadsDB) TransferIfAssignee(string, string, string, string) (bool, error) {
	return false, errNoDatabase
}
func (noBeadsDB) ChildrenOf(...string) (map[string][]*beads.Issue, error) { return nil, errNoDatabase }
func (noBeadsDB) ListAgentBeads() (map[string]*beads.Issue, error)        { return nil, errNoDatabase }
func (noBeadsDB) ListAgentBeadsFromWisps() (map[string]*beads.Issue, error) {
	return nil, errNoDatabase
}
func (noBeadsDB) ListWispIDs() (map[string]bool, error) { return nil, errNoDatabase }
func (noBeadsDB) CreateAgentBead(string, string, *beads.AgentFields) (*beads.Issue, error) {
	return nil, errNoDatabase
}
func (noBeadsDB) DetachMolecule(string) (*beads.Issue, error) { return nil, errNoDatabase }
func (noBeadsDB) EnsureRigBead(string, *beads.RigFields) (*beads.Issue, error) {
	return nil, errNoDatabase
}
func (noBeadsDB) CreateRigBead(string, *beads.RigFields) (*beads.Issue, error) {
	return nil, errNoDatabase
}
func (noBeadsDB) DemoteToWisp(string) error     { return errNoDatabase }
func (noBeadsDB) ReopenUnassigned(string) error { return errNoDatabase }

var (
	_ doctorBeads = doctorDB{}
	_ doctorBeads = noBeadsDB{}
)

// noBD is a CheckContext for townRoot whose bd finds no database anywhere.
func noBD(townRoot string) *CheckContext {
	return &CheckContext{TownRoot: townRoot, openBeads: func(beadsSite) doctorBeads { return noBeadsDB{} }}
}
