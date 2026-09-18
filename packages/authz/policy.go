package authz

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

var (
	// ErrMissingPolicyID reports a policy built without its own identifier.
	ErrMissingPolicyID = errors.New("authz: access policy needs an identifier")

	// ErrPolicyProjectMismatch reports a policy compiled for a Project that does
	// not own it. A restriction resolves in its owner, never in the Project doing
	// the reaching (LNK-6).
	ErrPolicyProjectMismatch = errors.New("authz: access policy belongs to another project")

	// ErrUnknownKind reports a resource kind outside the enum storage declares.
	ErrUnknownKind = errors.New("authz: unknown resource kind")

	// ErrUnknownAction reports an action outside the enum storage declares.
	ErrUnknownAction = errors.New("authz: unknown action")

	// ErrUnknownOrigin reports a compilation asked for on behalf of neither a
	// membership nor a link. Super admin is not an origin: its Grants are minted
	// per Project without a policy (CP-6).
	ErrUnknownOrigin = errors.New("authz: unknown grant origin")

	// ErrMissingResourceType reports a rule or a request naming no resource type.
	// There is no sentinel meaning every type.
	ErrMissingResourceType = errors.New("authz: a rule names exactly one resource type")

	// ErrMissingSubject reports a compartment restriction with no subject type, or
	// with neither a literal id nor a parameter to resolve one from.
	ErrMissingSubject = errors.New("authz: a compartment restriction needs a subject type and one id")

	// ErrUnrestrictedClinicalType reports an unrestricted rule over a type that
	// carries patient data, which is refused when the rule is authored (LNK-5).
	ErrUnrestrictedClinicalType = errors.New("authz: an unrestricted rule may not cover a clinical resource type")

	// ErrInvalidParameterName reports a declared parameter with no name, or one
	// declared twice.
	ErrInvalidParameterName = errors.New("authz: a policy parameter needs one distinct name")

	// ErrUndeclaredParameter reports a rule naming a parameter its policy does not
	// declare, so no binding would ever be asked to supply it.
	ErrUndeclaredParameter = errors.New("authz: rule names a parameter the policy does not declare")

	// ErrParameterUnresolved reports a declared parameter the binding supplies no
	// value for. An unresolved parameter denies; there is no default subject.
	ErrParameterUnresolved = errors.New("authz: binding supplies no value for a declared parameter")

	// ErrUnexpectedParameter reports a binding supplying a value the policy
	// declares no parameter for, which means the binding was written against a
	// different policy.
	ErrUnexpectedParameter = errors.New("authz: binding supplies a parameter the policy does not declare")
)

// ParameterName is one value a binding supplies to a policy. A policy declares
// the parameters it takes, so a rule can only name one some binding is asked
// for (FR-055).
type ParameterName string

// Parameters are the values one binding supplies. A missing or empty value is
// absent rather than a subject id of its own, which is why it denies instead of
// matching everything.
type Parameters map[ParameterName]storage.LogicalID

// ParseParameters reads a binding's stored parameters. Absent JSON is no
// parameters at all, and malformed JSON is an error, never an empty set a
// parameterized rule would then resolve against.
func ParseParameters(raw json.RawMessage) (Parameters, error) {
	if len(raw) == 0 {
		return Parameters{}, nil
	}

	values := Parameters{}
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("authz: read policy parameters: %w", err)
	}

	if values == nil {
		return Parameters{}, nil
	}

	return values, nil
}

// CompartmentSubject is a rule's restriction before a binding resolves it. The
// subject type is fixed when the rule is authored; its ids are either literals
// or the single value one named parameter resolves to.
type CompartmentSubject struct {
	resourceType storage.ResourceType
	ids          []storage.LogicalID
	parameter    ParameterName
}

// LiteralSubject restricts a rule to the subjects named when the rule is
// authored, such as the patients one research policy covers.
//
// Naming several is the same restriction as several rules naming one each:
// every id compiles to its own Grant and storage holds them together. It exists
// so a roster is one row rather than a row per member, which is an ergonomic
// difference and not a change in what a policy can say.
func LiteralSubject(
	resourceType storage.ResourceType, ids ...storage.LogicalID,
) (CompartmentSubject, error) {
	if resourceType == "" || len(ids) == 0 {
		return CompartmentSubject{}, fmt.Errorf("%w: %q names %d subjects",
			ErrMissingSubject, resourceType, len(ids))
	}

	named := make([]storage.LogicalID, 0, len(ids))

	for _, id := range ids {
		if id == "" {
			return CompartmentSubject{}, fmt.Errorf("%w: %q names an empty subject", ErrMissingSubject, resourceType)
		}

		// A repeated id would compile to a second Grant saying what the first
		// already says, which costs a predicate and answers nothing new.
		if !slices.Contains(named, id) {
			named = append(named, id)
		}
	}

	return CompartmentSubject{resourceType: resourceType, ids: named}, nil
}

