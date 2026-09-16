package project

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var bootEpoch = time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC)

func hostOperator() PrincipalRef {
	return PrincipalRef{Kind: PrincipalUser, ID: "usr_operator"}
}

func superProject(t *testing.T) SuperProject {
	t.Helper()

	proj, err := NewSuperProject("prj_super", "super", "Super Project")
	if err != nil {
		t.Fatalf("NewSuperProject: %v", err)
	}

	return proj
}

func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

// stubRandom yields deterministic bytes so a test can assert on what was minted.
// Nothing in production reads from one.
type stubRandom struct{ last byte }

func (r *stubRandom) Read(p []byte) (int, error) {
	for i := range p {
		r.last++
		p[i] = r.last
	}

	return len(p), nil
}

type failingRandom struct{}

func (failingRandom) Read([]byte) (int, error) {
	return 0, errors.New("no entropy available")
}

// fakeBootstrapStore mirrors the conditional write a real store must perform:
// CompleteClaim applies only while the install is still pending.
type fakeBootstrapStore struct {
	instance  Instance
	project   SuperProject
	found     bool
	creates   int
	claims    []Claim
	loadErr   error
	createErr error
	claimErr  error
}

func (s *fakeBootstrapStore) Instance(_ context.Context) (Instance, bool, error) {
	if s.loadErr != nil {
		return Instance{}, false, s.loadErr
	}

	return s.instance, s.found, nil
}

func (s *fakeBootstrapStore) CreateInstance(_ context.Context, project SuperProject, instance Instance) error {
	if s.createErr != nil {
		return s.createErr
	}

	if s.found {
		return errors.New("instance already exists")
	}

	s.project, s.instance, s.found = project, instance, true
	s.creates++

	return nil
}

func (s *fakeBootstrapStore) CompleteClaim(_ context.Context, claim Claim) error {
	if s.claimErr != nil {
		return s.claimErr
	}

	if !s.found || s.instance.State() != BootstrapPending {
		return errors.New("bootstrap is no longer pending")
	}

	s.instance = claim.Instance
	s.claims = append(s.claims, claim)

	return nil
}

