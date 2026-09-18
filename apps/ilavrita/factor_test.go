package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
)

// factoredServer wires a server that can hold second factors, and hands back
// the key so a test can read a stored one the way the server does.
func factoredServer(t *testing.T) (http.Handler, *sql.DB, *heldClock) {
	t.Helper()

	routes, db := authenticatedServer(t)

	// A factor turns on a thirty-second step, so the clock is one the test
	// moves rather than one it waits out.
	clock := &heldClock{at: time.Date(2026, time.March, 1, 9, 0, 0, 0, time.UTC)}
	serving.now = clock.now

	encoded, err := project.MintSealingKey(rand.Reader)
	if err != nil {
		t.Fatalf("mint a sealing key: %v", err)
	}

	key, err := project.ParseSealingKey(encoded)
	if err != nil {
		t.Fatalf("parse a sealing key: %v", err)
	}

	serving.factors = sqlite.NewFactorStore(db, key)

	return routes, db, clock
}

// storedSecret reads the sealed material straight out of the table, the way an
// attacker holding the file would.
func storedSecret(t *testing.T, db *sql.DB) string {
	t.Helper()

	var sealed string

	err := db.QueryRowContext(context.Background(),
		"SELECT sealed_secret FROM user_second_factors").Scan(&sealed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("read the stored factor: %v", err)
	}

	return sealed
}

// sessionToken logs in and returns the bearer token, which every route below
// acts as.
func sessionToken(t *testing.T, routes http.Handler, code string) string {
	t.Helper()

	answer := logInWithCode(t, routes, loginAddress, loginPassword, code)
	assertStatus(t, answer, http.StatusOK)

	var held loginResponse
	if err := json.Unmarshal(answer.Body.Bytes(), &held); err != nil {
		t.Fatalf("decode the login: %v", err)
	}

	return held.Token
}

// logInWithCode posts a login carrying a second factor.
func logInWithCode(t *testing.T, routes http.Handler, email, password, code string) *httptest.ResponseRecorder {
	t.Helper()

	body := `{"project":"` + loginSlug + `","email":"` + email +
		`","password":"` + password + `"`
	if code != "" {
		body += `,"code":"` + code + `"`
	}

	return call{
		method: http.MethodPost, path: authBasePath + loginPath,
		body: body + "}", contentType: "application/json", anonymous: true,
	}.send(t, routes)
}

// enrol begins an enrolment as the caller and returns the secret it was shown.
func enrol(t *testing.T, routes http.Handler, token string) project.TOTPSecret {
	t.Helper()

	answer := enrolling(t, routes, token, "")
	assertStatus(t, answer, http.StatusCreated)

	return secretFrom(t, answer)
}

// enrolling makes one enrolment request, with or without a code.
func enrolling(t *testing.T, routes http.Handler, token, code string) *httptest.ResponseRecorder {
	t.Helper()

	body := "{}"
	if code != "" {
		body = `{"code":"` + code + `"}`
	}

	return call{
		method: http.MethodPost, path: authBasePath + factorPath,
		bearer: token, contentType: "application/json", body: body,
	}.send(t, routes)
}

// replace begins a move to a new phone, which needs a code from the old one.
func replace(t *testing.T, routes http.Handler, token, code string) *httptest.ResponseRecorder {
	t.Helper()

	return enrolling(t, routes, token, code)
}

// secretFrom reads the secret an enrolment handed over.
func secretFrom(t *testing.T, answer *httptest.ResponseRecorder) project.TOTPSecret {
	t.Helper()

	var held factorEnrolment
	if err := json.Unmarshal(answer.Body.Bytes(), &held); err != nil {
		t.Fatalf("decode the enrolment: %v", err)
	}

	secret, err := project.ParseTOTPSecret(held.Secret)
	if err != nil {
		t.Fatalf("read the secret it handed over: %v", err)
	}

	return secret
}

// codeNow is the code the enrolled phone would show at the server's own time.
func codeNow(secret project.TOTPSecret, clock *heldClock) string {
	return secret.Code(project.Step(clock.now()))
}

// activate proves an enrolment, which is the only way a factor becomes required.
func activate(t *testing.T, routes http.Handler, token, code string) *httptest.ResponseRecorder {
	t.Helper()

	return call{
		method: http.MethodPost, path: authBasePath + factorActivatePath,
		bearer: token, contentType: "application/json", body: `{"code":"` + code + `"}`,
	}.send(t, routes)
}

