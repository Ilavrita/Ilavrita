package project

import "fmt"

// defaultEnvironment is what a Project runs as when nothing names one, mirroring
// the column's own default so Go and the row cannot disagree.
const defaultEnvironment = "production"

// Project is one isolation boundary as a persisted record. SuperProject
// describes the Project the migration creates and fixes its kind; this is the
// shape every Project reads back as, including that one.
type Project struct {
	id                ID
	kind              Kind
	slug              string
	name              string
	state             State
	environment       string
	allowClinicalData bool
}

// Config is what both constructors take, and the shape a store rehydrates
// a persisted row through. It names no kind: that comes from the constructor, so
// no caller can provision an ordinary Project as the privileged one.
type Config struct {
	ID                ID
	Slug              string
	Name              string
	State             State
	Environment       string
	AllowClinicalData bool
}

// NewProject builds an ordinary Project. It cannot produce the Super Project,
// which is provisioned once by the bootstrapper and never by this path.
func NewProject(cfg Config) (Project, error) {
	return newProject(KindStandard, cfg)
}

// NewSuperProjectRecord rebuilds the privileged Project as a persisted record.
// It refuses clinical data outright rather than taking it as an argument: the
// Project that administers the install never stores patient data.
func NewSuperProjectRecord(cfg Config) (Project, error) {
	cfg.AllowClinicalData = false

	return newProject(KindSuper, cfg)
}

// newProject applies the invariants the table states: an id that is neither
// empty nor the system sentinel, a slug and a name, a recognised state, and a
// Super Project that holds no clinical data.
func newProject(kind Kind, cfg Config) (Project, error) {
	if err := ValidateID(cfg.ID); err != nil {
		return Project{}, err
	}

	// 'system' is the platform_resource sentinel for a system-scoped row, so a
	// Project claiming it would collide with system scope.
	if cfg.ID == SystemScope {
		return Project{}, fmt.Errorf("%w: %q is the system sentinel", ErrInvalidProjectID, cfg.ID)
	}

	if cfg.Slug == "" || cfg.Name == "" {
		return Project{}, fmt.Errorf("%w: %s", ErrMissingProjectName, cfg.ID)
	}

	if !cfg.State.Valid() {
		return Project{}, fmt.Errorf("%w: %q", ErrUnknownState, string(cfg.State))
	}

	if kind == KindSuper && cfg.AllowClinicalData {
		return Project{}, fmt.Errorf("%w: %s administers the install", ErrClinicalDataInSuperProject, cfg.ID)
	}

	environment := cfg.Environment
	if environment == "" {
		environment = defaultEnvironment
	}

	return Project{
		id: cfg.ID, kind: kind, slug: cfg.Slug, name: cfg.Name, state: cfg.State,
		environment: environment, allowClinicalData: cfg.AllowClinicalData,
	}, nil
}

// ID returns the Project's identifier.
func (p Project) ID() ID {
	return p.id
}

// Kind returns whether this is the Super Project.
func (p Project) Kind() Kind {
	return p.kind
}

// Slug returns the name a request resolves the Project by.
func (p Project) Slug() string {
	return p.slug
}

// Name returns the Project's display name.
func (p Project) Name() string {
	return p.name
}

// State returns the Project's lifecycle position.
func (p Project) State() State {
	return p.state
}

// Environment returns what this Project runs as. It gates nothing here and
// exists so a caller can tell production from a sandbox.
func (p Project) Environment() string {
	return p.environment
}

// AllowsClinicalData reports whether this Project may hold patient data. The
// Super Project never may.
func (p Project) AllowsClinicalData() bool {
	return p.allowClinicalData
}

// TransitionTo returns the Project in its next state.
func (p Project) TransitionTo(next State) (Project, error) {
	state, err := p.state.TransitionTo(next)
	if err != nil {
		return Project{}, err
	}

	p.state = state

	return p, nil
}

// String renders a Project.
func (p Project) String() string {
	return fmt.Sprintf("project %s (%s, %s, clinical data %t)", p.id, p.kind, p.state, p.allowClinicalData)
}
