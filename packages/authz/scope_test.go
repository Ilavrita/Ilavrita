package authz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

const (
	lab     = project.ID("prj_lab")
	archive = project.ID("prj_archive")
)

var (
	member      = project.PrincipalRef{Kind: project.PrincipalUser, ID: "usr_clinician"}
	decidedAt   = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	errResolver = errors.New("resolver unavailable")
)

type stubMemberships struct {
	held map[project.ID]project.Membership
	err  error
}

func (s *stubMemberships) Membership(
	_ context.Context, proj project.ID, principal project.PrincipalRef,
) (project.Membership, bool, error) {
	if s.err != nil {
		return project.Membership{}, false, s.err
	}

	membership, ok := s.held[proj]
	if !ok || membership.Principal() != principal {
		return project.Membership{}, false, nil
	}

	return membership, true, nil
}

type stubProjects struct {
	states map[project.ID]project.State
	err    error
}

// State answers with the empty state for a Project it does not know, which
// admits nothing.
func (s *stubProjects) State(_ context.Context, proj project.ID) (project.State, error) {
	if s.err != nil {
		return "", s.err
	}

	return s.states[proj], nil
}

type stubPolicies struct {
	policies []AccessPolicy
	err      error
}

func (s *stubPolicies) Policy(_ context.Context, ref project.PolicyRef) (AccessPolicy, bool, error) {
	if s.err != nil {
		return AccessPolicy{}, false, s.err
	}

	for _, policy := range s.policies {
		if policy.Matches(ref) {
			return policy, true, nil
		}
	}

	return AccessPolicy{}, false, nil
}

// stubLinks deliberately filters on the grantee end only, never on Effective, so
// every lifecycle gate under test is one BuildScope applies itself.
type stubLinks struct {
	links []project.Link
	asked []project.ID
	err   error
}

func (s *stubLinks) Inbound(_ context.Context, grantee project.ID) ([]project.Link, error) {
	s.asked = append(s.asked, grantee)

	if s.err != nil {
		return nil, s.err
	}

	reaching := make([]project.Link, 0, len(s.links))
	for _, link := range s.links {
		if link.Grantee() == grantee {
			reaching = append(reaching, link)
		}
	}

	return reaching, nil
}

type fixture struct {
	memberships *stubMemberships
	projects    *stubProjects
	policies    *stubPolicies
	links       *stubLinks
}

// newFixture wires a clinician holding one bound policy in an active home
// Project, with an active grantor Project whose own policy shares Observations.
func newFixture(t *testing.T) *fixture {
	t.Helper()

	return &fixture{
		memberships: &stubMemberships{held: map[project.ID]project.Membership{
			clinic: boundMembership(t, project.PolicyAttachment{Policy: "pol_chart"}),
		}},
		projects: &stubProjects{states: map[project.ID]project.State{
			clinic: project.StateActive, lab: project.StateActive, archive: project.StateActive,
		}},
		policies: &stubPolicies{policies: []AccessPolicy{
			chartPolicy(t, clinic), chartPolicy(t, lab), chartPolicy(t, archive), ownChartPolicy(t),
		}},
		links: &stubLinks{},
	}
}

func (f *fixture) resolvers() Resolvers {
	return Resolvers{Memberships: f.memberships, Projects: f.projects, Policies: f.policies, Links: f.links}
}

// unbound replaces the home membership with one holding no policy at all, so any
// Grant a test then sees can only have come from a link.
func (f *fixture) unbound(t *testing.T) *fixture {
	t.Helper()

	f.memberships.held[clinic] = boundMembership(t)

	return f
}

func (f *fixture) probe() Request {
	return Request{
		Principal: member, Project: clinic,
		Kind: storage.KindFHIR, Type: "Observation", Action: storage.ActionRead,
		Now: decidedAt, Resolvers: f.resolvers(),
	}
}