// TestAPendingFactorStandsBetweenNobodyAndTheirAccount. Someone who scanned the
// code and then lost the phone must still be able to log in and start again.
func TestAPendingFactorStandsBetweenNobodyAndTheirAccount(t *testing.T) {
	routes, _, _ := factoredServer(t)

	enrol(t, routes, sessionToken(t, routes, ""))

	// Enrolled but never proved, so a login with no code still works.
	assertStatus(t, logInWithCode(t, routes, loginAddress, loginPassword, ""), http.StatusOK)
}

// TestAProvedFactorIsRequiredFromThenOn.
func TestAProvedFactorIsRequiredFromThenOn(t *testing.T) {
	routes, _, clock := factoredServer(t)

	token := sessionToken(t, routes, "")
	secret := enrol(t, routes, token)

	assertStatus(t, activate(t, routes, token, codeNow(secret, clock)), http.StatusOK)

	// Activating spent that code. The next one is the next step, which is what
	// a person gets by waiting for the phone to roll over.
	clock.advance(31 * time.Second)

	// The password alone is no longer enough.
	assertStatus(t, logInWithCode(t, routes, loginAddress, loginPassword, ""), http.StatusUnauthorized)

	// And with the code it is.
	assertStatus(t, logInWithCode(t, routes, loginAddress, loginPassword,
		codeNow(secret, clock)), http.StatusOK)
}

// TestAMissingFactorAnswersExactlyLikeAWrongPassword. A distinct answer would
// tell whoever is guessing that the password was right, and that this identity
// exists and has a second factor.
func TestAMissingFactorAnswersExactlyLikeAWrongPassword(t *testing.T) {
	routes, _, clock := factoredServer(t)

	token := sessionToken(t, routes, "")
	secret := enrol(t, routes, token)
	assertStatus(t, activate(t, routes, token, codeNow(secret, clock)), http.StatusOK)
	clock.advance(31 * time.Second)

	answers := map[string]*httptest.ResponseRecorder{
		"the right password and no code":      logInWithCode(t, routes, loginAddress, loginPassword, ""),
		"the right password and a wrong code": logInWithCode(t, routes, loginAddress, loginPassword, "000000"),
		"a wrong password and the right code": logInWithCode(t, routes, loginAddress, "not the password", codeNow(secret, clock)),
		"an address that does not exist":      logInWithCode(t, routes, "nobody@example.test", loginPassword, "000000"),
	}

	var shape string

	for name, answer := range answers {
		if answer.Code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, answer.Code)
		}

		if shape == "" {
			shape = answer.Body.String()

			continue
		}

		if answer.Body.String() != shape {
			t.Errorf("%s answered a different body:\n%s\n%s", name, shape, answer.Body.String())
		}
	}
}

// TestACodeCannotBeReplayedIntoASecondSession. A code watched over someone's
// shoulder is good for thirty seconds without this.
func TestACodeCannotBeReplayedIntoASecondSession(t *testing.T) {
	routes, _, clock := factoredServer(t)

	token := sessionToken(t, routes, "")
	secret := enrol(t, routes, token)
	assertStatus(t, activate(t, routes, token, codeNow(secret, clock)), http.StatusOK)
	clock.advance(31 * time.Second)

	code := codeNow(secret, clock)

	assertStatus(t, logInWithCode(t, routes, loginAddress, loginPassword, code), http.StatusOK)
	assertStatus(t, logInWithCode(t, routes, loginAddress, loginPassword, code), http.StatusUnauthorized)
}

// TestTheStoredFactorIsSealed. A database read on its own must not hand over
// anyone's second factor.
func TestTheStoredFactorIsSealed(t *testing.T) {
	routes, db, _ := factoredServer(t)

	secret := enrol(t, routes, sessionToken(t, routes, ""))

	stored := storedSecret(t, db)
	if stored == "" {
		t.Fatal("nothing was stored")
	}

	if stored == secret.Encoded() {
		t.Error("the secret is stored as the authenticator app holds it")
	}

	if strings.Contains(stored, secret.Encoded()) {
		t.Error("the stored material carries the secret")
	}
}

