package pocketbase

import (
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/project"
)

// legacyClientDatabase builds a database whose client_applications table
// predates the OAuth endpoints, with a registration already in it. Every other
// table is current, which is what an install upgrading from an earlier release
// actually is.
func legacyClientDatabase(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/legacy.db?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	if err := ApplySchema(t.Context(), db); err != nil {
		t.Fatalf("apply the schema: %v", err)
	}

	// The redirect table names client_applications, so it goes first: dropping a
	// parent out from under a child is not what an upgrade looks like.
	execAll(t, db, []string{
		"DROP TABLE client_redirect_uris",
		"DROP TABLE client_applications",
	})

	legacy, err := os.ReadFile(filepath.Join("testdata", "legacy_client_applications.sql"))
	if err != nil {
		t.Fatalf("read the legacy declaration: %v", err)
	}

	applyStatements(t, db, string(legacy))

	execAll(t, db, []string{
		"INSERT INTO projects (id, kind, slug, name, state, created_at, updated_at, state_changed_at)" +
			" VALUES ('prj_a', 'standard', 'prj_a', 'prj_a', 'active', 0, 0, 0)",
		"INSERT INTO client_applications (project_id, id, name, description, state," +
			" created_at, updated_at, revoked_at, version)" +
			" VALUES ('prj_a', 'cli_old', 'Nightly loader', '', 'active', 0, 0, NULL, 1)",
	})

	return db
}

// TestAnInstallPredatingTheOauthEndpointsGainsSomewhereToSayWhichProofItOffers.
func TestAnInstallPredatingTheOauthEndpointsGainsSomewhereToSayWhichProofItOffers(t *testing.T) {
	db := legacyClientDatabase(t)

	if err := AssertClientKind(t.Context(), db); !errors.Is(err, ErrClientKindMissing) {
		t.Fatalf("before the migration: got %v, want ErrClientKindMissing", err)
	}

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	if err := AssertClientKind(t.Context(), db); err != nil {
		t.Fatalf("after the migration: %v", err)
	}
}

// TestTheMigrationCarriesEveryRegistrationAcrossAsConfidential.
//
// Every registration that predates the column was issued a client secret when
// it was created — that is the only way this server has ever registered one — so
// confidential is what each already was. Carrying them across as public would
// let every one of them redeem an authorization code presenting nothing.
func TestTheMigrationCarriesEveryRegistrationAcrossAsConfidential(t *testing.T) {
	db := legacyClientDatabase(t)

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	var kind string

	if err := db.QueryRowContext(t.Context(),
		"SELECT kind FROM client_applications WHERE id = 'cli_old'").Scan(&kind); err != nil {
		t.Fatalf("read the carried registration: %v", err)
	}

	if kind != string(project.ClientConfidential) {
		t.Errorf("a registration that predates the column came across as %q, want confidential", kind)
	}
}

// TestARegistrationRoundTripsItsKindAndAddresses, because a redirect written and
// not read back is a redirect nothing compares against — and a comparison
// against nothing refuses every address, including the registered one.
func TestARegistrationRoundTripsItsKindAndAddresses(t *testing.T) {
	db := legacyClientDatabase(t)

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	addresses, err := project.NewRedirectURIs(
		"https://app.example.test/callback", "com.example.app:/oauth")
	if err != nil {
		t.Fatalf("NewRedirectURIs: %v", err)
	}

	registered, err := project.NewClientApplication("prj_a", project.ClientApplicationConfig{
		ID: "cli_pub", Name: "Patient app", State: project.ServiceActive,
		Kind: project.ClientPublic, RedirectURIs: addresses,
	})
	if err != nil {
		t.Fatalf("NewClientApplication: %v", err)
	}

	store := NewClientApplicationStore(db)
	if _, err := store.Create(t.Context(), registered); err != nil {
		t.Fatalf("create: %v", err)
	}

	read, _, found, err := store.ByID(t.Context(), "prj_a", "cli_pub")
	if err != nil || !found {
		t.Fatalf("read back: found %v, err %v", found, err)
	}

	if read.Kind() != project.ClientPublic {
		t.Errorf("read back as %q, want public", read.Kind())
	}

	if read.RedirectURIs().Len() != 2 {
		t.Fatalf("read back %d addresses, want 2", read.RedirectURIs().Len())
	}

	for _, stated := range addresses.Stated() {
		if !read.RedirectURIs().Allows(string(stated)) {
			t.Errorf("%q did not survive the round trip", stated)
		}
	}

	// And an address nobody registered is still refused after the round trip, so
	// what came back is the registered set rather than everything.
	if read.RedirectURIs().Allows("https://evil.test/callback") {
		t.Error("a registration read back from the database allowed an address nobody registered")
	}
}

