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
	// ErrEmailTaken reports an address already claimed inside the realm the
	// identity belongs to. It is the unique index's own guarantee, surfaced as a
	// typed error rather than as a driver-shaped constraint violation.
	ErrEmailTaken = errors.New("pocketbase: the realm already holds this address")

	// ErrIdentityNotInvited reports a create past the invited state. Every
	// identity begins invited, because the move out of it is the one that sets a
	// credential, and a User hands out no hash a create could write.
	ErrIdentityNotInvited = errors.New("pocketbase: an identity is created in the invited state")

	// ErrEmailNotNormalised reports a row whose display address does not
	// normalise to the value beside it. Nothing but code ties the two columns
	// together, so a row written around NormaliseEmail is refused, not returned.
	ErrEmailNotNormalised = errors.New("pocketbase: stored address is not the normalisation of its display form")

	// ErrUserScopeMismatch reports a scope column outside the enum, or one that
	// contradicts the home Project beside it. The realm is computed from those
	// two columns, so a row disagreeing with itself is refused, never guessed at.
	ErrUserScopeMismatch = errors.New("pocketbase: user scope and home project disagree")
)

// credentialOnFile stands in for a hash no read ever fetches. A User answers
// only whether a credential exists, so the column is read as that one bit and
// the hash itself never enters a value a caller could log or serialise.
const credentialOnFile = project.PasswordHash("[credential on file]")

// userColumns is what every read selects, in the order scanUser reads them. The
// password hash appears only as the single fact the domain asks of it.
const userColumns = "id, scope, home_project_id, email_normalized, email_display," +
	" COALESCE(password_hash, '') <> '', state, mfa_required, version"

// UserVersion is the optimistic-concurrency counter users.version carries. It is
// not storage.VersionID, which names one immutable resource version rather than
// one generation of a mutable identity row.
type UserVersion int64

// UserStore persists identities on SQLite. It exposes no method that can change
// a scope or a home Project after Create, so an identity cannot move to another
// realm: one that belongs elsewhere is a new identity, never an updated one.
type UserStore struct {
	db *sql.DB
}

// NewUserStore binds a store to an open database. The caller owns the pool and
// is responsible for opening it with foreign keys enforced.
func NewUserStore(db *sql.DB) *UserStore {
	return &UserStore{db: db}
}

// Create inserts an identity at version 1. An address already claimed in the
// same realm is ErrEmailTaken and nothing is written, so a create can never take
// over a row the caller was not able to read.
func (s *UserStore) Create(ctx context.Context, user project.User) (UserVersion, error) {
	// The hash never leaves a User, so a create that had to carry one would write
	// a row contradicting its own state column. Invited is the only entry point
	// the lifecycle has, and it is the state that holds no credential.
	if user.State() != project.UserInvited {
		return 0, fmt.Errorf("%w: %s is %s", ErrIdentityNotInvited, user.ID(), user.State())
	}

	const insert = "INSERT INTO users" +
		" (id, scope, home_project_id, email_normalized, email_display," +
		" password_hash, state, mfa_required, created_at, updated_at, version)" +
		" VALUES (?, ?, ?, ?, ?, NULL, ?, ?, ?, ?, 1)" +
		" ON CONFLICT (identity_realm, email_normalized) DO NOTHING" +
		" RETURNING version"

	stamp := time.Now().UTC().UnixMilli()
	email := user.Email()

	var version int64

	err := conn(ctx, s.db).QueryRowContext(ctx, insert,
		string(user.ID()), string(user.Scope()), homeProjectArg(user),
		email.Normalized(), email.Display(),
		string(user.State()), asInteger(user.MFARequired()), stamp, stamp,
	).Scan(&version)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("%w: %s in %s", ErrEmailTaken, email.Normalized(), user.IdentityRealm())
	case err != nil:
		return 0, fmt.Errorf("pocketbase: create user: %w", err)
	}

	return UserVersion(version), nil
}

// ByID reads one identity, in whatever realm it belongs to. An absent row is a
// clean miss: found is false and no User is returned to read, so a caller cannot
// carry a zero value onward as though it identified someone.
func (s *UserStore) ByID(ctx context.Context, id project.UserID) (project.User, UserVersion, bool, error) {
	if id == "" {
		return project.User{}, 0, false, fmt.Errorf("%w: user", project.ErrMissingID)
	}

	const query = "SELECT " + userColumns + " FROM users WHERE id = ?"

	return scanUser(conn(ctx, s.db).QueryRowContext(ctx, query, string(id)))
}

// ByRealmEmail resolves one identity inside one realm. The realm is a required
// argument and the address is an Email only NormaliseEmail can have produced, so
// no caller can search an address across realms or against a raw spelling.
func (s *UserStore) ByRealmEmail(
	ctx context.Context,
	realm project.IdentityRealm,
	email project.Email,
) (project.User, UserVersion, bool, error) {
	if realm == "" {
		return project.User{}, 0, false, fmt.Errorf("%w: a lookup names its realm", project.ErrInvalidProjectID)
	}

	if email.IsZero() {
		return project.User{}, 0, false, fmt.Errorf("%w: lookup", project.ErrMissingEmail)
	}

	const query = "SELECT " + userColumns + " FROM users WHERE identity_realm = ? AND email_normalized = ?"

	return scanUser(conn(ctx, s.db).QueryRowContext(ctx, query, string(realm), email.Normalized()))
}