// TestAFactorSealedByAnotherKeyDoesNotLetALoginThrough. A misconfigured key
// must not become a way past everyone's second factor.
func TestAFactorSealedByAnotherKeyDoesNotLetALoginThrough(t *testing.T) {
	routes, db, clock := factoredServer(t)

	token := sessionToken(t, routes, "")
	secret := enrol(t, routes, token)
	assertStatus(t, activate(t, routes, token, codeNow(secret, clock)), http.StatusOK)
	clock.advance(31 * time.Second)

	// The deployment comes back up with a different key.
	replaced, err := project.MintSealingKey(rand.Reader)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	other, err := project.ParseSealingKey(replaced)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	serving.factors = sqlite.NewFactorStore(db, other)

	// Neither the code nor its absence gets in.
	assertStatus(t, logInWithCode(t, routes, loginAddress, loginPassword, ""), http.StatusInternalServerError)
	assertStatus(t, logInWithCode(t, routes, loginAddress, loginPassword,
		codeNow(secret, clock)), http.StatusInternalServerError)
}

// TestWithdrawingRequiresACurrentCode. A session is a bearer credential:
// without this, stealing one would be enough to take the second factor off the
// account it protects.
func TestWithdrawingRequiresACurrentCode(t *testing.T) {
	routes, _, clock := factoredServer(t)

	token := sessionToken(t, routes, "")
	secret := enrol(t, routes, token)
	assertStatus(t, activate(t, routes, token, codeNow(secret, clock)), http.StatusOK)
	clock.advance(31 * time.Second)

	withdraw := func(code string) *httptest.ResponseRecorder {
		return call{
			method: http.MethodDelete, path: authBasePath + factorPath,
			bearer: token, contentType: "application/json", body: `{"code":"` + code + `"}`,
		}.send(t, routes)
	}

	assertStatus(t, withdraw("000000"), http.StatusUnauthorized)

	// The factor is still required, so the refusal above changed nothing.
	assertStatus(t, logInWithCode(t, routes, loginAddress, loginPassword, ""), http.StatusUnauthorized)

	assertStatus(t, withdraw(codeNow(secret, clock)), http.StatusNoContent)

	// And now the password alone is enough again.
	assertStatus(t, logInWithCode(t, routes, loginAddress, loginPassword, ""), http.StatusOK)
}

// TestOnlyAnIdentifiedCallerManagesAFactor, and only its own: there is no user
// in the path, because a factor somebody else could enrol for you is not one.
func TestOnlyAnIdentifiedCallerManagesAFactor(t *testing.T) {
	routes, _, _ := factoredServer(t)

	for name, sent := range map[string]call{
		"enrol":    {method: http.MethodPost, path: authBasePath + factorPath, body: "{}"},
		"activate": {method: http.MethodPost, path: authBasePath + factorActivatePath, body: `{"code":"000000"}`},
		"withdraw": {method: http.MethodDelete, path: authBasePath + factorPath, body: `{"code":"000000"}`},
	} {
		sent.anonymous = true
		sent.contentType = "application/json"

		if answer := sent.send(t, routes); answer.Code != http.StatusUnauthorized {
			t.Errorf("%s answered %d to an unidentified caller, want 401", name, answer.Code)
		}
	}
}

// TestADeploymentWithNoKeyHoldsNoFactors, rather than storing the secrets in
// the clear.
func TestADeploymentWithNoKeyHoldsNoFactors(t *testing.T) {
	routes, db, _ := factoredServer(t)

	token := sessionToken(t, routes, "")

	serving.factors = sqlite.NewFactorStore(db, project.SealingKey{})

	answer := call{
		method: http.MethodPost, path: authBasePath + factorPath,
		bearer: token, contentType: "application/json", body: "{}",
	}.send(t, routes)

	if answer.Code != http.StatusNotImplemented {
		t.Errorf("status %d, want 501", answer.Code)
	}

	// And a login needs no code, because nothing could have been enrolled.
	assertStatus(t, logInWithCode(t, routes, loginAddress, loginPassword, ""), http.StatusOK)
}

