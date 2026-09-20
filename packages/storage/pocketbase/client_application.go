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
	// ErrClientApplicationNameTaken reports a name the Project already registers.
	// It is the unique index's own guarantee, surfaced as a typed error rather
	// than as a driver-shaped constraint violation.
	ErrClientApplicationNameTaken = errors.New("pocketbase: the project already registers this name")

	// ErrRegistrationNotActive reports a create past the active state. Active is
	// the only entry point a registration has, because a revoked one is a row
	// nobody can revive.
	ErrRegistrationNotActive = errors.New("pocketbase: a registration is created in the active state")

	// ErrCredentialAlreadyLive reports a second active credential for one
	// application. Rotation supersedes the live one first, so two current secrets
	// is a window nobody is tracking and the index refuses it.
	ErrCredentialAlreadyLive = errors.New("pocketbase: the application already holds a live credential")

	// ErrSecretNotMinted reports a write carrying the sentinel a read rebuilds a
	// credential with. A hash no secret hashes to would answer nothing forever, so
	// it is refused rather than written.
	ErrSecretNotMinted = errors.New("pocketbase: a credential is written with a minted hash, never a rebuilt one")
)

// secretOnFile stands in for a hash no read ever fetches, mirroring
// credentialOnFile. A Credential answers only whether live material exists, so
// the column is read as that one bit and the hash never enters a value a caller
// could log or serialise.
const secretOnFile = project.CredentialHash("[secret on file]")

// clientApplicationColumns is what every read selects, in the order the scan
// reads them.
const clientApplicationColumns = "id, name, description, state, kind," +
	" COALESCE(jwks, ''), version"

// credentialColumns is what every credential read selects. The hash appears only
// as the single fact the domain asks of it; no projection in this package selects
// the column itself.
const credentialColumns = "id, client_application_id," +
	" COALESCE(secret_hash, '') <> '', state, created_at, expires_at, COALESCE(revoked_at, 0)"

const (
	createApplication = "INSERT INTO client_applications" +
		" (project_id, id, name, description, state, kind," +
		" created_at, updated_at, revoked_at, version)" +
		" VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, 1)" +
		" ON CONFLICT (project_id, name) DO NOTHING" +
		" RETURNING version"

	createRedirectURI = "INSERT INTO client_redirect_uris" +
		" (project_id, client_application_id, uri, created_at) VALUES (?, ?, ?, ?)"

	// Ordered, so a registration reads back the way it was written and two reads
	// of one registration compare equal.
	readRedirectURIs = "SELECT uri FROM client_redirect_uris" +
		" WHERE project_id = ? AND client_application_id = ? ORDER BY uri"

	readApplication = "SELECT " + clientApplicationColumns +
		" FROM client_applications WHERE project_id = ? AND id = ?"

	// The revocation instant rides along in the statement that sets the state, so
	// no row observes a revoked registration that never died.
	updateApplicationState = "UPDATE client_applications" +
		" SET state = ?, updated_at = ?," +
		" revoked_at = CASE WHEN ? = 'revoked' THEN ? ELSE revoked_at END," +
		" version = version + 1" +
		" WHERE project_id = ? AND id = ? AND version = ?" +
		" RETURNING version"

	issueCredentialStatement = "INSERT INTO client_application_credentials" +
		" (project_id, client_application_id, id, secret_hash, state, created_at, expires_at, revoked_at)" +
		" VALUES (?, ?, ?, ?, 'active', ?, ?, NULL)" +
		" ON CONFLICT (project_id, client_application_id) WHERE state = 'active'" +
		" DO NOTHING RETURNING id"

	supersedeCredentialStatement = "UPDATE client_application_credentials" +
		" SET state = 'superseded', expires_at = ?" +
		" WHERE project_id = ? AND client_application_id = ? AND id = ? AND state = 'active'" +
		" RETURNING id"

	revokeCredentialStatement = "UPDATE client_application_credentials" +
		" SET state = 'revoked', secret_hash = NULL, revoked_at = ?" +
		" WHERE project_id = ? AND client_application_id = ? AND id = ? AND state <> 'revoked'" +
		" RETURNING id"

	revokeEveryCredential = "UPDATE client_application_credentials" +
		" SET state = 'revoked', secret_hash = NULL, revoked_at = ?" +
		" WHERE project_id = ? AND client_application_id = ? AND state <> 'revoked'"

	listCredentials = "SELECT " + credentialColumns + " FROM client_application_credentials" +
		" WHERE project_id = ? AND client_application_id = ? ORDER BY created_at, id"

	// The hash is selected here because this is the comparison, the same way a
	// session resolve selects the token digest it is about to compare. It is the
	// only other projection in this package that reads one, and it never leaves
	// the method below.
	provableCredentials = "SELECT id, COALESCE(secret_hash, ''), state," +
		" created_at, expires_at, COALESCE(revoked_at, 0)" +
		" FROM client_application_credentials" +
		" WHERE project_id = ? AND client_application_id = ? AND state <> 'revoked'"
)