// chartPolicy restricts every action it names to one patient, so a Grant it
// mints is never unrestricted and the write rule proves a state gate, not an
// absent rule, is what refuses a write.
func chartPolicy(t *testing.T, owner project.ID) AccessPolicy {
	t.Helper()

	subject := mustLiteralSubject(t, "Patient", "pat_1")

	return mustPolicy(t, PolicyConfig{
		Project: owner,
		ID:      "pol_chart",
		Rules: []Rule{
			mustRule(t, "Observation", storage.ActionRead, subject),
			mustRule(t, "Observation", storage.ActionSearch, subject),
			mustRule(t, "Observation", storage.ActionWrite, subject),
			mustRule(t, "Observation", storage.ActionHistory, subject),
		},
	})
}

func boundMembership(t *testing.T, attachments ...project.PolicyAttachment) project.Membership {
	t.Helper()

	return mustMembership(t, project.MembershipConfig{
		ID: "mbr_1", Project: clinic, ProjectKind: project.KindStandard,
		Principal: member, State: project.MembershipActive,
		Policies: attachments, Source: project.SourceInvite,
	})
}

func mustMembership(t *testing.T, cfg project.MembershipConfig) project.Membership {
	t.Helper()

	membership, err := project.NewMembership(cfg)
	if err != nil {
		t.Fatalf("NewMembership: %v", err)
	}

	return membership
}

func mustDataLink(
	t *testing.T, id project.LinkID, grantor, grantee project.ID, types ...storage.ResourceType,
) project.Link {
	t.Helper()

	link, err := project.NewDataLink(id, grantor, grantee, "pol_chart", types...)
	if err != nil {
		t.Fatalf("NewDataLink: %v", err)
	}

	return link
}

// activate walks a link through both approvals into the active state, which is
// the only route the domain offers.
func activate(t *testing.T, link project.Link, at time.Time) project.Link {
	t.Helper()

	link, err := link.ApproveGrantor("mbr_grantor", at)
	if err != nil {
		t.Fatalf("ApproveGrantor: %v", err)
	}

	link, err = link.ApproveGrantee("mbr_grantee", at)
	if err != nil {
		t.Fatalf("ApproveGrantee: %v", err)
	}

	link, err = link.TransitionTo(project.LinkActive, at)
	if err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}

	return link
}

func effectiveLabLink(t *testing.T) project.Link {
	t.Helper()

	return activate(t, mustDataLink(t, "lnk_1", lab, clinic, "Observation"), decidedAt.Add(-time.Hour))
}

func summarize(scope storage.Scope) []string {
	summaries := make([]string, 0, len(scope.Grants()))
	for _, grant := range scope.Grants() {
		compartment := "unrestricted"
		if grant.Compartment != nil {
			compartment = fmt.Sprintf("%s/%s", grant.Compartment.Type, grant.Compartment.ID)
		}

		summaries = append(summaries, fmt.Sprintf("%s|%s|%s|%s|%s|%s",
			grant.Project, grant.Kind, grant.Type, grant.Action, grant.Source, compartment))
	}

	return summaries
}

// buildScope runs the builder and asserts the invariant that holds on every
// return: a Super Admin Source is never minted here, and an error always
// accompanies an empty Scope.
func buildScope(t *testing.T, req Request) (storage.Scope, error) {
	t.Helper()

	scope, err := BuildScope(context.Background(), req)

	if err != nil && !scope.IsEmpty() {
		t.Fatalf("BuildScope returned %d grants alongside error %v", len(scope.Grants()), err)
	}

	for _, grant := range scope.Grants() {
		if grant.Source == storage.SourceSuperAdmin {
			t.Fatalf("BuildScope minted a super-admin Grant: %v", grant)
		}
	}

	return scope, err
}

func assertEmpty(t *testing.T, scope storage.Scope, because string) {
	t.Helper()

	if !scope.IsEmpty() {
		t.Fatalf("%s: expected an empty Scope, got %v", because, summarize(scope))
	}
}

func assertGrants(t *testing.T, scope storage.Scope, want []string) {
	t.Helper()

	if got := summarize(scope); !slices.Equal(got, want) {
		t.Fatalf("grants: got %v, want %v", got, want)
	}
}

