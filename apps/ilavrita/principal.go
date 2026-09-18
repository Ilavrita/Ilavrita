package main

import (
	"errors"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/pocketbase/pocketbase/core"
)

var (
	// errNoPrincipal reports that nothing identified the caller. Who is asking is
	// prior to what they may do, so it is answered before any Scope is built.
	errNoPrincipal = errors.New("ilavrita: nothing identifies the caller of this request")

	// errNotServing reports a route reached before startup wired one. It denies
	// rather than answering from a database nobody opened.
	errNotServing = errors.New("ilavrita: no database is wired to this server")
)

// caller is the identity one request is served as: one principal, and the home
// Project its standing lives in.
type caller struct {
	project   project.ID
	principal project.PrincipalRef
}

// resolve answers who is asking. A session token is the only answer: it is the
// one thing that proves a credential was presented, and a request carrying none
// is refused before a Scope exists to widen.
func (b *backend) resolve(request *core.RequestEvent) (caller, error) {
	session, found, err := b.session(request)
	if err != nil {
		return caller{}, err
	}

	if !found {
		return caller{}, errNoPrincipal
	}

	return caller{project: session.Project(), principal: session.Principal()}, nil
}

// authorizationRequest is the decision one interaction rests on. LinkedProjects
// stays nil: no by-key route lets a caller name a grantor Project, so only the
// caller's own home Project is ever reachable.
func (b *backend) authorizationRequest(request *core.RequestEvent, want decision) (authz.Request, error) {
	who, err := b.resolve(request)
	if err != nil {
		return authz.Request{}, err
	}

	return authz.Request{
		Principal: who.principal,
		Project:   who.project,
		Kind:      want.Kind,
		Type:      want.Type,
		Action:    want.Action,
		Now:       time.Now().UTC(),
		Resolvers: b.resolvers,
	}, nil
}

// authorize is the one way a route reaches storage: it resolves who is asking,
// builds the Scope for this decision and hands back the stores that Scope
// bounds. A route that skips it holds the zero Scope, which grants nothing.
func authorize(request *core.RequestEvent, want decision) (access, error) {
	if serving == nil {
		return access{}, errNotServing
	}

	decided, err := serving.authorizationRequest(request, want)
	if err != nil {
		return access{}, err
	}

	scope, err := authz.BuildScope(request.Request.Context(), decided)
	if err != nil {
		return access{}, err
	}

	return access{
		Resources:    serving.resources,
		Payloads:     serving.payloads,
		Searches:     serving.resources,
		Versions:     serving.resources,
		Transactions: serving.resources,
		Scope:        scope,
		Project:      decided.Project,
	}, nil
}
