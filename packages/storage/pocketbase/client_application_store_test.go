package pocketbase

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

// issuedAt is a fixed instant, so a credential's whole life is stated rather than
// measured against a clock the test cannot hold still.
var issuedAt = time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)

// bytesFrom is a reader that never runs short, which is what an issuance needs.
func bytesFrom(fill byte) *bytes.Reader {
	return bytes.NewReader(bytes.Repeat([]byte{fill}, 256))
}

func newApplicationStore(t *testing.T) (*ClientApplicationStore, *sql.DB) {
	t.Helper()

	_, db := newStore(t)

	return NewClientApplicationStore(db), db
}

func registration(
	t *testing.T, owner project.ID, id project.ClientApplicationID, name string,
) project.ClientApplication {
	t.Helper()

	app, err := project.NewClientApplication(owner, project.ClientApplicationConfig{
		ID: id, Name: name, State: project.ServiceActive, Kind: project.ClientConfidential,
	})
	if err != nil {
		t.Fatalf("NewClientApplication: %v", err)
	}

	return app
}

func mustRegister(
	t *testing.T, store *ClientApplicationStore, app project.ClientApplication,
) ClientApplicationVersion {
	t.Helper()

	version, err := store.Create(t.Context(), app)
	if err != nil {
		t.Fatalf("create %s: %v", app.ID(), err)
	}

	return version
}

func mustIssue(
	t *testing.T, store *ClientApplicationStore, app project.ClientApplication,
	id project.CredentialID, fill byte,
) (project.Credential, project.ClientSecret) {
	t.Helper()

	credential, secret, err := app.IssueCredential(project.CredentialConfig{
		ID: id, CreatedAt: issuedAt, ExpiresAt: issuedAt.Add(30 * 24 * time.Hour),
	}, bytesFrom(fill))
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}

	if err := store.IssueCredential(t.Context(), credential); err != nil {
		t.Fatalf("store.IssueCredential: %v", err)
	}

	return credential, secret
}

// TestARegistrationRoundTripsThroughItsOwnProject. A read rebuilds through the
// domain constructor, so what comes back is what the constructor would accept.
func TestARegistrationRoundTripsThroughItsOwnProject(t *testing.T) {
	store, _ := newApplicationStore(t)
	app := registration(t, "prj_a", "cli_loader", "Nightly loader")

	if version := mustRegister(t, store, app); version != 1 {
		t.Errorf("created at version %d, want 1", version)
	}

	read, version, found, err := store.ByID(t.Context(), "prj_a", "cli_loader")
	if err != nil || !found {
		t.Fatalf("read back: found %t, err %v", found, err)
	}

	if read.Name() != "Nightly loader" || read.State() != project.ServiceActive || version != 1 {
		t.Errorf("read back %v at version %d", read, version)
	}

	// The same id in another Project is a different registration, and this one
	// does not answer for it.
	if _, _, found, err := store.ByID(t.Context(), "prj_b", "cli_loader"); err != nil || found {
		t.Errorf("a registration answered outside its Project: found %t, err %v", found, err)
	}
}

// TestASecondRegistrationCannotTakeANameTheProjectTrusts, so an operator revoking
// by name can never be looking at the wrong integration.
func TestASecondRegistrationCannotTakeANameTheProjectTrusts(t *testing.T) {
	store, _ := newApplicationStore(t)
	mustRegister(t, store, registration(t, "prj_a", "cli_one", "Nightly loader"))

	_, err := store.Create(t.Context(), registration(t, "prj_a", "cli_two", "Nightly loader"))
	if !errors.Is(err, ErrClientApplicationNameTaken) {
		t.Errorf("got %v, want ErrClientApplicationNameTaken", err)
	}

	// The refused create wrote nothing.
	if _, _, found, err := store.ByID(t.Context(), "prj_a", "cli_two"); err != nil || found {
		t.Errorf("a refused create left a row: found %t, err %v", found, err)
	}

	// Another Project may register the same name, because the index is per Project.
	if _, err := store.Create(t.Context(), registration(t, "prj_b", "cli_two", "Nightly loader")); err != nil {
		t.Errorf("a second Project was refused a name: %v", err)
	}
}

