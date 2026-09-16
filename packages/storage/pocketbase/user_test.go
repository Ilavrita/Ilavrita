package pocketbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// storedHash is a credential no read is allowed to surface. It is written by raw
// SQL, because the store itself creates invited identities only.
const storedHash = "argon2id$v=19$m=65536,t=3,p=4$c2FsdHk$aGFzaGVk"

func newUserStore(t *testing.T) (*UserStore, *sql.DB) {
	t.Helper()

	_, db := newStore(t)

	return NewUserStore(db), db
}

func address(t *testing.T, raw string) project.Email {
	t.Helper()

	email, err := project.NormaliseEmail(raw)
	if err != nil {
		t.Fatalf("NormaliseEmail(%q): %v", raw, err)
	}

	return email
}

func invitedServerUser(t *testing.T, id project.UserID, raw string) project.User {
	t.Helper()

	user, err := project.NewServerUser(project.UserConfig{
		ID: id, Email: address(t, raw), State: project.UserInvited,
	})
	if err != nil {
		t.Fatalf("NewServerUser(%s): %v", id, err)
	}

	return user
}

func invitedProjectUser(t *testing.T, home project.ID, id project.UserID, raw string) project.User {
	t.Helper()

	user, err := project.NewProjectUser(home, project.UserConfig{
		ID: id, Email: address(t, raw), State: project.UserInvited,
	})
	if err != nil {
		t.Fatalf("NewProjectUser(%s): %v", id, err)
	}

	return user
}

func mustCreate(t *testing.T, store *UserStore, user project.User) UserVersion {
	t.Helper()

	version, err := store.Create(t.Context(), user)
	if err != nil {
		t.Fatalf("create %s: %v", user.ID(), err)
	}

	return version
}

// seededRow is a prj_a row written around the store, which is how a
// credentialled identity and a self-contradicting one reach these tests at all.
// The store creates invited identities only, and never writes a hash.
type seededRow struct {
	id         project.UserID
	normalized string
	display    string
	hash       any
	state      project.UserState
}

func (r seededRow) write(t *testing.T, db *sql.DB) {
	t.Helper()

	const insert = "INSERT INTO users" +
		" (id, scope, home_project_id, email_normalized, email_display," +
		" password_hash, state, created_at, updated_at)" +
		" VALUES (?, 'project', 'prj_a', ?, ?, ?, ?, 0, 0)"

	if _, err := db.ExecContext(t.Context(), insert,
		string(r.id), r.normalized, r.display, r.hash, string(r.state)); err != nil {
		t.Fatalf("seed %s: %v", r.id, err)
	}
}

func mustRead(t *testing.T, store *UserStore, id project.UserID) (project.User, UserVersion) {
	t.Helper()

	user, version, found, err := store.ByID(t.Context(), id)
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}

	if !found {
		t.Fatalf("read %s: no row", id)
	}

	return user, version
}

func TestOneAddressIsADistinctIdentityInEveryProject(t *testing.T) {
	store, _ := newUserStore(t)

	const raw = "nurse@clinic.example"

	mustCreate(t, store, invitedProjectUser(t, "prj_a", "usr_a", raw))
	mustCreate(t, store, invitedProjectUser(t, "prj_b", "usr_b", raw))
	mustCreate(t, store, invitedServerUser(t, "usr_sys", raw))

	realms := map[project.IdentityRealm]project.UserID{
		project.DeriveRealm("prj_a"): "usr_a",
		project.DeriveRealm("prj_b"): "usr_b",
		project.SystemRealm:          "usr_sys",
	}

	for realm, want := range realms {
		user, _, found, err := store.ByRealmEmail(t.Context(), realm, address(t, raw))
		if err != nil || !found {
			t.Fatalf("lookup in %s: found = %v, err = %v", realm, found, err)
		}

		if user.ID() != want {
			t.Errorf("%s resolved %s, want %s; one Project's identity answered another's lookup",
				realm, user.ID(), want)
		}

		if user.IdentityRealm() != realm {
			t.Errorf("%s returned an identity whose own realm is %s", realm, user.IdentityRealm())
		}
	}
}