// TestAWrongCodeOnTheFactorRoutesSaysSo. The caller there is plainly
// authenticated, so answering "no authenticated principal" would send them
// looking for the wrong problem.
func TestAWrongCodeOnTheFactorRoutesSaysSo(t *testing.T) {
	routes, _, clock := factoredServer(t)

	token := sessionToken(t, routes, "")
	secret := enrol(t, routes, token)
	assertStatus(t, activate(t, routes, token, codeNow(secret, clock)), http.StatusOK)
	clock.advance(31 * time.Second)

	// A wrong code while a replacement is awaiting proof, which is the case
	// where a code is what the route is actually asking for.
	assertStatus(t, replace(t, routes, token, codeNow(secret, clock)), http.StatusCreated)
	clock.advance(31 * time.Second)

	answer := activate(t, routes, token, "000000")
	assertStatus(t, answer, http.StatusUnauthorized)

	if !strings.Contains(answer.Body.String(), "second-factor code was refused") {
		t.Errorf("a wrong code answered %s", answer.Body.String())
	}

	// The login route says nothing of the kind, whatever refused it: telling a
	// caller their code was the problem would tell them the password was right.
	refused := logInWithCode(t, routes, loginAddress, loginPassword, "000000")
	assertStatus(t, refused, http.StatusUnauthorized)

	if strings.Contains(refused.Body.String(), "second-factor") {
		t.Errorf("the login route named the second factor: %s", refused.Body.String())
	}
}

// TestAFactorFaultIsNotAWrongPassword. An outage in this server must not lock
// out the people whose code was right, nor count against their throttle.
func TestAFactorFaultIsNotAWrongPassword(t *testing.T) {
	routes, db, clock := factoredServer(t)

	token := sessionToken(t, routes, "")
	secret := enrol(t, routes, token)
	assertStatus(t, activate(t, routes, token, codeNow(secret, clock)), http.StatusOK)
	clock.advance(31 * time.Second)

	// The table goes away under the server, which is what a fault looks like
	// from here.
	if _, err := db.ExecContext(context.Background(),
		"ALTER TABLE user_second_factors RENAME TO user_second_factors_gone"); err != nil {
		t.Fatalf("break the table: %v", err)
	}

	answer := logInWithCode(t, routes, loginAddress, loginPassword, codeNow(secret, clock))
	if answer.Code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500: a fault is not a wrong password", answer.Code)
	}

	// The correct password was never counted as a guess, so the caller is not
	// working through their limit while this server is broken.
	if _, err := db.ExecContext(context.Background(),
		"ALTER TABLE user_second_factors_gone RENAME TO user_second_factors"); err != nil {
		t.Fatalf("restore the table: %v", err)
	}

	clock.advance(31 * time.Second)
	assertStatus(t, logInWithCode(t, routes, loginAddress, loginPassword,
		codeNow(secret, clock)), http.StatusOK)
}

// TestAStolenSessionCannotTurnTheFactorOff.
//
// Re-enrolling is how somebody moves to a new phone, and it is also the request
// somebody holding a stolen session would make to switch the factor off — they
// are the same request. Without a code from the factor in force, the second
// factor would survive nothing it exists to survive.
func TestAStolenSessionCannotTurnTheFactorOff(t *testing.T) {
	routes, _, clock := factoredServer(t)

	token := sessionToken(t, routes, "")
	secret := enrol(t, routes, token)
	assertStatus(t, activate(t, routes, token, codeNow(secret, clock)), http.StatusOK)
	clock.advance(31 * time.Second)

	// Everything somebody holding only the session could present.
	for name, code := range map[string]string{
		"no code at all": "",
		"a wrong code":   "000000",
		"a made-up code": "123456",
		"not a code":     "abcdef",
	} {
		answer := enrolling(t, routes, token, code)
		if answer.Code == http.StatusCreated {
			t.Errorf("%s replaced a factor in force", name)
		}
	}

	// And the factor is still in force.
	assertStatus(t, logInWithCode(t, routes, loginAddress, loginPassword, ""), http.StatusUnauthorized)
	assertStatus(t, logInWithCode(t, routes, loginAddress, loginPassword,
		codeNow(secret, clock)), http.StatusOK)
}

