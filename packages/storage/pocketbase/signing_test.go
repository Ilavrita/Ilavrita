package pocketbase

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
)

// aSealingKey mints one for a test.
func aSealingKey(t *testing.T) project.SealingKey {
	t.Helper()

	encoded, err := project.MintSealingKey(rand.Reader)
	if err != nil {
		t.Fatalf("MintSealingKey: %v", err)
	}

	key, err := project.ParseSealingKey(encoded)
	if err != nil {
		t.Fatalf("ParseSealingKey: %v", err)
	}

	return key
}

// signingDatabase is an empty current database, which is all a signing key
// needs: the key belongs to the install rather than to any Project.
func signingDatabase(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/signing.db?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	return db
}

// TestTheSigningKeyOutlivesTheProcessThatMintedIt.
//
// An identity token names a `kid` and a reader resolves it against the
// published set. A server that minted a new key on every start would publish a
// set that no longer contains the key its live tokens were signed with.
func TestTheSigningKeyOutlivesTheProcessThatMintedIt(t *testing.T) {
	db := signingDatabase(t)
	sealing := aSealingKey(t)

	first, err := NewSigningKeyStore(db, sealing).EnsureActive(t.Context(), rand.Reader)
	if err != nil {
		t.Fatalf("EnsureActive: %v", err)
	}

	// A second store over the same database is the next process to start.
	second, err := NewSigningKeyStore(db, sealing).EnsureActive(t.Context(), rand.Reader)
	if err != nil {
		t.Fatalf("EnsureActive again: %v", err)
	}

	if first.ID() != second.ID() {
		t.Fatalf("a restart minted %s beside %s", second.ID(), first.ID())
	}

	published, err := first.PublishedJWKS()
	if err != nil {
		t.Fatalf("PublishedJWKS: %v", err)
	}

	again, err := second.PublishedJWKS()
	if err != nil {
		t.Fatalf("PublishedJWKS: %v", err)
	}

	if published != again {
		t.Error("a restart published a different key set," +
			" so tokens issued before it stopped verifying")
	}
}

// TestOnlyOneSigningKeyIsEverActive, because two would mean two answers to
// which key a token came from.
func TestOnlyOneSigningKeyIsEverActive(t *testing.T) {
	db := signingDatabase(t)
	store := NewSigningKeyStore(db, aSealingKey(t))

	if _, err := store.EnsureActive(t.Context(), rand.Reader); err != nil {
		t.Fatalf("EnsureActive: %v", err)
	}

	// Straight past the store, because the store is what this asserts about: the
	// schema has to refuse a second active key on its own.
	_, err := db.ExecContext(t.Context(),
		"INSERT INTO identity_signing_keys (id, private_key, state, created_at, retired_at)"+
			" VALUES ('another', 'sealed', 'active', 0, NULL)")
	if err == nil {
		t.Error("a second active signing key was written")
	}

	var active int
	if err := db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM identity_signing_keys WHERE state = 'active'").Scan(&active); err != nil {
		t.Fatalf("count: %v", err)
	}

	if active != 1 {
		t.Errorf("%d keys are active, want 1", active)
	}
}

// TestTheStoredSigningKeyIsNotReadableFromTheDatabaseAlone.
//
// The whole reason it is sealed: a leaked file should not be the ability to
// assert anybody's identity to any client.
func TestTheStoredSigningKeyIsNotReadableFromTheDatabaseAlone(t *testing.T) {
	db := signingDatabase(t)

	key, err := NewSigningKeyStore(db, aSealingKey(t)).EnsureActive(t.Context(), rand.Reader)
	if err != nil {
		t.Fatalf("EnsureActive: %v", err)
	}

	var stored string

	err = db.QueryRowContext(t.Context(),
		"SELECT private_key FROM identity_signing_keys WHERE state = 'active'").Scan(&stored)
	if err != nil {
		t.Fatalf("read the row: %v", err)
	}

	if _, err := project.ParseSigningKey(key.ID(), []byte(stored)); err == nil {
		t.Error("the stored column parses as a signing key, so it was written in the clear")
	}
}

// TestAKeyThisDeploymentCannotOpenIsAnErrorRatherThanAnAbsence.
//
// Reporting "no key" would make the next start mint a second one beside a key
// it merely could not read, and every token already issued would stop verifying
// against the newly published set.
func TestAKeyThisDeploymentCannotOpenIsAnErrorRatherThanAnAbsence(t *testing.T) {
	db := signingDatabase(t)

	_, err := NewSigningKeyStore(db, aSealingKey(t)).EnsureActive(t.Context(), rand.Reader)
	if err != nil {
		t.Fatalf("EnsureActive: %v", err)
	}

	wrong := NewSigningKeyStore(db, aSealingKey(t))

	if _, found, err := wrong.Active(t.Context()); found || !errors.Is(err, ErrSigningKeyUnsealable) {
		t.Errorf("Active reported found=%v err=%v, want an unsealable error", found, err)
	}

	if _, err := wrong.EnsureActive(t.Context(), rand.Reader); !errors.Is(err, ErrSigningKeyUnsealable) {
		t.Errorf("EnsureActive minted past a key it could not read, err = %v", err)
	}
}