// UpdateState moves an identity to its next lifecycle state under the version
// the caller last read. Activating an invitation is refused here: that move sets
// a credential, and it travels through the acceptance path or not at all.
func (s *UserStore) UpdateState(
	ctx context.Context,
	id project.UserID,
	next project.UserState,
	expect UserVersion,
) (UserVersion, error) {
	if next == project.UserInvited {
		return 0, fmt.Errorf("%w: nothing returns to %s", project.ErrInvalidTransition, next)
	}

	user, version, found, err := s.ByID(ctx, id)

	switch {
	case err != nil:
		return 0, err
	case !found:
		return 0, fmt.Errorf("%w: user %s", storage.ErrNotFound, id)
	case version != expect:
		return 0, fmt.Errorf("%w: user %s stands at version %d", storage.ErrVersionConflict, id, version)
	}

	if _, err := user.TransitionTo(next); err != nil {
		return 0, err
	}

	// The table accepts any state beside any credential, so re-enabling an
	// identity revoked before it ever accepted its invitation would write an
	// active row with no password. It is refused here and again in the statement.
	if next == project.UserActive && !user.HasCredential() {
		return 0, fmt.Errorf("%w: %s holds no credential to re-enable", project.ErrInvalidCredentialState, id)
	}

	return s.writeState(ctx, id, next, expect)
}

// writeState carries every precondition the move was decided under: the id, the
// observed version, and a credential whenever the target is active. A row that
// changed since the read matches nothing and is refused, never overwritten.
func (s *UserStore) writeState(
	ctx context.Context,
	id project.UserID,
	next project.UserState,
	expect UserVersion,
) (UserVersion, error) {
	const update = "UPDATE users SET state = ?, updated_at = ?, version = version + 1" +
		" WHERE id = ? AND version = ?" +
		" AND (? <> 'active' OR COALESCE(password_hash, '') <> '')" +
		" RETURNING version"

	var version int64

	err := conn(ctx, s.db).QueryRowContext(ctx, update,
		string(next), time.Now().UTC().UnixMilli(),
		string(id), int64(expect), string(next),
	).Scan(&version)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("%w: user %s moved since it was read", storage.ErrVersionConflict, id)
	case err != nil:
		return 0, fmt.Errorf("pocketbase: update user state: %w", err)
	}

	return UserVersion(version), nil
}

// userRow is one users row as the reads select it: every column the domain needs
// to rebuild an identity, and the password hash only as whether one exists.
type userRow struct {
	id            string
	scope         string
	homeProject   sql.NullString
	normalized    string
	display       string
	credentialled bool
	state         string
	mfaRequired   bool
	version       int64
}

// scanUser reads one row and rebuilds the identity it describes. No row is a
// clean miss rather than an error, because an address that exists nowhere and
// one that exists in another realm must be indistinguishable to a caller.
func scanUser(src row) (project.User, UserVersion, bool, error) {
	var scanned userRow

	switch err := src.Scan(
		&scanned.id, &scanned.scope, &scanned.homeProject,
		&scanned.normalized, &scanned.display, &scanned.credentialled,
		&scanned.state, &scanned.mfaRequired, &scanned.version,
	); {
	case errors.Is(err, sql.ErrNoRows):
		return project.User{}, 0, false, nil
	case err != nil:
		return project.User{}, 0, false, fmt.Errorf("pocketbase: scan user: %w", err)
	}

	user, err := scanned.user()
	if err != nil {
		return project.User{}, 0, false, err
	}

	return user, UserVersion(scanned.version), true, nil
}

// user rebuilds the identity through the domain constructors, so every invariant
// the table cannot express is re-checked on the way out and a row written around
// them fails the read instead of reaching a caller as a usable identity.
func (r userRow) user() (project.User, error) {
	email, err := r.email()
	if err != nil {
		return project.User{}, err
	}

	cfg := project.UserConfig{
		ID: project.UserID(r.id), Email: email, PasswordHash: r.credential(),
		State: project.UserState(r.state), MFARequired: r.mfaRequired,
	}

	switch project.UserScope(r.scope) {
	case project.ScopeServer:
		if r.homeProject.Valid {
			return project.User{}, fmt.Errorf("%w: %s is server-scoped and names %s",
				ErrUserScopeMismatch, r.id, r.homeProject.String)
		}

		return project.NewServerUser(cfg)
	case project.ScopeProject:
		return project.NewProjectUser(project.ID(r.homeProject.String), cfg)
	default:
		return project.User{}, fmt.Errorf("%w: %q for %s", ErrUserScopeMismatch, r.scope, r.id)
	}
}

// email re-derives the normalised address from the display column and refuses a
// row where the two disagree. Only code keeps them in step, so a row inserted
// around NormaliseEmail would otherwise answer a lookup it never matched.
func (r userRow) email() (project.Email, error) {
	email, err := project.NormaliseEmail(r.display)
	if err != nil {
		return project.Email{}, fmt.Errorf("pocketbase: user %s: %w", r.id, err)
	}

	if email.Normalized() != r.normalized {
		return project.Email{}, fmt.Errorf("%w: %s", ErrEmailNotNormalised, r.id)
	}

	return email, nil
}

// credential reports the stored hash as the one fact the domain asks of it. The
// hash is never selected, so it cannot reach a log line through a User.
func (r userRow) credential() project.PasswordHash {
	if !r.credentialled {
		return ""
	}

	return credentialOnFile
}

// homeProjectArg binds the owning Project, or SQL NULL for a server-scoped
// identity, which is what makes the generated realm column read 'system'.
func homeProjectArg(user project.User) any {
	home, owned := user.HomeProject()
	if !owned {
		return nil
	}

	return string(home)
}

// asInteger binds a Go bool to the 0/1 integer SQLite stores booleans as.
func asInteger(set bool) int {
	if set {
		return 1
	}

	return 0
}
