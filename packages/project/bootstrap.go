package project

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"time"
)

// Sizes of the secrets this package mints, in bytes drawn from the reader.
const (
	claimTokenBytes  = 32
	membershipIDSize = 16
)

// BootstrapState is an install's position in its one-time claim. Pending is the
// only state that can mint a Super Admin, and complete is terminal.
type BootstrapState string

// The bootstrap states of an install.
const (
	BootstrapPending  BootstrapState = "pending"
	BootstrapComplete BootstrapState = "complete"
)

// Valid reports whether the state is one this server recognises.
func (s BootstrapState) Valid() bool {
	return s == BootstrapPending || s == BootstrapComplete
}

// ClaimToken is the single-use secret that claims an install. It is handed to
// whoever holds the host once and never stored, only its hash.
type ClaimToken string

// Reveal returns the token in the clear, for the one host-side handover.
func (t ClaimToken) Reveal() string {
	return string(t)
}

// String redacts, so a token cannot reach a log line through ordinary
// formatting and Reveal stays the only way to read one.
func (t ClaimToken) String() string {
	if t == "" {
		return ""
	}

	return "[redacted claim token]"
}

// hash is what the instance row keeps. The token is 32 bytes of entropy, so a
// single SHA-256 leaves a guesser nothing to shorten.
func (t ClaimToken) hash() string {
	sum := sha256.Sum256([]byte(t))

	return hex.EncodeToString(sum[:])
}

// mintToken draws a token from the reader. A short or failed read mints nothing
// rather than a token an attacker could predict.
func mintToken(random io.Reader) (ClaimToken, error) {
	raw := make([]byte, claimTokenBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", fmt.Errorf("project: cannot mint a claim token: %w", err)
	}

	return ClaimToken(base64.RawURLEncoding.EncodeToString(raw)), nil
}

// SuperProject describes the Project an install is administered from. Its kind
// and its refusal of clinical data are fixed here, never passed in.
type SuperProject struct {
	id   ID
	slug string
	name string
}

// NewSuperProject describes the Project the migration creates. It is created
// with no members: standing in it comes only from spending the claim token.
func NewSuperProject(id ID, slug, name string) (SuperProject, error) {
	if err := ValidateID(id); err != nil {
		return SuperProject{}, err
	}

	if slug == "" || name == "" {
		return SuperProject{}, fmt.Errorf("%w: %s", ErrMissingProjectName, id)
	}

	return SuperProject{id: id, slug: slug, name: name}, nil
}

// ID returns the Project's identifier.
func (p SuperProject) ID() ID {
	return p.id
}

// Slug returns the name a request resolves the Project by.
func (p SuperProject) Slug() string {
	return p.slug
}

// Name returns the Project's display name.
func (p SuperProject) Name() string {
	return p.name
}

// Kind is always the super kind, so no caller can provision this Project as an
// ordinary one and no ordinary Project can be provisioned through here.
func (p SuperProject) Kind() Kind {
	return KindSuper
}

// State is the state the Project is created in.
func (p SuperProject) State() State {
	return StateActive
}

// AllowsClinicalData is always false: the Super Project administers, and never
// holds patient data, mirroring the schema CHECK rather than trusting a caller.
func (p SuperProject) AllowsClinicalData() bool {
	return false
}

// Instance is the one row describing this install: which Project administers it
// and whether its single claim has been spent.
type Instance struct {
	superProject ID
	state        BootstrapState
	tokenHash    string
	expiresAt    time.Time
}

// InstanceConfig is the input to NewInstance, and the shape a store rehydrates
// a persisted instance row through.
type InstanceConfig struct {
	SuperProject ID
	State        BootstrapState
	TokenHash    string
	TokenExpires time.Time
}

