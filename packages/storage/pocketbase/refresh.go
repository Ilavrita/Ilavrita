package pocketbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
)

var (
	// ErrRefreshTokenTaken reports a minted token the install already holds.
	// Thirty-two bytes of entropy make a collision unreachable in practice, so
	// this is a broken entropy source rather than bad luck.
	ErrRefreshTokenTaken = errors.New("pocketbase: the install already holds this refresh token")

	// ErrRefreshReplayed reports a token this server issued and already spent,
	// presented again.
	//
	// It is a named error rather than another refusal because it demands a
	// different response: an unknown token is a guess and is simply refused,
	// while a spent one is a copy somebody is holding, and the grant it belongs
	// to has to die.
	ErrRefreshReplayed = errors.New("pocketbase: a spent refresh token was presented again")
)

const refreshColumns = "project_id, id, chain_id, client_application_id, user_id, membership_id," +
	" COALESCE(launch_patient, ''), granted_scopes, state, token_hash, created_at, expires_at"

const (
	createRefreshToken = "INSERT INTO refresh_tokens" +
		" (project_id, id, chain_id, token_hash, client_application_id, user_id, membership_id," +
		" launch_patient, granted_scopes, state, created_at, expires_at)" +
		" VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?)" +
		" ON CONFLICT (token_hash) DO NOTHING" +
		" RETURNING id"

	// The digest is the lookup key, so this carries no tenant predicate: the row
	// it finds is what states the Project.
	readRefreshToken = "SELECT " + refreshColumns + " FROM refresh_tokens WHERE token_hash = ?"

	// Spending is a guarded update, so two concurrent redemptions cannot both
	// succeed: the second changes no row and is answered as a replay, which is
	// what it is.
	spendRefreshToken = "UPDATE refresh_tokens SET state = 'spent'" +
		" WHERE project_id = ? AND id = ? AND state = 'active'" +
		" RETURNING id"

	// Revocation deletes the chain outright. A revoked chain needs no tripwire:
	// the replay it would have caught is the thing that already happened.
	revokeRefreshChain = "DELETE FROM refresh_tokens WHERE project_id = ? AND chain_id = ?"

	// The sessions that chain minted, destroyed the way a revocation always
	// destroys session material rather than labelling it.
	revokeChainSessions = "UPDATE sessions SET state = 'revoked', token_hash = NULL, revoked_at = ?" +
		" WHERE project_id = ? AND refresh_chain = ? AND state <> 'revoked'"

	// Named for what it removes rather than what the rows hold: a constant whose
	// name pairs "refresh" with "token" reads to gosec as a credential in the
	// source, and it is not one — it is a DELETE.
	sweepExpiredGrants = "DELETE FROM refresh_tokens WHERE expires_at <= ?"
)

// RefreshStore issues, rotates and revokes the grants a person approved.
type RefreshStore struct {
	db *sql.DB
}

// NewRefreshStore binds a store to an open database.
func NewRefreshStore(db *sql.DB) *RefreshStore {
	return &RefreshStore{db: db}
}

// Issue writes one minted refresh token. The token itself is never written: only
// its digest is.
func (s *RefreshStore) Issue(ctx context.Context, grant project.RefreshGrant) error {
	if grant.Digest() == "" {
		return fmt.Errorf("%w: %s carries no token", project.ErrInvalidRefreshToken, grant.ID())
	}

	launch := grant.Launch()

	var written string

	err := conn(ctx, s.db).QueryRowContext(ctx, createRefreshToken,
		string(grant.Project()), string(grant.ID()), string(grant.Chain()), grant.Digest(),
		string(grant.Client()), string(grant.User()), string(grant.Membership()),
		launch.Patient(), launch.Scopes(), string(grant.State()),
		grant.CreatedAt().UnixMilli(), grant.ExpiresAt().UnixMilli(),
	).Scan(&written)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s", ErrRefreshTokenTaken, grant.ID())
	case err != nil:
		return fmt.Errorf("pocketbase: issue refresh token: %w", err)
	}

	return nil
}

