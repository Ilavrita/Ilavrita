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

// ErrSessionTokenTaken reports a minted token the install already holds. Thirty-
// two bytes of entropy make a collision unreachable in practice, so this is a
// broken entropy source rather than bad luck, and it is refused rather than
// retried with a second draw from the same reader.
var ErrSessionTokenTaken = errors.New("pocketbase: the install already holds this session token")

// sessionColumns is what a resolve selects. The digest is selected here because
// this is the comparison: it is the only projection in this package that reads
// one, and it never leaves the method.
const sessionColumns = "project_id, id, user_id, membership_id," +
	" COALESCE(token_hash, ''), state," +
	// Absence is NULL in the table and the empty string in the record, and the
	// two mean the same thing: a login nobody's app holds, narrowed by nothing.
	" COALESCE(launch_patient, ''), COALESCE(granted_scopes, '')," +
	" COALESCE(refresh_chain, '')," +
	" created_at, expires_at, COALESCE(revoked_at, 0)"

const (
	createSession = "INSERT INTO sessions" +
		" (project_id, id, token_hash, user_id, membership_id, state," +
		" launch_patient, granted_scopes, refresh_chain," +
		" created_at, expires_at, revoked_at)" +
		// NULLIF writes absence as NULL rather than as an empty string the
		// CHECK refuses, so the record's zero value and the row's agree.
		" VALUES (?, ?, ?, ?, ?, 'active', NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?, NULL)" +
		// The uniqueness index is partial, so the conflict target repeats its
		// predicate: without it SQLite matches no index and refuses the statement.
		" ON CONFLICT (token_hash) WHERE token_hash IS NOT NULL DO NOTHING" +
		" RETURNING id"

	// The digest is the lookup key, so this is the one statement in the package
	// that carries no tenant predicate: the row it finds is what states the
	// Project every later query is bound by.
	resolveSession = "SELECT " + sessionColumns + " FROM sessions WHERE token_hash = ?"

	revokeSession = "UPDATE sessions SET state = 'revoked', token_hash = NULL, revoked_at = ?" +
		" WHERE project_id = ? AND id = ? AND state <> 'revoked'" +
		" RETURNING id"

	revokeEverySession = "UPDATE sessions SET state = 'revoked', token_hash = NULL, revoked_at = ?" +
		" WHERE project_id = ? AND user_id = ? AND state <> 'revoked'"
)

// SessionStore issues and resolves sessions. It is what turns a later request
// into a principal, and the only place a presented token is compared.
type SessionStore struct {
	db *sql.DB
}

// NewSessionStore binds a store to an open database. The caller owns the pool and
// is responsible for opening it with foreign keys enforced.
func NewSessionStore(db *sql.DB) *SessionStore {
	return &SessionStore{db: db}
}

// Issue writes one minted session. The token itself is never written: only its
// digest is, so a stolen database yields nothing a caller could present.
func (s *SessionStore) Issue(ctx context.Context, session project.Session) error {
	if session.Digest() == "" {
		return fmt.Errorf("%w: %s carries no token", project.ErrInvalidSession, session.ID())
	}

	var written string

	launch := session.Launch()

	err := conn(ctx, s.db).QueryRowContext(ctx, createSession,
		string(session.Project()), string(session.ID()), session.Digest(),
		string(session.User()), string(session.Membership()),
		launch.Patient(), launch.Scopes(), string(session.RefreshChain()),
		session.CreatedAt().UnixMilli(), session.ExpiresAt().UnixMilli(),
	).Scan(&written)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s", ErrSessionTokenTaken, session.ID())
	case err != nil:
		return fmt.Errorf("pocketbase: issue session: %w", err)
	}

	return nil
}

