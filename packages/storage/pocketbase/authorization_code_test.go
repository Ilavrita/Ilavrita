package pocketbase

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
)

// codeVerifier is the PKCE verifier every code in this file is bound to.
const codeVerifier = "a-verifier-of-exactly-the-length-rfc7636-wants"

// codedAt is the instant the codes here are issued at.
var codedAt = time.UnixMilli(1_700_000_000_000).UTC()

// approvingDatabase is a current database holding a Project, an identity, a
// membership and one registration to issue codes to.
func approvingDatabase(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("sqlite", "file:"+t.TempDir()+"/codes.db?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}

	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	if err := PrepareSchema(t.Context(), db); err != nil {
		t.Fatalf("prepare: %v", err)
	}

	execAll(t, db, []string{
		"INSERT INTO projects (id, kind, slug, name, state, created_at, updated_at, state_changed_at)" +
			" VALUES ('prj_a', 'standard', 'prj_a', 'prj_a', 'active', 0, 0, 0)",
		"INSERT INTO users (id, scope, email_normalized, email_display, state, created_at, updated_at)" +
			" VALUES ('usr_1', 'server', 'one@example.test', 'one@example.test', 'active', 0, 0)",
		"INSERT INTO project_memberships (project_id, id, project_kind, user_id, state," +
			" invitation_source, created_at, updated_at, activated_at)" +
			" VALUES ('prj_a', 'pm_1', 'standard', 'usr_1', 'active', 'api', 0, 0, 0)",
		"INSERT INTO client_applications (project_id, id, name, description, state, kind," +
			" created_at, updated_at, revoked_at, version)" +
			" VALUES ('prj_a', 'cli_app', 'Patient app', '', 'active', 'public', 0, 0, NULL, 1)",
	})

	return db
}

// anApproval mints one code for an approval given at codedAt.
func anApproval(t *testing.T, id project.AuthorizationCodeID) (
	project.AuthorizationCode, project.AuthorizationCodeToken,
) {
	t.Helper()

	return anApprovalAt(t, id, codedAt)
}

// anApprovalAt mints one code for an approval given at some instant.
func anApprovalAt(t *testing.T, id project.AuthorizationCodeID, at time.Time) (
	project.AuthorizationCode, project.AuthorizationCodeToken,
) {
	t.Helper()

	sum := sha256.Sum256([]byte(codeVerifier))

	challenge, err := project.ParseCodeChallenge(base64.RawURLEncoding.EncodeToString(sum[:]), "S256")
	if err != nil {
		t.Fatalf("ParseCodeChallenge: %v", err)
	}

	launch, err := project.NewLaunchContext("pat_7", "patient/Observation.read")
	if err != nil {
		t.Fatalf("NewLaunchContext: %v", err)
	}

	code, token, err := project.IssueAuthorizationCode(
		"prj_a", id, "cli_app", "usr_1", "pm_1",
		"https://app.example.test/callback", challenge, launch,
		at, 30*time.Second, rand.Reader)
	if err != nil {
		t.Fatalf("IssueAuthorizationCode: %v", err)
	}

	return code, token
}

// TestACodeIsRedeemedOnceAndNeverAgain.
//
// Single use is the whole reason a code is short-lived rather than a token: a
// code that survives its redemption is one an interceptor can spend after the
// client already did.
func TestACodeIsRedeemedOnceAndNeverAgain(t *testing.T) {
	db := approvingDatabase(t)
	store := NewAuthorizationCodeStore(db)

	code, token := anApproval(t, "acd_1")
	if err := store.Issue(t.Context(), code); err != nil {
		t.Fatalf("issue: %v", err)
	}

	now := codedAt.Add(time.Second)

	redeemed, found, err := store.Redeem(
		t.Context(), token, "cli_app", "https://app.example.test/callback", codeVerifier, now)
	if err != nil || !found {
		t.Fatalf("first redemption: found %v, err %v", found, err)
	}

	if redeemed.Launch().Scopes() != "patient/Observation.read" {
		t.Errorf("redeemed %v, want the approval that was given", redeemed.Launch())
	}

	_, found, err = store.Redeem(
		t.Context(), token, "cli_app", "https://app.example.test/callback", codeVerifier, now)
	if err != nil {
		t.Fatalf("second redemption: %v", err)
	}

	if found {
		t.Error("a code was redeemed twice")
	}
}

// TestAFailedRedemptionStillSpendsTheCode.
//
// The row is taken before it is judged. A code left alive after somebody holding
// it guessed wrong at the verifier is one they may keep guessing at; a client
// that genuinely lost the race starts the flow again, which costs a redirect.
func TestAFailedRedemptionStillSpendsTheCode(t *testing.T) {
	db := approvingDatabase(t)
	store := NewAuthorizationCodeStore(db)

	code, token := anApproval(t, "acd_1")
	if err := store.Issue(t.Context(), code); err != nil {
		t.Fatalf("issue: %v", err)
	}

	now := codedAt.Add(time.Second)

	// Somebody holding the code but not the verifier.
	if _, found, err := store.Redeem(t.Context(), token, "cli_app",
		"https://app.example.test/callback",
		"a-different-verifier-of-the-very-same-length-ok", now); err != nil || found {
		t.Fatalf("a wrong verifier was accepted: found %v, err %v", found, err)
	}

	// And the rightful client now finds nothing, because the attempt spent it.
	if _, found, err := store.Redeem(t.Context(), token, "cli_app",
		"https://app.example.test/callback", codeVerifier, now); err != nil || found {
		t.Fatalf("the code survived a failed attempt: found %v, err %v", found, err)
	}
}

// TestACodeIsNotRedeemedByWhatItWasNotIssuedTo, through the store rather than
// the domain: the checks must survive the round trip through the row.
func TestACodeIsNotRedeemedByWhatItWasNotIssuedTo(t *testing.T) {
	now := codedAt.Add(time.Second)

	for name, held := range map[string]struct {
		client   project.ClientApplicationID
		redirect string
		verifier string
		when     time.Time
	}{
		"another client":  {"cli_other", "https://app.example.test/callback", codeVerifier, now},
		"another address": {"cli_app", "https://app.example.test/other", codeVerifier, now},
		"no verifier":     {"cli_app", "https://app.example.test/callback", "", now},
		"after expiry": {
			"cli_app", "https://app.example.test/callback", codeVerifier,
			codedAt.Add(31 * time.Second),
		},
	} {
		t.Run(name, func(t *testing.T) {
			db := approvingDatabase(t)
			store := NewAuthorizationCodeStore(db)

			code, token := anApproval(t, "acd_1")
			if err := store.Issue(t.Context(), code); err != nil {
				t.Fatalf("issue: %v", err)
			}

			_, found, err := store.Redeem(
				t.Context(), token, held.client, held.redirect, held.verifier, held.when)
			if err != nil {
				t.Fatalf("redeem: %v", err)
			}

			if found {
				t.Error("a code was redeemed by something it was not issued to")
			}
		})
	}
}

// TestAnUnknownCodeRedeemsNothing, and says so the same way every other refusal
// does, because telling an unknown code from a spent one tells a caller which
// codes exist.
func TestAnUnknownCodeRedeemsNothing(t *testing.T) {
	db := approvingDatabase(t)
	store := NewAuthorizationCodeStore(db)

	invented, err := project.ParseAuthorizationCode("a code nobody issued")
	if err != nil {
		t.Fatalf("ParseAuthorizationCode: %v", err)
	}

	_, found, err := store.Redeem(t.Context(), invented, "cli_app",
		"https://app.example.test/callback", codeVerifier, codedAt.Add(time.Second))
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}

	if found {
		t.Error("a code nobody issued was redeemed")
	}
}