func TestAMalformedRequestIsRefusedRatherThanAnswered(t *testing.T) {
	base := newFixture(t).probe()

	cases := []struct {
		name string
		want error
		call func(Request) Request
	}{
		{"no principal", ErrInvalidPrincipal, func(r Request) Request {
			r.Principal = project.PrincipalRef{}

			return r
		}},
		{"unknown principal kind", ErrInvalidPrincipal, func(r Request) Request {
			r.Principal = project.PrincipalRef{Kind: "operator", ID: "usr_1"}

			return r
		}},
		{"no project", project.ErrInvalidProjectID, func(r Request) Request {
			r.Project = ""

			return r
		}},
		{"wildcard project", project.ErrInvalidProjectID, func(r Request) Request {
			r.Project = "*"

			return r
		}},
		{"wildcard grantor", project.ErrInvalidProjectID, func(r Request) Request {
			r.LinkedProjects = []project.ID{"*"}

			return r
		}},
		{"unknown kind", ErrUnknownKind, func(r Request) Request {
			r.Kind = "clinical"

			return r
		}},
		{"no resource type", ErrMissingResourceType, func(r Request) Request {
			r.Type = ""

			return r
		}},
		{"unknown action", ErrUnknownAction, func(r Request) Request {
			r.Action = "export"

			return r
		}},
		{"no instant", ErrMissingInstant, func(r Request) Request {
			r.Now = time.Time{}

			return r
		}},
		{"no resolvers", ErrMissingResolver, func(r Request) Request {
			r.Resolvers = Resolvers{}

			return r
		}},
		{"one nil resolver", ErrMissingResolver, func(r Request) Request {
			r.Resolvers.Links = nil

			return r
		}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			scope, err := buildScope(t, testCase.call(base))
			if !errors.Is(err, testCase.want) {
				t.Fatalf("error: got %v, want %v", err, testCase.want)
			}

			assertEmpty(t, scope, testCase.name)
		})
	}
}

func TestNoMembershipGrantsNothingWithoutAnError(t *testing.T) {
	fix := newFixture(t)
	delete(fix.memberships.held, clinic)

	scope, err := buildScope(t, fix.probe())
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	assertEmpty(t, scope, "a principal with no standing")
}

func TestAMembershipGrantsWhatItsPolicyStates(t *testing.T) {
	scope, err := buildScope(t, newFixture(t).probe())
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	assertGrants(t, scope, []string{"prj_clinic|fhir|Observation|read|membership|Patient/pat_1"})
}

func TestResolutionIsExactMatchNotSomePolicySomewhere(t *testing.T) {
	req := newFixture(t).probe()
	req.Type = "Condition"

	scope, err := buildScope(t, req)
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	assertEmpty(t, scope, "a type the bound policy never names")
}

func TestABindingResolvesItsOwnParameters(t *testing.T) {
	fix := newFixture(t)
	fix.memberships.held[clinic] = boundMembership(t, project.PolicyAttachment{
		Policy: "pol_own_chart", Parameters: json.RawMessage(`{"patient":"pat_9"}`),
	})

	scope, err := buildScope(t, fix.probe())
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	assertGrants(t, scope, []string{"prj_clinic|fhir|Observation|read|membership|Patient/pat_9"})
}

func TestABindingSupplyingNoParameterDenies(t *testing.T) {
	fix := newFixture(t)
	fix.memberships.held[clinic] = boundMembership(t, project.PolicyAttachment{Policy: "pol_own_chart"})

	scope, err := buildScope(t, fix.probe())
	if !errors.Is(err, ErrParameterUnresolved) {
		t.Fatalf("error: got %v, want %v", err, ErrParameterUnresolved)
	}

	assertEmpty(t, scope, "an unresolved parameter")
}

func TestAPolicyThatNoLongerExistsGrantsNothing(t *testing.T) {
	fix := newFixture(t)
	fix.policies.policies = nil

	scope, err := buildScope(t, fix.probe())
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	assertEmpty(t, scope, "a binding naming a deleted policy")
}

