package project

import (
	"errors"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/storage"
)

var linkEpoch = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

func approved(t *testing.T, link Link) Link {
	t.Helper()

	link, err := link.ApproveGrantor("pm_grantor", linkEpoch)
	if err != nil {
		t.Fatalf("ApproveGrantor: %v", err)
	}

	link, err = link.ApproveGrantee("pm_grantee", linkEpoch)
	if err != nil {
		t.Fatalf("ApproveGrantee: %v", err)
	}

	return link
}

func activated(t *testing.T, link Link) Link {
	t.Helper()

	link, err := approved(t, link).TransitionTo(LinkActive, linkEpoch)
	if err != nil {
		t.Fatalf("TransitionTo(active): %v", err)
	}

	return link
}

func proposedDataLink(t *testing.T, id LinkID, grantor, grantee ID) Link {
	t.Helper()

	link, err := NewDataLink(id, grantor, grantee, "shared-observations", "Observation")
	if err != nil {
		t.Fatalf("NewDataLink: %v", err)
	}

	return link
}

func proposedAdminLink(t *testing.T, id LinkID, grantor, grantee ID) Link {
	t.Helper()

	link, err := NewAdminLink(id, grantor, grantee,
		[]PrincipalRef{clinician()}, CapabilityQuotaWrite, CapabilitySettingsWrite)
	if err != nil {
		t.Fatalf("NewAdminLink: %v", err)
	}

	return link
}

func TestLinkStatusTransitions(t *testing.T) {
	tests := []struct {
		name  string
		from  LinkStatus
		to    LinkStatus
		allow bool
	}{
		{name: "proposed activates", from: LinkProposed, to: LinkActive, allow: true},
		{name: "proposed revokes", from: LinkProposed, to: LinkRevoked, allow: true},
		{name: "proposed never suspends", from: LinkProposed, to: LinkSuspended, allow: false},
		{name: "active suspends", from: LinkActive, to: LinkSuspended, allow: true},
		{name: "active revokes", from: LinkActive, to: LinkRevoked, allow: true},
		{name: "active never returns to proposed", from: LinkActive, to: LinkProposed, allow: false},
		{name: "suspended reactivates", from: LinkSuspended, to: LinkActive, allow: true},
		{name: "suspended revokes", from: LinkSuspended, to: LinkRevoked, allow: true},
		{name: "revoked never reactivates", from: LinkRevoked, to: LinkActive, allow: false},
		{name: "revoked never suspends", from: LinkRevoked, to: LinkSuspended, allow: false},
		{name: "no self transition", from: LinkActive, to: LinkActive, allow: false},
		{name: "no unknown target", from: LinkActive, to: "trusted", allow: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.from.CanTransitionTo(tc.to) != tc.allow {
				t.Fatalf("CanTransitionTo(%s to %s) = %v, want %v", tc.from, tc.to, !tc.allow, tc.allow)
			}
		})
	}
}

func TestActivationNeedsBothApprovals(t *testing.T) {
	tests := []struct {
		name    string
		approve func(t *testing.T, link Link) Link
		allow   bool
	}{
		{
			name:    "neither side",
			approve: func(_ *testing.T, link Link) Link { return link },
			allow:   false,
		},
		{
			name: "grantor only",
			approve: func(t *testing.T, link Link) Link {
				t.Helper()
				link, err := link.ApproveGrantor("pm_grantor", linkEpoch)
				if err != nil {
					t.Fatalf("ApproveGrantor: %v", err)
				}

				return link
			},
			allow: false,
		},
		{
			name: "grantee only",
			approve: func(t *testing.T, link Link) Link {
				t.Helper()
				link, err := link.ApproveGrantee("pm_grantee", linkEpoch)
				if err != nil {
					t.Fatalf("ApproveGrantee: %v", err)
				}

				return link
			},
			allow: false,
		},
		{name: "both sides", approve: approved, allow: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			link := tc.approve(t, proposedDataLink(t, "lnk_1", "prj_clinic", "prj_org"))

			active, err := link.TransitionTo(LinkActive, linkEpoch)
			switch {
			case tc.allow && err != nil:
				t.Fatalf("TransitionTo(active) = %v, want nil", err)
			case tc.allow && !active.Effective(linkEpoch):
				t.Fatal("a fully approved, activated link must be effective")
			case !tc.allow && !errors.Is(err, ErrApprovalIncomplete):
				t.Fatalf("TransitionTo(active) = %v, want ErrApprovalIncomplete", err)
			}
		})
	}
}

