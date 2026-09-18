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
	const query = "SELECT state, sealed_secret, last_step, created_at, activated_at" +
		" FROM user_second_factors WHERE user_id = ?"

	var (
		state, sealed     string
		lastStep, created int64
		activated         sql.NullInt64
	)

	switch err := conn(ctx, s.db).QueryRowContext(ctx, query, string(user)).
		Scan(&state, &sealed, &lastStep, &created, &activated); {
	case errors.Is(err, sql.ErrNoRows):
		return project.SecondFactor{}, false, nil
	case err != nil:
		return project.SecondFactor{}, false, fmt.Errorf("pocketbase: read a second factor: %w", err)
	}

	raw, err := s.key.Open(sealed)
	if err != nil {
		return project.SecondFactor{}, false, fmt.Errorf("%w: %s: %w", ErrFactorUnsealable, user, err)
	}

	secret, err := project.ParseTOTPSecret(string(raw))
	if err != nil {
		return project.SecondFactor{}, false, fmt.Errorf("%w: %s: %w", ErrFactorUnsealable, user, err)
	}

	held, err := project.RestoreSecondFactor(user, project.FactorState(state), secret,
		lastStep, instant(created), instant(activated.Int64))
	if err != nil {
		return project.SecondFactor{}, false, fmt.Errorf("pocketbase: rebuild a second factor: %w", err)
	}

	return held, true, nil
}

// Enrol stores a new factor, replacing whatever the identity had. Re-enrolling
// is how someone who lost their phone starts again, and the new secret has to
// be proved before it is required of them.
func (s *FactorStore) Enrol(ctx context.Context, factor project.SecondFactor) error {
	sealed, err := s.key.Seal([]byte(factor.Secret().Encoded()))
	if err != nil {
		return fmt.Errorf("pocketbase: seal a second factor: %w", err)
	}

	const upsert = "INSERT INTO user_second_factors" +
		" (user_id, state, sealed_secret, last_step, created_at, activated_at)" +
		" VALUES (?, ?, ?, 0, ?, NULL)" +
		" ON CONFLICT (user_id) DO UPDATE SET" +
		" state = excluded.state, sealed_secret = excluded.sealed_secret," +
		" last_step = 0, created_at = excluded.created_at, activated_at = NULL"

	if _, err := conn(ctx, s.db).ExecContext(ctx, upsert,
		string(factor.User()), string(factor.State()), sealed,
		factor.CreatedAt().UnixMilli()); err != nil {
		return fmt.Errorf("pocketbase: enrol a second factor: %w", err)
	}

	return nil
}

// Prove records a factor that has just been proved: its state, and the step it
// may not be used at again.
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
	const update = "UPDATE user_second_factors SET state = ?, last_step = ?, activated_at = ?" +
		" WHERE user_id = ? AND last_step < ?"

	var activated any
	if !factor.ActivatedAt().IsZero() {
		activated = factor.ActivatedAt().UnixMilli()
	}

	answer, err := conn(ctx, s.db).ExecContext(ctx, update,
		string(factor.State()), factor.LastStep(), activated,
		string(factor.User()), factor.LastStep())
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