// ParameterSubject restricts a rule to the subject a binding names, which is how
// one policy serves every patient reading their own chart (FR-055).
func ParameterSubject(resourceType storage.ResourceType, parameter ParameterName) (CompartmentSubject, error) {
	if resourceType == "" || parameter == "" {
		return CompartmentSubject{}, fmt.Errorf("%w: %q/%q", ErrMissingSubject, resourceType, parameter)
	}

	return CompartmentSubject{resourceType: resourceType, parameter: parameter}, nil
}

// Type returns the subject's resource type.
func (s CompartmentSubject) Type() storage.ResourceType {
	return s.resourceType
}

// Parameter returns the parameter the subject's id resolves from, if any.
func (s CompartmentSubject) Parameter() (ParameterName, bool) {
	return s.parameter, s.parameter != ""
}

// IDs returns the literal subjects the rule was authored with, empty on a
// subject whose id a binding supplies.
func (s CompartmentSubject) IDs() []storage.LogicalID {
	return slices.Clone(s.ids)
}

// Resolve turns the subject into the Compartments a rule's Grants carry. A
// parameter with no bound value denies: a restriction that cannot name its
// subject would restrict nothing.
func (s CompartmentSubject) Resolve(params Parameters) ([]storage.Compartment, error) {
	if s.resourceType == "" {
		return nil, ErrMissingSubject
	}

	ids := s.ids

	if s.parameter != "" {
		value, ok := params[s.parameter]
		if !ok || value == "" {
			return nil, fmt.Errorf("%w: %s", ErrParameterUnresolved, s.parameter)
		}

		ids = []storage.LogicalID{value}
	}

	if len(ids) == 0 {
		return nil, ErrMissingSubject
	}

	compartments := make([]storage.Compartment, 0, len(ids))
	for _, id := range ids {
		compartments = append(compartments, storage.Compartment{Type: s.resourceType, ID: id})
	}

	return compartments, nil
}

// Rule authorizes one action on one resource type under one restriction. One
// rule compiles to one Grant, so no rule can widen to a second type or action
// (LNK-3).
type Rule struct {
	kind         storage.Kind
	resourceType storage.ResourceType
	action       storage.Action
	subject      CompartmentSubject
	unrestricted bool
	filter       *storage.Filter
	projection   *storage.Projection
}

// NewRule authors a rule restricted to one compartment subject.
func NewRule(
	kind storage.Kind, resourceType storage.ResourceType, action storage.Action, subject CompartmentSubject,
) (Rule, error) {
	if err := validateTriple(kind, resourceType, action); err != nil {
		return Rule{}, err
	}

	if subject.resourceType == "" || (len(subject.ids) == 0 && subject.parameter == "") {
		return Rule{}, fmt.Errorf("%w: %s %s", ErrMissingSubject, resourceType, action)
	}

	return Rule{kind: kind, resourceType: resourceType, action: action, subject: subject}, nil
}

// NewUnrestrictedRule authors the explicitly unrestricted case LNK-5 requires to
// be declared, and refuses it for a type carrying patient data.
func NewUnrestrictedRule(kind storage.Kind, resourceType storage.ResourceType, action storage.Action) (Rule, error) {
	if err := validateTriple(kind, resourceType, action); err != nil {
		return Rule{}, err
	}

	if kind == storage.KindFHIR && CarriesClinicalData(resourceType) {
		return Rule{}, fmt.Errorf("%w: %s", ErrUnrestrictedClinicalType, resourceType)
	}

	return Rule{kind: kind, resourceType: resourceType, action: action, unrestricted: true}, nil
}

// WithFilter narrows the rule to the resources whose named element matches,
// such as the final Observations of a patient whose preliminary ones a
// reviewing clinician must not see.
//
// It takes a Filter by value because storage.NewFilter is the only thing that
// builds one, so a rule cannot come to carry a restriction nobody validated. A
// filter only ever narrows, so it needs no guard of its own — but neither does
// it lift one: a filter on an unrestricted rule over a clinical type leaves it
// unrestricted across every patient, which is what LNK-5 refuses.
func (r Rule) WithFilter(filter storage.Filter) Rule {
	// The receiver and the parameter are both copies, so this narrows a new rule
	// and leaves the one it was called on alone.
	r.filter = &filter

	return r
}