// TestAReadNeverCarriesTheStoredSecret. The projection reads the hash as one bit,
// so no value a caller holds can carry material to a log line.
func TestAReadNeverCarriesTheStoredSecret(t *testing.T) {
	store, _ := newApplicationStore(t)
	app := registration(t, "prj_a", "cli_loader", "Nightly loader")
	mustRegister(t, store, app)

	written, secret := mustIssue(t, store, app, "cac_one", 'k')

	credentials, err := store.Credentials(t.Context(), "prj_a", "cli_loader")
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}

	if len(credentials) != 1 {
		t.Fatalf("read %d credentials, want 1", len(credentials))
	}

	read := credentials[0]
	if !read.HasSecret() {
		t.Error("a live credential reports no material on file")
	}

	if read.StoredHash() != secretOnFile {
		t.Errorf("a read carried %q rather than the sentinel", read.StoredHash())
	}

	if read.StoredHash() == written.StoredHash() {
		t.Error("a read carried the real digest")
	}

	// A rebuilt credential cannot answer, because it holds the sentinel and not the
	// digest. Matching is step 2's work, against material this store never hands out.
	if read.Matches(secret, issuedAt.Add(time.Hour)) {
		t.Error("a rebuilt credential matched a secret, so a read carried usable material")
	}
}

// sourceOf reads one file in this package. It goes through the package
// directory as a file system so a source-reading test cannot be handed a path
// that leaves it.
func sourceOf(t *testing.T, name string) string {
	t.Helper()

	source, err := fs.ReadFile(os.DirFS("."), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}

	return string(source)
}

// TestNoProjectionInThisPackageSelectsASecretHash reads its own source. It is the
// line between this step and authentication: the day a projection selects the
// column is the day this server can check a presented secret.
func TestNoProjectionInThisPackageSelectsASecretHash(t *testing.T) {
	for _, name := range []string{"client_application.go", "bot.go", "resolvers.go", "migrate.go"} {
		for _, fragment := range strings.Split(sourceOf(t, name), `"`) {
			if !strings.Contains(strings.ToUpper(fragment), "SELECT") {
				continue
			}

			// The COALESCE form reads the column as one bit and never returns it.
			if strings.Contains(fragment, "secret_hash") && !strings.Contains(fragment, "COALESCE(secret_hash") {
				t.Errorf("%s selects secret_hash directly: %s", name, fragment)
			}
		}
	}
}

// TestARebuiltCredentialCannotBeWrittenBackAsAMintedOne. The sentinel a read
// carries would otherwise be stored as a digest no secret hashes to, which would
// answer nothing forever.
func TestARebuiltCredentialCannotBeWrittenBackAsAMintedOne(t *testing.T) {
	store, _ := newApplicationStore(t)
	app := registration(t, "prj_a", "cli_loader", "Nightly loader")
	mustRegister(t, store, app)
	mustIssue(t, store, app, "cac_one", 'k')

	credentials, err := store.Credentials(t.Context(), "prj_a", "cli_loader")
	if err != nil || len(credentials) != 1 {
		t.Fatalf("Credentials: %v", err)
	}

	if err := store.IssueCredential(t.Context(), credentials[0]); !errors.Is(err, ErrSecretNotMinted) {
		t.Errorf("got %v, want ErrSecretNotMinted", err)
	}
}

// TestIssuingASecondLiveCredentialWritesNothing. Two current secrets is a window
// nobody is tracking, so the partial index refuses it rather than a count racing.
func TestIssuingASecondLiveCredentialWritesNothing(t *testing.T) {
	store, _ := newApplicationStore(t)
	app := registration(t, "prj_a", "cli_loader", "Nightly loader")
	mustRegister(t, store, app)
	mustIssue(t, store, app, "cac_one", 'k')

	second, _, err := app.IssueCredential(project.CredentialConfig{
		ID: "cac_two", CreatedAt: issuedAt, ExpiresAt: issuedAt.Add(time.Hour),
	}, bytesFrom('z'))
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}

	if err := store.IssueCredential(t.Context(), second); !errors.Is(err, ErrCredentialAlreadyLive) {
		t.Errorf("got %v, want ErrCredentialAlreadyLive", err)
	}

	credentials, err := store.Credentials(t.Context(), "prj_a", "cli_loader")
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}

	if len(credentials) != 1 {
		t.Errorf("the refused issuance left %d credentials", len(credentials))
	}
}