// NewInstance builds the install record, refusing every row the instance table
// refuses. A claimed install holding token material would be claimable twice.
func NewInstance(cfg InstanceConfig) (Instance, error) {
	if err := ValidateID(cfg.SuperProject); err != nil {
		return Instance{}, err
	}

	if !cfg.State.Valid() {
		return Instance{}, fmt.Errorf("%w: %q", ErrUnknownState, string(cfg.State))
	}

	if cfg.TokenHash != "" && cfg.TokenExpires.IsZero() {
		return Instance{}, fmt.Errorf("%w: a token without an expiry never dies", ErrInvalidInstance)
	}

	if cfg.State == BootstrapComplete && (cfg.TokenHash != "" || !cfg.TokenExpires.IsZero()) {
		return Instance{}, fmt.Errorf("%w: a claimed install keeps no token", ErrInvalidInstance)
	}

	return Instance{
		superProject: cfg.SuperProject, state: cfg.State,
		tokenHash: cfg.TokenHash, expiresAt: cfg.TokenExpires,
	}, nil
}

// SuperProject returns the Project this install is administered from.
func (i Instance) SuperProject() ID {
	return i.superProject
}

// State returns the install's bootstrap position.
func (i Instance) State() BootstrapState {
	return i.state
}

// IsComplete reports whether the install's one claim has been spent.
func (i Instance) IsComplete() bool {
	return i.state == BootstrapComplete
}

// TokenExpiresAt returns when the claim token stops being redeemable, if one is
// still on file.
func (i Instance) TokenExpiresAt() (time.Time, bool) {
	if i.tokenHash == "" {
		return time.Time{}, false
	}

	return i.expiresAt, true
}

// TokenDigest returns the material the insert binds. It is the hash and never
// the token, mirroring Credential.StoredHash: a store must persist it, and
// nothing else has any use for it.
func (i Instance) TokenDigest() string {
	return i.tokenHash
}

// matches reports whether this is the token on file, compared in constant time
// so a wrong guess cannot be narrowed by how long the answer took.
func (i Instance) matches(token ClaimToken) bool {
	if i.tokenHash == "" || token == "" {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(i.tokenHash), []byte(token.hash())) == 1
}

// spend returns the install with its claim spent. The hash and the expiry go
// with it, so the same token cannot be presented a second time.
func (i Instance) spend() Instance {
	i.state, i.tokenHash, i.expiresAt = BootstrapComplete, "", time.Time{}

	return i
}

// Claim is the single write that ends bootstrap: the first Super Admin and the
// spent instance row, which a store must apply as one conditional transaction.
type Claim struct {
	Instance   Instance
	Membership Membership
	At         time.Time
}

// BootstrapStore persists an install. CompleteClaim must apply its two writes in
// one transaction, conditional on the row still being pending, so two callers
// racing the same token cannot both mint a Super Admin.
type BootstrapStore interface {
	// Instance reads the install record, reporting false when the migration has
	// not provisioned one yet.
	Instance(ctx context.Context) (Instance, bool, error)

	// CreateInstance writes the Super Project and the install record together. It
	// must fail rather than overwrite when either already exists.
	CreateInstance(ctx context.Context, project SuperProject, instance Instance) error

	// CompleteClaim inserts the membership and spends the install in one
	// transaction, applying nothing unless the row is still pending.
	CompleteClaim(ctx context.Context, claim Claim) error
}

// ProvisionConfig is what provisioning needs: the Project to administer from,
// and how long its one claim token stays redeemable.
type ProvisionConfig struct {
	SuperProject SuperProject
	TokenTTL     time.Duration
}

// Provisioning is what a bootstrap run produced. Token carries a value only on
// the run that created the install; a later run returns the install and no token.
type Provisioning struct {
	Instance Instance
	Token    ClaimToken
	Created  bool
}

// Bootstrapper provisions an install and spends its one claim. It is the only
// path that mints the first Super Admin, which is why no environment variable
// confers one and no signup promotes itself.
type Bootstrapper struct {
	store  BootstrapStore
	now    func() time.Time
	random io.Reader
}

// NewBootstrapper wires a bootstrapper. A nil clock or reader falls back to the
// real ones, so weak wiring cannot substitute a predictable token.
func NewBootstrapper(store BootstrapStore, now func() time.Time, random io.Reader) *Bootstrapper {
	if now == nil {
		now = time.Now
	}

	if random == nil {
		random = rand.Reader
	}

	return &Bootstrapper{store: store, now: now, random: random}
}

