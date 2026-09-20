package project

import (
	"encoding/base64"
	"fmt"
	"io"
	"strings"
	"time"
)

// Prefixes that keep the three principal namespaces disjoint.
// ux_pm_active_principal is unique on the generated principal_id alone, so two
// families sharing an id would be one principal to it.
const (
	clientApplicationPrefix = "cli_"
	botPrefix               = "bot_"
	credentialPrefix        = "cac_"
)

// Sizes of what this file mints, in bytes drawn from the reader.
const (
	principalIDBytes  = 16
	clientSecretBytes = 32
)

// maxCredentialLifetime is how long a secret may live. A secret that outlives
// the quarter it was issued in is one nobody rotated, and the schema states the
// same ceiling so neither side can drift.
const maxCredentialLifetime = 90 * 24 * time.Hour

// ServiceState is a programmatic principal's lifecycle position. There is no
// invited state: nothing invites a machine, and the missing state is the point.
type ServiceState string

// The lifecycle states of a client application or a bot.
const (
	ServiceActive    ServiceState = "active"
	ServiceSuspended ServiceState = "suspended"
	ServiceRevoked   ServiceState = "revoked"
)

// Valid reports whether the state is one this server recognises.
func (s ServiceState) Valid() bool {
	switch s {
	case ServiceActive, ServiceSuspended, ServiceRevoked:
		return true
	default:
		return false
	}
}

// CanTransitionTo reports whether the lifecycle allows this move. Revoked is
// terminal; a returning integration is a new registration, not a revived one.
func (s ServiceState) CanTransitionTo(next ServiceState) bool {
	if s == next || !next.Valid() {
		return false
	}

	switch s {
	case ServiceActive:
		return next == ServiceSuspended || next == ServiceRevoked
	case ServiceSuspended:
		return next == ServiceActive || next == ServiceRevoked
	default:
		return false
	}
}

// TransitionTo returns the next state, or refuses the move. An unrecognised
// current state fails closed rather than being read as the nearest known one.
func (s ServiceState) TransitionTo(next ServiceState) (ServiceState, error) {
	if !s.Valid() {
		return "", fmt.Errorf("%w: %q", ErrUnknownState, string(s))
	}

	if !s.CanTransitionTo(next) {
		return "", fmt.Errorf("%w: %s to %s", ErrInvalidTransition, s, next)
	}

	return next, nil
}

// validateServiceID refuses an id outside its family's space. The prefix is the
// whole guarantee, so it is checked here and again by the table's own CHECK.
func validateServiceID(id, prefix string) error {
	if len(id) <= len(prefix) || !strings.HasPrefix(id, prefix) {
		return fmt.Errorf("%w: %q does not begin %q", ErrInvalidServiceID, id, prefix)
	}

	return nil
}