func TestAResolverAnsweringWithAnotherPolicyIsRefused(t *testing.T) {
	fix := newFixture(t)
	fix.policies = &stubPolicies{policies: []AccessPolicy{chartPolicy(t, clinic)}}

	req := fix.probe()
	req.Resolvers.Policies = mismatchedPolicies{policy: chartPolicy(t, lab)}

	scope, err := buildScope(t, req)
	if !errors.Is(err, ErrPolicyMismatch) {
		t.Fatalf("error: got %v, want %v", err, ErrPolicyMismatch)
	}

	assertEmpty(t, scope, "a resolver answering with another Project's policy")
}

// mismatchedPolicies answers every reference with the same policy, which is the
// resolver bug a reference check exists to catch.
type mismatchedPolicies struct {
	policy AccessPolicy
}

func (m mismatchedPolicies) Policy(_ context.Context, _ project.PolicyRef) (AccessPolicy, bool, error) {
	return m.policy, true, nil
}

func TestProjectLifecycleGatesTheDataPlane(t *testing.T) {
	cases := []struct {
		name  string
		state project.State
		grant bool
	}{
		{"active admits a read", project.StateActive, true},
		{"archived admits a read", project.StateArchived, true},
		{"suspended admits nothing", project.StateSuspended, false},
		{"deleting admits nothing", project.StateDeleting, false},
		{"an unrecognised state admits nothing", project.State("retired"), false},
		{"an absent project admits nothing", project.State(""), false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fix := newFixture(t)
			fix.projects.states[clinic] = testCase.state

			scope, err := buildScope(t, fix.probe())
			if err != nil {
				t.Fatalf("BuildScope: %v", err)
			}

			if scope.IsEmpty() == testCase.grant {
				t.Fatalf("state %q: empty=%v, want grant=%v", testCase.state, scope.IsEmpty(), testCase.grant)
			}
		})
	}
}

func TestAnArchivedProjectKeepsReadsAndRefusesWrites(t *testing.T) {
	fix := newFixture(t)
	fix.projects.states[clinic] = project.StateArchived

	write := fix.probe()
	write.Action = storage.ActionWrite

	scope, err := buildScope(t, write)
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	assertEmpty(t, scope, "a write into an archived Project")

	fix.projects.states[clinic] = project.StateActive

	scope, err = buildScope(t, write)
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	assertGrants(t, scope, []string{"prj_clinic|fhir|Observation|write|membership|Patient/pat_1"})
}

func TestProjectAdminAloneGrantsNothingOnTheDataPlane(t *testing.T) {
	fix := newFixture(t)
	fix.memberships.held[clinic] = mustMembership(t, project.MembershipConfig{
		ID: "mbr_admin", Project: clinic, ProjectKind: project.KindStandard,
		Principal: member, State: project.MembershipActive,
		Admin: true, Source: project.SourceInvite,
	})

	for _, action := range []storage.Action{
		storage.ActionRead, storage.ActionWrite, storage.ActionDelete,
		storage.ActionSearch, storage.ActionHistory,
	} {
		req := fix.probe()
		req.Action = action

		scope, err := buildScope(t, req)
		if err != nil {
			t.Fatalf("BuildScope: %v", err)
		}

		assertEmpty(t, scope, fmt.Sprintf("an admin with no policy asking to %s", action))
	}
}

func TestSuperAdminStandingGrantsNothingHere(t *testing.T) {
	fix := newFixture(t)
	fix.memberships.held[clinic] = mustMembership(t, project.MembershipConfig{
		ID: "mbr_super", Project: clinic, ProjectKind: project.KindSuper,
		Principal: member, State: project.MembershipActive,
		Admin: true, SuperAdmin: true, Source: project.SourceBootstrap,
	})

	scope, err := buildScope(t, fix.probe())
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	assertEmpty(t, scope, "super admin standing with no policy")
}

