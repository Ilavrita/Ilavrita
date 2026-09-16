package project

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

func clinician() PrincipalRef {
	return PrincipalRef{Kind: PrincipalUser, ID: "usr_clinician"}
}

func memberConfig(project ID, kind Kind) MembershipConfig {
	return MembershipConfig{
		ID: "pm_1", Project: project, ProjectKind: kind,
		Principal: clinician(), State: MembershipActive, Source: SourceInvite,
	}
}

func mustMembership(t *testing.T, cfg MembershipConfig) Membership {
	t.Helper()

	member, err := NewMembership(cfg)
	if err != nil {
		t.Fatalf("NewMembership: %v", err)
	}

	return member
}

func TestMembershipStateTransitions(t *testing.T) {
	tests := []struct {
		name  string
		from  MembershipState
		to    MembershipState
		allow bool
	}{
		{name: "invited activates", from: MembershipInvited, to: MembershipActive, allow: true},
		{name: "invited is revocable", from: MembershipInvited, to: MembershipRevoked, allow: true},
		{name: "invited never suspends", from: MembershipInvited, to: MembershipSuspended, allow: false},
		{name: "active suspends", from: MembershipActive, to: MembershipSuspended, allow: true},
		{name: "active revokes", from: MembershipActive, to: MembershipRevoked, allow: true},
		{name: "active never returns to invited", from: MembershipActive, to: MembershipInvited, allow: false},
		{name: "suspended reactivates", from: MembershipSuspended, to: MembershipActive, allow: true},
		{name: "suspended revokes", from: MembershipSuspended, to: MembershipRevoked, allow: true},
		{name: "revoked never reactivates", from: MembershipRevoked, to: MembershipActive, allow: false},
		{name: "revoked never suspends", from: MembershipRevoked, to: MembershipSuspended, allow: false},
		{name: "no self transition", from: MembershipActive, to: MembershipActive, allow: false},
		{name: "no unknown target", from: MembershipActive, to: "elevated", allow: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.from.CanTransitionTo(tc.to) != tc.allow {
				t.Fatalf("CanTransitionTo(%s to %s) = %v, want %v", tc.from, tc.to, !tc.allow, tc.allow)
			}

			_, err := tc.from.TransitionTo(tc.to)
			if tc.allow != (err == nil) {
				t.Fatalf("TransitionTo(%s to %s) = %v", tc.from, tc.to, err)
			}
		})
	}
}

func TestSuperAdminIsRefusedOutsideTheSuperProject(t *testing.T) {
	cfg := memberConfig("prj_clinic", KindStandard)
	cfg.Admin, cfg.SuperAdmin = true, true

	_, err := NewMembership(cfg)
	if !errors.Is(err, ErrSuperAdminOutsideSuperProject) {
		t.Fatalf("NewMembership = %v, want ErrSuperAdminOutsideSuperProject", err)
	}
}

func TestSuperAdminHoldsOnlyInAnActiveSuperProjectMembership(t *testing.T) {
	tests := []struct {
		name  string
		kind  Kind
		state MembershipState
		super bool
	}{
		{name: "active in the super project", kind: KindSuper, state: MembershipActive, super: true},
		{name: "invited in the super project", kind: KindSuper, state: MembershipInvited, super: false},
		{name: "suspended in the super project", kind: KindSuper, state: MembershipSuspended, super: false},
		{name: "revoked in the super project", kind: KindSuper, state: MembershipRevoked, super: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := memberConfig("prj_super", tc.kind)
			cfg.State, cfg.Admin, cfg.SuperAdmin = tc.state, true, true

			member := mustMembership(t, cfg)

			if member.IsSuperAdmin() != tc.super {
				t.Errorf("IsSuperAdmin() = %v, want %v", member.IsSuperAdmin(), tc.super)
			}

			if member.IsAdmin() != (tc.state == MembershipActive) {
				t.Errorf("IsAdmin() = %v for state %s", member.IsAdmin(), tc.state)
			}
		})
	}
}

func TestLinkSourcedMembershipHoldsNoPrivilege(t *testing.T) {
	member, err := NewLinkedMembership("pm_link", "prj_clinic", clinician(), "lnk_org")
	if err != nil {
		t.Fatalf("NewLinkedMembership: %v", err)
	}

	if member.IsAdmin() || member.IsSuperAdmin() {
		t.Error("a link-minted membership must hold no admin standing; that is one write from a data grant")
	}

	if len(member.Policies()) != 0 {
		t.Error("a link-minted membership must bind no policy, so it resolves to an empty Scope")
	}

	if via, ok := member.ViaLink(); !ok || via != "lnk_org" {
		t.Errorf("ViaLink() = %q, %v; want lnk_org, true", via, ok)
	}
}

func TestNewMembershipRefusesToClaimALinkSource(t *testing.T) {
	cfg := memberConfig("prj_clinic", KindStandard)
	cfg.Source = SourceLink
	cfg.Admin = true

	if _, err := NewMembership(cfg); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("NewMembership with a link source = %v, want ErrInvalidSource", err)
	}
}

func TestPoliciesResolveInTheOwningProject(t *testing.T) {
	cfg := memberConfig("prj_clinic", KindStandard)
	cfg.Policies = []PolicyAttachment{{Policy: "practitioner-read", Ordinal: 1, Parameters: json.RawMessage(`{}`)}}

	member := mustMembership(t, cfg)

	bindings := member.Policies()
	if len(bindings) != 1 {
		t.Fatalf("Policies() returned %d bindings, want 1", len(bindings))
	}

	if bindings[0].Policy().Project() != "prj_clinic" {
		t.Errorf("policy resolved against %q, want the membership's own project", bindings[0].Policy().Project())
	}
}