// TestADeploymentWithNoSealingKeySignsNothing.
//
// OpenID Connect is off on such a deployment. What it must not do is store the
// private half in the clear, which would put everybody's identity in the file.
func TestADeploymentWithNoSealingKeySignsNothing(t *testing.T) {
	db := signingDatabase(t)
	store := NewSigningKeyStore(db, project.SealingKey{})

	if store.Available() {
		t.Error("a store with no sealing key reports itself available")
	}

	if _, found, err := store.Active(t.Context()); found || err != nil {
		t.Errorf("Active reported found=%v err=%v, want neither", found, err)
	}

	if _, err := store.EnsureActive(t.Context(), rand.Reader); !errors.Is(err, ErrNoSealingKey) {
		t.Errorf("EnsureActive: %v, want ErrNoSealingKey", err)
	}

	var rows int
	if err := db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM identity_signing_keys").Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}

	if rows != 0 {
		t.Errorf("%d keys were written without a sealing key", rows)
	}
}

// TestASigningKeyIsUsableAfterARoundTripThroughStorage, which is what a token
// minted after a restart depends on.
func TestASigningKeyIsUsableAfterARoundTripThroughStorage(t *testing.T) {
	db := signingDatabase(t)
	sealing := aSealingKey(t)

	if _, err := NewSigningKeyStore(db, sealing).EnsureActive(t.Context(), rand.Reader); err != nil {
		t.Fatalf("EnsureActive: %v", err)
	}

	restored, found, err := NewSigningKeyStore(db, sealing).Active(t.Context())
	if err != nil || !found {
		t.Fatalf("Active: found=%v err=%v", found, err)
	}

	issued := time.UnixMilli(1_700_000_000_000).UTC()

	if _, err := restored.Sign(project.IdentityClaims{
		Issuer: "https://ilavrita.example.test", Subject: "usr_1", Audience: "cli_1",
		IssuedAt: issued, ExpiresAt: issued.Add(5 * time.Minute),
	}); err != nil {
		t.Errorf("a restored key could not sign: %v", err)
	}
}

// TestALostRaceReturnsTheKeyThatLanded.
//
// Two processes starting at once both find no key and both mint one. Only one
// row lands, and the loser must go on to sign with that one: a server signing
// with the key it minted and publishing the key that was stored would issue
// tokens nothing could verify.
//
// The race is staged rather than waited for. A trigger lands the competitor's
// row in the moment between the check and the insert, which is the only window
// where this can go wrong and is not one a test can hit by timing.
func TestALostRaceReturnsTheKeyThatLanded(t *testing.T) {
	db := signingDatabase(t)
	sealing := aSealingKey(t)

	competitor, err := project.NewSigningKey("competitor", rand.Reader)
	if err != nil {
		t.Fatalf("NewSigningKey: %v", err)
	}

	encoded, err := competitor.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	sealed, err := sealing.Seal(encoded)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	// Inlined rather than bound, because SQLite refuses a parameter inside a
	// trigger body. The value is this test's own base64, quoted the way SQLite
	// quotes a literal.
	//nolint:gosec // the value is this test's own base64, and a trigger takes no parameter
	stage := "CREATE TRIGGER stage_the_race BEFORE INSERT ON identity_signing_keys BEGIN" +
		" INSERT INTO identity_signing_keys (id, private_key, state, created_at, retired_at)" +
		" VALUES ('competitor', '" + strings.ReplaceAll(sealed, "'", "''") + "', 'active', 0, NULL);" +
		" END"

	if _, err := db.ExecContext(t.Context(), stage); err != nil {
		t.Fatalf("stage the race: %v", err)
	}

	held, err := NewSigningKeyStore(db, sealing).EnsureActive(t.Context(), rand.Reader)
	if err != nil {
		t.Fatalf("EnsureActive: %v", err)
	}

	if held.ID() != "competitor" {
		t.Fatalf("the loser signs with %q, but %q is the key that landed", held.ID(), "competitor")
	}

	// And what it signs with is what the published set names, which is the thing
	// a client actually depends on.
	published, err := held.PublishedJWKS()
	if err != nil {
		t.Fatalf("PublishedJWKS: %v", err)
	}

	stored, found, err := NewSigningKeyStore(db, sealing).Active(t.Context())
	if err != nil || !found {
		t.Fatalf("Active: found=%v err=%v", found, err)
	}

	expected, err := stored.PublishedJWKS()
	if err != nil {
		t.Fatalf("PublishedJWKS: %v", err)
	}

	if published != expected {
		t.Error("the key signing is not the key published")
	}
}