// TestARegistrationWithNoAddressesReadsBackAsOne, which is what a backend
// service is: it does no authorization-code flow and has no redirect to state.
func TestARegistrationWithNoAddressesReadsBackAsOne(t *testing.T) {
	db := legacyClientDatabase(t)

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	read, _, found, err := NewClientApplicationStore(db).ByID(t.Context(), "prj_a", "cli_old")
	if err != nil || !found {
		t.Fatalf("read back: found %v, err %v", found, err)
	}

	if !read.RedirectURIs().IsZero() {
		t.Errorf("a registration naming no address read back with %d", read.RedirectURIs().Len())
	}
}

// TestTheTableRefusesARedirectCarryingAFragment. A fragment never reaches a
// server, so an address carrying one states something it cannot mean. Go refuses
// it too; this is the schema saying so on its own, so a writer that bypassed the
// domain still cannot store one.
func TestTheTableRefusesARedirectCarryingAFragment(t *testing.T) {
	db := legacyClientDatabase(t)

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	execAll(t, db, []string{
		"INSERT INTO client_applications (project_id, id, name, description, state, kind," +
			" created_at, updated_at, revoked_at, version)" +
			" VALUES ('prj_a', 'cli_frag', 'Fragment app', '', 'active', 'public', 0, 0, NULL, 1)",
	})

	_, err := db.ExecContext(t.Context(),
		"INSERT INTO client_redirect_uris (project_id, client_application_id, uri, created_at)"+
			" VALUES ('prj_a', 'cli_frag', 'https://app.example.test/callback#token', 0)")

	if err == nil {
		t.Fatal("the table accepted a redirect carrying a fragment")
	}
}

// TestEveryGuaranteeADeclarationDependsOnIsAsserted.
//
// Each Assert in assertServable answers a shape some migration was supposed to
// produce. Inlined in PrepareSchema they were unreachable — the migration that
// would make one fail runs immediately before it — so any one of them could be
// deleted and nothing would notice.
//
// This reaches them the only way anything can: against a database as it stands
// before the migrations run.
//
// Each case names the error it must produce rather than merely requiring one.
// Requiring any error proves nothing here — a fixture that declares only the
// tables its own test needs fails the first assertion that looks for a table it
// never created, so every assertion after the first would look covered while
// none of them ran.
//
// That also bounds what this can cover: only a fixture that is current except
// for one thing can reach the assertion about that thing. The three below are
// the ones built by applying the whole schema and then putting a single table
// back as it was. The other assertions in the list have no such fixture, so they
// remain unreached — which is a gap in the fixtures, not in the list.
func TestEveryGuaranteeADeclarationDependsOnIsAsserted(t *testing.T) {
	for name, held := range map[string]struct {
		build func(*testing.T) *sql.DB
		want  error
	}{
		"queues predating their claim columns": {legacyQueueDatabase, ErrQueueClaimsMissing},
		"sessions predating SMART":             {legacySessionsDatabase, ErrSessionLaunchMissing},
		"registrations predating OAuth":        {legacyClientDatabase, ErrClientKindMissing},
		"sessions one release behind":          {launchedSessionsDatabase, ErrSessionRefreshChainMissing},
	} {
		t.Run(name, func(t *testing.T) {
			db := held.build(t)

			if err := assertServable(t.Context(), db); !errors.Is(err, held.want) {
				t.Fatalf("before the migrations: got %v, want %v", err, held.want)
			}

			if err := PrepareSchema(t.Context(), db); err != nil {
				t.Fatalf("prepare: %v", err)
			}

			if err := assertServable(t.Context(), db); err != nil {
				t.Fatalf("still not servable after the migrations ran: %v", err)
			}
		})
	}
}