// Filter returns the rule's element restriction, absent on a rule that narrows
// by compartment alone.
func (r Rule) Filter() (storage.Filter, bool) {
	if r.filter == nil {
		return storage.Filter{}, false
	}

	return *r.filter, true
}

// Returning names the elements the rule hands back, such as an Observation's
// status and value without the note beside them.
//
// It narrows how much of a resource is returned and never which resources are
// reached, so it is the one restriction that cannot be a query predicate and
// the one that says nothing about whose data a rule covers: a projection over
// an unrestricted clinical rule still reaches every patient, which is what
// LNK-5 refuses.
func (r Rule) Returning(projection storage.Projection) Rule {
	// The receiver and the parameter are both copies, so this narrows a new rule
	// and leaves the one it was called on alone.
	r.projection = &projection

	return r
}

// Projection returns the elements the rule hands back, absent on a rule that
// returns whole resources.
func (r Rule) Projection() (storage.Projection, bool) {
	if r.projection == nil {
		return storage.Projection{}, false
	}

	return *r.projection, true
}

// grantFilter hands a compiled Grant its own copy. A Grant's Filter field is
// exported, so sharing the rule's pointer would let anyone holding one compiled
// Grant rewrite the policy every later compilation reads.
func (r Rule) grantFilter() *storage.Filter {
	if r.filter == nil {
		return nil
	}

	held := *r.filter

	return &held
}

// grantProjection hands a compiled Grant its own copy, for the reason
// grantFilter does.
func (r Rule) grantProjection() *storage.Projection {
	if r.projection == nil {
		return nil
	}

	held := *r.projection

	return &held
}

// Kind returns the resource family the rule covers.
func (r Rule) Kind() storage.Kind {
	return r.kind
}

// Type returns the one resource type the rule covers.
func (r Rule) Type() storage.ResourceType {
	return r.resourceType
}

// Action returns the one action the rule covers.
func (r Rule) Action() storage.Action {
	return r.action
}

// Subject returns the rule's compartment restriction, absent on an explicitly
// unrestricted rule.
func (r Rule) Subject() (CompartmentSubject, bool) {
	return r.subject, !r.unrestricted
}

// Covers reports whether the rule answers this exact triple. Matching is exact:
// a rule for a neighbouring type or action answers nothing here.
func (r Rule) Covers(kind storage.Kind, resourceType storage.ResourceType, action storage.Action) bool {
	return r.kind == kind && r.resourceType == resourceType && r.action == action
}

// compartments resolves the rule's restriction for one binding, one entry per
// Grant the rule mints. An unrestricted rule resolves to a single nil, which is
// the explicit declaration of no restriction and never an absent one (LNK-5).
func (r Rule) compartments(params Parameters) ([]*storage.Compartment, error) {
	if r.unrestricted {
		return []*storage.Compartment{nil}, nil
	}

	resolved, err := r.subject.Resolve(params)
	if err != nil {
		return nil, err
	}

	held := make([]*storage.Compartment, 0, len(resolved))

	for _, compartment := range resolved {
		held = append(held, &compartment)
	}

	return held, nil
}

// Origin is why a policy is being compiled. Super admin is absent on purpose:
// its cross-Project sweep mints one Grant per Project with no policy behind it,
// so no compilation here can produce one (CP-6).
type Origin string

// The reasons a policy is compiled.
const (
	OriginMembership Origin = "membership"
	OriginLink       Origin = "link"
)

// Source maps the origin onto the Grant field an audit reads to tell a member's
// own Project from one reached through a link.
func (o Origin) Source() (storage.GrantSource, bool) {
	switch o {
	case OriginMembership:
		return storage.SourceMembership, true
	case OriginLink:
		return storage.SourceLink, true
	default:
		return "", false
	}
}

// GrantRequest names the one decision a policy is compiled for. Kind, Type and
// Action are required: compilation answers this exact triple, never everything
// the policy allows.
type GrantRequest struct {
	Project    project.ID
	Kind       storage.Kind
	Type       storage.ResourceType
	Action     storage.Action
	Origin     Origin
	Parameters Parameters
}

// PolicyConfig is the input to NewAccessPolicy. Parameters are declared up
// front, so a rule can be checked against them when the policy is authored.
type PolicyConfig struct {
	Project    project.ID
	ID         storage.LogicalID
	Parameters []ParameterName
	Rules      []Rule
}