// TestRotationLeavesOneActiveAndOneSupersededCredential. The supersede lands
// first, so a torn rotation leaves a working outgoing secret rather than two
// nobody is tracking.
func TestRotationLeavesOneActiveAndOneSupersededCredential(t *testing.T) {
	store, _ := newApplicationStore(t)
	app := registration(t, "prj_a", "cli_loader", "Nightly loader")
	mustRegister(t, store, app)
	mustIssue(t, store, app, "cac_one", 'k')

	if err := store.Supersede(t.Context(), "prj_a", "cli_loader", "cac_one",
		issuedAt.Add(time.Hour)); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	mustIssue(t, store, app, "cac_two", 'z')

	states := map[project.CredentialID]project.CredentialState{}

	credentials, err := store.Credentials(t.Context(), "prj_a", "cli_loader")
	if err != nil {
		t.Fatalf("Credentials: %v", err)
	}

	for _, credential := range credentials {
		states[credential.ID()] = credential.State()
	}

	if states["cac_one"] != project.CredentialSuperseded || states["cac_two"] != project.CredentialActive {
		t.Errorf("after a rotation the credentials read %v", states)
	}

	// A third live credential is a constraint violation, not a count to check.
	third, _, err := app.IssueCredential(project.CredentialConfig{
		ID: "cac_three", CreatedAt: issuedAt, ExpiresAt: issuedAt.Add(time.Hour),
	}, bytesFrom('q'))
	if err != nil {
		t.Fatalf("IssueCredential: %v", err)
	}

	if err := store.IssueCredential(t.Context(), third); err == nil {
		t.Error("a third live credential was accepted")
	}
}

// TestRevokingEveryCredentialLeavesNothingToMatch is the answer to a leak: one
// statement, no window, no ordering to get wrong.
func TestRevokingEveryCredentialLeavesNothingToMatch(t *testing.T) {
	store, db := newApplicationStore(t)
	app := registration(t, "prj_a", "cli_loader", "Nightly loader")
	mustRegister(t, store, app)
	mustIssue(t, store, app, "cac_one", 'k')

	if err := store.Supersede(t.Context(), "prj_a", "cli_loader", "cac_one",
		issuedAt.Add(time.Hour)); err != nil {
		t.Fatalf("Supersede: %v", err)
	}

	mustIssue(t, store, app, "cac_two", 'z')

	destroyed, err := store.RevokeAllCredentials(t.Context(), "prj_a", "cli_loader", issuedAt.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("RevokeAllCredentials: %v", err)
	}

	if destroyed != 2 {
		t.Errorf("destroyed %d credentials, want 2", destroyed)
	}

	if live := liveSecrets(t, db); live != 0 {
		t.Errorf("%d credentials still hold material after a full revocation", live)
	}
}

// liveSecrets counts the credential rows still holding material.
func liveSecrets(t *testing.T, db *sql.DB) int {
	t.Helper()

	var live int
	if err := db.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM client_application_credentials WHERE secret_hash IS NOT NULL").Scan(
		&live); err != nil {
		t.Fatalf("count live material: %v", err)
	}

	return live
}

// TestRevokingARegistrationDestroysItsSecrets. The foreign key cannot express
// this, because a revocation is deliberately not a deletion: the row survives for
// the audit and the secrets do not.
func TestRevokingARegistrationDestroysItsSecrets(t *testing.T) {
	store, db := newApplicationStore(t)
	app := registration(t, "prj_a", "cli_loader", "Nightly loader")
	version := mustRegister(t, store, app)
	mustIssue(t, store, app, "cac_one", 'k')

	if _, err := store.UpdateState(t.Context(), "prj_a", "cli_loader",
		project.ServiceRevoked, version); err != nil {
		t.Fatalf("UpdateState: %v", err)
	}

	if live := liveSecrets(t, db); live != 0 {
		t.Errorf("revoking a registration left %d live secrets", live)
	}

	// The credential row survives, because the audit needs it.
	var surviving int
	if err := db.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM client_application_credentials").Scan(&surviving); err != nil {
		t.Fatalf("count credentials: %v", err)
	}

	if surviving != 1 {
		t.Errorf("revocation deleted the credential history: %d rows remain", surviving)
	}
}