func TestTheRealmRefusesTheSameAddressTwice(t *testing.T) {
	store, _ := newUserStore(t)

	tests := []struct {
		name  string
		first project.User
		again project.User
	}{
		{
			name:  "inside one project",
			first: invitedProjectUser(t, "prj_a", "usr_1", "duplicate@clinic.example"),
			again: invitedProjectUser(t, "prj_a", "usr_2", "  Duplicate@Clinic.Example  "),
		},
		{
			name:  "inside the system realm",
			first: invitedServerUser(t, "usr_3", "admin@clinic.example"),
			again: invitedServerUser(t, "usr_4", "ADMIN@clinic.example."),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mustCreate(t, store, tc.first)

			if _, err := store.Create(t.Context(), tc.again); !errors.Is(err, ErrEmailTaken) {
				t.Fatalf("second create: %v, want ErrEmailTaken", err)
			}

			user, _, found, err := store.ByRealmEmail(t.Context(), tc.first.IdentityRealm(), tc.first.Email())
			if err != nil || !found || user.ID() != tc.first.ID() {
				t.Fatalf("the refused create disturbed the row it collided with: %s, %v, %v",
					user.ID(), found, err)
			}
		})
	}
}

func TestAProjectIdentityIsInvisibleFromEveryOtherRealm(t *testing.T) {
	store, _ := newUserStore(t)

	const raw = "clinician@clinic.example"

	mustCreate(t, store, invitedProjectUser(t, "prj_a", "usr_a", raw))

	for _, realm := range []project.IdentityRealm{project.SystemRealm, project.DeriveRealm("prj_b")} {
		user, version, found, err := store.ByRealmEmail(t.Context(), realm, address(t, raw))
		if err != nil {
			t.Fatalf("lookup in %s: %v", realm, err)
		}

		if found {
			t.Fatalf("%s resolved %s, an identity homed in prj_a", realm, user.ID())
		}

		if user != (project.User{}) || version != 0 {
			t.Errorf("a miss in %s carried a value: %#v at version %d", realm, user, version)
		}
	}
}

func TestAnAbsentRowIsACleanMiss(t *testing.T) {
	store, _ := newUserStore(t)

	user, version, found, err := store.ByID(t.Context(), "usr_ghost")
	if err != nil || found || user != (project.User{}) || version != 0 {
		t.Errorf("ByID of an absent id: %#v, %d, %v, %v", user, version, found, err)
	}

	user, version, found, err = store.ByRealmEmail(t.Context(), project.SystemRealm, address(t, "ghost@clinic.example"))
	if err != nil || found || user != (project.User{}) || version != 0 {
		t.Errorf("ByRealmEmail of an absent address: %#v, %d, %v, %v", user, version, found, err)
	}

	if _, err := store.UpdateState(t.Context(), "usr_ghost", project.UserDisabled, 1); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("UpdateState of an absent id: %v, want storage.ErrNotFound", err)
	}
}

func TestUpdateStateDetectsAVersionConflict(t *testing.T) {
	store, _ := newUserStore(t)

	created := mustCreate(t, store, invitedProjectUser(t, "prj_a", "usr_a", "nurse@clinic.example"))

	next, err := store.UpdateState(t.Context(), "usr_a", project.UserDisabled, created)
	if err != nil {
		t.Fatalf("first revoke: %v", err)
	}

	if next != created+1 {
		t.Fatalf("version advanced to %d, want %d", next, created+1)
	}

	if _, err := store.UpdateState(t.Context(), "usr_a", project.UserDisabled, created); !errors.Is(err, storage.ErrVersionConflict) {
		t.Fatalf("stale revoke: %v, want storage.ErrVersionConflict", err)
	}

	user, version := mustRead(t, store, "usr_a")
	if version != next || user.State() != project.UserDisabled {
		t.Errorf("the refused write moved the row: %s at version %d", user.State(), version)
	}
}