func TestOnlyAnActiveMembershipResolvesToAPolicy(t *testing.T) {
	for _, state := range []project.MembershipState{
		project.MembershipInvited, project.MembershipSuspended, project.MembershipRevoked,
	} {
		fix := newFixture(t)
		fix.memberships.held[clinic] = mustMembership(t, project.MembershipConfig{
			ID: "mbr_1", Project: clinic, ProjectKind: project.KindStandard,
			Principal: member, State: state,
			Policies: []project.PolicyAttachment{{Policy: "pol_chart"}}, Source: project.SourceInvite,
		})

		scope, err := buildScope(t, fix.probe())
		if err != nil {
			t.Fatalf("BuildScope: %v", err)
		}

		assertEmpty(t, scope, fmt.Sprintf("a %s membership", state))
	}
}

func TestALinkMintedMembershipGrantsNothing(t *testing.T) {
	linked, err := project.NewLinkedMembership("mbr_linked", clinic, member, "lnk_admin")
	if err != nil {
		t.Fatalf("NewLinkedMembership: %v", err)
	}

	fix := newFixture(t)
	fix.memberships.held[clinic] = linked

	for _, action := range []storage.Action{storage.ActionRead, storage.ActionSearch, storage.ActionHistory} {
		req := fix.probe()
		req.Action = action

		scope, err := buildScope(t, req)
		if err != nil {
			t.Fatalf("BuildScope: %v", err)
		}

		assertEmpty(t, scope, fmt.Sprintf("a link-minted membership asking to %s", action))
	}
}

func TestAnEffectiveLinkTheCallerNamedGrantsFromTheGrantor(t *testing.T) {
	fix := newFixture(t).unbound(t)
	fix.links.links = []project.Link{effectiveLabLink(t)}

	req := fix.probe()
	req.LinkedProjects = []project.ID{lab}

	scope, err := buildScope(t, req)
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	assertGrants(t, scope, []string{"prj_lab|fhir|Observation|read|link|Patient/pat_1"})
}

func TestMembershipAndLinkGrantsCarrySeparateSources(t *testing.T) {
	fix := newFixture(t)
	fix.links.links = []project.Link{effectiveLabLink(t)}

	req := fix.probe()
	req.LinkedProjects = []project.ID{lab}

	scope, err := buildScope(t, req)
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	assertGrants(t, scope, []string{
		"prj_clinic|fhir|Observation|read|membership|Patient/pat_1",
		"prj_lab|fhir|Observation|read|link|Patient/pat_1",
	})
}

func TestALinkContributesNothingUntilEveryGateHolds(t *testing.T) {
	expired := effectiveLabLink(t).WithExpiry(decidedAt.Add(-time.Minute))
	future := activate(t, mustDataLink(t, "lnk_1", lab, clinic, "Observation"), decidedAt.Add(time.Hour))

	halfApproved, err := mustDataLink(t, "lnk_1", lab, clinic, "Observation").
		ApproveGrantor("mbr_grantor", decidedAt.Add(-time.Hour))
	if err != nil {
		t.Fatalf("ApproveGrantor: %v", err)
	}

	suspended, err := effectiveLabLink(t).TransitionTo(project.LinkSuspended, decidedAt)
	if err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}

	revoked, err := effectiveLabLink(t).TransitionTo(project.LinkRevoked, decidedAt)
	if err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}

	admin, err := project.NewAdminLink(
		"lnk_admin", lab, clinic, []project.PrincipalRef{member}, project.CapabilityMembershipRead,
	)
	if err != nil {
		t.Fatalf("NewAdminLink: %v", err)
	}

	cases := []struct {
		name string
		link project.Link
	}{
		{"proposed", mustDataLink(t, "lnk_1", lab, clinic, "Observation")},
		{"half approved", halfApproved},
		{"suspended", suspended},
		{"revoked", revoked},
		{"expired", expired},
		{"not yet activated", future},
		{"administrative", activate(t, admin, decidedAt.Add(-time.Hour))},
		{"covering another type", activate(t, mustDataLink(t, "lnk_1", lab, clinic, "Condition"), decidedAt.Add(-time.Hour))},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fix := newFixture(t).unbound(t)
			fix.links.links = []project.Link{testCase.link}

			req := fix.probe()
			req.LinkedProjects = []project.ID{lab}

			scope, err := buildScope(t, req)
			if err != nil {
				t.Fatalf("BuildScope: %v", err)
			}

			assertEmpty(t, scope, fmt.Sprintf("a %s link", testCase.name))
		})
	}
}