func provisioned(t *testing.T, clock func() time.Time) (*Bootstrapper, *fakeBootstrapStore, ClaimToken) {
	t.Helper()

	store := &fakeBootstrapStore{}
	boot := NewBootstrapper(store, clock, &stubRandom{})

	run, err := boot.Provision(context.Background(), ProvisionConfig{
		SuperProject: superProject(t), TokenTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	return boot, store, run.Token
}

func TestProvisionCreatesTheSuperProjectWithNoMembers(t *testing.T) {
	_, store, token := provisioned(t, fixedClock(bootEpoch))

	if store.creates != 1 {
		t.Fatalf("creates = %d, want 1", store.creates)
	}

	if store.project.Kind() != KindSuper || store.project.AllowsClinicalData() {
		t.Errorf("super project = %v/%v, want super kind holding no clinical data",
			store.project.Kind(), store.project.AllowsClinicalData())
	}

	if store.instance.State() != BootstrapPending {
		t.Errorf("state = %q, want pending", store.instance.State())
	}

	if len(store.claims) != 0 {
		t.Errorf("claims = %d, want a super project created with zero members", len(store.claims))
	}

	if token == "" {
		t.Fatal("Provision minted no claim token")
	}
}

func TestProvisionStoresOnlyTheTokenHashAndAnExpiry(t *testing.T) {
	_, store, token := provisioned(t, fixedClock(bootEpoch))

	if store.instance.tokenHash == token.Reveal() {
		t.Fatal("instance stores the claim token in the clear")
	}

	if store.instance.tokenHash != token.hash() {
		t.Errorf("tokenHash = %q, want the token's hash", store.instance.tokenHash)
	}

	expires, ok := store.instance.TokenExpiresAt()
	if !ok || !expires.Equal(bootEpoch.Add(time.Hour)) {
		t.Errorf("TokenExpiresAt() = %v/%v, want the epoch plus the ttl", expires, ok)
	}
}

func TestProvisionIsIdempotentAndNeverIssuesASecondToken(t *testing.T) {
	boot, store, first := provisioned(t, fixedClock(bootEpoch))

	again, err := boot.Provision(context.Background(), ProvisionConfig{
		SuperProject: superProject(t), TokenTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("Provision (second run): %v", err)
	}

	if again.Created {
		t.Error("Created = true on a re-run, want the existing install untouched")
	}

	if again.Token != "" {
		t.Error("a re-run issued a second claim token")
	}

	if store.creates != 1 {
		t.Errorf("creates = %d, want 1", store.creates)
	}

	if store.instance.tokenHash != first.hash() {
		t.Error("a re-run replaced the token on file")
	}
}

func TestProvisionDeniesEveryMalformedRequest(t *testing.T) {
	tests := []struct {
		name string
		cfg  ProvisionConfig
		want error
	}{
		{name: "no super project", cfg: ProvisionConfig{TokenTTL: time.Hour}, want: ErrInvalidProjectID},
		{
			name: "token never expires",
			cfg:  ProvisionConfig{SuperProject: superProject(t), TokenTTL: 0},
			want: ErrInvalidTokenExpiry,
		},
		{
			name: "negative lifetime",
			cfg:  ProvisionConfig{SuperProject: superProject(t), TokenTTL: -time.Hour},
			want: ErrInvalidTokenExpiry,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeBootstrapStore{}
			boot := NewBootstrapper(store, fixedClock(bootEpoch), &stubRandom{})

			run, err := boot.Provision(context.Background(), tc.cfg)

			if !errors.Is(err, tc.want) {
				t.Fatalf("Provision error = %v, want %v", err, tc.want)
			}

			if run.Token != "" || store.creates != 0 {
				t.Error("a refused provision still wrote something")
			}
		})
	}
}

func TestProvisionFailsClosedWhenEntropyOrTheStoreFails(t *testing.T) {
	tests := []struct {
		name  string
		store *fakeBootstrapStore
		boot  func(*fakeBootstrapStore) *Bootstrapper
	}{
		{
			name:  "no entropy",
			store: &fakeBootstrapStore{},
			boot: func(s *fakeBootstrapStore) *Bootstrapper {
				return NewBootstrapper(s, fixedClock(bootEpoch), failingRandom{})
			},
		},
		{
			name:  "instance read fails",
			store: &fakeBootstrapStore{loadErr: errors.New("timeout")},
			boot: func(s *fakeBootstrapStore) *Bootstrapper {
				return NewBootstrapper(s, fixedClock(bootEpoch), &stubRandom{})
			},
		},
		{
			name:  "create fails",
			store: &fakeBootstrapStore{createErr: errors.New("constraint")},
			boot: func(s *fakeBootstrapStore) *Bootstrapper {
				return NewBootstrapper(s, fixedClock(bootEpoch), &stubRandom{})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run, err := tc.boot(tc.store).Provision(context.Background(), ProvisionConfig{
				SuperProject: superProject(t), TokenTTL: time.Hour,
			})

			if err == nil {
				t.Fatal("Provision succeeded, want an error")
			}

			if run.Token != "" || run.Created {
				t.Error("a failed provision returned a token")
			}
		})
	}
}

func TestClaimMintsTheFirstSuperAdmin(t *testing.T) {
	boot, store, token := provisioned(t, fixedClock(bootEpoch))

	member, err := boot.Claim(context.Background(), token, hostOperator())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if !member.IsSuperAdmin() {
		t.Error("IsSuperAdmin() = false, want the first claim to confer it")
	}

	if !member.IsAdmin() {
		t.Error("IsAdmin() = false, want a super admin to be an admin too")
	}

	if member.Project() != store.project.ID() || member.ProjectKind() != KindSuper {
		t.Errorf("membership stands in %q/%q, want the super project", member.Project(), member.ProjectKind())
	}

	if member.Source() != SourceBootstrap {
		t.Errorf("Source() = %q, want bootstrap", member.Source())
	}

	if member.State() != MembershipActive {
		t.Errorf("State() = %q, want active", member.State())
	}

	if _, viaLink := member.ViaLink(); viaLink {
		t.Error("the first super admin is link-sourced")
	}

	if !strings.HasPrefix(string(member.ID()), "pm_") {
		t.Errorf("ID() = %q, want a minted membership id", member.ID())
	}
}

func TestClaimSpendsTheInstanceInOneWrite(t *testing.T) {
	boot, store, token := provisioned(t, fixedClock(bootEpoch))

	member, err := boot.Claim(context.Background(), token, hostOperator())
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if len(store.claims) != 1 {
		t.Fatalf("claims = %d, want the membership and the instance written together", len(store.claims))
	}

	claim := store.claims[0]
	if claim.Membership.ID() != member.ID() || !claim.At.Equal(bootEpoch) {
		t.Errorf("claim = %v at %v, want the minted membership at the claim instant", claim.Membership.ID(), claim.At)
	}

	if !claim.Instance.IsComplete() {
		t.Error("the persisted instance is not complete")
	}

	if _, ok := claim.Instance.TokenExpiresAt(); ok {
		t.Error("a claimed install still holds token material")
	}

	if claim.Instance.tokenHash != "" {
		t.Error("a claimed install still holds the token hash")
	}
}

func TestClaimIsSingleUseEvenWithTheRightToken(t *testing.T) {
	boot, store, token := provisioned(t, fixedClock(bootEpoch))

	if _, err := boot.Claim(context.Background(), token, hostOperator()); err != nil {
		t.Fatalf("first Claim: %v", err)
	}

	second, err := boot.Claim(context.Background(), token, PrincipalRef{Kind: PrincipalUser, ID: "usr_intruder"})

	if !errors.Is(err, ErrBootstrapComplete) {
		t.Fatalf("second Claim error = %v, want ErrBootstrapComplete", err)
	}

	if second.IsSuperAdmin() || second.IsAdmin() || second.ID() != "" {
		t.Error("a replayed token minted a second super admin")
	}

	if len(store.claims) != 1 {
		t.Errorf("claims = %d, want the second claim to write nothing", len(store.claims))
	}
}

func TestClaimDeniesAndMintsNothing(t *testing.T) {
	tests := []struct {
		name      string
		token     func(minted ClaimToken) ClaimToken
		principal PrincipalRef
		at        time.Time
		want      error
	}{
		{
			name:      "wrong token",
			token:     func(ClaimToken) ClaimToken { return "not-the-token" },
			principal: hostOperator(), at: bootEpoch, want: ErrClaimTokenInvalid,
		},
		{
			name:      "empty token",
			token:     func(ClaimToken) ClaimToken { return "" },
			principal: hostOperator(), at: bootEpoch, want: ErrClaimTokenInvalid,
		},
		{
			name:      "token altered by one byte",
			token:     func(m ClaimToken) ClaimToken { return m[:len(m)-1] + "x" },
			principal: hostOperator(), at: bootEpoch, want: ErrClaimTokenInvalid,
		},
		{
			name:      "expired at the instant it expires",
			token:     func(m ClaimToken) ClaimToken { return m },
			principal: hostOperator(), at: bootEpoch.Add(time.Hour), want: ErrClaimTokenExpired,
		},
		{
			name:      "long expired",
			token:     func(m ClaimToken) ClaimToken { return m },
			principal: hostOperator(), at: bootEpoch.Add(48 * time.Hour), want: ErrClaimTokenExpired,
		},
		{
			name:      "unnamed principal",
			token:     func(m ClaimToken) ClaimToken { return m },
			principal: PrincipalRef{}, at: bootEpoch, want: ErrInvalidPrincipal,
		},
		{
			name:      "principal of an unknown kind",
			token:     func(m ClaimToken) ClaimToken { return m },
			principal: PrincipalRef{Kind: "root", ID: "usr_operator"}, at: bootEpoch, want: ErrInvalidPrincipal,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeBootstrapStore{}
			clock := bootEpoch
			boot := NewBootstrapper(store, func() time.Time { return clock }, &stubRandom{})

			run, err := boot.Provision(context.Background(), ProvisionConfig{
				SuperProject: superProject(t), TokenTTL: time.Hour,
			})
			if err != nil {
				t.Fatalf("Provision: %v", err)
			}

			clock = tc.at
			member, err := boot.Claim(context.Background(), tc.token(run.Token), tc.principal)

			if !errors.Is(err, tc.want) {
				t.Fatalf("Claim error = %v, want %v", err, tc.want)
			}

			if member.IsSuperAdmin() || member.IsAdmin() || member.ID() != "" {
				t.Error("a denied claim minted standing")
			}

			if len(store.claims) != 0 || store.instance.State() != BootstrapPending {
				t.Error("a denied claim wrote to the install")
			}
		})
	}
}

func TestClaimDeniesBeforeProvisioning(t *testing.T) {
	store := &fakeBootstrapStore{}
	boot := NewBootstrapper(store, fixedClock(bootEpoch), &stubRandom{})

	member, err := boot.Claim(context.Background(), "any-token", hostOperator())

	if !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("Claim error = %v, want ErrNotProvisioned", err)
	}

	if member.IsSuperAdmin() || len(store.claims) != 0 {
		t.Error("a claim against an unprovisioned install minted standing")
	}
}

