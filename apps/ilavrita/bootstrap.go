package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/pocketbase/pocketbase/core"
)

// An install has to be brought into use by somebody, once.
//
// Every route here resolves standing first, and standing comes from a
// membership, and the first membership has nobody to grant it. That is what the
// claim is: one token, minted when the install is created, spent once to make
// the first Super Admin. It is not first-signup-wins, because whoever reaches
// the port first is not who owns the host.
//
// The token goes to a file rather than to the log. A credential in a log is a
// credential in every place logs are shipped to, and this one makes an
// administrator of whoever reads it.

const (
	// claimFile is where the token is handed over, inside the data directory,
	// which is the one place whoever runs this server already holds.
	claimFile = "claim-token"

	// claimTTL is how long the token stands. Long enough to bring an install up
	// deliberately, short enough that a forgotten one stops being a way in.
	claimTTL = 24 * time.Hour

	// superProjectID is the Project every Super Admin's membership lives in.
	superProjectID = project.ID("prj_super")
)

// provisionInstall creates the Super Project and mints the one claim token.
//
// A second start returns the install untouched and mints nothing: a restart
// must not re-arm a way in, and an install already claimed has no reason to
// hand out another.
func provisionInstall(ctx context.Context, store project.BootstrapStore, dataDir string) error {
	superProject, err := project.NewSuperProject(superProjectID, "super", "Super Project")
	if err != nil {
		return err
	}

	held, err := project.NewBootstrapper(store, nil, rand.Reader).Provision(ctx,
		project.ProvisionConfig{SuperProject: superProject, TokenTTL: claimTTL})
	if err != nil {
		return fmt.Errorf("ilavrita: provision the install: %w", err)
	}

	if !held.Created {
		return nil
	}

	return handOverClaim(dataDir, held.Token)
}

// handOverClaim writes the token where the host's owner can read it and nobody
// else can.
func handOverClaim(dataDir string, token project.ClaimToken) error {
	path := filepath.Join(dataDir, claimFile)

	if err := os.WriteFile(path, []byte(token.Reveal()+"\n"), 0o600); err != nil {
		return fmt.Errorf("ilavrita: hand over the claim token: %w", err)
	}

	// The path, never the token. What this line says is where to look, which is
	// only useful to somebody who can already read the data directory.
	log.Printf("this install is unclaimed; the one claim token is at %s", path)

	return nil
}

// claimRequest is what spends the token: who the first administrator is, and
// the proof they hold the host.
type claimRequest struct {
	Token    string `json:"token"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// claimInstall spends the token and makes the first Super Admin.
//
// It creates the identity as well as the membership, because there is no other
// way to have one: every route that creates an identity resolves standing
// first, and this is the request that brings the first standing into being.
func claimInstall(request *core.RequestEvent) error {
	if serving == nil {
		return refuse(request, errNotServing)
	}

	var body claimRequest
	if err := json.NewDecoder(request.Request.Body).Decode(&body); err != nil {
		return refuse(request, errMalformedControlRequest)
	}

	if body.Token == "" {
		return refuse(request, errMalformedControlRequest)
	}

	ctx := request.Request.Context()

	// Whether this claim can succeed is settled before anything is written.
	// Creating the identity first would make this an unauthenticated route that
	// writes a row for every request — including every request holding a token
	// that was never going to work.
	if err := stillClaimable(ctx, serving.projects, project.ClaimToken(body.Token)); err != nil {
		return refuse(request, err)
	}

	id, err := mintIdentityID()
	if err != nil {
		return refuse(request, err)
	}

	invited, err := serving.invite(ctx, superProjectID, createIdentityRequest{
		ID: id, Email: body.Email, Password: body.Password,
	})
	if err != nil {
		return refuse(request, err)
	}

	membership, err := project.NewBootstrapper(serving.projects, nil, rand.Reader).Claim(
		ctx, project.ClaimToken(body.Token),
		project.PrincipalRef{Kind: project.PrincipalUser, ID: project.PrincipalID(invited.ID())})
	if err != nil {
		return refuse(request, err)
	}

	return request.JSON(http.StatusCreated, claimResponse{
		Project:    string(superProjectID),
		Identity:   string(invited.ID()),
		Membership: string(membership.ID()),
	})
}

// claimResponse names what the claim produced, and carries no credential: the
// password the caller set is proved at login like any other.
type claimResponse struct {
	Project    string `json:"project"`
	Identity   string `json:"identity"`
	Membership string `json:"membership"`
}

// stillClaimable reports whether this install has a claim left to spend.
//
// It is not the check that makes the claim safe — CompleteClaim applies nothing
// unless the row is still pending, which is what settles two callers racing the
// same token. This is what stops an install that was claimed months ago from
// writing an identity for every request that asks.
func stillClaimable(
	ctx context.Context, store project.BootstrapStore, token project.ClaimToken,
) error {
	instance, found, err := store.Instance(ctx)
	if err != nil {
		return err
	}

	if !found {
		return project.ErrNotProvisioned
	}

	if !instance.Accepts(token) {
		return project.ErrClaimTokenInvalid
	}

	return nil
}

// mintIdentityID draws the first administrator's identifier.
//
// Every other identity is named by whoever creates it, and this one has nobody
// to name it: the request that makes it is the request that brings standing
// into being.
func mintIdentityID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("ilavrita: mint an identity: %w", err)
	}

	return "usr_" + base64.RawURLEncoding.EncodeToString(raw), nil
}