// ClientApplicationVersion is the optimistic-concurrency counter
// client_applications.version carries. It is not storage.VersionID, which names
// one immutable resource version, and it is not UserVersion: one row's generation
// is never another row's precondition.
type ClientApplicationVersion int64

// ClientApplicationStore persists client applications and their credentials on
// SQLite. It exposes no method that can change an owning Project after Create, so
// a registration that belongs elsewhere is a new registration — and none that can
// return a stored secret, so this package cannot authenticate anyone.
type ClientApplicationStore struct {
	db *sql.DB
}

// NewClientApplicationStore binds a store to an open database. The caller owns
// the pool and is responsible for opening it with foreign keys enforced.
func NewClientApplicationStore(db *sql.DB) *ClientApplicationStore {
	return &ClientApplicationStore{db: db}
}

// Create registers a client application at version 1. A name already taken inside
// the Project is ErrClientApplicationNameTaken and nothing is written, so a create
// can never take over a registration the caller was not able to read.
func (s *ClientApplicationStore) Create(
	ctx context.Context, app project.ClientApplication,
) (ClientApplicationVersion, error) {
	if app.State() != project.ServiceActive {
		return 0, fmt.Errorf("%w: %s is %s", ErrRegistrationNotActive, app.ID(), app.State())
	}

	stamp := time.Now().UTC().UnixMilli()

	var version int64

	err := conn(ctx, s.db).QueryRowContext(ctx, createApplication,
		string(app.Project()), string(app.ID()), app.Name(), app.Description(),
		string(app.State()), string(app.Kind()), app.JWKS().Document(), stamp, stamp,
	).Scan(&version)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("%w: %q in %s", ErrClientApplicationNameTaken, app.Name(), app.Project())
	case err != nil:
		return 0, fmt.Errorf("pocketbase: create client application: %w", err)
	}

	// Written in whatever transaction the caller opened. A registration whose
	// addresses failed to land would be one that redeems no code, and a caller
	// that read it back would see a client which had registered none — so the
	// two go together or neither does.
	for _, address := range app.RedirectURIs().Stated() {
		if _, err := conn(ctx, s.db).ExecContext(ctx, createRedirectURI,
			string(app.Project()), string(app.ID()), string(address), stamp,
		); err != nil {
			return 0, fmt.Errorf("pocketbase: register redirect uri for %s: %w", app.ID(), err)
		}
	}

	return ClientApplicationVersion(version), nil
}