func TestClaimDeniesWhenTheInstallHoldsNoToken(t *testing.T) {
	instance, err := NewInstance(InstanceConfig{SuperProject: "prj_super", State: BootstrapPending})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}

	store := &fakeBootstrapStore{instance: instance, found: true}
	boot := NewBootstrapper(store, fixedClock(bootEpoch), &stubRandom{})

	member, err := boot.Claim(context.Background(), "", hostOperator())

	if !errors.Is(err, ErrClaimTokenInvalid) {
		t.Fatalf("Claim error = %v, want ErrClaimTokenInvalid", err)
	}

	if member.IsSuperAdmin() || len(store.claims) != 0 {
		t.Error("an install holding no token still minted standing")
	}
}

func TestClaimDeniesAnInstallInAnUnrecognisedState(t *testing.T) {
	store := &fakeBootstrapStore{instance: Instance{}, found: true}
	boot := NewBootstrapper(store, fixedClock(bootEpoch), &stubRandom{})

	member, err := boot.Claim(context.Background(), "any-token", hostOperator())

	if !errors.Is(err, ErrUnknownState) {
		t.Fatalf("Claim error = %v, want ErrUnknownState", err)
	}

	if member.IsSuperAdmin() || len(store.claims) != 0 {
		t.Error("an install in an unrecognised state minted standing")
	}
}

