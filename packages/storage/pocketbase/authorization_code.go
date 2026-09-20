package pocketbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
)

// ErrAuthorizationCodeTaken reports a minted code the install already holds.
// Thirty-two bytes of entropy make a collision unreachable in practice, so this
// is a broken entropy source rather than bad luck, and it is refused rather than
// retried with a second draw from the same reader.
var ErrAuthorizationCodeTaken = errors.New("pocketbase: the install already holds this authorization code")

const (
	createAuthorizationCode = "INSERT INTO authorization_codes" +
		" (project_id, id, code_hash, client_application_id, user_id, membership_id," +
		" redirect_uri, code_challenge, launch_patient, granted_scopes, created_at, expires_at)" +
		" VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?)" +
		" ON CONFLICT (code_hash) DO NOTHING" +
		" RETURNING id"

	// The delete is the claim. Selecting first and deleting after would leave a
	// window two concurrent redemptions both pass through, and a code redeemed
	// twice is the one thing single use exists to stop — so the row is taken by
	// the statement that finds it, and what comes back is what the caller then
	// judges.
	//
	// It carries no tenant predicate for the same reason a session resolve does
	// not: the digest is the lookup key, and the row it finds is what states the
	// Project.
	redeemAuthorizationCode = "DELETE FROM authorization_codes WHERE code_hash = ?" +
		" RETURNING project_id, id, client_application_id, user_id, membership_id," +
		" redirect_uri, code_challenge, COALESCE(launch_patient, ''), granted_scopes," +
		" code_hash, created_at, expires_at"

	// Expiry passes without anyone writing anything, so a sweep is housekeeping
	// rather than correctness: nothing redeems an expired code either way.
	sweepAuthorizationCodes = "DELETE FROM authorization_codes WHERE expires_at <= ?"
)

// AuthorizationCodeStore issues and redeems the approvals a person gave.
type AuthorizationCodeStore struct {
	db *sql.DB
}

// NewAuthorizationCodeStore binds a store to an open database.
func NewAuthorizationCodeStore(db *sql.DB) *AuthorizationCodeStore {
	return &AuthorizationCodeStore{db: db}
}

// Issue writes one minted code. The code itself is never written: only its
// digest is.
func (s *AuthorizationCodeStore) Issue(ctx context.Context, code project.AuthorizationCode) error {
	if code.Digest() == "" {
		return fmt.Errorf("%w: %s carries no code", project.ErrInvalidAuthorizationCode, code.ID())
	}

	launch := code.Launch()

	var written string

	err := conn(ctx, s.db).QueryRowContext(ctx, createAuthorizationCode,
		string(code.Project()), string(code.ID()), code.Digest(),
		string(code.Client()), string(code.User()), string(code.Membership()),
		string(code.RedirectURI()), code.Challenge().Value(),
		launch.Patient(), launch.Scopes(),
		code.CreatedAt().UnixMilli(), code.ExpiresAt().UnixMilli(),
	).Scan(&written)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: %s", ErrAuthorizationCodeTaken, code.ID())
	case err != nil:
		return fmt.Errorf("pocketbase: issue authorization code: %w", err)
	}

	return nil
}

// Redeem exchanges one code for the approval it stands for, and destroys it.
//
// The row is taken before it is judged, so a code that fails any check is spent
// all the same. That is deliberate: the alternative leaves a code alive after
// somebody holding it guessed wrong at the verifier, and a code that survives a
// failed attempt is one an attacker may keep attacking. A client that genuinely
// lost the race starts the flow again, which costs a redirect.
//
// Every refusal answers the same way — found is false and no error — because
// telling an unknown code from an expired one, or from one bound to another
// client, tells a caller which codes exist.
func (s *AuthorizationCodeStore) Redeem(
	ctx context.Context,
	token project.AuthorizationCodeToken,
	client project.ClientApplicationID,
	redirect, verifier string,
	now time.Time,
) (project.AuthorizationCode, bool, error) {
	if token.IsZero() {
		return project.AuthorizationCode{}, false, nil
	}

	var (
		proj, id, scannedClient, user, membership string
		redirectURI, challenge, patient, scopes   string
		digest                                    string
		createdAt, expiresAt                      int64
	)

	switch err := conn(ctx, s.db).QueryRowContext(ctx, redeemAuthorizationCode, token.Digest()).Scan(
		&proj, &id, &scannedClient, &user, &membership,
		&redirectURI, &challenge, &patient, &scopes,
		&digest, &createdAt, &expiresAt); {
	case errors.Is(err, sql.ErrNoRows):
		return project.AuthorizationCode{}, false, nil
	case err != nil:
		return project.AuthorizationCode{}, false, fmt.Errorf("pocketbase: redeem authorization code: %w", err)
	}

	code, err := project.NewAuthorizationCode(project.ID(proj), project.AuthorizationCodeRecord{
		ID: project.AuthorizationCodeID(id), Client: project.ClientApplicationID(scannedClient),
		User: project.UserID(user), Membership: project.MembershipID(membership),
		RedirectURI: redirectURI, CodeChallenge: challenge,
		LaunchPatient: patient, GrantedScopes: scopes, Digest: digest,
		CreatedAt: time.UnixMilli(createdAt).UTC(), ExpiresAt: time.UnixMilli(expiresAt).UTC(),
	})
	if err != nil {
		return project.AuthorizationCode{}, false,
			fmt.Errorf("pocketbase: rebuild authorization code %s: %w", id, err)
	}

	// The comparison, the clock, the client, the address and the verifier travel
	// together, so no caller can check one and forget another.
	if !code.Redeemable(token, client, redirect, verifier, now) {
		return project.AuthorizationCode{}, false, nil
	}

	return code, true, nil
}

// Sweep destroys the codes nobody redeemed, and reports how many. It is
// housekeeping: an expired code is refused whether or not its row is still
// there, so nothing depends on this having run.
func (s *AuthorizationCodeStore) Sweep(ctx context.Context, now time.Time) (int64, error) {
	result, err := conn(ctx, s.db).ExecContext(ctx, sweepAuthorizationCodes, now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("pocketbase: sweep authorization codes: %w", err)
	}

	swept, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("pocketbase: count swept authorization codes: %w", err)
	}

	return swept, nil
}