// AccessPolicy is one Project's set of rules. It grants only what it states: a
// triple no rule names compiles to no Grant, which is an empty Scope rather than
// a wider one.
type AccessPolicy struct {
	policyProject project.ID
	id            storage.LogicalID
	parameters    []ParameterName
	rules         []Rule
}

// NewAccessPolicy builds a policy owned by one Project. A rule naming a
// parameter the policy does not declare is refused here, so no binding can be
// asked for a value nothing would resolve.
func NewAccessPolicy(cfg PolicyConfig) (AccessPolicy, error) {
	if err := project.ValidateID(cfg.Project); err != nil {
		return AccessPolicy{}, err
	}

	if cfg.ID == "" {
		return AccessPolicy{}, fmt.Errorf("%w: access policy", ErrMissingPolicyID)
	}

	declared, err := declareParameters(cfg.Parameters)
	if err != nil {
		return AccessPolicy{}, err
	}

	for _, rule := range cfg.Rules {
		if err := checkRuleParameter(rule, declared); err != nil {
			return AccessPolicy{}, err
		}
	}

	return AccessPolicy{
		policyProject: cfg.Project, id: cfg.ID,
		parameters: declared, rules: slices.Clone(cfg.Rules),
	}, nil
}

// declareParameters rejects an unnamed or repeated parameter, so resolution has
// exactly one value to look for per name.
func declareParameters(names []ParameterName) ([]ParameterName, error) {
	declared := make([]ParameterName, 0, len(names))
	for _, name := range names {
		if name == "" || slices.Contains(declared, name) {
			return nil, fmt.Errorf("%w: %q", ErrInvalidParameterName, string(name))
		}

		declared = append(declared, name)
	}

	return declared, nil
}

// checkRuleParameter refuses a rule whose subject reads a parameter the policy
// never declares.
func checkRuleParameter(rule Rule, declared []ParameterName) error {
	name, ok := rule.subject.Parameter()
	if !ok || slices.Contains(declared, name) {
		return nil
	}

	return fmt.Errorf("%w: %s", ErrUndeclaredParameter, name)
}

// Project returns the Project that owns the policy.
func (p AccessPolicy) Project() project.ID {
	return p.policyProject
}

// ID returns the policy's logical id.
func (p AccessPolicy) ID() storage.LogicalID {
	return p.id
}

// Parameters returns the names a binding must supply values for.
func (p AccessPolicy) Parameters() []ParameterName {
	return slices.Clone(p.parameters)
}

// Rules returns a copy, so a caller holding a policy cannot append to it.
func (p AccessPolicy) Rules() []Rule {
	return slices.Clone(p.rules)
}

// Matches reports whether this policy is the one a binding or a link names. Both
// halves must agree: a same-named policy in another Project is a different
// policy (LNK-6).
func (p AccessPolicy) Matches(ref project.PolicyRef) bool {
	return !ref.IsZero() && ref.Project() == p.policyProject && ref.ID() == p.id
}

// Compile resolves the policy into the Grants one request may carry. Every Grant
// names the requested triple literally, so a rule can never answer with a type
// or action the caller did not ask for.
func (p AccessPolicy) Compile(req GrantRequest) ([]storage.Grant, error) {
	source, err := p.admit(req)
	if err != nil {
		return nil, err
	}

	values, err := p.resolveParameters(req.Parameters)
	if err != nil {
		return nil, err
	}

	grants := make([]storage.Grant, 0, len(p.rules))

	for _, rule := range p.rules {
		if !rule.Covers(req.Kind, req.Type, req.Action) {
			continue
		}

		compartments, err := rule.compartments(values)
		if err != nil {
			return nil, err
		}

		for _, compartment := range compartments {
			grants = append(grants, storage.Grant{
				Project: req.Project, Kind: req.Kind, Type: req.Type,
				Action: req.Action, Source: source, Compartment: compartment,
				Filter: rule.grantFilter(), Projection: rule.grantProjection(),
			})
		}
	}

	return grants, nil
}

// admit refuses a request this policy must not answer: another Project's, an
// unrecognised triple, or an origin outside the two a policy can serve.
func (p AccessPolicy) admit(req GrantRequest) (storage.GrantSource, error) {
	if req.Project != p.policyProject {
		return "", fmt.Errorf("%w: %s asked of %s", ErrPolicyProjectMismatch, req.Project, p.policyProject)
	}

	if err := validateTriple(req.Kind, req.Type, req.Action); err != nil {
		return "", err
	}

	source, ok := req.Origin.Source()
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownOrigin, string(req.Origin))
	}

	return source, nil
}