// Provision creates the Super Project and the install record and mints the one
// claim token. A second run returns the existing install untouched, so a restart
// re-arms nothing and never issues a second token.
func (b *Bootstrapper) Provision(ctx context.Context, cfg ProvisionConfig) (Provisioning, error) {
	if err := ValidateID(cfg.SuperProject.ID()); err != nil {
		return Provisioning{}, err
	}

	if cfg.TokenTTL <= 0 {
		return Provisioning{}, fmt.Errorf("%w: %s", ErrInvalidTokenExpiry, cfg.TokenTTL)
	}

	existing, found, err := b.store.Instance(ctx)
	if err != nil {
		return Provisioning{}, err
	}

	if found {
		return Provisioning{Instance: existing}, nil
	}

	token, err := mintToken(b.random)
	if err != nil {
		return Provisioning{}, err
	}

	instance, err := NewInstance(InstanceConfig{
		SuperProject: cfg.SuperProject.ID(), State: BootstrapPending,
		TokenHash: token.hash(), TokenExpires: b.now().Add(cfg.TokenTTL),
	})
	if err != nil {
		return Provisioning{}, err
	}

	if err := b.store.CreateInstance(ctx, cfg.SuperProject, instance); err != nil {
		return Provisioning{}, err
	}

	return Provisioning{Instance: instance, Token: token, Created: true}, nil
}

// Claim spends the install's one token and mints the first Super Admin. Every
// failure path mints nothing: a wrong, expired, unminted or already-spent token
// leaves the install exactly as it was.
func (b *Bootstrapper) Claim(ctx context.Context, token ClaimToken, principal PrincipalRef) (Membership, error) {
	if !principal.Valid() {
		return Membership{}, fmt.Errorf("%w: %q", ErrInvalidPrincipal, string(principal.Kind))
	}

	instance, found, err := b.store.Instance(ctx)
	if err != nil {
		return Membership{}, err
	}

	if !found {
		return Membership{}, ErrNotProvisioned
	}

	// An unrecognised state denies as itself rather than being read as the
	// nearest known one, which for a bootstrap row would be pending.
	if !instance.state.Valid() {
		return Membership{}, fmt.Errorf("%w: %q", ErrUnknownState, string(instance.state))
	}

	if instance.state != BootstrapPending {
		return Membership{}, fmt.Errorf("%w: %s", ErrBootstrapComplete, instance.superProject)
	}

	// The hash is checked before the expiry, so only a caller who already held
	// the right token learns that it aged out.
	if !instance.matches(token) {
		return Membership{}, ErrClaimTokenInvalid
	}

	now := b.now()
	if !now.Before(instance.expiresAt) {
		return Membership{}, fmt.Errorf("%w: %s", ErrClaimTokenExpired, instance.expiresAt.UTC().Format(time.RFC3339))
	}

	member, err := b.firstSuperAdmin(instance.superProject, principal)
	if err != nil {
		return Membership{}, err
	}

	claim := Claim{Instance: instance.spend(), Membership: member, At: now}
	if err := b.store.CompleteClaim(ctx, claim); err != nil {
		return Membership{}, err
	}

	return member, nil
}

// firstSuperAdmin builds the standing a claim confers. Admin rides along because
// the schema refuses a super admin that is not also an admin.
func (b *Bootstrapper) firstSuperAdmin(super ID, principal PrincipalRef) (Membership, error) {
	id, err := b.mintMembershipID()
	if err != nil {
		return Membership{}, err
	}

	return NewMembership(MembershipConfig{
		ID: id, Project: super, ProjectKind: KindSuper,
		Principal: principal, State: MembershipActive,
		Admin: true, SuperAdmin: true, Source: SourceBootstrap,
	})
}

// mintMembershipID draws the first membership's identifier, so no caller can
// choose one that collides with a membership the install already holds.
func (b *Bootstrapper) mintMembershipID() (MembershipID, error) {
	raw := make([]byte, membershipIDSize)
	if _, err := io.ReadFull(b.random, raw); err != nil {
		return "", fmt.Errorf("project: cannot mint a membership id: %w", err)
	}

	return MembershipID("pm_" + hex.EncodeToString(raw)), nil
}