// Resolve turns a presented token into the session it names, or into nothing. An
// unknown token, a revoked one and an expired one are the same answer: found is
// false and no error, because telling them apart tells a caller which tokens
// exist.
func (s *SessionStore) Resolve(
	ctx context.Context, token project.SessionToken, now time.Time,
) (project.Session, bool, error) {
	if token.IsZero() {
		return project.Session{}, false, nil
	}

	var (
		proj, id, user, membership, digest, state string
		launchPatient, grantedScopes, chain       string
		createdAt, expiresAt, revokedAt           int64
	)

	switch err := conn(ctx, s.db).QueryRowContext(ctx, resolveSession, token.Digest()).Scan(
		&proj, &id, &user, &membership, &digest, &state,
		&launchPatient, &grantedScopes, &chain,
		&createdAt, &expiresAt, &revokedAt); {
	case errors.Is(err, sql.ErrNoRows):
		return project.Session{}, false, nil
	case err != nil:
		return project.Session{}, false, fmt.Errorf("pocketbase: resolve session: %w", err)
	}

	record := project.SessionRecord{
		ID: project.SessionID(id), User: project.UserID(user),
		Membership: project.MembershipID(membership), Digest: digest,
		State:         project.SessionState(state),
		LaunchPatient: launchPatient, GrantedScopes: grantedScopes,
		RefreshChain: project.RefreshChainID(chain),
		CreatedAt:    time.UnixMilli(createdAt).UTC(), ExpiresAt: time.UnixMilli(expiresAt).UTC(),
	}

	if revokedAt != 0 {
		record.RevokedAt = time.UnixMilli(revokedAt).UTC()
	}

	session, err := project.NewSession(project.ID(proj), record)
	if err != nil {
		return project.Session{}, false, fmt.Errorf("pocketbase: rebuild session %s: %w", id, err)
	}

	// The comparison and the clock travel together, so an expired session answers
	// nothing however healthy its row looks.
	if !session.Matches(token, now) {
		return project.Session{}, false, nil
	}

	return session, true, nil
}

// Revoke destroys one session's material. The digest is written NULL in the
// statement that sets the state, so nothing observes a revoked session that still
// holds something to compare against.
func (s *SessionStore) Revoke(
	ctx context.Context, proj project.ID, id project.SessionID, at time.Time,
) error {
	if at.IsZero() {
		return fmt.Errorf("%w: %s revoked at no instant", project.ErrInvalidSession, id)
	}

	var written string

	err := conn(ctx, s.db).QueryRowContext(ctx, revokeSession,
		at.UTC().UnixMilli(), string(proj), string(id)).Scan(&written)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: no live session %s in %s", storage.ErrNotFound, id, proj)
	case err != nil:
		return fmt.Errorf("pocketbase: revoke session: %w", err)
	}

	return nil
}

// RevokeEveryUserSession signs one identity out everywhere in one Project, which
// is the answer to a stolen token: one statement, no window, no token to name.
func (s *SessionStore) RevokeEveryUserSession(
	ctx context.Context, proj project.ID, user project.UserID, at time.Time,
) (int64, error) {
	if at.IsZero() {
		return 0, fmt.Errorf("%w: %s revoked at no instant", project.ErrInvalidSession, user)
	}

	result, err := conn(ctx, s.db).ExecContext(ctx, revokeEverySession,
		at.UTC().UnixMilli(), string(proj), string(user))
	if err != nil {
		return 0, fmt.Errorf("pocketbase: revoke every session: %w", err)
	}

	revoked, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("pocketbase: count revoked sessions: %w", err)
	}

	return revoked, nil
}

// Live reports whether one session is still one a request may be served as.
//
// It answers from the row rather than from a token, because a socket bound
// earlier holds no token to present again — and must not: a credential kept in
// memory for the life of a connection is one a crash dump carries. What it
// still holds is which session it was, and that is enough to ask whether that
// session is still there.
func (s *SessionStore) Live(
	ctx context.Context, owner project.ID, id project.SessionID, now time.Time,
) (bool, error) {
	const query = "SELECT state, expires_at FROM sessions WHERE project_id = ? AND id = ?"

	var (
		state     string
		expiresAt int64
	)

	switch err := conn(ctx, s.db).QueryRowContext(ctx, query, string(owner), string(id)).
		Scan(&state, &expiresAt); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("pocketbase: read a session's standing: %w", err)
	}

	return project.SessionState(state) == project.SessionActive &&
		now.Before(time.UnixMilli(expiresAt).UTC()), nil
}