// TestARegistrationUpdateDetectsAVersionConflict, so a decision made against a row that
// has since moved is refused rather than overwriting it.
func TestARegistrationUpdateDetectsAVersionConflict(t *testing.T) {
	store, _ := newApplicationStore(t)
	app := registration(t, "prj_a", "cli_loader", "Nightly loader")
	version := mustRegister(t, store, app)

	if _, err := store.UpdateState(t.Context(), "prj_a", "cli_loader",
		project.ServiceSuspended, version); err != nil {
		t.Fatalf("first move: %v", err)
	}

	_, err := store.UpdateState(t.Context(), "prj_a", "cli_loader", project.ServiceRevoked, version)
	if !errors.Is(err, storage.ErrVersionConflict) {
		t.Errorf("got %v, want ErrVersionConflict", err)
	}
}

// TestARegistrationUpdateRefusesAMoveTheLifecycleForbids. The read comes first so the
// lifecycle runs against the persisted state, and an illegal move is named rather
// than silently matching no row.
func TestARegistrationUpdateRefusesAMoveTheLifecycleForbids(t *testing.T) {
	store, _ := newApplicationStore(t)
	app := registration(t, "prj_a", "cli_loader", "Nightly loader")
	version := mustRegister(t, store, app)

	next, err := store.UpdateState(t.Context(), "prj_a", "cli_loader", project.ServiceRevoked, version)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}

	_, err = store.UpdateState(t.Context(), "prj_a", "cli_loader", project.ServiceActive, next)
	if !errors.Is(err, project.ErrInvalidTransition) {
		t.Errorf("reviving a revoked registration: got %v, want ErrInvalidTransition", err)
	}
}

// TestARegistrationCreateIsUndoneWhenTheTransactionRollsBack. Every statement goes through
// conn(ctx, db), so a store called inside a transaction writes through it rather
// than waiting on a connection it holds.
func TestARegistrationCreateIsUndoneWhenTheTransactionRollsBack(t *testing.T) {
	store, db := newApplicationStore(t)
	transactor := NewResourceStore(db)

	sentinel := errors.New("roll this back")

	err := transactor.WithinTransaction(t.Context(), func(ctx context.Context) error {
		if _, err := store.Create(ctx, registration(t, "prj_a", "cli_loader", "Nightly loader")); err != nil {
			return err
		}

		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("WithinTransaction: %v", err)
	}

	if _, _, found, err := store.ByID(t.Context(), "prj_a", "cli_loader"); err != nil || found {
		t.Errorf("a rolled back create survived: found %t, err %v", found, err)
	}
}

// TestNoStatementRewritesAnOwningProject. A registration that belongs elsewhere is
// a new registration, never an updated one.
func TestNoStatementRewritesAnOwningProject(t *testing.T) {
	for _, name := range []string{"client_application.go", "bot.go"} {
		for _, fragment := range strings.Split(sourceOf(t, name), `"`) {
			if !strings.Contains(strings.ToUpper(fragment), " SET ") {
				continue
			}

			if strings.Contains(fragment, "project_id =") && !strings.Contains(fragment, "WHERE") {
				t.Errorf("%s writes an owning Project: %s", name, fragment)
			}
		}
	}
}

// TestABotStoreRoundTripsAndHasNoCredentialSurface. A bot holds no secret, so the
// absence of a credential method is the guarantee rather than a gap.
func TestABotStoreRoundTripsAndHasNoCredentialSurface(t *testing.T) {
	_, db := newStore(t)
	store := NewBotStore(db)

	bot, err := project.NewBot("prj_a", project.BotConfig{
		ID: "bot_worker", Name: "Nightly worker", State: project.ServiceActive,
	})
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}

	version, err := store.Create(t.Context(), bot)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	read, readVersion, found, err := store.ByID(t.Context(), "prj_a", "bot_worker")
	if err != nil || !found || readVersion != version {
		t.Fatalf("read back: found %t, version %d, err %v", found, readVersion, err)
	}

	if read.Name() != "Nightly worker" {
		t.Errorf("read back %v", read)
	}

	if _, _, found, err := store.ByID(t.Context(), "prj_b", "bot_worker"); err != nil || found {
		t.Errorf("a bot answered outside its Project: found %t, err %v", found, err)
	}
}
