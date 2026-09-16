package authz

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

const clinic = project.ID("prj_clinic")

func mustLiteralSubject(t *testing.T, resourceType storage.ResourceType, id storage.LogicalID) CompartmentSubject {
	t.Helper()

	subject, err := LiteralSubject(resourceType, id)
	if err != nil {
		t.Fatalf("LiteralSubject: %v", err)
	}

	return subject
}

func mustParameterSubject(t *testing.T, resourceType storage.ResourceType, name ParameterName) CompartmentSubject {
	t.Helper()

	subject, err := ParameterSubject(resourceType, name)
	if err != nil {
		t.Fatalf("ParameterSubject: %v", err)
	}

	return subject
}

func mustRule(t *testing.T, resourceType storage.ResourceType, action storage.Action, subject CompartmentSubject) Rule {
	t.Helper()

	rule, err := NewRule(storage.KindFHIR, resourceType, action, subject)
	if err != nil {
		t.Fatalf("NewRule: %v", err)
	}

	return rule
}

func mustPolicy(t *testing.T, cfg PolicyConfig) AccessPolicy {
	t.Helper()

	policy, err := NewAccessPolicy(cfg)
	if err != nil {
		t.Fatalf("NewAccessPolicy: %v", err)
	}

	return policy
}

// ownChartPolicy restricts a patient to their own chart, with the subject left
// to the binding, which is the parameterized case FR-055 exists for.
func ownChartPolicy(t *testing.T) AccessPolicy {
	t.Helper()

	return mustPolicy(t, PolicyConfig{
		Project:    clinic,
		ID:         "pol_own_chart",
		Parameters: []ParameterName{"patient"},
		Rules: []Rule{
			mustRule(t, "Observation", storage.ActionRead, mustParameterSubject(t, "Patient", "patient")),
			mustRule(t, "Observation", storage.ActionSearch, mustParameterSubject(t, "Patient", "patient")),
			mustRule(t, "Patient", storage.ActionRead, mustParameterSubject(t, "Patient", "patient")),
		},
	})
}

func readRequest(resourceType storage.ResourceType, params Parameters) GrantRequest {
	return GrantRequest{
		Project: clinic, Kind: storage.KindFHIR, Type: resourceType,
		Action: storage.ActionRead, Origin: OriginMembership, Parameters: params,
	}
}

func TestAPolicyWithNoRulesGrantsNothing(t *testing.T) {
	policy := mustPolicy(t, PolicyConfig{Project: clinic, ID: "pol_empty"})

	tests := []struct {
		name   string
		kind   storage.Kind
		target storage.ResourceType
		action storage.Action
	}{
		{name: "fhir read", kind: storage.KindFHIR, target: "Patient", action: storage.ActionRead},
		{name: "fhir search", kind: storage.KindFHIR, target: "Observation", action: storage.ActionSearch},
		{name: "fhir write", kind: storage.KindFHIR, target: "Observation", action: storage.ActionWrite},
		{name: "fhir history", kind: storage.KindFHIR, target: "Patient", action: storage.ActionHistory},
		{name: "platform read", kind: storage.KindPlatform, target: "Project", action: storage.ActionRead},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			grants, err := policy.Compile(GrantRequest{
				Project: clinic, Kind: tc.kind, Type: tc.target,
				Action: tc.action, Origin: OriginMembership,
			})
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}

			if len(grants) != 0 {
				t.Fatalf("a policy stating no rule minted %d Grants", len(grants))
			}

			if !storage.NewScope(grants...).IsEmpty() {
				t.Error("no rules must compile to an empty Scope")
			}
		})
	}
}

func TestCompileAnswersOnlyTheRequestedTriple(t *testing.T) {
	policy := ownChartPolicy(t)
	bound := Parameters{"patient": "pat_1"}

	tests := []struct {
		name   string
		target storage.ResourceType
		action storage.Action
		grants int
	}{
		{name: "the bound type and action", target: "Observation", action: storage.ActionRead, grants: 1},
		{name: "another bound action", target: "Observation", action: storage.ActionSearch, grants: 1},
		{name: "an action the policy never states", target: "Observation", action: storage.ActionWrite},
		{name: "a type the policy never states", target: "Condition", action: storage.ActionRead},
		{name: "history is not read", target: "Patient", action: storage.ActionHistory},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := readRequest(tc.target, bound)
			request.Action = tc.action

			grants, err := policy.Compile(request)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}

			if len(grants) != tc.grants {
				t.Fatalf("Compile minted %d Grants, want %d", len(grants), tc.grants)
			}

			for _, grant := range grants {
				if grant.Type != tc.target || grant.Action != tc.action {
					t.Errorf("Grant answers %s/%s, not the requested %s/%s",
						grant.Type, grant.Action, tc.target, tc.action)
				}
			}
		})
	}
}