// TestTheCodeItselfIsNeverWritten, so a stolen database yields nothing a caller
// could present.
func TestTheCodeItselfIsNeverWritten(t *testing.T) {
	db := approvingDatabase(t)

	code, token := anApproval(t, "acd_1")
	if err := NewAuthorizationCodeStore(db).Issue(t.Context(), code); err != nil {
		t.Fatalf("issue: %v", err)
	}

	var stored string

	if err := db.QueryRowContext(t.Context(),
		"SELECT code_hash FROM authorization_codes WHERE id = 'acd_1'").Scan(&stored); err != nil {
		t.Fatalf("read the row: %v", err)
	}

	if stored == token.Reveal() {
		t.Fatal("the code was written in the clear")
	}

	if stored != token.Digest() {
		t.Errorf("the row holds %q, which is neither the code nor its digest", stored)
	}
}

// TestSweepingDestroysWhatExpiredAndSparesWhatDidNot.
//
// Both halves matter. A sweep that took everything would sign out every app
// mid-flow, and one that took nothing would leave the table growing forever —
// so the test holds one code of each kind and names which must survive.
func TestSweepingDestroysWhatExpiredAndSparesWhatDidNot(t *testing.T) {
	db := approvingDatabase(t)
	store := NewAuthorizationCodeStore(db)

	// Expires at codedAt+30s.
	expired, _ := anApprovalAt(t, "acd_old", codedAt)
	if err := store.Issue(t.Context(), expired); err != nil {
		t.Fatalf("issue the expired one: %v", err)
	}

	// Approved later, so it expires at codedAt+50s and is still live below.
	live, liveToken := anApprovalAt(t, "acd_new", codedAt.Add(20*time.Second))
	if err := store.Issue(t.Context(), live); err != nil {
		t.Fatalf("issue the live one: %v", err)
	}

	sweptAt := codedAt.Add(31 * time.Second)

	swept, err := store.Sweep(t.Context(), sweptAt)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if swept != 1 {
		t.Errorf("swept %d codes, want only the expired one", swept)
	}

	// The live code is still redeemable, which is the half a sweep must not
	// break: an app mid-flow keeps its approval.
	if _, found, err := store.Redeem(t.Context(), liveToken, "cli_app",
		"https://app.example.test/callback", codeVerifier, sweptAt); err != nil || !found {
		t.Fatalf("the sweep took a live code: found %v, err %v", found, err)
	}
}

// TestACodeHoldingNothingToCompareAgainstIsNotIssued, because a row with no
// digest would be one no presenter could ever match and nothing could ever
// clean up by redeeming.
func TestACodeHoldingNothingToCompareAgainstIsNotIssued(t *testing.T) {
	db := approvingDatabase(t)

	err := NewAuthorizationCodeStore(db).Issue(t.Context(), project.AuthorizationCode{})
	if err == nil {
		t.Fatal("a code carrying nothing to compare against was issued")
	}

	var held int

	if err := db.QueryRowContext(t.Context(),
		"SELECT count(*) FROM authorization_codes").Scan(&held); err != nil {
		t.Fatalf("count: %v", err)
	}

	if held != 0 {
		t.Errorf("a refused issue wrote %d rows", held)
	}
}