func TestClaimReturnsNothingWhenTheWriteIsRefused(t *testing.T) {
	tests := []struct {
		name  string
		store func(*fakeBootstrapStore)
		boot  func(*fakeBootstrapStore) *Bootstrapper
	}{
		{
			name:  "the conditional write loses its race",
			store: func(s *fakeBootstrapStore) { s.claimErr = errors.New("bootstrap is no longer pending") },
			boot: func(s *fakeBootstrapStore) *Bootstrapper {
				return NewBootstrapper(s, fixedClock(bootEpoch), &stubRandom{})
			},
		},
		{
			name:  "the instance read fails",
			store: func(s *fakeBootstrapStore) { s.loadErr = errors.New("timeout") },
			boot: func(s *fakeBootstrapStore) *Bootstrapper {
				return NewBootstrapper(s, fixedClock(bootEpoch), &stubRandom{})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			boot, store, token := provisioned(t, fixedClock(bootEpoch))
			tc.store(store)

			member, err := boot.Claim(context.Background(), token, hostOperator())

			if err == nil {
				t.Fatal("Claim succeeded, want an error")
			}

			if member.IsSuperAdmin() || member.ID() != "" {
				t.Error("a failed claim returned standing")
			}

			if len(store.claims) != 0 {
				t.Error("a failed claim recorded a write")
			}
		})
	}
}