func TestAParameterThatFailsToResolveDenies(t *testing.T) {
	policy := ownChartPolicy(t)

	tests := []struct {
		name   string
		params Parameters
		want   error
	}{
		{name: "no parameters at all", params: nil, want: ErrParameterUnresolved},
		{name: "an empty parameter set", params: Parameters{}, want: ErrParameterUnresolved},
		{name: "an empty value", params: Parameters{"patient": ""}, want: ErrParameterUnresolved},
		{name: "a different parameter", params: Parameters{"practitioner": "prc_1"}, want: ErrParameterUnresolved},
		{
			name:   "a value the policy declares nothing for",
			params: Parameters{"patient": "pat_1", "practitioner": "prc_1"},
			want:   ErrUnexpectedParameter,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			grants, err := policy.Compile(readRequest("Observation", tc.params))
			if !errors.Is(err, tc.want) {
				t.Fatalf("Compile = %v, want %v", err, tc.want)
			}

			if len(grants) != 0 {
				t.Fatalf("an unresolved parameter still minted %d Grants", len(grants))
			}
		})
	}
}

func TestAResolvedParameterNamesTheSubjectItWasBoundTo(t *testing.T) {
	policy := ownChartPolicy(t)

	tests := []struct {
		name    string
		params  Parameters
		subject storage.LogicalID
	}{
		{name: "one patient", params: Parameters{"patient": "pat_1"}, subject: "pat_1"},
		{name: "another patient", params: Parameters{"patient": "pat_2"}, subject: "pat_2"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			grants, err := policy.Compile(readRequest("Observation", tc.params))
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}

			want := storage.Grant{
				Project: clinic, Kind: storage.KindFHIR, Type: "Observation",
				Action: storage.ActionRead, Source: storage.SourceMembership,
				Compartment: &storage.Compartment{Type: "Patient", ID: tc.subject},
			}

			if len(grants) != 1 || !reflect.DeepEqual(grants[0], want) {
				t.Fatalf("Compile = %+v, want one Grant %+v", grants, want)
			}
		})
	}
}

func TestOneRulePerSubjectMintsOneGrantPerSubject(t *testing.T) {
	policy := mustPolicy(t, PolicyConfig{
		Project: clinic,
		ID:      "pol_two_patients",
		Rules: []Rule{
			mustRule(t, "Observation", storage.ActionRead, mustLiteralSubject(t, "Patient", "pat_1")),
			mustRule(t, "Observation", storage.ActionRead, mustLiteralSubject(t, "Patient", "pat_2")),
		},
	})

	grants, err := policy.Compile(readRequest("Observation", nil))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	if len(grants) != 2 {
		t.Fatalf("Compile minted %d Grants, want one per subject", len(grants))
	}

	for index, want := range []storage.LogicalID{"pat_1", "pat_2"} {
		if grants[index].Compartment == nil || grants[index].Compartment.ID != want {
			t.Errorf("Grant %d restricts to %+v, want %s", index, grants[index].Compartment, want)
		}
	}
}

func TestCompileRefusesAnotherProjectsRequest(t *testing.T) {
	policy := ownChartPolicy(t)

	request := readRequest("Observation", Parameters{"patient": "pat_1"})
	request.Project = "prj_partner"

	grants, err := policy.Compile(request)
	if !errors.Is(err, ErrPolicyProjectMismatch) {
		t.Fatalf("Compile = %v, want ErrPolicyProjectMismatch", err)
	}

	if len(grants) != 0 {
		t.Fatalf("a foreign Project still drew %d Grants out of the policy", len(grants))
	}
}