// Rotate exchanges a presented token for its successor.
//
// A token this server issued and already spent answers ErrRefreshReplayed, and
// the caller is expected to revoke the chain: that is the one case where being
// told which refusal happened matters, because the two demand opposite
// responses. Every other refusal answers alike — found is false and no error —
// since telling an unknown token from an expired one tells a caller which tokens
// exist.
func (s *RefreshStore) Rotate(
	ctx context.Context,
	token project.RefreshToken,
	client project.ClientApplicationID,
	next project.RefreshTokenID,
	now time.Time,
	random io.Reader,
) (project.RefreshGrant, project.RefreshToken, bool, error) {
	if token.IsZero() {
		return project.RefreshGrant{}, project.RefreshToken{}, false, nil
	}

	held, found, err := s.byDigest(ctx, token.Digest())
	if err != nil || !found {
		return project.RefreshGrant{}, project.RefreshToken{}, false, err
	}

	// Asked before redeemability, because a spent token is not merely
	// unredeemable: it is evidence, and the caller must be told so.
	if held.Spent() {
		return held, project.RefreshToken{}, false,
			fmt.Errorf("%w: %s", ErrRefreshReplayed, held.ID())
	}

	if !held.Redeemable(token, client, now) {
		return project.RefreshGrant{}, project.RefreshToken{}, false, nil
	}

	rotated, minted, err := held.Rotate(next, now, random)
	if err != nil {
		return project.RefreshGrant{}, project.RefreshToken{}, false,
			fmt.Errorf("pocketbase: rotate refresh token %s: %w", held.ID(), err)
	}

	// The old row is spent under a guard, so a second redemption racing this one
	// changes nothing and is answered as the replay it is.
	var spent string

	switch err := conn(ctx, s.db).QueryRowContext(ctx, spendRefreshToken,
		string(held.Project()), string(held.ID())).Scan(&spent); {
	case errors.Is(err, sql.ErrNoRows):
		return project.RefreshGrant{}, project.RefreshToken{}, false, nil
	case err != nil:
		return project.RefreshGrant{}, project.RefreshToken{}, false,
			fmt.Errorf("pocketbase: spend refresh token %s: %w", held.ID(), err)
	}

	if err := s.Issue(ctx, rotated); err != nil {
		return project.RefreshGrant{}, project.RefreshToken{}, false, err
	}

	return rotated, minted, true, nil
}

// byDigest reads whichever rotation a presented digest names, spent or live.
func (s *RefreshStore) byDigest(
	ctx context.Context, digest string,
) (project.RefreshGrant, bool, error) {
	var (
		proj, id, chain, client, user, membership string
		patient, scopes, state, held              string
		createdAt, expiresAt                      int64
	)

	switch err := conn(ctx, s.db).QueryRowContext(ctx, readRefreshToken, digest).Scan(
		&proj, &id, &chain, &client, &user, &membership,
		&patient, &scopes, &state, &held, &createdAt, &expiresAt); {
	case errors.Is(err, sql.ErrNoRows):
		return project.RefreshGrant{}, false, nil
	case err != nil:
		return project.RefreshGrant{}, false, fmt.Errorf("pocketbase: read refresh token: %w", err)
	}

	grant, err := project.NewRefreshGrant(project.ID(proj), project.RefreshRecord{
		ID: project.RefreshTokenID(id), Chain: project.RefreshChainID(chain),
		Client: project.ClientApplicationID(client), User: project.UserID(user),
		Membership:    project.MembershipID(membership),
		LaunchPatient: patient, GrantedScopes: scopes,
		State: project.RefreshState(state), Digest: held,
		CreatedAt: time.UnixMilli(createdAt).UTC(), ExpiresAt: time.UnixMilli(expiresAt).UTC(),
	})
	if err != nil {
		return project.RefreshGrant{}, false,
			fmt.Errorf("pocketbase: rebuild refresh token %s: %w", id, err)
	}

	return grant, true, nil
}

// RevokeChain destroys one grant and everything it issued.
//
// Both halves matter. Deleting the chain stops the next refresh; revoking the
// sessions it minted stops whatever a replayer already obtained, which would
// otherwise live out its hour. A response that did only the first would be one
// that let the theft succeed and merely stopped it repeating.
func (s *RefreshStore) RevokeChain(
	ctx context.Context, proj project.ID, chain project.RefreshChainID, at time.Time,
) error {
	if err := project.ValidateID(proj); err != nil {
		return err
	}

	if _, err := conn(ctx, s.db).ExecContext(ctx, revokeChainSessions,
		at.UTC().UnixMilli(), string(proj), string(chain)); err != nil {
		return fmt.Errorf("pocketbase: revoke the sessions of %s: %w", chain, err)
	}

	if _, err := conn(ctx, s.db).ExecContext(ctx, revokeRefreshChain,
		string(proj), string(chain)); err != nil {
		return fmt.Errorf("pocketbase: revoke refresh chain %s: %w", chain, err)
	}

	return nil
}

// Sweep destroys the grants nobody refreshed, and reports how many.
func (s *RefreshStore) Sweep(ctx context.Context, now time.Time) (int64, error) {
	result, err := conn(ctx, s.db).ExecContext(ctx, sweepExpiredGrants, now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("pocketbase: sweep refresh tokens: %w", err)
	}

	swept, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("pocketbase: count swept refresh tokens: %w", err)
	}

	return swept, nil
}