func TestALinkCannotBeActivatedWithOneApproval(t *testing.T) {
	link, err := mustDataLink(t, "lnk_1", lab, clinic, "Observation").
		ApproveGrantor("mbr_grantor", decidedAt.Add(-time.Hour))
	if err != nil {
		t.Fatalf("ApproveGrantor: %v", err)
	}

	if _, err := link.TransitionTo(project.LinkActive, decidedAt); !errors.Is(err, project.ErrApprovalIncomplete) {
		t.Fatalf("TransitionTo: got %v, want %v", err, project.ErrApprovalIncomplete)
	}
}

// leakyLinks answers with every link it holds, whatever grantee was asked for,
// which is the resolver bug the re-check of both ends exists to catch.
type leakyLinks struct {
	links []project.Link
}

func (l leakyLinks) Inbound(_ context.Context, _ project.ID) ([]project.Link, error) {
	return l.links, nil
}

func TestALinkIntoAnotherGranteeContributesNothing(t *testing.T) {
	req := newFixture(t).unbound(t).probe()
	req.LinkedProjects = []project.ID{lab}
	req.Resolvers.Links = leakyLinks{links: []project.Link{
		activate(t, mustDataLink(t, "lnk_other", lab, archive, "Observation"), decidedAt.Add(-time.Hour)),
	}}

	scope, err := buildScope(t, req)
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	assertEmpty(t, scope, "a link whose grantee is another Project")
}

func TestALinkNobodyNamedContributesNothing(t *testing.T) {
	fix := newFixture(t).unbound(t)
	fix.links.links = []project.Link{effectiveLabLink(t)}

	scope, err := buildScope(t, fix.probe())
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	assertEmpty(t, scope, "an effective link the caller never opted into")

	if len(fix.links.asked) != 0 {
		t.Fatalf("links errResolver for %v without any grantor named", fix.links.asked)
	}
}

func TestALinkNeverConfersAWriteOrADelete(t *testing.T) {
	for _, action := range []storage.Action{storage.ActionWrite, storage.ActionDelete} {
		fix := newFixture(t).unbound(t)
		fix.links.links = []project.Link{effectiveLabLink(t)}

		req := fix.probe()
		req.Action = action
		req.LinkedProjects = []project.ID{lab}

		scope, err := buildScope(t, req)
		if err != nil {
			t.Fatalf("BuildScope: %v", err)
		}

		assertEmpty(t, scope, fmt.Sprintf("a link asked to %s", action))
	}
}

func TestASuspendedGrantorAuthorizesNoReachIntoIt(t *testing.T) {
	for _, state := range []project.State{
		project.StateSuspended, project.StateDeleting, project.State("retired"),
	} {
		fix := newFixture(t).unbound(t)
		fix.links.links = []project.Link{effectiveLabLink(t)}
		fix.projects.states[lab] = state

		req := fix.probe()
		req.LinkedProjects = []project.ID{lab}

		scope, err := buildScope(t, req)
		if err != nil {
			t.Fatalf("BuildScope: %v", err)
		}

		assertEmpty(t, scope, fmt.Sprintf("a %s grantor", state))
	}
}

func TestLinksDoNotCompose(t *testing.T) {
	fix := newFixture(t).unbound(t)
	fix.links.links = []project.Link{
		activate(t, mustDataLink(t, "lnk_lab", lab, clinic, "Observation"), decidedAt.Add(-time.Hour)),
		activate(t, mustDataLink(t, "lnk_archive", archive, lab, "Observation"), decidedAt.Add(-time.Hour)),
	}

	req := fix.probe()
	req.LinkedProjects = []project.ID{archive}

	scope, err := buildScope(t, req)
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	assertEmpty(t, scope, "a grantor reachable only through a second link")

	if !slices.Equal(fix.links.asked, []project.ID{clinic}) {
		t.Fatalf("links errResolver for %v, want one hop from %s only", fix.links.asked, clinic)
	}
}