func TestCompileSourcesOnlyMembershipAndLinkGrants(t *testing.T) {
	policy := mustPolicy(t, PolicyConfig{
		Project: clinic,
		ID:      "pol_literal",
		Rules:   []Rule{mustRule(t, "Observation", storage.ActionRead, mustLiteralSubject(t, "Patient", "pat_1"))},
	})

	tests := []struct {
		name   string
		origin Origin
		want   storage.GrantSource
	}{
		{name: "a membership binding", origin: OriginMembership, want: storage.SourceMembership},
		{name: "a link", origin: OriginLink, want: storage.SourceLink},
		{name: "super admin", origin: Origin(storage.SourceSuperAdmin)},
		{name: "an unstated origin", origin: ""},
		{name: "an invented origin", origin: "bootstrap"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := readRequest("Observation", nil)
			request.Origin = tc.origin

			grants, err := policy.Compile(request)
			if tc.want == "" {
				if !errors.Is(err, ErrUnknownOrigin) {
					t.Fatalf("Compile = %v, want ErrUnknownOrigin", err)
				}

				if len(grants) != 0 {
					t.Fatalf("an unknown origin still minted %d Grants", len(grants))
				}

				return
			}

			if err != nil {
				t.Fatalf("Compile: %v", err)
			}

			if len(grants) != 1 || grants[0].Source != tc.want {
				t.Fatalf("Compile = %+v, want one Grant sourced %s", grants, tc.want)
			}
		})
	}
}

func TestCompileRefusesATripleItCannotRecognise(t *testing.T) {
	policy := ownChartPolicy(t)

	tests := []struct {
		name   string
		kind   storage.Kind
		target storage.ResourceType
		action storage.Action
		want   error
	}{
		{name: "unknown kind", kind: "clinical", target: "Observation", action: storage.ActionRead, want: ErrUnknownKind},
		{name: "no kind", kind: "", target: "Observation", action: storage.ActionRead, want: ErrUnknownKind},
		{name: "no resource type", kind: storage.KindFHIR, action: storage.ActionRead, want: ErrMissingResourceType},
		{name: "wildcard is not a type", kind: storage.KindFHIR, target: "*", action: "", want: ErrUnknownAction},
		{name: "unknown action", kind: storage.KindFHIR, target: "Observation", action: "vread", want: ErrUnknownAction},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			grants, err := policy.Compile(GrantRequest{
				Project: clinic, Kind: tc.kind, Type: tc.target, Action: tc.action,
				Origin: OriginMembership, Parameters: Parameters{"patient": "pat_1"},
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Compile = %v, want %v", err, tc.want)
			}

			if len(grants) != 0 {
				t.Fatalf("an unrecognised triple still minted %d Grants", len(grants))
			}
		})
	}
}