// redirectURIs reads the addresses one registration named.
func (s *ClientApplicationStore) redirectURIs(
	ctx context.Context, proj project.ID, id project.ClientApplicationID,
) (project.RedirectURIs, error) {
	rows, err := conn(ctx, s.db).QueryContext(ctx, readRedirectURIs, string(proj), string(id))
	if err != nil {
		return project.RedirectURIs{}, fmt.Errorf("pocketbase: read redirect uris for %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()

	var stated []string

	for rows.Next() {
		var address string
		if err := rows.Scan(&address); err != nil {
			return project.RedirectURIs{}, fmt.Errorf("pocketbase: scan redirect uri for %s: %w", id, err)
		}

		stated = append(stated, address)
	}

	if err := rows.Err(); err != nil {
		return project.RedirectURIs{}, fmt.Errorf("pocketbase: read redirect uris for %s: %w", id, err)
	}

	// Rebuilt through the same constructor a fresh registration goes through, so
	// an address nothing could have written is refused rather than served as one
	// a code may be handed back to.
	held, err := project.NewRedirectURIs(stated...)
	if err != nil {
		return project.RedirectURIs{}, fmt.Errorf("pocketbase: rebuild redirect uris for %s: %w", id, err)
	}

	return held, nil
}

// ByID reads one registration inside one Project. An absent row is a clean miss,
// so a caller cannot carry a zero value onward as though it named one.
func (s *ClientApplicationStore) ByID(
	ctx context.Context, proj project.ID, id project.ClientApplicationID,
) (project.ClientApplication, ClientApplicationVersion, bool, error) {
	if err := project.ValidateID(proj); err != nil {
		return project.ClientApplication{}, 0, false, err
	}

	if err := project.ValidateClientApplicationID(id); err != nil {
		return project.ClientApplication{}, 0, false, err
	}

	var (
		scannedID, name, description, state, kind, keys string
		version                                         int64
	)

	err := conn(ctx, s.db).QueryRowContext(ctx, readApplication, string(proj), string(id)).Scan(
		&scannedID, &name, &description, &state, &kind, &keys, &version)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return project.ClientApplication{}, 0, false, nil
	case err != nil:
		return project.ClientApplication{}, 0, false, fmt.Errorf("pocketbase: read client application: %w", err)
	}

	// Read with the registration rather than on demand, so nothing holds a
	// ClientApplication whose addresses it has not looked up: a registration that
	// answered "no address registered" because nobody fetched them would refuse
	// every redirect rather than the wrong ones, but it would refuse them for a
	// reason no operator could find.
	addresses, err := s.redirectURIs(ctx, proj, project.ClientApplicationID(scannedID))
	if err != nil {
		return project.ClientApplication{}, 0, false, err
	}

	// Rebuilt through the same constructor a fresh registration goes through, so
	// a key set nothing could have written is refused rather than verified
	// against.
	registered, err := project.ParseJWKS(keys)
	if err != nil {
		return project.ClientApplication{}, 0, false,
			fmt.Errorf("pocketbase: rebuild the keys of %s: %w", scannedID, err)
	}

	app, err := project.NewClientApplication(proj, project.ClientApplicationConfig{
		ID: project.ClientApplicationID(scannedID), Name: name,
		Description: description, State: project.ServiceState(state),
		Kind: project.ClientKind(kind), RedirectURIs: addresses, JWKS: registered,
	})
	if err != nil {
		return project.ClientApplication{}, 0, false,
			fmt.Errorf("pocketbase: rebuild client application %s in %s: %w", scannedID, proj, err)
	}

	return app, ClientApplicationVersion(version), true, nil
}

// UpdateState moves a registration under the version the caller last read. It
// reads first so the lifecycle runs against the persisted state, and carries every
// precondition into the statement so the decision cannot go stale.
func (s *ClientApplicationStore) UpdateState(
	ctx context.Context,
	proj project.ID,
	id project.ClientApplicationID,
	next project.ServiceState,
	expect ClientApplicationVersion,
) (ClientApplicationVersion, error) {
	app, version, found, err := s.ByID(ctx, proj, id)

	switch {
	case err != nil:
		return 0, err
	case !found:
		return 0, fmt.Errorf("%w: client application %s", storage.ErrNotFound, id)
	case version != expect:
		return 0, fmt.Errorf("%w: client application %s stands at version %d",
			storage.ErrVersionConflict, id, version)
	}

	if _, err := app.TransitionTo(next); err != nil {
		return 0, err
	}

	stamp := time.Now().UTC().UnixMilli()

	var written int64

	err = conn(ctx, s.db).QueryRowContext(ctx, updateApplicationState,
		string(next), stamp, string(next), stamp, string(proj), string(id), int64(expect),
	).Scan(&written)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("%w: client application %s moved since it was read", storage.ErrVersionConflict, id)
	case err != nil:
		return 0, fmt.Errorf("pocketbase: update client application state: %w", err)
	}

	return ClientApplicationVersion(written), nil
}

// IssueCredential writes one minted credential. A second active credential
// matches nothing and is refused, so an application never accumulates more
// secrets than a rotation needs.
func (s *ClientApplicationStore) IssueCredential(ctx context.Context, credential project.Credential) error {
	hash := credential.StoredHash()

	// A credential a read rebuilt carries the sentinel rather than a digest, so
	// writing one back would store a hash no secret could ever match.
	if hash == secretOnFile || !credential.HasSecret() {
		return fmt.Errorf("%w: %s", ErrSecretNotMinted, credential.ID())
	}

	var written string

	err := conn(ctx, s.db).QueryRowContext(ctx, issueCredentialStatement,
		string(credential.Project()), string(credential.ClientApplication()), string(credential.ID()),
		string(hash), credential.CreatedAt().UnixMilli(), credential.ExpiresAt().UnixMilli(),
	).Scan(&written)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s", ErrCredentialAlreadyLive, credential.ClientApplication())
	case err != nil:
		return fmt.Errorf("pocketbase: issue credential: %w", err)
	}

	return nil
}

