package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/project"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
)

// unclaimedServer is an install nobody has brought into use yet, wired the way
// startup wires one.
func unclaimedServer(t *testing.T) (http.Handler, *sql.DB, string) {
	t.Helper()

	db := preparedDatabase(t)
	dataDir := t.TempDir()

	serve(t, &backend{
		resources: sqlite.NewResourceStore(db),
		users:     sqlite.NewUserStore(db),
		projects:  sqlite.NewProjectStore(db),
		sessions:  sqlite.NewSessionStore(db),
		audits:    sqlite.NewAuditStore(db),
		attempts:  newAttemptLimiter(sqlite.NewAttemptStore(db), testKeys, nil),
		resolvers: authz.Resolvers{
			Memberships: sqlite.NewMembershipResolver(db),
			Projects:    sqlite.NewProjectResolver(db),
			Policies:    sqlite.NewPolicyResolver(db),
			Links:       sqlite.NewLinkStore(db),
		},
	})

	if err := provisionInstall(t.Context(), serving.projects, dataDir); err != nil {
		t.Fatalf("provision the install: %v", err)
	}

	return allRoutes(t), db, dataDir
}

// handedOverToken reads the token the install wrote for whoever holds the host.
func handedOverToken(t *testing.T, dataDir string) string {
	t.Helper()

	// Read through a root, so the path this opens cannot reach past the
	// directory the install handed it over in.
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		t.Fatalf("open %s: %v", dataDir, err)
	}

	defer func() { _ = root.Close() }()

	file, err := root.Open(claimFile)
	if err != nil {
		t.Fatalf("read the claim token: %v", err)
	}

	defer func() { _ = file.Close() }()

	held, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("read the claim token: %v", err)
	}

	return strings.TrimSpace(string(held))
}

// claiming spends a token, or tries to, and answers with the status.
func claiming(t *testing.T, routes http.Handler, token, email string) int {
	t.Helper()

	return call{
		method: http.MethodPost, path: "/auth/claim",
		body: `{"token":"` + token + `","email":"` + email +
			`","password":"a-long-enough-password"}`,
	}.send(t, routes).Code
}

// identitiesIn counts what has been written, which is what says whether a
// refused request wrote anything.
func identitiesIn(t *testing.T, db *sql.DB) int {
	t.Helper()

	var held int
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM users").Scan(&held); err != nil {
		t.Fatalf("count the identities: %v", err)
	}

	return held
}

// TestAnInstallHandsItsClaimToWhoeverHoldsTheHost.
//
// Not to the log. A credential in a log is a credential in every place logs are
// shipped to, and this one makes an administrator of whoever reads it. The data
// directory is the one place whoever runs this server already holds.
func TestAnInstallHandsItsClaimToWhoeverHoldsTheHost(t *testing.T) {
	_, _, dataDir := unclaimedServer(t)

	held, err := os.Stat(filepath.Join(dataDir, claimFile))
	if err != nil {
		t.Fatalf("the claim was not handed over: %v", err)
	}

	if mode := held.Mode().Perm(); mode != 0o600 {
		t.Errorf("the claim is readable as %v", mode)
	}

	if handedOverToken(t, dataDir) == "" {
		t.Error("the claim file is empty")
	}
}

// TestARestartRearmsNothing, because a restart is not a reason to hand out a
// second way in.
func TestARestartRearmsNothing(t *testing.T) {
	_, _, dataDir := unclaimedServer(t)

	first := handedOverToken(t, dataDir)

	for range 3 {
		if err := provisionInstall(t.Context(), serving.projects, dataDir); err != nil {
			t.Fatalf("provision again: %v", err)
		}
	}

	if again := handedOverToken(t, dataDir); again != first {
		t.Error("a restart minted a second claim token")
	}
}

// TestAWrongClaimWritesNothing.
//
// This is the one route an install serves before anybody can authenticate. One
// that created an identity for every request would be a way to fill a database
// from outside, and the request holding a wrong token was never going to claim
// anything anyway.
func TestAWrongClaimWritesNothing(t *testing.T) {
	routes, db, _ := unclaimedServer(t)

	before := identitiesIn(t, db)

	for _, token := range []string{"", "not-the-token", "nor-this-one"} {
		if status := claiming(t, routes, token, "someone@example.test"); status < 400 {
			t.Errorf("a claim holding %q answered %d", token, status)
		}
	}

	if after := identitiesIn(t, db); after != before {
		t.Errorf("%d identities were written by claims that failed", after-before)
	}
}

// TestTheClaimIsSpentOnce, and what it produces is an administrator who can log
// in — which is the whole point: an install nobody can authenticate to is one
// nobody can use.
func TestTheClaimIsSpentOnce(t *testing.T) {
	routes, db, dataDir := unclaimedServer(t)

	token := handedOverToken(t, dataDir)

	if status := claiming(t, routes, token, "admin@example.test"); status != http.StatusCreated {
		t.Fatalf("the claim answered %d", status)
	}

	// The same token again claims nothing, and writes nothing for trying.
	held := identitiesIn(t, db)

	if again := claiming(t, routes, token, "other@example.test"); again < 400 {
		t.Errorf("the claim was spent twice: %d", again)
	}

	if after := identitiesIn(t, db); after != held {
		t.Errorf("a second claim wrote %d identities", after-held)
	}

	// And the administrator it made can log in, with the standing it granted.
	session := call{
		method: http.MethodPost, path: "/auth/login",
		body: `{"project":"super","email":"admin@example.test",` +
			`"password":"a-long-enough-password"}`,
	}.send(t, routes)

	assertStatus(t, session, http.StatusOK)

	var issued struct {
		Token string `json:"token"`
	}

	if err := json.Unmarshal(session.Body.Bytes(), &issued); err != nil {
		t.Fatalf("read the session: %v", err)
	}

	described := call{
		method: http.MethodGet, path: "/auth/session", bearer: issued.Token,
	}.send(t, routes)

	assertStatus(t, described, http.StatusOK)

	if !strings.Contains(described.Body.String(), `"superAdmin":true`) {
		t.Errorf("the claim made no Super Admin: %s", described.Body)
	}
}

// TestAClaimNeedsAnInstallToClaim, so a server whose migration has not run
// answers that rather than creating an identity against nothing.
func TestAClaimNeedsAnInstallToClaim(t *testing.T) {
	db := preparedDatabase(t)

	serve(t, &backend{
		resources: sqlite.NewResourceStore(db),
		users:     sqlite.NewUserStore(db),
		projects:  sqlite.NewProjectStore(db),
	})

	if err := stillClaimable(t.Context(), serving.projects, project.ClaimToken("anything")); err == nil {
		t.Error("an unprovisioned install reported itself claimable")
	}
}
