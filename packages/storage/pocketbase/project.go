package pocketbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

var (
	// ErrProjectSlugTaken reports a slug the install already resolves. It is the
	// unique index's own guarantee, surfaced as a typed error rather than as a
	// driver-shaped constraint violation.
	ErrProjectSlugTaken = errors.New("pocketbase: the install already resolves this slug")

	// ErrInstanceExists reports a second provisioning run against an install that
	// already holds one. The claim is single-use, so a second instance row would
	// be a second chance to mint a Super Admin.
	ErrInstanceExists = errors.New("pocketbase: the install is already provisioned")

	// ErrClaimNoLongerPending reports a claim applied to an install that moved
	// since it was read. Two callers racing one token must not both mint.
	ErrClaimNoLongerPending = errors.New("pocketbase: the install is no longer pending")

	// ErrUnknownBootstrapState reports an instance row holding a state outside the
	// enum. An unrecognised state denies rather than being read as pending.
	ErrUnknownBootstrapState = errors.New("pocketbase: the install holds an unrecognised bootstrap state")

	// ErrSuperProjectNotCreatedHere reports the privileged Project offered to the
	// ordinary create path. It is provisioned once, with its install record, so
	// there is no second way to bring one into existence.
	ErrSuperProjectNotCreatedHere = errors.New("pocketbase: the super project is provisioned with its install record")
)

// instanceRow is the single row the install record lives in, named by a CHECK
// rather than by convention.
const instanceRow = "instance"

// schemaVersion is what a provisioning run stamps: the schema the install was
// created under, not a migration counter.
const schemaVersion = 1

// projectColumns is what every read selects, in the order the scan reads them.
const projectColumns = "id, kind, slug, name, state, environment, allow_clinical_data, version"

const (
	createProject = "INSERT INTO projects" +
		" (id, kind, slug, name, state, environment, allow_clinical_data," +
		" created_at, updated_at, state_changed_at, version)" +
		" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)" +
		" ON CONFLICT (slug) DO NOTHING" +
		" RETURNING version"

	readProject = "SELECT " + projectColumns + " FROM projects WHERE id = ?"

	readProjectBySlug = "SELECT " + projectColumns + " FROM projects WHERE slug = ?"

	// state_changed_at moves with the state and not with any other edit, so the
	// lifecycle's own history stays readable.
	updateProjectState = "UPDATE projects SET state = ?, updated_at = ?, state_changed_at = ?," +
		" version = version + 1 WHERE id = ? AND version = ?" +
		" RETURNING version"

	readInstance = "SELECT super_project_id, bootstrap_state," +
		" COALESCE(bootstrap_token_hash, ''), COALESCE(bootstrap_token_expires_at, 0)" +
		" FROM instance WHERE id = ?"

	createInstanceRow = "INSERT INTO instance" +
		" (id, schema_version, super_project_id, bootstrap_state," +
		" bootstrap_token_hash, bootstrap_token_expires_at, created_at, updated_at)" +
		" VALUES (?, ?, ?, ?, ?, ?, ?, ?)" +
		" ON CONFLICT (id) DO NOTHING" +
		" RETURNING id"

	// The claim is conditional on the row still being pending, so two callers
	// racing one token cannot both mint a Super Admin.
	spendClaim = "UPDATE instance SET bootstrap_state = 'complete'," +
		" bootstrap_token_hash = NULL, bootstrap_token_expires_at = NULL, updated_at = ?" +
		" WHERE id = ? AND bootstrap_state = 'pending'" +
		" RETURNING id"

	createMembership = "INSERT INTO project_memberships" +
		" (project_id, id, project_kind, user_id, client_application_id, bot_id," +
		" profile_type, profile_id, state, admin, super_admin, invitation_source," +
		" created_at, updated_at, activated_at)" +
		" VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
)

// ProjectVersion is the optimistic-concurrency counter projects.version carries.
type ProjectVersion int64

// ProjectStore persists Projects and the install record. It is the only writer of
// the instance row, so the single claim that mints the first Super Admin has
// exactly one path through this package.
type ProjectStore struct {
	db *sql.DB
}

var _ project.BootstrapStore = (*ProjectStore)(nil)

// NewProjectStore binds a store to an open database. The caller owns the pool and
// is responsible for opening it with foreign keys enforced.
func NewProjectStore(db *sql.DB) *ProjectStore {
	return &ProjectStore{db: db}
}

