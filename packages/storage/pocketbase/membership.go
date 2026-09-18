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
	// ErrMembershipRefused reports a membership row the database would not accept:
	// a second live membership for one principal, a principal no registry
	// describes, or a privilege the CHECKs confine. Exactly one live membership
	// per principal is what makes resolution at token issuance deterministic.
	ErrMembershipRefused = errors.New("pocketbase: the project refuses this membership")

	// ErrBindingRefused reports a policy binding the database would not accept,
	// which is a link-minted membership or a policy another Project owns.
	// Administrative reach must not become a data grant.
	ErrBindingRefused = errors.New("pocketbase: the membership refuses this policy binding")
)

const (
	readMembershipVersion = "SELECT state, version FROM project_memberships" +
		" WHERE project_id = ? AND id = ?"

	// authz_version moves with the state, because the state is what an
	// authorization decision reads; version moves with every edit.
	updateMembershipState = "UPDATE project_memberships" +
		" SET state = ?, updated_at = ?," +
		" activated_at = CASE WHEN ? = 'active' THEN ? ELSE activated_at END," +
		" revoked_at = CASE WHEN ? = 'revoked' THEN ? ELSE revoked_at END," +
		" version = version + 1, authz_version = authz_version + 1" +
		" WHERE project_id = ? AND id = ? AND version = ?" +
		" RETURNING version"

	// membership_link_sourced is bound to 0, so the composite foreign key makes a
	// binding on a link-minted membership a constraint violation.
	bindPolicy = "INSERT INTO project_membership_policies" +
		" (project_id, membership_id, policy_id, ordinal, policy_params, membership_link_sourced)" +
		" VALUES (?, ?, ?, ?, ?, 0)" +
		" ON CONFLICT (project_id, membership_id, policy_id) DO NOTHING" +
		" RETURNING policy_id"

	unbindPolicy = "DELETE FROM project_membership_policies" +
		" WHERE project_id = ? AND membership_id = ? AND policy_id = ?"

	// A binding changes what a decision resolves to, so it bumps the counter a
	// cache keys on even though it edits another table.
	touchAuthzVersion = "UPDATE project_memberships SET authz_version = authz_version + 1," +
		" updated_at = ? WHERE project_id = ? AND id = ?"
)

// MembershipVersion is the optimistic-concurrency counter
// project_memberships.version carries.
type MembershipVersion int64

// MembershipStore persists one principal's standing in one Project. It writes no
// privilege a Membership does not already hold: the domain type is the only place
// admin, super admin and a profile are decided.
type MembershipStore struct {
	db *sql.DB
}

// NewMembershipStore binds a store to an open database. The caller owns the pool
// and is responsible for opening it with foreign keys enforced.
func NewMembershipStore(db *sql.DB) *MembershipStore {
	return &MembershipStore{db: db}
}

// Create writes one membership and its policy bindings in one transaction, so a
// member never exists for a moment holding standing nobody scoped.
func (s *MembershipStore) Create(ctx context.Context, member project.Membership) error {
	return s.within(ctx, func(ctx context.Context) error {
		if err := writeMembership(ctx, s.db, member, time.Now().UTC()); err != nil {
			return err
		}

		for _, binding := range member.StoredPolicies() {
			if err := s.write(ctx, member.Project(), member.ID(),
				binding.Policy().ID(), binding.Ordinal(), binding.Parameters()); err != nil {
				return err
			}
		}

		return nil
	})
}

// UpdateState moves a membership under the version the caller last read. The
// lifecycle runs against the persisted state, so an illegal move is named rather
// than silently matching no row.
func (s *MembershipStore) UpdateState(
	ctx context.Context,
	proj project.ID,
	id project.MembershipID,
	next project.MembershipState,
	expect MembershipVersion,
) (MembershipVersion, error) {
	var (
		state   string
		version int64
	)

	switch err := conn(ctx, s.db).QueryRowContext(ctx, readMembershipVersion,
		string(proj), string(id)).Scan(&state, &version); {
	case errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("%w: membership %s in %s", storage.ErrNotFound, id, proj)
	case err != nil:
		return 0, fmt.Errorf("pocketbase: read membership %s: %w", id, err)
	}

	if MembershipVersion(version) != expect {
		return 0, fmt.Errorf("%w: membership %s stands at version %d", storage.ErrVersionConflict, id, version)
	}

	if _, err := project.MembershipState(state).TransitionTo(next); err != nil {
		return 0, err
	}

	stamp := time.Now().UTC().UnixMilli()

	var written int64

	err := conn(ctx, s.db).QueryRowContext(ctx, updateMembershipState,
		string(next), stamp, string(next), stamp, string(next), stamp,
		string(proj), string(id), int64(expect),
	).Scan(&written)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, fmt.Errorf("%w: membership %s moved since it was read", storage.ErrVersionConflict, id)
	case err != nil:
		return 0, fmt.Errorf("pocketbase: update membership state: %w", err)
	}

	return MembershipVersion(written), nil
}

