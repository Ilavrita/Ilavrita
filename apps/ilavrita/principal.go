package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/pocketbase/pocketbase/core"
)

// developmentPrincipalVariable configures one fixed identity with no credential
// behind it. Unset is the safe state and the default: nothing then names a
// caller, so every FHIR route answers 401.
const developmentPrincipalVariable = "ILAVRITA_DEV_PRINCIPAL"

// developmentPrincipalParts is the whole format: <project>:<kind>:<principal>.
const developmentPrincipalParts = 3

var (
	// errNoPrincipal reports that nothing identified the caller. Who is asking is
	// prior to what they may do, so it is answered before any Scope is built.
	errNoPrincipal = errors.New("ilavrita: nothing identifies the caller of this request")

	// errNotServing reports a route reached before startup wired one. It denies
	// rather than answering from a database nobody opened.
	errNotServing = errors.New("ilavrita: no database is wired to this server")

	// errMalformedDevelopmentPrincipal refuses a value the parser cannot read
	// whole, which stops the process rather than degrading it.
	errMalformedDevelopmentPrincipal = errors.New(
		"ilavrita: " + developmentPrincipalVariable + " must read <project>:<kind>:<principal>")
)

// caller is the identity one request is served as: one principal, and the home
// Project its standing lives in.
type caller struct {
	project   project.ID
	principal project.PrincipalRef
}

// configuredDevelopmentPrincipal reads the scaffold from the environment.
// Nothing configured is no error: it is the deny-by-default state every
// deployment that has not opted out of authentication runs in.
func configuredDevelopmentPrincipal() (*caller, error) {
	value := strings.TrimSpace(os.Getenv(developmentPrincipalVariable))
	if value == "" {
		return nil, nil
	}

	return parseDevelopmentPrincipal(value)
}

// parseDevelopmentPrincipal reads one configured identity and refuses anything
// else. A typo falling back to deny-by-default would read as a broken
// deployment; refusing to start reads as the mistake it is.
func parseDevelopmentPrincipal(value string) (*caller, error) {
	parts := strings.Split(value, ":")
	if len(parts) != developmentPrincipalParts {
		return nil, fmt.Errorf("%w: read %d parts", errMalformedDevelopmentPrincipal, len(parts))
	}

	home := project.ID(strings.TrimSpace(parts[0]))
	if err := project.ValidateID(home); err != nil {
		return nil, fmt.Errorf("%s: %w", developmentPrincipalVariable, err)
	}

	principal := project.PrincipalRef{
		Kind: project.PrincipalKind(strings.TrimSpace(parts[1])),
		ID:   project.PrincipalID(strings.TrimSpace(parts[2])),
	}

	if !principal.Valid() {
		return nil, fmt.Errorf("%w: %q names no principal", errMalformedDevelopmentPrincipal, value)
	}

	if err := validateMachineNamespace(principal); err != nil {
		return nil, fmt.Errorf("%s: %w", developmentPrincipalVariable, err)
	}

	return &caller{project: home, principal: principal}, nil
}

// validateMachineNamespace refuses a machine principal whose id lies outside its
// registry's namespace. Such a principal resolves to nothing, so the process
// would start and then deny every request as though it were a policy decision.
func validateMachineNamespace(principal project.PrincipalRef) error {
	switch principal.Kind {
	case project.PrincipalClientApplication:
		return project.ValidateClientApplicationID(project.ClientApplicationID(principal.ID))
	case project.PrincipalBot:
		return project.ValidateBotID(project.BotID(principal.ID))
	default:
		return nil
	}
}

// warnAboutDevelopmentPrincipal says once, at startup, that this process checks
// no credential at all. Once per request the line would be rate-limited into
// invisibility; here it is the first thing an operator reads.
func warnAboutDevelopmentPrincipal(developmentPrincipal *caller) {
	if developmentPrincipal == nil {
		return
	}

	log.Printf(
		"WARNING: %s is set, so FHIR authentication is DISABLED. Every request is served as "+
			"principal %s %q in project %q, with no credential checked anywhere. "+
			"Never set it where real data lives.",
		developmentPrincipalVariable, developmentPrincipal.principal.Kind,
		developmentPrincipal.principal.ID, developmentPrincipal.project,
	)

	// A machine principal also needs a live registry row, which this process
	// cannot check before the database is open. Saying so here is cheaper than an
	// operator reading every request's 401 as a policy decision.
	if developmentPrincipal.principal.Kind != project.PrincipalUser {
		log.Printf(
			"NOTE: %s names a %s, which resolves only while %q is registered and active in project %q.",
			developmentPrincipalVariable, developmentPrincipal.principal.Kind,
			developmentPrincipal.principal.ID, developmentPrincipal.project,
		)
	}
}

// resolve answers who is asking. The scaffold reads nothing from the request:
// deny by default, so without it nothing names a principal and the request is
// refused before a Scope exists to widen.
func (b *backend) resolve(_ *core.RequestEvent) (caller, error) {
	if b.developmentPrincipal == nil {
		return caller{}, errNoPrincipal
	}

	return *b.developmentPrincipal, nil
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
		Versions:     serving.resources,
		Transactions: serving.resources,
		Scope:        scope,
		Project:      decided.Project,
	}, nil
}