// Create writes an ordinary Project at version 1. A slug the install already
// resolves is ErrProjectSlugTaken and nothing is written.
func (s *ProjectStore) Create(ctx context.Context, proj project.Project) (ProjectVersion, error) {
	if proj.Kind() == project.KindSuper {
		return 0, fmt.Errorf("%w: %s", ErrSuperProjectNotCreatedHere, proj.ID())
	}

	return s.insert(ctx, proj)
}

// insert writes one Project row, whatever its kind. It is unexported because the
// Super Project reaches it only through CreateInstance, which writes the install
// record in the same transaction.
func (s *ProjectStore) insert(ctx context.Context, proj project.Project) (ProjectVersion, error) {
	stamp := time.Now().UTC().UnixMilli()

	var version int64

	err := conn(ctx, s.db).QueryRowContext(ctx, createProject,
		string(proj.ID()), string(proj.Kind()), proj.Slug(), proj.Name(), string(proj.State()),
		proj.Environment(), asInteger(proj.AllowsClinicalData()), stamp, stamp, stamp,
	).Scan(&version)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("%w: %q", ErrProjectSlugTaken, proj.Slug())
	case err != nil:
		return 0, fmt.Errorf("pocketbase: create project: %w", err)
	}

	return ProjectVersion(version), nil
}

// ByID reads one Project. An absent row is a clean miss, so a caller cannot carry
// a zero value onward as though it named one.
func (s *ProjectStore) ByID(
	ctx context.Context, id project.ID,
) (project.Project, ProjectVersion, bool, error) {
	if err := project.ValidateID(id); err != nil {
		return project.Project{}, 0, false, err
	}

	return scanProject(conn(ctx, s.db).QueryRowContext(ctx, readProject, string(id)))
}

// BySlug resolves the Project a request names. The slug is unique across the
// install, which is what makes a request's Project unambiguous.
func (s *ProjectStore) BySlug(
	ctx context.Context, slug string,
) (project.Project, ProjectVersion, bool, error) {
	if slug == "" {
		return project.Project{}, 0, false,
			fmt.Errorf("%w: a lookup names its slug", project.ErrMissingProjectName)
	}

	return scanProject(conn(ctx, s.db).QueryRowContext(ctx, readProjectBySlug, slug))
}

// UpdateState moves a Project under the version the caller last read. It reads
// first so the lifecycle runs against the persisted state, and carries every
// precondition into the statement so the decision cannot go stale.
func (s *ProjectStore) UpdateState(
	ctx context.Context, id project.ID, next project.State, expect ProjectVersion,
) (ProjectVersion, error) {
	proj, version, found, err := s.ByID(ctx, id)

	switch {
	case err != nil:
		return 0, err
	case !found:
		return 0, fmt.Errorf("%w: project %s", storage.ErrNotFound, id)
	case version != expect:
		return 0, fmt.Errorf("%w: project %s stands at version %d", storage.ErrVersionConflict, id, version)
	}

	if _, err := proj.TransitionTo(next); err != nil {
		return 0, err
	}

	stamp := time.Now().UTC().UnixMilli()

	var written int64

	err = conn(ctx, s.db).QueryRowContext(ctx, updateProjectState,
		string(next), stamp, stamp, string(id), int64(expect),
	).Scan(&written)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("%w: project %s moved since it was read", storage.ErrVersionConflict, id)
	case err != nil:
		return 0, fmt.Errorf("pocketbase: update project state: %w", err)
	}

	return ProjectVersion(written), nil
}

// scanProject rebuilds one row through the domain constructor matching its kind,
// so a row written around the constructors fails the read rather than reaching a
// caller as a usable Project.
func scanProject(src row) (project.Project, ProjectVersion, bool, error) {
	var (
		id, kind, slug, name, state, environment string
		clinical                                 bool
		version                                  int64
	)

	switch err := src.Scan(&id, &kind, &slug, &name, &state, &environment, &clinical, &version); {
	case errors.Is(err, sql.ErrNoRows):
		return project.Project{}, 0, false, nil
	case err != nil:
		return project.Project{}, 0, false, fmt.Errorf("pocketbase: scan project: %w", err)
	}

	cfg := project.Config{
		ID: project.ID(id), Slug: slug, Name: name, State: project.State(state),
		Environment: environment, AllowClinicalData: clinical,
	}

	var (
		rebuilt project.Project
		err     error
	)

	switch project.Kind(kind) {
	case project.KindSuper:
		rebuilt, err = project.NewSuperProjectRecord(cfg)
	case project.KindStandard:
		rebuilt, err = project.NewProject(cfg)
	default:
		return project.Project{}, 0, false, fmt.Errorf("%w: %s holds %q", project.ErrUnknownKind, id, kind)
	}

	if err != nil {
		return project.Project{}, 0, false, fmt.Errorf("pocketbase: rebuild project %s: %w", id, err)
	}

	return rebuilt, ProjectVersion(version), true, nil
}