func TestOnlyAnActiveMembershipResolvesToItsPolicies(t *testing.T) {
	tests := []struct {
		name     string
		state    MembershipState
		bindings int
	}{
		{name: "active", state: MembershipActive, bindings: 1},
		{name: "invited", state: MembershipInvited, bindings: 0},
		{name: "suspended", state: MembershipSuspended, bindings: 0},
		{name: "revoked", state: MembershipRevoked, bindings: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := memberConfig("prj_clinic", KindStandard)
			cfg.State = tc.state
			cfg.Policies = []PolicyAttachment{{Policy: "practitioner-read"}}

			member := mustMembership(t, cfg)

			if len(member.Policies()) != tc.bindings {
				t.Errorf("Policies() returned %d bindings, want %d", len(member.Policies()), tc.bindings)
			}
		})
	}
}

func TestPolicyBindingsCannotBeWidenedThroughTheAccessor(t *testing.T) {
	cfg := memberConfig("prj_clinic", KindStandard)
	cfg.Policies = []PolicyAttachment{{Policy: "practitioner-read"}}

	member := mustMembership(t, cfg)

	escaped := member.Policies()
	escaped[0].policy.project = "prj_other"

	if member.Policies()[0].Policy().Project() != "prj_clinic" {
		t.Error("mutating the returned bindings moved a policy into another project")
	}
}

func TestMembershipRejectsAnIncompleteRecord(t *testing.T) {
	tests := []struct {
		name string
		edit func(*MembershipConfig)
		want error
	}{
		{name: "no id", edit: func(c *MembershipConfig) { c.ID = "" }, want: ErrMissingID},
		{name: "no project", edit: func(c *MembershipConfig) { c.Project = "" }, want: ErrInvalidProjectID},
		{name: "wildcard project", edit: func(c *MembershipConfig) { c.Project = "*" }, want: ErrInvalidProjectID},
		{name: "unknown project kind", edit: func(c *MembershipConfig) { c.ProjectKind = "root" }, want: ErrUnknownKind},
		{name: "unnamed principal", edit: func(c *MembershipConfig) { c.Principal = PrincipalRef{} }, want: ErrInvalidPrincipal},
		{name: "unknown state", edit: func(c *MembershipConfig) { c.State = "elevated" }, want: ErrUnknownState},
		{name: "unknown source", edit: func(c *MembershipConfig) { c.Source = "guessed" }, want: ErrInvalidSource},
		{
			name: "profile without an id",
			edit: func(c *MembershipConfig) { c.Profile = &ProfileRef{Type: "Practitioner"} },
			want: ErrInvalidProfile,
		},
		{
			name: "policy without an id",
			edit: func(c *MembershipConfig) { c.Policies = []PolicyAttachment{{}} },
			want: ErrMissingPolicy,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := memberConfig("prj_clinic", KindStandard)
			tc.edit(&cfg)

			if _, err := NewMembership(cfg); !errors.Is(err, tc.want) {
				t.Fatalf("NewMembership = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestProfileCarriesNoProjectOfItsOwn(t *testing.T) {
	profile := reflect.TypeOf(ProfileRef{})

	for i := range profile.NumField() {
		if profile.Field(i).Type == reflect.TypeOf(storage.ProjectID("")) {
			t.Fatalf("ProfileRef.%s names a project; a profile must resolve in the membership's own project",
				profile.Field(i).Name)
		}
	}
}

func TestMembershipTransitionRefusesAForbiddenMove(t *testing.T) {
	member := mustMembership(t, memberConfig("prj_clinic", KindStandard))

	revoked, err := member.TransitionTo(MembershipRevoked)
	if err != nil {
		t.Fatalf("TransitionTo(revoked): %v", err)
	}

	if _, err := revoked.TransitionTo(MembershipActive); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("reactivating a revoked membership = %v, want ErrInvalidTransition", err)
	}
}

func TestStandingIsHeldOnlyWhileActiveAndDirect(t *testing.T) {
	tests := []struct {
		name   string
		state  MembershipState
		stands bool
	}{
		{name: "active", state: MembershipActive, stands: true},
		{name: "invited", state: MembershipInvited},
		{name: "suspended", state: MembershipSuspended},
		{name: "revoked", state: MembershipRevoked},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := memberConfig("prj_clinic", KindStandard)
			cfg.State = tc.state

			if got := mustMembership(t, cfg).HoldsStanding(); got != tc.stands {
				t.Errorf("HoldsStanding() = %v, want %v for a %s membership", got, tc.stands, tc.state)
			}
		})
	}
}

// TestALinkMintedMembershipHoldsNoStanding is FR-052 at the domain edge: an
// administrative link mints an active membership, and standing is what every
// data-plane step reads, so that membership must hold none.
func TestALinkMintedMembershipHoldsNoStanding(t *testing.T) {
	linked, err := NewLinkedMembership("pm_linked", "prj_clinic", clinician(), "lnk_1")
	if err != nil {
		t.Fatalf("NewLinkedMembership: %v", err)
	}

	if linked.State() != MembershipActive {
		t.Fatalf("State() = %s, want an active membership for the case to bite", linked.State())
	}

	if linked.HoldsStanding() {
		t.Error("HoldsStanding() is true for a link-minted membership")
	}
}
