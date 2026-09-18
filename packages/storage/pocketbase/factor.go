package pocketbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Ilavrita/Ilavrita/packages/project"
)

// ErrFactorUnsealable reports a stored second factor this deployment cannot
// open. It denies rather than letting the login through: a factor nobody can
// read is not one anybody proved, and treating it as absent would turn a
// misconfigured key into a way past everyone's second factor.
var ErrFactorUnsealable = errors.New("pocketbase: a stored second factor cannot be opened")

// FactorStore keeps one second factor per identity, sealed.
//
// The key never reaches the database. What is stored is material this store
// cannot read without it, which is what makes a leaked file less than a leaked
// factor.
type FactorStore struct {
	db  *sql.DB
	key project.SealingKey
}

// NewFactorStore binds the store to a database and the key that seals what it
// holds. A store built without a key can still report that no factor exists,
// which is what a deployment that has not configured one should answer.
func NewFactorStore(db *sql.DB, key project.SealingKey) *FactorStore {
	return &FactorStore{db: db, key: key}
}

// Available reports whether this deployment can hold a factor at all. One
// configured without a key seals nothing, and enrolling would mean storing the
// secret in the clear — which would make the database a list of everyone's
// second factor.
func (s *FactorStore) Available() bool { return s != nil && !s.key.IsZero() }

// Enrolled returns one identity's factor, if they have one.
func (s *FactorStore) Enrolled(
	ctx context.Context, user project.UserID,
) (project.SecondFactor, bool, error) {
	const query = "SELECT state, sealed_secret, pending_secret, last_step, created_at, activated_at" +
		" FROM user_second_factors WHERE user_id = ?"

	var (
		state, sealed     string
		pending           sql.NullString
		lastStep, created int64
		activated         sql.NullInt64
	)

	switch err := conn(ctx, s.db).QueryRowContext(ctx, query, string(user)).
		Scan(&state, &sealed, &pending, &lastStep, &created, &activated); {
	case errors.Is(err, sql.ErrNoRows):
		return project.SecondFactor{}, false, nil
	case err != nil:
		return project.SecondFactor{}, false, fmt.Errorf("pocketbase: read a second factor: %w", err)
	}

	secret, err := s.unseal(user, sealed)
	if err != nil {
		return project.SecondFactor{}, false, err
	}

	var replacement project.TOTPSecret

	if pending.Valid {
		if replacement, err = s.unseal(user, pending.String); err != nil {
			return project.SecondFactor{}, false, err
		}
	}

	held, err := project.RestoreSecondFactor(user, project.FactorState(state), secret, replacement,
		lastStep, instant(created), instant(activated.Int64))
	if err != nil {
		return project.SecondFactor{}, false, fmt.Errorf("pocketbase: rebuild a second factor: %w", err)
	}

	return held, true, nil
}

// unseal opens one stored secret.
func (s *FactorStore) unseal(user project.UserID, sealed string) (project.TOTPSecret, error) {
	raw, err := s.key.Open(sealed)
	if err != nil {
		return project.TOTPSecret{}, fmt.Errorf("%w: %s: %w", ErrFactorUnsealable, user, err)
	}

	secret, err := project.ParseTOTPSecret(string(raw))
	if err != nil {
		return project.TOTPSecret{}, fmt.Errorf("%w: %s: %w", ErrFactorUnsealable, user, err)
	}

	return secret, nil
}

// Enrol stores a factor for an identity that has none in force, replacing an
// unproved one if there is one. It is the first enrolment, and the one somebody
// makes again after a first attempt went wrong.
func (s *FactorStore) Enrol(ctx context.Context, factor project.SecondFactor) error {
	sealed, err := s.key.Seal([]byte(factor.Secret().Encoded()))
	if err != nil {
		return fmt.Errorf("pocketbase: seal a second factor: %w", err)
	}

	const upsert = "INSERT INTO user_second_factors" +
		" (user_id, state, sealed_secret, pending_secret, last_step, created_at, activated_at)" +
		" VALUES (?, ?, ?, NULL, 0, ?, NULL)" +
		" ON CONFLICT (user_id) DO UPDATE SET" +
		" state = excluded.state, sealed_secret = excluded.sealed_secret," +
		" pending_secret = NULL, last_step = 0," +
		" created_at = excluded.created_at, activated_at = NULL" +
		// A factor in force is never replaced this way. Doing so would turn it
		// off, which is what somebody holding a stolen session would want.
		" WHERE user_second_factors.state <> 'active'"

	answer, err := conn(ctx, s.db).ExecContext(ctx, upsert,
		string(factor.User()), string(factor.State()), sealed,
		factor.CreatedAt().UnixMilli())
	if err != nil {
		return fmt.Errorf("pocketbase: enrol a second factor: %w", err)
	}

	changed, err := answer.RowsAffected()
	if err != nil {
		return fmt.Errorf("pocketbase: enrol a second factor: %w", err)
	}

	if changed == 0 {
		return project.ErrFactorInForce
	}

	return nil
}