func TestALinkThatIsNotEffectiveGrantsNothing(t *testing.T) {
	expiry := linkEpoch.Add(time.Hour)

	tests := []struct {
		name  string
		build func(t *testing.T) Link
		at    time.Time
	}{
		{
			name:  "proposed but never approved",
			build: func(t *testing.T) Link { t.Helper(); return proposedDataLink(t, "lnk_1", "prj_clinic", "prj_org") },
			at:    linkEpoch,
		},
		{
			name: "approved but never activated",
			build: func(t *testing.T) Link {
				t.Helper()
				return approved(t, proposedDataLink(t, "lnk_1", "prj_clinic", "prj_org"))
			},
			at: linkEpoch,
		},
		{
			name: "suspended",
			build: func(t *testing.T) Link {
				t.Helper()
				link, err := activated(t, proposedDataLink(t, "lnk_1", "prj_clinic", "prj_org")).
					TransitionTo(LinkSuspended, linkEpoch)
				if err != nil {
					t.Fatalf("TransitionTo(suspended): %v", err)
				}

				return link
			},
			at: linkEpoch,
		},
		{
			name: "revoked",
			build: func(t *testing.T) Link {
				t.Helper()
				link, err := activated(t, proposedDataLink(t, "lnk_1", "prj_clinic", "prj_org")).
					TransitionTo(LinkRevoked, linkEpoch)
				if err != nil {
					t.Fatalf("TransitionTo(revoked): %v", err)
				}

				return link
			},
			at: linkEpoch,
		},
		{
			name: "expired",
			build: func(t *testing.T) Link {
				t.Helper()
				return activated(t, proposedDataLink(t, "lnk_1", "prj_clinic", "prj_org")).WithExpiry(expiry)
			},
			at: expiry,
		},
		{
			name: "read before it activated",
			build: func(t *testing.T) Link {
				t.Helper()
				return activated(t, proposedDataLink(t, "lnk_1", "prj_clinic", "prj_org"))
			},
			at: linkEpoch.Add(-time.Hour),
		},
		{
			name:  "the zero link",
			build: func(_ *testing.T) Link { return Link{} },
			at:    linkEpoch,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			link := tc.build(t)

			if link.Effective(tc.at) {
				t.Fatal("this link must not be effective")
			}

			if share, ok := link.Share(tc.at); ok || len(share.ResourceTypes()) != 0 {
				t.Error("an ineffective link shared a resource type")
			}

			if delegation, ok := link.Delegation(tc.at); ok || len(delegation.Capabilities()) != 0 {
				t.Error("an ineffective link delegated a capability")
			}
		})
	}
}

func TestAdministrativeCapabilityNeverBecomesADataScope(t *testing.T) {
	admin := activated(t, proposedAdminLink(t, "lnk_admin", "prj_clinic", "prj_org"))

	if _, ok := admin.Share(linkEpoch); ok {
		t.Fatal("an administrative link produced a data share; that is the FR-052 escalation")
	}

	delegation, ok := admin.Delegation(linkEpoch)
	if !ok {
		t.Fatal("an active administrative link must delegate its capabilities")
	}

	for _, capability := range delegation.Capabilities() {
		if !capability.LinkConferrable() {
			t.Errorf("delegation carries %q, which no link may confer", capability)
		}
	}
}

func TestDataLinkDelegatesNoControlPlaneCapability(t *testing.T) {
	data := activated(t, proposedDataLink(t, "lnk_data", "prj_clinic", "prj_org"))

	if _, ok := data.Delegation(linkEpoch); ok {
		t.Fatal("a data link delegated a control-plane capability")
	}

	if data.Kind() != LinkKindData {
		t.Errorf("Kind() = %q, want %q", data.Kind(), LinkKindData)
	}
}