// Instance reads the install record, reporting false when the migration has not
// provisioned one yet.
func (s *ProjectStore) Instance(ctx context.Context) (project.Instance, bool, error) {
	var (
		super, state, tokenHash string
		expires                 int64
	)

	switch err := conn(ctx, s.db).QueryRowContext(ctx, readInstance, instanceRow).Scan(
		&super, &state, &tokenHash, &expires); {
	case errors.Is(err, sql.ErrNoRows):
		return project.Instance{}, false, nil
	case err != nil:
		return project.Instance{}, false, fmt.Errorf("pocketbase: read the install record: %w", err)
	}

	if !project.BootstrapState(state).Valid() {
		return project.Instance{}, false, fmt.Errorf("%w: %q", ErrUnknownBootstrapState, state)
	}

	cfg := project.InstanceConfig{
		SuperProject: project.ID(super), State: project.BootstrapState(state), TokenHash: tokenHash,
	}

	if expires != 0 {
		cfg.TokenExpires = time.UnixMilli(expires).UTC()
	}

	instance, err := project.NewInstance(cfg)
	if err != nil {
		return project.Instance{}, false, fmt.Errorf("pocketbase: rebuild the install record: %w", err)
	}

	return instance, true, nil
}

// CreateInstance writes the Super Project and the install record together, in one
// transaction, so an install can never hold one without the other.
func (s *ProjectStore) CreateInstance(
	ctx context.Context, super project.SuperProject, instance project.Instance,
) error {
	record, err := project.NewSuperProjectRecord(project.Config{
		ID: super.ID(), Slug: super.Slug(), Name: super.Name(), State: super.State(),
	})
	if err != nil {
		return fmt.Errorf("pocketbase: describe the super project: %w", err)
	}

	return s.within(ctx, func(ctx context.Context) error {
		if _, err := s.insert(ctx, record); err != nil {
			return err
		}

		return s.writeInstance(ctx, instance)
	})
}

// writeInstance writes the install record, refusing rather than overwriting when
// one already exists. A second row would be a second chance to mint a Super Admin.
func (s *ProjectStore) writeInstance(ctx context.Context, instance project.Instance) error {
	stamp := time.Now().UTC().UnixMilli()
	expires, held := instance.TokenExpiresAt()

	var expiresArg, hashArg any
	if held {
		expiresArg, hashArg = expires.UTC().UnixMilli(), instance.TokenDigest()
	}

	var written string

	err := conn(ctx, s.db).QueryRowContext(ctx, createInstanceRow,
		instanceRow, schemaVersion, string(instance.SuperProject()), string(instance.State()),
		hashArg, expiresArg, stamp, stamp,
	).Scan(&written)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrInstanceExists
	case err != nil:
		return fmt.Errorf("pocketbase: create the install record: %w", err)
	}

	return nil
}

// CompleteClaim inserts the membership and spends the install in one transaction,
// applying nothing unless the row is still pending.
func (s *ProjectStore) CompleteClaim(ctx context.Context, claim project.Claim) error {
	return s.within(ctx, func(ctx context.Context) error {
		if err := writeMembership(ctx, s.db, claim.Membership, claim.At); err != nil {
			return err
		}

		var written string

		err := conn(ctx, s.db).QueryRowContext(ctx, spendClaim,
			claim.At.UTC().UnixMilli(), instanceRow).Scan(&written)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrClaimNoLongerPending
		case err != nil:
			return fmt.Errorf("pocketbase: spend the install claim: %w", err)
		}

		return nil
	})
}

// within runs work inside one transaction, joining the caller's if the context
// already carries one so a claim stays a single commit boundary either way.
func (s *ProjectStore) within(ctx context.Context, work func(ctx context.Context) error) error {
	if _, joined := ctx.Value(transactionKey{}).(*sql.Tx); joined {
		return work(ctx)
	}

	return NewResourceStore(s.db).WithinTransaction(ctx, work)
}