// Supersede marks the live credential a rotation's outgoing half, dying at the
// instant given. It is the first half of a rotation, so a torn one leaves a
// working outgoing secret rather than two nobody is tracking.
func (s *ClientApplicationStore) Supersede(
	ctx context.Context,
	proj project.ID,
	client project.ClientApplicationID,
	id project.CredentialID,
	expiresAt time.Time,
) error {
	if expiresAt.IsZero() {
		return fmt.Errorf("%w: %s closes at no instant", project.ErrInvalidCredentialExpiry, id)
	}

	var written string

	err := conn(ctx, s.db).QueryRowContext(ctx, supersedeCredentialStatement,
		expiresAt.UTC().UnixMilli(), string(proj), string(client), string(id),
	).Scan(&written)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: no live credential %s for %s", storage.ErrNotFound, id, client)
	case err != nil:
		return fmt.Errorf("pocketbase: supersede credential: %w", err)
	}

	return nil
}

// RevokeCredential destroys one credential's material. The hash is written NULL in
// the statement that sets the state, so nothing observes a revoked credential that
// still holds something to compare against.
func (s *ClientApplicationStore) RevokeCredential(
	ctx context.Context,
	proj project.ID,
	client project.ClientApplicationID,
	id project.CredentialID,
	at time.Time,
) error {
	if at.IsZero() {
		return fmt.Errorf("%w: %s revoked at no instant", project.ErrInvalidSecretState, id)
	}

	var written string

	err := conn(ctx, s.db).QueryRowContext(ctx, revokeCredentialStatement,
		at.UTC().UnixMilli(), string(proj), string(client), string(id),
	).Scan(&written)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: no live credential %s for %s", storage.ErrNotFound, id, client)
	case err != nil:
		return fmt.Errorf("pocketbase: revoke credential: %w", err)
	}

	return nil
}

// RevokeAllCredentials destroys every live secret an application holds, which is
// the answer to a leak: one statement, no window, no ordering to get wrong.
func (s *ClientApplicationStore) RevokeAllCredentials(
	ctx context.Context, proj project.ID, client project.ClientApplicationID, at time.Time,
) (int64, error) {
	if at.IsZero() {
		return 0, fmt.Errorf("%w: %s revoked at no instant", project.ErrInvalidSecretState, client)
	}

	result, err := conn(ctx, s.db).ExecContext(ctx, revokeEveryCredential,
		at.UTC().UnixMilli(), string(proj), string(client))
	if err != nil {
		return 0, fmt.Errorf("pocketbase: revoke every credential: %w", err)
	}

	destroyed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("pocketbase: count revoked credentials: %w", err)
	}

	return destroyed, nil
}

// Credentials lists an application's credentials without their material, so an
// administrative view can show a rotation's progress and read no secret.
func (s *ClientApplicationStore) Credentials(
	ctx context.Context, proj project.ID, client project.ClientApplicationID,
) ([]project.Credential, error) {
	if err := project.ValidateID(proj); err != nil {
		return nil, err
	}

	rows, err := conn(ctx, s.db).QueryContext(ctx, listCredentials, string(proj), string(client))
	if err != nil {
		return nil, fmt.Errorf("pocketbase: read credentials of %s: %w", client, err)
	}
	defer func() { _ = rows.Close() }()

	var credentials []project.Credential

	for rows.Next() {
		credential, err := scanCredential(proj, rows)
		if err != nil {
			return nil, err
		}

		credentials = append(credentials, credential)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pocketbase: read credentials of %s: %w", client, err)
	}

	return credentials, nil
}

// scanCredential rebuilds one row through the domain constructor, so a row
// written around it fails the read rather than reaching a caller as a usable
// credential.
func scanCredential(proj project.ID, src row) (project.Credential, error) {
	var (
		id, client, state             string
		held                          bool
		createdAt, expiresAt, revoked int64
	)

	if err := src.Scan(&id, &client, &held, &state, &createdAt, &expiresAt, &revoked); err != nil {
		return project.Credential{}, fmt.Errorf("pocketbase: scan credential: %w", err)
	}

	record := project.CredentialRecord{
		ID:     project.CredentialID(id),
		Client: project.ClientApplicationID(client),
		State:  project.CredentialState(state),

		// The hash is never selected, so a rebuilt credential carries the sentinel
		// and IssueCredential refuses to write it back.
		Hash:      heldSecret(held),
		CreatedAt: time.UnixMilli(createdAt).UTC(),
		ExpiresAt: time.UnixMilli(expiresAt).UTC(),
	}

	if revoked != 0 {
		record.RevokedAt = time.UnixMilli(revoked).UTC()
	}

	credential, err := project.NewCredential(proj, record)
	if err != nil {
		return project.Credential{}, fmt.Errorf("pocketbase: rebuild credential %s: %w", id, err)
	}

	return credential, nil
}