func TestUpdateStateRefusesAMoveTheLifecycleForbids(t *testing.T) {
	store, _ := newUserStore(t)

	version := mustCreate(t, store, invitedProjectUser(t, "prj_a", "usr_a", "nurse@clinic.example"))

	tests := []struct {
		name string
		next project.UserState
	}{
		{name: "an invitation activated without accepting it", next: project.UserActive},
		{name: "a return to invited", next: project.UserInvited},
		{name: "a move to a state outside the enum", next: project.UserState("deleted")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.UpdateState(t.Context(), "usr_a", tc.next, version); !errors.Is(err, project.ErrInvalidTransition) {
				t.Fatalf("UpdateState to %s: %v, want project.ErrInvalidTransition", tc.next, err)
			}

			user, current := mustRead(t, store, "usr_a")
			if current != version || user.State() != project.UserInvited {
				t.Errorf("the refused move wrote anyway: %s at version %d", user.State(), current)
			}
		})
	}
}

func TestReEnablingNeedsTheCredentialTheRowActuallyHolds(t *testing.T) {
	store, db := newUserStore(t)

	seededRow{
		id: "usr_held", normalized: "held@clinic.example", display: "held@clinic.example",
		hash: storedHash, state: project.UserDisabled,
	}.write(t, db)

	version, err := store.UpdateState(t.Context(), "usr_held", project.UserActive, 1)
	if err != nil {
		t.Fatalf("re-enable a credentialled identity: %v", err)
	}

	user, current := mustRead(t, store, "usr_held")
	if current != version || user.State() != project.UserActive || !user.HasCredential() {
		t.Errorf("re-enabled row reads %s, credential %v, version %d", user.State(), user.HasCredential(), current)
	}

	// Revoked before it ever accepted, so it holds no password. Activating it
	// would leave an active row nobody can authenticate against.
	revoked := mustCreate(t, store, invitedProjectUser(t, "prj_a", "usr_never", "never@clinic.example"))

	revoked, err = store.UpdateState(t.Context(), "usr_never", project.UserDisabled, revoked)
	if err != nil {
		t.Fatalf("revoke before acceptance: %v", err)
	}

	_, err = store.UpdateState(t.Context(), "usr_never", project.UserActive, revoked)
	if !errors.Is(err, project.ErrInvalidCredentialState) {
		t.Fatalf("activate a credentialless identity: %v, want project.ErrInvalidCredentialState", err)
	}
}

func TestAReadNeverCarriesTheStoredHash(t *testing.T) {
	store, db := newUserStore(t)

	seededRow{
		id: "usr_a", normalized: "nurse@clinic.example", display: "nurse@clinic.example",
		hash: storedHash, state: project.UserActive,
	}.write(t, db)

	user, _ := mustRead(t, store, "usr_a")
	if !user.HasCredential() {
		t.Fatal("a row holding a password reads as holding none")
	}

	rendered := fmt.Sprintf("%v %+v %#v", user, user, user)
	if strings.Contains(rendered, storedHash) {
		t.Errorf("the stored hash reached a formatted User: %s", rendered)
	}
}

func TestAReadRefusesARowThatContradictsItself(t *testing.T) {
	store, db := newUserStore(t)

	tests := []struct {
		name string
		row  seededRow
		want error
	}{
		{
			name: "two email columns naming different mailboxes",
			row: seededRow{
				id: "usr_drift", normalized: "attacker@evil.example",
				display: "nurse@clinic.example", state: project.UserInvited,
			},
			want: ErrEmailNotNormalised,
		},
		{
			name: "an unnormalised address in the indexed column",
			row: seededRow{
				id: "usr_case", normalized: "Nurse@Clinic.Example",
				display: "Nurse@Clinic.Example", state: project.UserInvited,
			},
			want: ErrEmailNotNormalised,
		},
		{
			name: "an active row holding no password",
			row: seededRow{
				id: "usr_hollow", normalized: "hollow@clinic.example",
				display: "hollow@clinic.example", state: project.UserActive,
			},
			want: project.ErrInvalidCredentialState,
		},
		{
			name: "an invited row already holding one",
			row: seededRow{
				id: "usr_early", normalized: "early@clinic.example", display: "early@clinic.example",
				hash: storedHash, state: project.UserInvited,
			},
			want: project.ErrInvalidCredentialState,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.row.write(t, db)

			_, _, found, err := store.ByID(t.Context(), tc.row.id)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ByID: %v, want %v", err, tc.want)
			}

			if found {
				t.Error("a contradictory row was reported as found")
			}
		})
	}
}