// TestARegistrationRoundTripsItsKeySet.
//
// This exists because it did not. The insert was missing its jwks column while
// the read selected one, so a registration was written with its key set bound to
// created_at and read back holding no keys at all — and every backend service
// test passed, because an assertion that verifies against nothing fails the same
// way an assertion signed by the wrong key does.
//
// A negative test cannot tell those apart. Only a round trip can.
func TestARegistrationRoundTripsItsKeySet(t *testing.T) {
	db := legacyClientDatabase(t)

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	set, err := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": "svc", "alg": "RS384",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
	}}})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	keys, err := project.ParseJWKS(string(set))
	if err != nil {
		t.Fatalf("ParseJWKS: %v", err)
	}

	registered, err := project.NewClientApplication("prj_a", project.ClientApplicationConfig{
		ID: "cli_svc", Name: "Nightly service", State: project.ServiceActive,
		Kind: project.ClientConfidential, JWKS: keys,
	})
	if err != nil {
		t.Fatalf("NewClientApplication: %v", err)
	}

	store := NewClientApplicationStore(db)
	if _, err := store.Create(t.Context(), registered); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Both reads, because they select through different statements and only one
	// of them was ever exercised.
	read, _, found, err := store.ByID(t.Context(), "prj_a", "cli_svc")
	if err != nil || !found {
		t.Fatalf("ByID: found %v, err %v", found, err)
	}

	if read.JWKS().Len() != 1 {
		t.Errorf("ByID read back %d keys, want the one registered", read.JWKS().Len())
	}

	borne, err := store.Bearing(t.Context(), "cli_svc")
	if err != nil {
		t.Fatalf("Bearing: %v", err)
	}

	if len(borne) != 1 {
		t.Fatalf("Bearing found %d registrations, want 1", len(borne))
	}

	if borne[0].Application.JWKS().Len() != 1 {
		t.Errorf("Bearing read back %d keys, want the one registered",
			borne[0].Application.JWKS().Len())
	}

	// And the columns beside it still hold what they should: the bug put a key
	// set into created_at, which nothing noticed because SQLite stores what it
	// is given.
	var created int64

	if err := db.QueryRowContext(t.Context(),
		"SELECT created_at FROM client_applications WHERE id = 'cli_svc'").Scan(&created); err != nil {
		t.Fatalf("read created_at: %v", err)
	}

	if created <= 0 {
		t.Errorf("created_at holds %d, which is not an instant", created)
	}
}

// TestOneClientIDMayBeBorneByTwoProjects, which is what makes a client id a
// per-Project name rather than an install-wide one — the containment tests rest
// on it, and so does the rule that a backend service is selected by its
// signature rather than by its id.
func TestOneClientIDMayBeBorneByTwoProjects(t *testing.T) {
	db := legacyClientDatabase(t)

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	execAll(t, db, []string{
		"INSERT INTO projects (id, kind, slug, name, state, created_at, updated_at, state_changed_at)" +
			" VALUES ('prj_b', 'standard', 'prj_b', 'prj_b', 'active', 0, 0, 0)",
		"INSERT INTO client_applications (project_id, id, name, description, state, kind," +
			" created_at, updated_at, revoked_at, version) VALUES" +
			" ('prj_a', 'cli_twin', 'Twin', '', 'active', 'confidential', 0, 0, NULL, 1)," +
			" ('prj_b', 'cli_twin', 'Twin', '', 'active', 'confidential', 0, 0, NULL, 1)",
	})

	borne, err := NewClientApplicationStore(db).Bearing(t.Context(), "cli_twin")
	if err != nil {
		t.Fatalf("Bearing: %v", err)
	}

	if len(borne) != 2 {
		t.Fatalf("Bearing found %d registrations, want both", len(borne))
	}

	if borne[0].Project == borne[1].Project {
		t.Errorf("both registrations came back under %s", borne[0].Project)
	}
}