// mintServiceID draws an identifier in one family's namespace. A short or failed
// read mints nothing rather than an identifier an attacker could predict.
func mintServiceID(random io.Reader, prefix string) (string, error) {
	raw := make([]byte, principalIDBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", fmt.Errorf("project: cannot mint a %q identifier: %w", prefix, err)
	}

	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// MintClientApplicationID draws an id in the namespace the schema CHECK requires.
func MintClientApplicationID(random io.Reader) (ClientApplicationID, error) {
	id, err := mintServiceID(random, clientApplicationPrefix)

	return ClientApplicationID(id), err
}

// MintBotID draws a bot identifier in its own namespace.
func MintBotID(random io.Reader) (BotID, error) {
	id, err := mintServiceID(random, botPrefix)

	return BotID(id), err
}

// MintCredentialID draws a credential identifier.
func MintCredentialID(random io.Reader) (CredentialID, error) {
	id, err := mintServiceID(random, credentialPrefix)

	return CredentialID(id), err
}

// ValidateClientApplicationID reports whether an id names a client application.
// It is exported because a caller naming one it did not mint, such as a
// configured development principal, must be refused before it reaches a query.
func ValidateClientApplicationID(id ClientApplicationID) error {
	return validateServiceID(string(id), clientApplicationPrefix)
}

// ValidateBotID reports whether an id names a bot.
func ValidateBotID(id BotID) error {
	return validateServiceID(string(id), botPrefix)
}

// ClientApplicationID identifies one registered programmatic caller inside one
// Project. It carries the cli_ prefix, which is what keeps it from colliding
// with a user id in the membership uniqueness index.
type ClientApplicationID string

// ClientApplication is one external caller registered in one Project. It holds
// no secret and no hash: credentials are separate records, so no rendering of
// this value can carry credential material however it is formatted.
type ClientApplication struct {
	id          ClientApplicationID
	project     ID
	name        string
	description string
	state       ServiceState
	kind        ClientKind
	redirects   RedirectURIs
}

// ClientKind is whether a registration can keep a secret.
//
// It is recorded rather than derived from whether a credential currently exists,
// because those two differ exactly when it matters: a confidential client whose
// only credential was revoked would, if derived, become a public client — one
// that redeems authorization codes presenting no secret at all. Revoking a
// credential must take capability away, never change which proof is demanded.
type ClientKind string

// The kinds of registration.
const (
	// ClientPublic is a registration that holds no secret: a browser app or a
	// native app, where anything shipped to the device is readable. PKCE is what
	// it proves itself with.
	ClientPublic ClientKind = "public"

	// ClientConfidential is a registration that keeps a secret somewhere a user
	// cannot read, and must present it to redeem a code.
	ClientConfidential ClientKind = "confidential"
)

// Valid reports whether the kind is one this server recognises.
func (k ClientKind) Valid() bool {
	return k == ClientPublic || k == ClientConfidential
}

// KeepsASecret reports whether this registration must present a client secret.
func (k ClientKind) KeepsASecret() bool { return k == ClientConfidential }

// ClientApplicationConfig is what NewClientApplication takes, and the shape a
// store rehydrates a row through. It names no Project: the owning Project is an
// argument, so a registration without one is not expressible and one belonging
// elsewhere is a different registration.
type ClientApplicationConfig struct {
	ID          ClientApplicationID
	Name        string
	Description string
	State       ServiceState

	// Kind is whether this registration keeps a secret. It has no default: a
	// zero value is refused, because the two answers demand different proof and
	// guessing would pick one for a caller who never said.
	Kind ClientKind

	// RedirectURIs are the addresses an authorization code may be handed back
	// to. A registration naming none does no authorization-code flow, which is
	// what a backend service is.
	RedirectURIs RedirectURIs
}

// NewClientApplication registers a caller in one Project.
func NewClientApplication(owner ID, cfg ClientApplicationConfig) (ClientApplication, error) {
	if err := ValidateID(owner); err != nil {
		return ClientApplication{}, err
	}

	if err := ValidateClientApplicationID(cfg.ID); err != nil {
		return ClientApplication{}, err
	}

	if cfg.Name == "" {
		return ClientApplication{}, fmt.Errorf("%w: %s", ErrMissingServiceName, cfg.ID)
	}

	if !cfg.State.Valid() {
		return ClientApplication{}, fmt.Errorf("%w: %q", ErrUnknownState, string(cfg.State))
	}

	if !cfg.Kind.Valid() {
		return ClientApplication{}, fmt.Errorf("%w: %q is not a client kind",
			ErrUnknownKind, string(cfg.Kind))
	}

	return ClientApplication{
		id: cfg.ID, project: owner, name: cfg.Name,
		description: cfg.Description, state: cfg.State,
		kind: cfg.Kind, redirects: cfg.RedirectURIs,
	}, nil
}

// Kind returns whether this registration keeps a secret.
func (a ClientApplication) Kind() ClientKind {
	return a.kind
}

// RedirectURIs returns the addresses an authorization code may be handed back to.
func (a ClientApplication) RedirectURIs() RedirectURIs {
	return a.redirects
}

// ID returns the registration's identifier.
func (a ClientApplication) ID() ClientApplicationID {
	return a.id
}

// Project returns the Project this registration belongs to.
func (a ClientApplication) Project() ID {
	return a.project
}

// Name returns the name an operator revokes this registration by.
func (a ClientApplication) Name() string {
	return a.name
}

// Description returns the registration's description.
func (a ClientApplication) Description() string {
	return a.description
}

// State returns the registration's lifecycle position.
func (a ClientApplication) State() ServiceState {
	return a.state
}

// Principal names this registration as a membership principal. The kind is
// fixed here, so no caller can pair this id with another family's kind.
func (a ClientApplication) Principal() PrincipalRef {
	return PrincipalRef{Kind: PrincipalClientApplication, ID: PrincipalID(a.id)}
}

// TransitionTo returns the registration in its next state.
func (a ClientApplication) TransitionTo(next ServiceState) (ClientApplication, error) {
	state, err := a.state.TransitionTo(next)
	if err != nil {
		return ClientApplication{}, err
	}

	a.state = state

	return a, nil
}

// IssueCredential mints one secret for this registration and returns the record
// beside it. It is a method rather than a free function so a suspended or
// revoked registration cannot be handed a fresh secret.
func (a ClientApplication) IssueCredential(
	cfg CredentialConfig, random io.Reader,
) (Credential, ClientSecret, error) {
	if a.state != ServiceActive {
		return Credential{}, ClientSecret{}, fmt.Errorf("%w: %s is %s", ErrServiceNotActive, a.id, a.state)
	}

	if err := validateServiceID(string(cfg.ID), credentialPrefix); err != nil {
		return Credential{}, ClientSecret{}, err
	}

	if err := cfg.validate(); err != nil {
		return Credential{}, ClientSecret{}, err
	}

	secret, err := mintClientSecret(random)
	if err != nil {
		return Credential{}, ClientSecret{}, err
	}

	return Credential{
		id: cfg.ID, project: a.project, client: a.id, hash: secret.hash(),
		state: CredentialActive, createdAt: cfg.CreatedAt.UTC(), expiresAt: cfg.ExpiresAt.UTC(),
	}, secret, nil
}

// String renders a registration. It holds no credential, so unlike User this
// needs no redaction: there is nothing here a log line could spell out.
func (a ClientApplication) String() string {
	return fmt.Sprintf("client application %s in %s (%s)", a.id, a.project, a.state)
}

// GoString renders the same text, so %#v does not reach the unexported fields
// and print a shape this type does not otherwise show.
func (a ClientApplication) GoString() string {
	return a.String()
}