func TestClaimFailsClosedWithoutEntropyForTheMembershipID(t *testing.T) {
	store := &fakeBootstrapStore{}
	boot := NewBootstrapper(store, fixedClock(bootEpoch), &stubRandom{})

	run, err := boot.Provision(context.Background(), ProvisionConfig{
		SuperProject: superProject(t), TokenTTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	starved := NewBootstrapper(store, fixedClock(bootEpoch), failingRandom{})
	member, err := starved.Claim(context.Background(), run.Token, hostOperator())

	if err == nil {
		t.Fatal("Claim succeeded without entropy, want an error")
	}

	if member.IsSuperAdmin() || len(store.claims) != 0 {
		t.Error("a claim that could not mint an id still wrote standing")
	}
}

func TestNewInstanceRefusesEveryRowTheSchemaRefuses(t *testing.T) {
	expiry := bootEpoch.Add(time.Hour)

	tests := []struct {
		name string
		cfg  InstanceConfig
		want error
	}{
		{
			name: "pending with a token",
			cfg:  InstanceConfig{SuperProject: "prj_super", State: BootstrapPending, TokenHash: "abc", TokenExpires: expiry},
		},
		{
			name: "complete with no token",
			cfg:  InstanceConfig{SuperProject: "prj_super", State: BootstrapComplete},
		},
		{
			name: "no super project",
			cfg:  InstanceConfig{State: BootstrapPending},
			want: ErrInvalidProjectID,
		},
		{
			name: "wildcard super project",
			cfg:  InstanceConfig{SuperProject: "*", State: BootstrapPending},
			want: ErrInvalidProjectID,
		},
		{
			name: "unknown state",
			cfg:  InstanceConfig{SuperProject: "prj_super", State: "claimed"},
			want: ErrUnknownState,
		},
		{
			name: "token that never expires",
			cfg:  InstanceConfig{SuperProject: "prj_super", State: BootstrapPending, TokenHash: "abc"},
			want: ErrInvalidInstance,
		},
		{
			name: "claimed install keeping a token",
			cfg:  InstanceConfig{SuperProject: "prj_super", State: BootstrapComplete, TokenHash: "abc", TokenExpires: expiry},
			want: ErrInvalidInstance,
		},
		{
			name: "claimed install keeping an expiry",
			cfg:  InstanceConfig{SuperProject: "prj_super", State: BootstrapComplete, TokenExpires: expiry},
			want: ErrInvalidInstance,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			instance, err := NewInstance(tc.cfg)

			if tc.want == nil {
				if err != nil {
					t.Fatalf("NewInstance = %v, want a valid instance", err)
				}

				if instance.SuperProject() != tc.cfg.SuperProject {
					t.Errorf("SuperProject() = %q, want %q", instance.SuperProject(), tc.cfg.SuperProject)
				}

				return
			}

			if !errors.Is(err, tc.want) {
				t.Fatalf("NewInstance error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNewSuperProjectFixesWhatCannotBeChosen(t *testing.T) {
	proj := superProject(t)

	if proj.Kind() != KindSuper || proj.State() != StateActive || proj.AllowsClinicalData() {
		t.Errorf("super project = %q/%q/%v, want an active super project holding no clinical data",
			proj.Kind(), proj.State(), proj.AllowsClinicalData())
	}

	tests := []struct {
		name           string
		id             ID
		slug, projName string
		want           error
	}{
		{name: "no id", id: "", slug: "super", projName: "Super", want: ErrInvalidProjectID},
		{name: "wildcard id", id: "*", slug: "super", projName: "Super", want: ErrInvalidProjectID},
		{name: "no slug", id: "prj_super", slug: "", projName: "Super", want: ErrMissingProjectName},
		{name: "no name", id: "prj_super", slug: "super", projName: "", want: ErrMissingProjectName},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewSuperProject(tc.id, tc.slug, tc.projName); !errors.Is(err, tc.want) {
				t.Fatalf("NewSuperProject error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestClaimTokenRedactsAndHashesStably(t *testing.T) {
	token := ClaimToken("s3cret-claim-token")

	if got := token.String(); got != "[redacted claim token]" {
		t.Errorf("String() = %q, want the token redacted", got)
	}

	if strings.Contains(token.String(), token.Reveal()) {
		t.Error("String() leaks the token")
	}

	if ClaimToken("").String() != "" {
		t.Error("an absent token formats as a redaction")
	}

	if token.hash() != ClaimToken("s3cret-claim-token").hash() {
		t.Error("hash() is not stable")
	}

	if token.hash() == ClaimToken("s3cret-claim-tokem").hash() {
		t.Error("hash() collides for different tokens")
	}

	if strings.Contains(token.hash(), token.Reveal()) {
		t.Error("hash() contains the token")
	}
}

func TestBootstrapStateValid(t *testing.T) {
	tests := []struct {
		name  string
		state BootstrapState
		valid bool
	}{
		{name: "pending", state: BootstrapPending, valid: true},
		{name: "complete", state: BootstrapComplete, valid: true},
		{name: "empty", state: "", valid: false},
		{name: "unknown", state: "claimed", valid: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.state.Valid() != tc.valid {
				t.Errorf("Valid() = %v, want %v", tc.state.Valid(), tc.valid)
			}
		})
	}
}