func TestCreateWritesTheRealmTheGeneratedColumnComputes(t *testing.T) {
	store, db := newUserStore(t)

	users := []project.User{
		invitedServerUser(t, "usr_sys", "admin@clinic.example"),
		invitedProjectUser(t, "prj_a", "usr_a", "nurse@clinic.example"),
	}

	for _, user := range users {
		if version := mustCreate(t, store, user); version != 1 {
			t.Fatalf("%s created at version %d, want 1", user.ID(), version)
		}

		var realm, display string
		if err := db.QueryRowContext(t.Context(),
			"SELECT identity_realm, email_display FROM users WHERE id = ?",
			string(user.ID())).Scan(&realm, &display); err != nil {
			t.Fatalf("read back %s: %v", user.ID(), err)
		}

		if project.IdentityRealm(realm) != user.IdentityRealm() {
			t.Errorf("%s stored realm %s, the domain computes %s", user.ID(), realm, user.IdentityRealm())
		}

		if display != user.Email().Display() {
			t.Errorf("%s stored display %q, want %q", user.ID(), display, user.Email().Display())
		}
	}
}

func TestCreateRefusesWhatItCannotPersistHonestly(t *testing.T) {
	store, _ := newUserStore(t)

	active, err := project.NewProjectUser("prj_a", project.UserConfig{
		ID: "usr_active", Email: address(t, "active@clinic.example"),
		PasswordHash: project.PasswordHash(storedHash), State: project.UserActive,
	})
	if err != nil {
		t.Fatalf("NewProjectUser: %v", err)
	}

	if _, err := store.Create(t.Context(), active); !errors.Is(err, ErrIdentityNotInvited) {
		t.Errorf("create an already-active identity: %v, want ErrIdentityNotInvited", err)
	}

	ghost := invitedProjectUser(t, "prj_ghost", "usr_ghost", "ghost@clinic.example")
	if _, err := store.Create(t.Context(), ghost); err == nil {
		t.Error("an identity homed in a Project that does not exist was created")
	}
}

// The scope CHECK refuses every shape below, so the decoder's own guard is
// reachable only from here. It is what a later migration or a direct write would
// meet, and placing an identity in the wrong realm is the failure it prevents.
func TestAnIdentityIsNeverPlacedInARealmItsRowDoesNotName(t *testing.T) {
	tests := []struct {
		name string
		row  userRow
		want error
	}{
		{
			name: "a scope outside the enum",
			row:  userRow{id: "usr_a", scope: "tenant", homeProject: sql.NullString{String: "prj_a", Valid: true}},
			want: ErrUserScopeMismatch,
		},
		{
			name: "a server scope naming a home project",
			row:  userRow{id: "usr_a", scope: "server", homeProject: sql.NullString{String: "prj_a", Valid: true}},
			want: ErrUserScopeMismatch,
		},
		{
			name: "a project scope naming none",
			row:  userRow{id: "usr_a", scope: "project"},
			want: project.ErrInvalidProjectID,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.row.normalized, tc.row.display = "nurse@clinic.example", "nurse@clinic.example"
			tc.row.state = string(project.UserInvited)

			if _, err := tc.row.user(); !errors.Is(err, tc.want) {
				t.Fatalf("decode: %v, want %v", err, tc.want)
			}
		})
	}
}

// A user written inside a transaction that rolls back must not survive it. Before
// UserStore read the ambient transaction it wrote on its own connection, so the
// row committed independently of the work it belonged to.
func TestCreateIsUndoneWhenTheTransactionRollsBack(t *testing.T) {
	users, db := newUserStore(t)
	resources := NewResourceStore(db)
	user := invitedServerUser(t, "usr_rollback", "rollback@example.org")
	stop := errors.New("abandon the unit of work")

	err := resources.WithinTransaction(t.Context(), func(ctx context.Context) error {
		if _, err := users.Create(ctx, user); err != nil {
			return err
		}

		return stop
	})

	if !errors.Is(err, stop) {
		t.Fatalf("err = %v, want the work's own error", err)
	}

	if _, _, found, err := users.ByID(t.Context(), user.ID()); err != nil {
		t.Fatalf("lookup after rollback: %v", err)
	} else if found {
		t.Error("the user survived a rolled-back transaction; the write escaped its unit of work")
	}
}