func TestCapabilitiesThatNoLinkMayConfer(t *testing.T) {
	forbidden := []AdminCapability{
		CapabilityMembershipWrite,
		CapabilityMembershipAdmin,
		CapabilityPolicyWrite,
		CapabilitySecretWrite,
		CapabilityAuthPolicyWrite,
		CapabilityIdentityProvider,
		"project.anything.invented.later",
	}

	for _, capability := range forbidden {
		t.Run(string(capability), func(t *testing.T) {
			if capability.LinkConferrable() {
				t.Fatalf("%q must not be link-conferrable; it is one write from a data grant", capability)
			}

			_, err := NewAdminLink("lnk_1", "prj_clinic", "prj_org", []PrincipalRef{clinician()}, capability)
			if !errors.Is(err, ErrCapabilityNotLinkConferrable) {
				t.Fatalf("NewAdminLink(%q) = %v, want ErrCapabilityNotLinkConferrable", capability, err)
			}
		})
	}
}

func TestDelegationAnswersOnlyForNamedPrincipalsAndCapabilities(t *testing.T) {
	admin := activated(t, proposedAdminLink(t, "lnk_admin", "prj_clinic", "prj_org"))

	delegation, ok := admin.Delegation(linkEpoch)
	if !ok {
		t.Fatal("an active administrative link must delegate")
	}

	stranger := PrincipalRef{Kind: PrincipalUser, ID: "usr_stranger"}

	tests := []struct {
		name       string
		capability AdminCapability
		principal  PrincipalRef
		allow      bool
	}{
		{name: "named principal, named capability", capability: CapabilityQuotaWrite, principal: clinician(), allow: true},
		{name: "named principal, unnamed capability", capability: CapabilityMembershipRead, principal: clinician()},
		{name: "named principal, forbidden capability", capability: CapabilityMembershipAdmin, principal: clinician()},
		{name: "unnamed principal", capability: CapabilityQuotaWrite, principal: stranger},
		{name: "zero principal", capability: CapabilityQuotaWrite, principal: PrincipalRef{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := delegation.Allows(tc.capability, tc.principal); got != tc.allow {
				t.Fatalf("Allows(%q, %q) = %v, want %v", tc.capability, tc.principal.ID, got, tc.allow)
			}
		})
	}
}

func TestAdministrativeLinkMustNameItsPrincipals(t *testing.T) {
	_, err := NewAdminLink("lnk_admin", "prj_clinic", "prj_org", nil, CapabilityQuotaWrite)
	if !errors.Is(err, ErrUnnamedPrincipal) {
		t.Fatalf("NewAdminLink with no principals = %v, want ErrUnnamedPrincipal", err)
	}
}

func TestLinksDoNotCompose(t *testing.T) {
	clinicToRegion := activated(t, proposedDataLink(t, "lnk_cr", "prj_clinic", "prj_region"))
	regionToOrg := activated(t, proposedDataLink(t, "lnk_ro", "prj_region", "prj_org"))
	links := []Link{clinicToRegion, regionToOrg}

	tests := []struct {
		name     string
		grantee  ID
		grantors []ID
	}{
		{name: "region reaches the clinic", grantee: "prj_region", grantors: []ID{"prj_clinic"}},
		{name: "org reaches the region only", grantee: "prj_org", grantors: []ID{"prj_region"}},
		{name: "the clinic reaches nothing", grantee: "prj_clinic", grantors: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reaching := Inbound(links, tc.grantee, linkEpoch)

			if len(reaching) != len(tc.grantors) {
				t.Fatalf("Inbound(%s) returned %d links, want %d", tc.grantee, len(reaching), len(tc.grantors))
			}

			for i, link := range reaching {
				if link.Grantor() != tc.grantors[i] {
					t.Fatalf("Inbound(%s)[%d] reaches %s, want %s", tc.grantee, i, link.Grantor(), tc.grantors[i])
				}
			}
		})
	}
}

func TestAdministrativeLinksDoNotComposeEither(t *testing.T) {
	clinicToRegion := activated(t, proposedAdminLink(t, "lnk_cr", "prj_clinic", "prj_region"))
	regionToOrg := activated(t, proposedAdminLink(t, "lnk_ro", "prj_region", "prj_org"))

	for _, link := range Inbound([]Link{clinicToRegion, regionToOrg}, "prj_org", linkEpoch) {
		if link.Grantor() == "prj_clinic" {
			t.Fatal("an administrative chain gave the org capability over the clinic")
		}
	}
}