// resolveParameters binds every parameter the policy declares. A missing value
// denies the whole policy rather than the one rule that reads it, and a value
// for no declared parameter means the binding names a different policy.
func (p AccessPolicy) resolveParameters(supplied Parameters) (Parameters, error) {
	resolved := make(Parameters, len(p.parameters))

	for _, name := range p.parameters {
		value, ok := supplied[name]
		if !ok || value == "" {
			return nil, fmt.Errorf("%w: %s", ErrParameterUnresolved, name)
		}

		resolved[name] = value
	}

	for name := range supplied {
		if !slices.Contains(p.parameters, name) {
			return nil, fmt.Errorf("%w: %s", ErrUnexpectedParameter, name)
		}
	}

	return resolved, nil
}

// knownKinds and knownActions mirror the enums storage declares without
// predicates of their own, so an unrecognised value denies here instead of
// reaching a Grant.
var (
	knownKinds = []storage.Kind{storage.KindFHIR, storage.KindPlatform}

	knownActions = []storage.Action{
		storage.ActionRead, storage.ActionWrite, storage.ActionDelete,
		storage.ActionSearch, storage.ActionHistory,
	}
)

// validateTriple rejects a triple no rule may name and no request may ask for.
func validateTriple(kind storage.Kind, resourceType storage.ResourceType, action storage.Action) error {
	if !slices.Contains(knownKinds, kind) {
		return fmt.Errorf("%w: %q", ErrUnknownKind, string(kind))
	}

	if resourceType == "" {
		return ErrMissingResourceType
	}

	if !slices.Contains(knownActions, action) {
		return fmt.Errorf("%w: %q", ErrUnknownAction, string(action))
	}

	return nil
}

// nonClinicalResourceTypes is the closed list of FHIR types an unrestricted rule
// may cover: directory, terminology, conformance and definitional content. It is
// an allowlist so an unclassified type is refused rather than assumed safe.
//
// Subscription is here because it is configuration: it holds a query and an
// endpoint, never a patient's record. What a subscription then delivers is a
// different question, decided against the standing that created it rather than
// against the subscription — so a rule covering Subscription grants nothing
// about the resources one would match.
var nonClinicalResourceTypes = []storage.ResourceType{
	"ActivityDefinition", "CapabilityStatement", "CatalogEntry", "ChargeItemDefinition",
	"CodeSystem", "CompartmentDefinition", "ConceptMap", "DeviceDefinition",
	"EffectEvidenceSynthesis", "Endpoint", "EventDefinition", "Evidence", "EvidenceVariable",
	"ExampleScenario", "GraphDefinition", "HealthcareService", "ImplementationGuide",
	"InsurancePlan", "Library", "Location", "Measure", "Medication", "MedicationKnowledge",
	"MedicinalProduct", "MedicinalProductAuthorization", "MedicinalProductContraindication",
	"MedicinalProductIndication", "MedicinalProductIngredient", "MedicinalProductInteraction",
	"MedicinalProductManufactured", "MedicinalProductPackaged", "MedicinalProductPharmaceutical",
	"MedicinalProductUndesirableEffect", "MessageDefinition", "NamingSystem",
	"ObservationDefinition", "OperationDefinition", "Organization", "OrganizationAffiliation",
	"PlanDefinition", "Practitioner", "PractitionerRole", "Questionnaire", "ResearchDefinition",
	"ResearchElementDefinition", "RiskEvidenceSynthesis", "Schedule", "SearchParameter", "Slot", "SpecimenDefinition",
	"StructureDefinition", "StructureMap", "Subscription", "Substance", "SubstanceNucleicAcid",
	"SubstancePolymer", "SubstanceProtein", "SubstanceReferenceInformation",
	"SubstanceSourceMaterial", "SubstanceSpecification", "TerminologyCapabilities", "TestReport",
	"TestScript", "ValueSet",
}

// CarriesClinicalData reports whether a resource type may hold patient data. A
// type nobody classified answers yes, which is what makes LNK-5's guard refuse an
// unrestricted rule over Binary, MolecularSequence or anything FHIR adds later.
func CarriesClinicalData(resourceType storage.ResourceType) bool {
	return !slices.Contains(nonClinicalResourceTypes, resourceType)
}