// heldSecret reports the stored hash as the one fact the domain asks of it.
func heldSecret(held bool) project.CredentialHash {
	if !held {
		return ""
	}

	return secretOnFile
}

// ProvesSecret reports whether a presented secret is one this registration
// currently holds.
//
// The comparison happens here rather than in a caller because Credentials
// deliberately rebuilds every record with a sentinel in place of the hash: no
// read in this package hands a digest out, so nothing outside could perform this
// comparison even if it wanted to. That is the same rule a session resolve
// follows, and this is the second and last place it is bent.
//
// A registration holding no live credential proves nothing, which is the answer
// a revoked secret leaves behind: revocation destroys the material, so there is
// nothing left to match.
func (s *ClientApplicationStore) ProvesSecret(
	ctx context.Context, proj project.ID, client project.ClientApplicationID,
	secret project.ClientSecret, now time.Time,
) (bool, error) {
	if err := project.ValidateID(proj); err != nil {
		return false, err
	}

	if err := project.ValidateClientApplicationID(client); err != nil {
		return false, err
	}

	rows, err := conn(ctx, s.db).QueryContext(ctx, provableCredentials, string(proj), string(client))
	if err != nil {
		return false, fmt.Errorf("pocketbase: read credentials for %s: %w", client, err)
	}
	defer func() { _ = rows.Close() }()

	proved := false

	for rows.Next() {
		var (
			id, hash, state                 string
			createdAt, expiresAt, revokedAt int64
		)

		if err := rows.Scan(&id, &hash, &state, &createdAt, &expiresAt, &revokedAt); err != nil {
			return false, fmt.Errorf("pocketbase: scan credential for %s: %w", client, err)
		}

		record := project.CredentialRecord{
			ID: project.CredentialID(id), Client: client,
			Hash: project.CredentialHash(hash), State: project.CredentialState(state),
			CreatedAt: time.UnixMilli(createdAt).UTC(), ExpiresAt: time.UnixMilli(expiresAt).UTC(),
		}

		if revokedAt != 0 {
			record.RevokedAt = time.UnixMilli(revokedAt).UTC()
		}

		credential, err := project.NewCredential(proj, record)
		if err != nil {
			return false, fmt.Errorf("pocketbase: rebuild credential %s: %w", id, err)
		}

		// Every credential is compared, and the loop does not stop at the first
		// match: stopping early makes the work depend on which secret was
		// presented, and the comparison inside is constant time precisely so
		// that it does not.
		if credential.Matches(secret, now) {
			proved = true
		}
	}

	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("pocketbase: read credentials for %s: %w", client, err)
	}

	return proved, nil
}

const (
	// Spending is the insert. A jti already present conflicts and writes
	// nothing, which is the whole of replay detection: there is no read to race
	// against, because the write is the check.
	spendAssertion = "INSERT INTO client_assertion_jtis" +
		" (project_id, client_application_id, jti, expires_at) VALUES (?, ?, ?, ?)" +
		" ON CONFLICT (project_id, client_application_id, jti) DO NOTHING" +
		" RETURNING jti"

	sweepSpentAssertions = "DELETE FROM client_assertion_jtis WHERE expires_at <= ?"
)

// SpendAssertion records that one client assertion has been used, and reports
// whether this was the first time.
//
// The write is the check. A read that asked "has this jti been seen?" and an
// insert that recorded it would leave a window two concurrent presentations both
// passed through, and an assertion used twice is the one thing a jti exists to
// stop — so the conflict does the deciding, and a second presentation writes
// nothing and is told so.
func (s *ClientApplicationStore) SpendAssertion(
	ctx context.Context, proj project.ID, held project.ClientAssertion,
) (bool, error) {
	if err := project.ValidateID(proj); err != nil {
		return false, err
	}

	var spent string

	err := conn(ctx, s.db).QueryRowContext(ctx, spendAssertion,
		string(proj), string(held.Client()), held.ID(), held.ExpiresAt().UnixMilli(),
	).Scan(&spent)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("pocketbase: spend the assertion of %s: %w", held.Client(), err)
	}

	return true, nil
}

// SweepAssertions forgets the jtis whose assertions have expired. After that the
// expiry refuses them, and remembering one longer would be remembering something
// nothing can present.
func (s *ClientApplicationStore) SweepAssertions(
	ctx context.Context, now time.Time,
) (int64, error) {
	result, err := conn(ctx, s.db).ExecContext(ctx, sweepSpentAssertions, now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("pocketbase: sweep spent assertions: %w", err)
	}

	swept, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("pocketbase: count swept assertions: %w", err)
	}

	return swept, nil
}