func TestANamedGrantorIsResolvedOnce(t *testing.T) {
	fix := newFixture(t).unbound(t)
	fix.links.links = []project.Link{effectiveLabLink(t)}

	req := fix.probe()
	req.LinkedProjects = []project.ID{lab, lab, clinic}

	scope, err := buildScope(t, req)
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	assertGrants(t, scope, []string{"prj_lab|fhir|Observation|read|link|Patient/pat_1"})
}

func TestEveryResolverFailureDeniesWithAnError(t *testing.T) {
	cases := []struct {
		name string
		fail func(*fixture)
	}{
		{"memberships", func(f *fixture) { f.memberships.err = errResolver }},
		{"projects", func(f *fixture) { f.projects.err = errResolver }},
		{"policies", func(f *fixture) { f.policies.err = errResolver }},
		{"links", func(f *fixture) { f.links.err = errResolver }},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fix := newFixture(t)
			fix.links.links = []project.Link{effectiveLabLink(t)}
			testCase.fail(fix)

			req := fix.probe()
			req.LinkedProjects = []project.ID{lab}

			scope, err := buildScope(t, req)
			if !errors.Is(err, errResolver) {
				t.Fatalf("error: got %v, want %v", err, errResolver)
			}

			assertEmpty(t, scope, testCase.name+" failing")
		})
	}
}

// TestBuildScopeNeverReadsAdministrativeStanding is the structural half of
// FR-052: the pipeline cannot OR an administrative check into a data decision if
// it never names one.
func TestBuildScopeNeverReadsAdministrativeStanding(t *testing.T) {
	source, err := os.ReadFile("scope.go")
	if err != nil {
		t.Fatalf("read scope.go: %v", err)
	}

	for _, forbidden := range []string{"IsAdmin", "IsSuperAdmin", "SourceSuperAdmin", "AdminCapability"} {
		if strings.Contains(string(source), forbidden) {
			t.Errorf("scope.go names %s; a data decision must not read administrative standing", forbidden)
		}
	}
}

// linkedMembership mints the standing an administrative link confers: active,
// holding no admin flag and no policy binding at all.
func linkedMembership(t *testing.T) project.Membership {
	t.Helper()

	linked, err := project.NewLinkedMembership("mbr_linked", clinic, member, "lnk_admin")
	if err != nil {
		t.Fatalf("NewLinkedMembership: %v", err)
	}

	return linked
}

func TestADeadMembershipReachesNoGrantor(t *testing.T) {
	for _, state := range []project.MembershipState{
		project.MembershipInvited, project.MembershipSuspended, project.MembershipRevoked,
	} {
		fix := newFixture(t)
		fix.memberships.held[clinic] = mustMembership(t, project.MembershipConfig{
			ID: "mbr_1", Project: clinic, ProjectKind: project.KindStandard,
			Principal: member, State: state, Source: project.SourceInvite,
		})
		fix.links.links = []project.Link{effectiveLabLink(t)}

		for _, action := range linkActions {
			req := fix.probe()
			req.Action = action
			req.LinkedProjects = []project.ID{lab}

			scope, err := buildScope(t, req)
			if err != nil {
				t.Fatalf("BuildScope: %v", err)
			}

			assertEmpty(t, scope, fmt.Sprintf("a %s membership asking to %s through a link", state, action))
		}
	}
}

// TestALinkMintedMembershipReachesNoGrantor is FR-052 on the reaching path: an
// administrative link mints standing that holds no capability, so it must not
// become a data grant in a third Project that merely shares with this one.
func TestALinkMintedMembershipReachesNoGrantor(t *testing.T) {
	fix := newFixture(t)
	fix.memberships.held[clinic] = linkedMembership(t)
	fix.links.links = []project.Link{effectiveLabLink(t)}

	for _, action := range linkActions {
		req := fix.probe()
		req.Action = action
		req.LinkedProjects = []project.ID{lab}

		scope, err := buildScope(t, req)
		if err != nil {
			t.Fatalf("BuildScope: %v", err)
		}

		assertEmpty(t, scope, fmt.Sprintf("a link-minted membership asking to %s through a link", action))
	}
}