// Replace puts a new secret behind a factor that is in force, awaiting proof.
//
// The secret in force is untouched: an account does not lose its second factor
// because somebody started moving to a new phone.
//
// The step is written under the one it replaces, so two moves racing with the
// same code cannot both win. On SQLite this build holds one connection, so the
// in-process check refuses the second before reaching here; the guard is for a
// backend whose transactions really do run at once.
func (s *FactorStore) Replace(ctx context.Context, factor project.SecondFactor) error {
	sealed, err := s.key.Seal([]byte(factor.Replacement().Encoded()))
	if err != nil {
		return fmt.Errorf("pocketbase: seal a second factor: %w", err)
	}

	const update = "UPDATE user_second_factors SET pending_secret = ?, last_step = ?" +
		" WHERE user_id = ? AND state = 'active' AND last_step < ?"

	answer, err := conn(ctx, s.db).ExecContext(ctx, update,
		sealed, factor.LastStep(), string(factor.User()), factor.LastStep())
	if err != nil {
		return fmt.Errorf("pocketbase: replace a second factor: %w", err)
	}

	changed, err := answer.RowsAffected()
	if err != nil {
		return fmt.Errorf("pocketbase: replace a second factor: %w", err)
	}

	if changed == 0 {
		return project.ErrCodeRefused
	}

	return nil
}

// Confirm puts the secret that was awaiting proof in force.
func (s *FactorStore) Confirm(ctx context.Context, factor project.SecondFactor) error {
	sealed, err := s.key.Seal([]byte(factor.Secret().Encoded()))
	if err != nil {
		return fmt.Errorf("pocketbase: seal a second factor: %w", err)
	}

	const update = "UPDATE user_second_factors" +
		" SET state = ?, sealed_secret = ?, pending_secret = NULL," +
		" last_step = ?, activated_at = ?" +
		" WHERE user_id = ? AND last_step < ?"

	answer, err := conn(ctx, s.db).ExecContext(ctx, update,
		string(factor.State()), sealed, factor.LastStep(),
		factor.ActivatedAt().UnixMilli(), string(factor.User()), factor.LastStep())
	if err != nil {
		return fmt.Errorf("pocketbase: confirm a second factor: %w", err)
	}

	changed, err := answer.RowsAffected()
	if err != nil {
		return fmt.Errorf("pocketbase: confirm a second factor: %w", err)
	}

	if changed == 0 {
		return project.ErrCodeRefused
	}

	return nil
}

// Prove records that a code was accepted: the step it belonged to, which it may
// not be used at again.
//
// It changes nothing else. What a code is allowed to change — activating a
// factor, or putting a replacement in force — is Confirm's business.
//
// The step is written under the one it replaces, so two logins racing with the
// same code cannot both win: the second updates no row and is refused.
//
// On SQLite this build holds one connection, so the two serialise and the
// in-process check refuses the second before reaching here. The guard is for a
// backend whose transactions really do run at once — PostgreSQL, which the
// schema is written to move to — where the read and the write are not one
// atomic step.
func (s *FactorStore) Prove(ctx context.Context, factor project.SecondFactor) error {
	const update = "UPDATE user_second_factors SET last_step = ?" +
		" WHERE user_id = ? AND last_step < ?"

	answer, err := conn(ctx, s.db).ExecContext(ctx, update,
		factor.LastStep(), string(factor.User()), factor.LastStep())
	if err != nil {
		return fmt.Errorf("pocketbase: record a proved second factor: %w", err)
	}

	changed, err := answer.RowsAffected()
	if err != nil {
		return fmt.Errorf("pocketbase: record a proved second factor: %w", err)
	}

	if changed == 0 {
		return project.ErrCodeRefused
	}

	return nil
}

// Withdraw removes an identity's factor.
func (s *FactorStore) Withdraw(ctx context.Context, user project.UserID) error {
	if _, err := conn(ctx, s.db).ExecContext(ctx,
		"DELETE FROM user_second_factors WHERE user_id = ?", string(user)); err != nil {
		return fmt.Errorf("pocketbase: withdraw a second factor: %w", err)
	}

	return nil
}