func TestARuleNeedsARestrictionItStates(t *testing.T) {
	tests := []struct {
		name    string
		subject CompartmentSubject
		want    error
	}{
		{name: "no restriction at all", subject: CompartmentSubject{}, want: ErrMissingSubject},
		{name: "a subject type with no id", subject: CompartmentSubject{resourceType: "Patient"}, want: ErrMissingSubject},
		{name: "an id with no subject type", subject: CompartmentSubject{id: "pat_1"}, want: ErrMissingSubject},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRule(storage.KindFHIR, "Observation", storage.ActionRead, tc.subject)
			if !errors.Is(err, tc.want) {
				t.Fatalf("NewRule = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSubjectConstructorsRefuseAHalfNamedSubject(t *testing.T) {
	if _, err := LiteralSubject("Patient", ""); !errors.Is(err, ErrMissingSubject) {
		t.Errorf("LiteralSubject with no id = %v, want ErrMissingSubject", err)
	}

	if _, err := LiteralSubject("", "pat_1"); !errors.Is(err, ErrMissingSubject) {
		t.Errorf("LiteralSubject with no type = %v, want ErrMissingSubject", err)
	}

	if _, err := ParameterSubject("Patient", ""); !errors.Is(err, ErrMissingSubject) {
		t.Errorf("ParameterSubject with no parameter = %v, want ErrMissingSubject", err)
	}
}

func TestAnUnrestrictedRuleIsRefusedOverClinicalData(t *testing.T) {
	tests := []struct {
		name    string
		kind    storage.Kind
		target  storage.ResourceType
		refused bool
	}{
		{name: "observations", kind: storage.KindFHIR, target: "Observation", refused: true},
		{name: "conditions", kind: storage.KindFHIR, target: "Condition", refused: true},
		{name: "document references", kind: storage.KindFHIR, target: "DocumentReference", refused: true},
		{name: "medication requests", kind: storage.KindFHIR, target: "MedicationRequest", refused: true},
		{name: "patients", kind: storage.KindFHIR, target: "Patient", refused: true},
		{name: "raw attachment bodies", kind: storage.KindFHIR, target: "Binary", refused: true},
		{name: "genomic sequences", kind: storage.KindFHIR, target: "MolecularSequence", refused: true},
		{name: "the audit trail", kind: storage.KindFHIR, target: "AuditEvent", refused: true},
		{name: "provenance", kind: storage.KindFHIR, target: "Provenance", refused: true},
		{name: "tasks", kind: storage.KindFHIR, target: "Task", refused: true},
		{name: "appointments", kind: storage.KindFHIR, target: "Appointment", refused: true},
		{name: "curated lists", kind: storage.KindFHIR, target: "List", refused: true},
		{name: "a type no release has defined", kind: storage.KindFHIR, target: "GenomicStudy", refused: true},
		{name: "the staff directory", kind: storage.KindFHIR, target: "Practitioner"},
		{name: "terminology", kind: storage.KindFHIR, target: "ValueSet"},
		{name: "organizations", kind: storage.KindFHIR, target: "Organization"},
		{name: "conformance", kind: storage.KindFHIR, target: "StructureDefinition"},
		{name: "platform resources", kind: storage.KindPlatform, target: "Project"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewUnrestrictedRule(tc.kind, tc.target, storage.ActionSearch)
			if tc.refused != errors.Is(err, ErrUnrestrictedClinicalType) {
				t.Fatalf("NewUnrestrictedRule(%s) = %v, refused want %v", tc.target, err, tc.refused)
			}
		})
	}
}

// TestAnUnclassifiedTypeIsTreatedAsClinical pins the direction the guard fails
// in: classification is an allowlist, so a type FHIR adds after this list was
// written is refused rather than silently unrestricted (LNK-5).
func TestAnUnclassifiedTypeIsTreatedAsClinical(t *testing.T) {
	for _, target := range []storage.ResourceType{"BodyStructure", "ExplanationOfBenefit", "Whatever"} {
		if !CarriesClinicalData(target) {
			t.Errorf("CarriesClinicalData(%s) = false; an unclassified type must be treated as clinical", target)
		}
	}
}

func TestAnUnrestrictedRuleCompilesToAGrantWithNoCompartment(t *testing.T) {
	rule, err := NewUnrestrictedRule(storage.KindFHIR, "Practitioner", storage.ActionRead)
	if err != nil {
		t.Fatalf("NewUnrestrictedRule: %v", err)
	}

	policy := mustPolicy(t, PolicyConfig{Project: clinic, ID: "pol_directory", Rules: []Rule{rule}})

	grants, err := policy.Compile(readRequest("Practitioner", nil))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	if len(grants) != 1 || grants[0].Compartment != nil {
		t.Fatalf("Compile = %+v, want one Grant carrying no Compartment", grants)
	}
}

func TestAPolicyRefusesARuleNamingAnUndeclaredParameter(t *testing.T) {
	rule := mustRule(t, "Observation", storage.ActionRead, mustParameterSubject(t, "Patient", "patient"))

	tests := []struct {
		name       string
		parameters []ParameterName
		want       error
	}{
		{name: "declared", parameters: []ParameterName{"patient"}},
		{name: "never declared", parameters: nil, want: ErrUndeclaredParameter},
		{name: "a different name declared", parameters: []ParameterName{"practitioner"}, want: ErrUndeclaredParameter},
		{name: "an unnamed parameter", parameters: []ParameterName{""}, want: ErrInvalidParameterName},
		{
			name:       "the same parameter twice",
			parameters: []ParameterName{"patient", "patient"},
			want:       ErrInvalidParameterName,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewAccessPolicy(PolicyConfig{
				Project: clinic, ID: "pol_own_chart", Parameters: tc.parameters, Rules: []Rule{rule},
			})
			if tc.want == nil {
				if err != nil {
					t.Fatalf("NewAccessPolicy: %v", err)
				}

				return
			}

			if !errors.Is(err, tc.want) {
				t.Fatalf("NewAccessPolicy = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAPolicyNeedsAnOwnerAndAnIdentifier(t *testing.T) {
	tests := []struct {
		name    string
		project project.ID
		id      storage.LogicalID
		want    error
	}{
		{name: "no project", project: "", id: "pol_1", want: project.ErrInvalidProjectID},
		{name: "the wildcard project", project: "*", id: "pol_1", want: project.ErrInvalidProjectID},
		{name: "no identifier", project: clinic, id: "", want: ErrMissingPolicyID},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewAccessPolicy(PolicyConfig{Project: tc.project, ID: tc.id})
			if !errors.Is(err, tc.want) {
				t.Fatalf("NewAccessPolicy = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParseParameters(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		want  Parameters
		fails bool
	}{
		{name: "absent", raw: "", want: Parameters{}},
		{name: "null", raw: "null", want: Parameters{}},
		{name: "empty object", raw: "{}", want: Parameters{}},
		{name: "one value", raw: `{"patient":"pat_1"}`, want: Parameters{"patient": "pat_1"}},
		{name: "a numeric value", raw: `{"patient":1}`, fails: true},
		{name: "a nested object", raw: `{"patient":{"id":"pat_1"}}`, fails: true},
		{name: "a list", raw: `["pat_1"]`, fails: true},
		{name: "malformed", raw: `{"patient":`, fails: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params, err := ParseParameters(json.RawMessage(tc.raw))
			if tc.fails {
				if err == nil {
					t.Fatalf("ParseParameters(%q) = %v, want an error", tc.raw, params)
				}

				if params != nil {
					t.Error("a malformed document must resolve no parameters at all")
				}

				return
			}

			if err != nil {
				t.Fatalf("ParseParameters(%q): %v", tc.raw, err)
			}

			if !reflect.DeepEqual(params, tc.want) {
				t.Fatalf("ParseParameters(%q) = %v, want %v", tc.raw, params, tc.want)
			}
		})
	}
}

func TestMatchesOnlyTheReferenceItsOwnerBound(t *testing.T) {
	policy := ownChartPolicy(t)

	tests := []struct {
		name   string
		owner  project.ID
		policy storage.LogicalID
		want   bool
	}{
		{name: "its own binding", owner: clinic, policy: "pol_own_chart", want: true},
		{name: "a same-named policy elsewhere", owner: "prj_partner", policy: "pol_own_chart"},
		{name: "another policy at home", owner: clinic, policy: "pol_wide"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if policy.Matches(bindingRef(t, tc.owner, tc.policy)) != tc.want {
				t.Fatalf("Matches(%s/%s) = %v, want %v", tc.owner, tc.policy, !tc.want, tc.want)
			}
		})
	}

	if policy.Matches(project.PolicyRef{}) {
		t.Error("a policy must not match a reference naming nothing")
	}
}

// bindingRef borrows a PolicyRef from a membership, which is the only way one is
// built: its Project is set from the membership's own, never by a caller.
func bindingRef(t *testing.T, owner project.ID, id storage.LogicalID) project.PolicyRef {
	t.Helper()

	member, err := project.NewMembership(project.MembershipConfig{
		ID: "pm_1", Project: owner, ProjectKind: project.KindStandard,
		Principal: project.PrincipalRef{Kind: project.PrincipalUser, ID: "usr_1"},
		State:     project.MembershipActive, Source: project.SourceInvite,
		Policies: []project.PolicyAttachment{{Policy: id}},
	})
	if err != nil {
		t.Fatalf("NewMembership: %v", err)
	}

	return member.Policies()[0].Policy()
}

func TestRulesCannotBeAppendedThroughTheAccessor(t *testing.T) {
	policy := ownChartPolicy(t)

	escaped := policy.Rules()
	escaped[0] = mustRule(t, "Condition", storage.ActionRead, mustLiteralSubject(t, "Patient", "pat_9"))

	grants, err := policy.Compile(readRequest("Condition", Parameters{"patient": "pat_1"}))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	if len(grants) != 0 {
		t.Error("rewriting the returned rules widened the policy")
	}
}

func TestDeclaredParametersCannotBeAppendedThroughTheAccessor(t *testing.T) {
	policy := ownChartPolicy(t)

	escaped := policy.Parameters()
	escaped[0] = "practitioner"

	if _, err := policy.Compile(readRequest("Observation", Parameters{"practitioner": "prc_1"})); err == nil {
		t.Error("rewriting the returned parameters changed what a binding must supply")
	}
}