func TestNoMembershipAtAllReachesNoGrantor(t *testing.T) {
	fix := newFixture(t)
	delete(fix.memberships.held, clinic)
	fix.links.links = []project.Link{effectiveLabLink(t)}

	req := fix.probe()
	req.LinkedProjects = []project.ID{lab}

	scope, err := buildScope(t, req)
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	assertEmpty(t, scope, "a principal with no standing in the grantee")
}

// strayMemberships answers every lookup with the same membership, which is the
// resolver bug a re-check of both ends exists to catch.
type strayMemberships struct {
	membership project.Membership
}

func (s strayMemberships) Membership(
	_ context.Context, _ project.ID, _ project.PrincipalRef,
) (project.Membership, bool, error) {
	return s.membership, true, nil
}

func TestAMembershipTheRequestDidNotNameGrantsNothing(t *testing.T) {
	stranger := project.PrincipalRef{Kind: project.PrincipalUser, ID: "usr_stranger"}

	cases := []struct {
		name       string
		membership project.Membership
	}{
		{"another principal", mustMembership(t, project.MembershipConfig{
			ID: "mbr_other", Project: clinic, ProjectKind: project.KindStandard,
			Principal: stranger, State: project.MembershipActive,
			Policies: []project.PolicyAttachment{{Policy: "pol_chart"}}, Source: project.SourceInvite,
		})},
		{"another project", mustMembership(t, project.MembershipConfig{
			ID: "mbr_lab", Project: lab, ProjectKind: project.KindStandard,
			Principal: member, State: project.MembershipActive,
			Policies: []project.PolicyAttachment{{Policy: "pol_chart"}}, Source: project.SourceInvite,
		})},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fix := newFixture(t).unbound(t)
			fix.links.links = []project.Link{effectiveLabLink(t)}

			req := fix.probe()
			req.LinkedProjects = []project.ID{lab}
			req.Resolvers.Memberships = strayMemberships{membership: testCase.membership}

			scope, err := buildScope(t, req)
			if err != nil {
				t.Fatalf("BuildScope: %v", err)
			}

			assertEmpty(t, scope, "a resolver answering with "+testCase.name+"'s membership")
		})
	}
}

// TestAnActiveMemberWithNoBindingStillReachesTheGrantor pins the other side of
// the standing gate: holding no policy of her own is not the same as holding no
// standing, so the gate must not over-deny (spec 4.6).
func TestAnActiveMemberWithNoBindingStillReachesTheGrantor(t *testing.T) {
	fix := newFixture(t).unbound(t)
	fix.links.links = []project.Link{effectiveLabLink(t)}

	req := fix.probe()
	req.LinkedProjects = []project.ID{lab}

	scope, err := buildScope(t, req)
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	assertGrants(t, scope, []string{"prj_lab|fhir|Observation|read|link|Patient/pat_1"})
}

// deniedMemberships answers with a membership it also reports as not found,
// which is the resolver bug the found gate exists to catch.
type deniedMemberships struct {
	membership project.Membership
}

func (d deniedMemberships) Membership(
	_ context.Context, _ project.ID, _ project.PrincipalRef,
) (project.Membership, bool, error) {
	return d.membership, false, nil
}

func TestAMembershipReportedNotFoundGrantsNothing(t *testing.T) {
	fix := newFixture(t)
	fix.links.links = []project.Link{effectiveLabLink(t)}

	req := fix.probe()
	req.LinkedProjects = []project.ID{lab}
	req.Resolvers.Memberships = deniedMemberships{
		membership: boundMembership(t, project.PolicyAttachment{Policy: "pol_chart"}),
	}

	scope, err := buildScope(t, req)
	if err != nil {
		t.Fatalf("BuildScope: %v", err)
	}

	assertEmpty(t, scope, "a membership the resolver reported as not found")
}