// BindPolicy attaches one AccessPolicy to a membership, in the membership's own
// Project. A binding names no Project of its own, so it cannot reach one the
// membership does not stand in.
func (s *MembershipStore) BindPolicy(
	ctx context.Context,
	proj project.ID,
	id project.MembershipID,
	attachment project.PolicyAttachment,
) error {
	if attachment.Policy == "" {
		return project.ErrMissingPolicy
	}

	return s.within(ctx, func(ctx context.Context) error {
		if err := s.write(ctx, proj, id, attachment.Policy, attachment.Ordinal, attachment.Parameters); err != nil {
			return err
		}

		return s.touch(ctx, proj, id)
	})
}

// UnbindPolicy removes one binding and bumps the counter a cache keys on, so a
// withdrawn grant stops applying on the very next decision.
func (s *MembershipStore) UnbindPolicy(
	ctx context.Context, proj project.ID, id project.MembershipID, policy storage.LogicalID,
) error {
	return s.within(ctx, func(ctx context.Context) error {
		result, err := conn(ctx, s.db).ExecContext(ctx, unbindPolicy,
			string(proj), string(id), string(policy))
		if err != nil {
			return fmt.Errorf("pocketbase: unbind policy %s: %w", policy, err)
		}

		removed, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("pocketbase: count unbound policies: %w", err)
		}

		if removed == 0 {
			return fmt.Errorf("%w: binding %s on membership %s", storage.ErrNotFound, policy, id)
		}

		return s.touch(ctx, proj, id)
	})
}

// write inserts one binding. A link-minted membership and a policy another
// Project owns are both refused by composite foreign keys rather than by a check
// here, so the driver's refusal is named rather than pre-empted.
func (s *MembershipStore) write(
	ctx context.Context, proj project.ID, id project.MembershipID,
	policy storage.LogicalID, ordinal int, parameters []byte,
) error {
	params := string(parameters)
	if params == "" {
		params = "{}"
	}

	var written string

	err := conn(ctx, s.db).QueryRowContext(ctx, bindPolicy,
		string(proj), string(id), string(policy), ordinal, params).Scan(&written)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// The binding already exists, which is what the caller asked for.
		return nil
	case err != nil:
		return fmt.Errorf("%w: %s on membership %s: %w", ErrBindingRefused, policy, id, err)
	}

	return nil
}

// touch bumps the counter an authorization cache keys on, for a change that
// happened in another table and would otherwise go unnoticed.
func (s *MembershipStore) touch(ctx context.Context, proj project.ID, id project.MembershipID) error {
	if _, err := conn(ctx, s.db).ExecContext(ctx, touchAuthzVersion,
		time.Now().UTC().UnixMilli(), string(proj), string(id)); err != nil {
		return fmt.Errorf("pocketbase: invalidate cached authorization for %s: %w", id, err)
	}

	return nil
}

// within runs work inside one transaction, joining the caller's if the context
// already carries one.
func (s *MembershipStore) within(ctx context.Context, work func(ctx context.Context) error) error {
	if _, joined := ctx.Value(transactionKey{}).(*sql.Tx); joined {
		return work(ctx)
	}

	return NewResourceStore(s.db).WithinTransaction(ctx, work)
}

// writeMembership writes one membership row. It binds the stored flags rather
// than the effective ones, because a membership that is not yet active still
// holds what it was granted.
func writeMembership(ctx context.Context, db *sql.DB, member project.Membership, at time.Time) error {
	principal := member.Principal()

	var user, application, bot any

	switch principal.Kind {
	case project.PrincipalUser:
		user = string(principal.ID)
	case project.PrincipalClientApplication:
		application = string(principal.ID)
	case project.PrincipalBot:
		bot = string(principal.ID)
	default:
		return fmt.Errorf("%w: %q", project.ErrInvalidPrincipal, string(principal.Kind))
	}

	var profileType, profileID any
	if profile, held := member.Profile(); held {
		profileType, profileID = string(profile.Type), string(profile.ID)
	}

	stamp := at.UTC().UnixMilli()

	var activated any
	if member.State() == project.MembershipActive {
		activated = stamp
	}

	_, err := conn(ctx, db).ExecContext(ctx, createMembership,
		string(member.Project()), string(member.ID()), string(member.ProjectKind()),
		user, application, bot, profileType, profileID, string(member.State()),
		asInteger(member.StoredAdmin()), asInteger(member.StoredSuperAdmin()),
		string(member.Source()), stamp, stamp, activated,
	)
	if err != nil {
		return fmt.Errorf("%w: %s in %s: %w", ErrMembershipRefused, principal.ID, member.Project(), err)
	}

	return nil
}