// TestMovingToANewPhoneNeverLeavesTheAccountWithoutAFactor.
//
// The old phone answers until the new one has proved itself. An account that
// lost its second factor for the minute somebody spent scanning a QR code would
// be one an attacker only has to wait for.
func TestMovingToANewPhoneNeverLeavesTheAccountWithoutAFactor(t *testing.T) {
	routes, _, clock := factoredServer(t)

	token := sessionToken(t, routes, "")
	old := enrol(t, routes, token)
	assertStatus(t, activate(t, routes, token, codeNow(old, clock)), http.StatusOK)
	clock.advance(31 * time.Second)

	// The move begins, proved by the old phone.
	begun := replace(t, routes, token, codeNow(old, clock))
	assertStatus(t, begun, http.StatusCreated)

	fresh := secretFrom(t, begun)

	clock.advance(31 * time.Second)

	// In between, the old phone still answers and the password alone does not.
	assertStatus(t, logInWithCode(t, routes, loginAddress, loginPassword, ""), http.StatusUnauthorized)
	assertStatus(t, logInWithCode(t, routes, loginAddress, loginPassword,
		codeNow(old, clock)), http.StatusOK)

	clock.advance(31 * time.Second)

	// The new phone does not answer until it has proved itself.
	assertStatus(t, logInWithCode(t, routes, loginAddress, loginPassword,
		codeNow(fresh, clock)), http.StatusUnauthorized)

	clock.advance(31 * time.Second)
	assertStatus(t, activate(t, routes, token, codeNow(fresh, clock)), http.StatusOK)
	clock.advance(31 * time.Second)

	// Now the new phone answers and the old one does not.
	assertStatus(t, logInWithCode(t, routes, loginAddress, loginPassword,
		codeNow(fresh, clock)), http.StatusOK)

	clock.advance(31 * time.Second)
	assertStatus(t, logInWithCode(t, routes, loginAddress, loginPassword,
		codeNow(old, clock)), http.StatusUnauthorized)
}

// TestAFirstEnrolmentNeedsNoCode, and neither does a second attempt at one: an
// unproved factor protects nobody, and asking for a code would strand whoever's
// first attempt went wrong.
func TestAFirstEnrolmentNeedsNoCode(t *testing.T) {
	routes, _, clock := factoredServer(t)

	token := sessionToken(t, routes, "")

	first := enrolling(t, routes, token, "")
	assertStatus(t, first, http.StatusCreated)

	// A second attempt, the phone having been lost before it was ever proved.
	second := enrolling(t, routes, token, "")
	assertStatus(t, second, http.StatusCreated)

	// It is the second secret that now counts.
	fresh := secretFrom(t, second)
	assertStatus(t, activate(t, routes, token, codeNow(fresh, clock)), http.StatusOK)
	clock.advance(31 * time.Second)

	assertStatus(t, logInWithCode(t, routes, loginAddress, loginPassword,
		codeNow(fresh, clock)), http.StatusOK)

	// And the first, which nobody proved, answers nothing.
	clock.advance(31 * time.Second)
	assertStatus(t, logInWithCode(t, routes, loginAddress, loginPassword,
		codeNow(secretFrom(t, first), clock)), http.StatusUnauthorized)
}

// TestTheCodeThatAuthorisedAMoveCannotAuthoriseItTwice, so a code seen once is
// not a second move somebody else can make.
func TestTheCodeThatAuthorisedAMoveCannotAuthoriseItTwice(t *testing.T) {
	routes, _, clock := factoredServer(t)

	token := sessionToken(t, routes, "")
	secret := enrol(t, routes, token)
	assertStatus(t, activate(t, routes, token, codeNow(secret, clock)), http.StatusOK)
	clock.advance(31 * time.Second)

	code := codeNow(secret, clock)

	assertStatus(t, replace(t, routes, token, code), http.StatusCreated)
	assertStatus(t, replace(t, routes, token, code), http.StatusUnauthorized)
}

// TestConfirmingWithNothingAwaitingProofSaysSo, rather than answering as though
// a code had been wrong.
func TestConfirmingWithNothingAwaitingProofSaysSo(t *testing.T) {
	routes, _, clock := factoredServer(t)

	token := sessionToken(t, routes, "")
	secret := enrol(t, routes, token)
	assertStatus(t, activate(t, routes, token, codeNow(secret, clock)), http.StatusOK)
	clock.advance(31 * time.Second)

	answer := activate(t, routes, token, codeNow(secret, clock))
	assertStatus(t, answer, http.StatusConflict)

	if !strings.Contains(answer.Body.String(), "awaiting proof") {
		t.Errorf("answered %s", answer.Body.String())
	}
}