func TestDataLinkRestrictionBelongsToTheGrantor(t *testing.T) {
	link := activated(t, proposedDataLink(t, "lnk_1", "prj_clinic", "prj_org"))

	share, ok := link.Share(linkEpoch)
	if !ok {
		t.Fatal("an active data link must share")
	}

	if share.Policy().Project() != "prj_clinic" {
		t.Errorf("policy resolved against %q, want the grantor prj_clinic", share.Policy().Project())
	}
}

func TestShareCoversOnlyTheTypesItNames(t *testing.T) {
	link, err := NewDataLink("lnk_1", "prj_clinic", "prj_org", "shared", "Observation", "Condition")
	if err != nil {
		t.Fatalf("NewDataLink: %v", err)
	}

	share, ok := activated(t, link).Share(linkEpoch)
	if !ok {
		t.Fatal("an active data link must share")
	}

	tests := []struct {
		resourceType storage.ResourceType
		covered      bool
	}{
		{resourceType: "Observation", covered: true},
		{resourceType: "Condition", covered: true},
		{resourceType: "Patient", covered: false},
		{resourceType: "Encounter", covered: false},
		{resourceType: "", covered: false},
	}

	for _, tc := range tests {
		t.Run(string(tc.resourceType), func(t *testing.T) {
			if share.Covers(tc.resourceType) != tc.covered {
				t.Fatalf("Covers(%q) = %v, want %v", tc.resourceType, !tc.covered, tc.covered)
			}
		})
	}
}

func TestShareCannotBeWidenedThroughTheAccessor(t *testing.T) {
	share, ok := activated(t, proposedDataLink(t, "lnk_1", "prj_clinic", "prj_org")).Share(linkEpoch)
	if !ok {
		t.Fatal("an active data link must share")
	}

	escaped := share.ResourceTypes()
	escaped[0] = "Patient"

	if share.Covers("Patient") {
		t.Error("mutating the returned slice widened the share")
	}
}

func TestLinkConstructionRejectsAnUnusableShape(t *testing.T) {
	tests := []struct {
		name string
		call func() (Link, error)
		want error
	}{
		{
			name: "no id",
			call: func() (Link, error) { return NewDataLink("", "prj_clinic", "prj_org", "p", "Observation") },
			want: ErrMissingID,
		},
		{
			name: "same project both ends",
			call: func() (Link, error) { return NewDataLink("lnk_1", "prj_clinic", "prj_clinic", "p", "Observation") },
			want: ErrSelfLink,
		},
		{
			name: "wildcard grantor",
			call: func() (Link, error) { return NewDataLink("lnk_1", "*", "prj_org", "p", "Observation") },
			want: ErrInvalidProjectID,
		},
		{
			name: "data link with no policy",
			call: func() (Link, error) { return NewDataLink("lnk_1", "prj_clinic", "prj_org", "", "Observation") },
			want: ErrMissingPolicy,
		},
		{
			name: "data link with no type",
			call: func() (Link, error) { return NewDataLink("lnk_1", "prj_clinic", "prj_org", "p") },
			want: ErrMissingResourceType,
		},
		{
			name: "data link with an empty type",
			call: func() (Link, error) { return NewDataLink("lnk_1", "prj_clinic", "prj_org", "p", "") },
			want: ErrMissingResourceType,
		},
		{
			name: "administrative link with no capability",
			call: func() (Link, error) {
				return NewAdminLink("lnk_1", "prj_clinic", "prj_org", []PrincipalRef{clinician()})
			},
			want: ErrMissingCapability,
		},
		{
			name: "administrative link with an unnamed principal",
			call: func() (Link, error) {
				return NewAdminLink("lnk_1", "prj_clinic", "prj_org", []PrincipalRef{{}}, CapabilityQuotaWrite)
			},
			want: ErrInvalidPrincipal,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.call(); !errors.Is(err, tc.want) {
				t.Fatalf("construction = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestActivationFixesTheHistoryFloor(t *testing.T) {
	link := activated(t, proposedDataLink(t, "lnk_1", "prj_clinic", "prj_org"))

	if !link.HistoryFrom().Equal(linkEpoch) {
		t.Errorf("HistoryFrom() = %s, want the activation time %s", link.HistoryFrom(), linkEpoch)
	}

	earlier := linkEpoch.Add(-24 * time.Hour)
	if !link.WithHistoryFrom(earlier).HistoryFrom().Equal(earlier) {
		t.Error("the grantor's explicit opt-in to earlier versions was not recorded")
	}
}
