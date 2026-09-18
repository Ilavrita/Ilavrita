package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/authz"
	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/files"
	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	sqlite "github.com/Ilavrita/Ilavrita/packages/storage/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

const (
	homeProject  = project.ID("clinic-a")
	otherProject = project.ID("clinic-b")

	conformancePolicyID = storage.LogicalID("pol_conformance")
)

// conformanceIssued is when the stub session was minted, so its life is stated
// rather than measured against a clock the suite cannot hold still.
var conformanceIssued = time.Now().UTC().Add(-time.Minute)

// A session is what a password login issues, and a password belongs to a person,
// so the principal these tests are served as is a user. A client application
// authenticates by presenting its own secret, which is a separate path.
var conformancePrincipal = project.PrincipalRef{
	Kind: project.PrincipalUser,
	ID:   "usr_conformance",
}

// everyAction is what a principal exercising all six interactions must hold.
var everyAction = []storage.Action{
	storage.ActionRead, storage.ActionWrite, storage.ActionDelete,
	storage.ActionHistory, storage.ActionSearch,
}

// fixedMembership answers with one standing membership, and with none for any
// other Project or principal, so a test cannot pass by resolving the wrong one.
type fixedMembership struct {
	held project.Membership
}

func (f fixedMembership) Membership(
	_ context.Context, proj project.ID, principal project.PrincipalRef,
) (project.Membership, bool, error) {
	if proj != f.held.Project() || principal != f.held.Principal() {
		return project.Membership{}, false, nil
	}

	return f.held, true, nil
}

// fixedSession answers with one session for any token, and with none when no
// token is presented. These tests are about the FHIR surface rather than about
// authentication, but the surface is now reachable only through a session, so
// they have to carry one.
type fixedSession struct {
	proj      project.ID
	principal project.PrincipalRef

	// ended is what a logout elsewhere looks like to a socket already
	// connected: the token still parses, and the session behind it is gone.
	ended *atomic.Bool
}

func (f fixedSession) Issue(context.Context, project.Session) error {
	return nil
}

func (f fixedSession) Resolve(
	_ context.Context, token project.SessionToken, _ time.Time,
) (project.Session, bool, error) {
	if token.IsZero() {
		return project.Session{}, false, nil
	}

	held, err := project.NewSession(f.proj, project.SessionRecord{
		ID: "ses_conformance", User: project.UserID(f.principal.ID),
		Membership: "pm_conformance", Digest: token.Digest(), State: project.SessionActive,
		CreatedAt: conformanceIssued, ExpiresAt: conformanceIssued.Add(time.Hour),
	})
	if err != nil {
		return project.Session{}, false, err
	}

	return held, true, nil
}

func (f fixedSession) Revoke(context.Context, project.ID, project.SessionID, time.Time) error {
	return nil
}

func (f fixedSession) Live(
	context.Context, project.ID, project.SessionID, time.Time,
) (bool, error) {
	return f.ended == nil || !f.ended.Load(), nil
}

// endTheSession makes the wired resolver report the caller's session gone.
func endTheSession(t *testing.T) {
	t.Helper()

	held, ok := serving.sessions.(fixedSession)
	if !ok || held.ended == nil {
		t.Fatal("the wired session resolver cannot report a session ending")
	}

	held.ended.Store(true)
}

type activeProject struct{}

func (activeProject) State(context.Context, project.ID) (project.State, error) {
	return project.StateActive, nil
}

type fixedPolicy struct {
	held authz.AccessPolicy
}

func (f fixedPolicy) Policy(_ context.Context, ref project.PolicyRef) (authz.AccessPolicy, bool, error) {
	if !f.held.Matches(ref) {
		return authz.AccessPolicy{}, false, nil
	}

	return f.held, true, nil
}

// conformancePolicy grants the named actions on every advertised type, which is
// what lets one table-driven suite cover the whole declared surface.
func conformancePolicy(t *testing.T, proj project.ID, actions []storage.Action) authz.AccessPolicy {
	t.Helper()

	var rules []authz.Rule

	for _, name := range fhir.ServedResourceTypes() {
		for _, action := range actions {
			rules = append(rules, widestRule(t, storage.ResourceType(name), action))
		}
	}

	policy, err := authz.NewAccessPolicy(authz.PolicyConfig{
		Project: proj, ID: conformancePolicyID, Rules: rules,
	})
	if err != nil {
		t.Fatalf("author the conformance policy: %v", err)
	}

	return policy
}

func conformanceMembership(t *testing.T, proj project.ID) project.Membership {
	t.Helper()

	held, err := project.NewMembership(project.MembershipConfig{
		ID:          "pm_conformance",
		Project:     proj,
		ProjectKind: project.KindStandard,
		Principal:   conformancePrincipal,
		State:       project.MembershipActive,
		Policies:    []project.PolicyAttachment{{Policy: conformancePolicyID}},
		Source:      project.SourceAPI,
	})
	if err != nil {
		t.Fatalf("build the conformance membership: %v", err)
	}

	return held
}

func seedProject(t *testing.T, db *sql.DB, id project.ID) {
	t.Helper()

	_, err := db.ExecContext(context.Background(),
		"INSERT INTO projects (id, kind, slug, name, state, created_at, updated_at, state_changed_at)"+
			" VALUES (?, 'standard', ?, ?, 'active', 0, 0, 0)", string(id), string(id), string(id))
	if err != nil {
		t.Fatalf("seed project %s: %v", id, err)
	}

	// The standing the suite acts as, as a row rather than only as a stub. A
	// Subscription records who it delivers as, and that key cascades: standing
	// withdrawn is a subscription that stops delivering.
	//
	// One identity holds standing in several Projects — that is what a
	// server-scoped one is for — so the identity is seeded once and the
	// membership once per Project.
	_, err = db.ExecContext(context.Background(),
		"INSERT INTO users (id, scope, email_normalized, email_display, state, created_at, updated_at)"+
			" VALUES (?, 'server', ?, ?, 'active', 0, 0) ON CONFLICT (id) DO NOTHING",
		string(conformancePrincipal.ID), string(id)+"@example.test", string(id)+"@example.test")
	if err != nil {
		t.Fatalf("seed the conformance identity: %v", err)
	}

	_, err = db.ExecContext(context.Background(),
		"INSERT INTO project_memberships"+
			" (project_id, id, project_kind, user_id, state, invitation_source,"+
			" created_at, updated_at, activated_at)"+
			" VALUES (?, 'pm_conformance', 'standard', ?, 'active', 'api', 0, 0, 0)",
		string(id), string(conformancePrincipal.ID))
	if err != nil {
		t.Fatalf("seed the conformance membership: %v", err)
	}
}

// serveProject wires the server to serve every request as one principal in one
// Project. Serving the same database as a second Project is how a test proves a
// by-key route can never reach across the boundary.
func serveProject(t *testing.T, db *sql.DB, proj project.ID, actions []storage.Action) {
	t.Helper()

	serveUnder(t, db, proj, conformancePolicy(t, proj, actions))
}

// serveUnder wires the server to one Project under one policy, which is how a
// test asks what a particular authorable policy shape actually permits.
func serveUnder(t *testing.T, db *sql.DB, proj project.ID, policy authz.AccessPolicy) {
	t.Helper()

	serve(t, &backend{
		resources:     sqlite.NewResourceStore(db),
		users:         sqlite.NewUserStore(db),
		audits:        sqlite.NewAuditStore(db),
		payloads:      files.NewDisk(t.TempDir()),
		notifications: sqlite.NewSubscriptionStore(db),
		sockets:       newHub(),
		attempts:      newAttemptLimiter(sqlite.NewAttemptStore(db), testKeys, nil),
		resolvers: authz.Resolvers{
			Memberships: fixedMembership{conformanceMembership(t, proj)},
			Projects:    activeProject{},
			Policies:    fixedPolicy{policy},
			Links:       noProjectLinks{},
		},
		sessions: fixedSession{
			proj: proj, principal: conformancePrincipal, ended: &atomic.Bool{},
		},
	})
}

// confinedReadPolicy is an ordinary authorable shape: a read restricted to one
// compartment beside writes that are not. Every advertised type is non-clinical,
// so the unrestricted half is legal for all of them.
func confinedReadPolicy(t *testing.T, proj project.ID) authz.AccessPolicy {
	t.Helper()

	subject, err := authz.LiteralSubject("Organization", confinedCompartment)
	if err != nil {
		t.Fatalf("author the compartment subject: %v", err)
	}

	var rules []authz.Rule

	for _, name := range fhir.ServedResourceTypes() {
		for _, action := range everyAction {
			rules = append(rules, confinedRule(t, storage.ResourceType(name), action, subject))
		}
	}

	policy, err := authz.NewAccessPolicy(authz.PolicyConfig{
		Project: proj, ID: conformancePolicyID, Rules: rules,
	})
	if err != nil {
		t.Fatalf("author the confined policy: %v", err)
	}

	return policy
}

// confinedCompartment is the one compartment the confined read may see.
const confinedCompartment = storage.LogicalID("own1")

func confinedRule(
	t *testing.T, resourceType storage.ResourceType, action storage.Action, subject authz.CompartmentSubject,
) authz.Rule {
	t.Helper()

	if action == storage.ActionRead {
		rule, err := authz.NewRule(storage.KindFHIR, resourceType, action, subject)
		if err != nil {
			t.Fatalf("author a %s %s rule: %v", resourceType, action, err)
		}

		return rule
	}

	return widestRule(t, resourceType, action)
}

// conformancePatient is the compartment every clinical fixture lands in. A
// clinical type is reachable only through one, so the suite has to name it.
const conformancePatient = storage.LogicalID("pat-conformance")

// widestRule authors the widest rule a type legally admits: unrestricted where
// that is allowed, and confined to the conformance patient where it is not. A
// clinical type has no unrestricted form, which is the whole point of the split.
func widestRule(t *testing.T, resourceType storage.ResourceType, action storage.Action) authz.Rule {
	t.Helper()

	if !authz.CarriesClinicalData(resourceType) {
		rule, err := authz.NewUnrestrictedRule(storage.KindFHIR, resourceType, action)
		if err != nil {
			t.Fatalf("author a %s %s rule: %v", resourceType, action, err)
		}

		return rule
	}

	subject, err := authz.LiteralSubject("Patient", conformancePatient)
	if err != nil {
		t.Fatalf("author the conformance compartment: %v", err)
	}

	rule, err := authz.NewRule(storage.KindFHIR, resourceType, action, subject)
	if err != nil {
		t.Fatalf("author a confined %s %s rule: %v", resourceType, action, err)
	}

	return rule
}

// servingFHIR wires a whole server for one test: the real routes over a real
// database, with an authorization decision that actually resolves.
func servingFHIR(t *testing.T, actions []storage.Action) http.Handler {
	t.Helper()

	db := preparedDatabase(t)
	seedProject(t, db, homeProject)
	serveProject(t, db, homeProject, actions)

	return fhirRoutes(t)
}

// fhirRoutes builds the mux the process itself serves, so route matching is part
// of what every test below asserts rather than something they assume.
func fhirRoutes(t *testing.T) http.Handler {
	t.Helper()

	publishAt(t, testHost)

	routes := router.NewRouter(
		func(response http.ResponseWriter, request *http.Request) (*core.RequestEvent, router.EventCleanupFunc) {
			return &core.RequestEvent{Event: router.Event{Response: response, Request: request}}, nil
		})

	registerFHIRRoutes(routes)

	mux, err := routes.BuildMux()
	if err != nil {
		t.Fatalf("build the router: %v", err)
	}

	return mux
}

// conformanceTokenValue is the session token the suite presents. The stub
// resolver accepts any it can parse, so what it says is only that a request
// carried one.
const conformanceTokenValue = "conformance-token"

// testHost is the Host httptest sends. A published URL is only ever built from
// a host this deployment recognises, so the suite has to name the one it uses.
const testHost = "example.com"

// publishAt tells the server where it is reachable, which is what startup does
// from the environment, for the duration of one test.
func publishAt(t *testing.T, host string) {
	t.Helper()

	publishing = publishedAddress{allowed: []string{host}}
	t.Cleanup(func() { publishing = publishedAddress{} })
}

// call is one request against the wired routes.
type call struct {
	method      string
	path        string
	body        string
	ifMatch     string
	accept      string
	contentType string
	host        string
	origin      string
	bearer      string

	// securityContext is the header a raw Binary submission names its access
	// context in, because a PDF has nowhere else to say it.
	securityContext string

	anonymous bool
}

func (c call) send(t *testing.T, routes http.Handler) *httptest.ResponseRecorder {
	t.Helper()

	var payload io.Reader
	if c.body != "" {
		payload = strings.NewReader(c.body)
	}

	sent := httptest.NewRequest(c.method, c.path, payload)

	switch {
	case c.contentType != "":
		sent.Header.Set(contentTypeField, c.contentType)
	case c.body != "":
		sent.Header.Set(contentTypeField, fhir.ContentType)
	}

	if c.ifMatch != "" {
		sent.Header.Set(ifMatchField, c.ifMatch)
	}

	if c.accept != "" {
		sent.Header.Set(acceptField, c.accept)
	}

	if c.host != "" {
		sent.Host = c.host
	}

	if c.origin != "" {
		sent.Header.Set(originField, c.origin)
	}

	if c.securityContext != "" {
		sent.Header.Set(securityContextField, c.securityContext)
	}

	// The surface authenticates now, so a call that names no session reaches
	// nothing. A test that wants that answer sets anonymous; one acting as a
	// session it actually obtained names the token.
	switch {
	case c.anonymous:
	case c.bearer != "":
		sent.Header.Set(authorizationField, bearerPrefix+c.bearer)
	default:
		sent.Header.Set(authorizationField, bearerPrefix+conformanceTokenValue)
	}

	recorder := httptest.NewRecorder()
	routes.ServeHTTP(recorder, sent)

	return recorder
}

func assertStatus(t *testing.T, answer *httptest.ResponseRecorder, want int) {
	t.Helper()

	if answer.Code != want {
		t.Fatalf("status %d, want %d; body %s", answer.Code, want, answer.Body.String())
	}
}

// assertIssue reads the OperationOutcome a failure must answer with. A status
// alone is not enough: a client that parses only the body must be able to tell
// the failures apart.
func assertIssue(t *testing.T, answer *httptest.ResponseRecorder, status int, code fhir.IssueCode) {
	t.Helper()

	assertStatus(t, answer, status)

	if got := answer.Header().Get(contentTypeField); !strings.Contains(got, fhir.ContentType) {
		t.Fatalf("content type %q, want %q", got, fhir.ContentType)
	}

	var outcome fhir.OperationOutcome
	if err := json.Unmarshal(answer.Body.Bytes(), &outcome); err != nil {
		t.Fatalf("decode the outcome: %v; body %s", err, answer.Body.String())
	}

	if outcome.ResourceType != "OperationOutcome" || len(outcome.Issue) != 1 {
		t.Fatalf("body is %+v, want one OperationOutcome issue", outcome)
	}

	if outcome.Issue[0].Code != code {
		t.Fatalf("issue code %q, want %q", outcome.Issue[0].Code, code)
	}
}

func decodeResource(t *testing.T, answer *httptest.ResponseRecorder) map[string]any {
	t.Helper()

	resource := map[string]any{}
	if err := json.Unmarshal(answer.Body.Bytes(), &resource); err != nil {
		t.Fatalf("decode the resource: %v; body %s", err, answer.Body.String())
	}

	return resource
}

func resourceID(t *testing.T, answer *httptest.ResponseRecorder) string {
	t.Helper()

	id, named := decodeResource(t, answer)["id"].(string)
	if !named || id == "" {
		t.Fatalf("the resource carries no id; body %s", answer.Body.String())
	}

	return id
}

// assertVersionHeaders checks what every successful single-resource answer owes:
// the version as a weak ETag, the instant it was written, and a meta that agrees
// with both rather than with whatever the client submitted.
func assertVersionHeaders(t *testing.T, answer *httptest.ResponseRecorder, version string) {
	t.Helper()

	if got := answer.Header().Get(etagField); got != `W/"`+version+`"` {
		t.Fatalf("ETag %q, want %q", got, `W/"`+version+`"`)
	}

	if answer.Header().Get(lastModifiedField) == "" {
		t.Fatal("no Last-Modified on a successful read")
	}

	meta, carried := decodeResource(t, answer)["meta"].(map[string]any)
	if !carried {
		t.Fatalf("the resource carries no meta; body %s", answer.Body.String())
	}

	if meta["versionId"] != version {
		t.Fatalf("meta.versionId %v, want %q", meta["versionId"], version)
	}
}

// assertStoredCount reads the rows a request actually left behind, which is the
// only way to tell a refused write from one that committed and was then refused.
func assertStoredCount(t *testing.T, db *sql.DB, resourceType string, want int) {
	t.Helper()

	var stored int

	query := "SELECT COUNT(*) FROM fhir_resource WHERE res_type = ?"
	if err := db.QueryRowContext(context.Background(), query, resourceType).Scan(&stored); err != nil {
		t.Fatalf("count stored %s rows: %v", resourceType, err)
	}

	if stored != want {
		t.Fatalf("%d %s row(s) stored, want %d", stored, resourceType, want)
	}
}

func resourcePath(resourceType, id string) string {
	return fhir.BasePath + "/" + resourceType + "/" + id
}

// submission is the smallest body this server accepts for a type. A clinical one
// names the subject that places it in a compartment, because a resource landing
// in none is reachable by no confined grant.
func submission(resourceType string) string {
	body := `{"resourceType":"` + resourceType + `"`

	// A Subscription states what it watches, how it delivers and whether it is
	// on. All three are checked when it is written, so a fixture states them.
	if resourceType == string(subscriptionType) {
		body += `,"criteria":"Observation?status=final","status":"requested"` +
			`,"channel":{"type":"rest-hook","endpoint":"https://example.test/hook"}`
	}

	// A Binary is bytes, and what a client called them is what this server
	// hands back, so there is nothing to store without it.
	if resourceType == string(binaryType) {
		body += `,"contentType":"text/plain","data":"aGVsbG8="`
	}

	if path, placed := compartmentPath(resourceType); placed {
		body += `,"` + path + `":{"reference":"Patient/` + string(conformancePatient) + `"}`
	}

	return body + `}`
}

// compartmentPath names the element this suite places a clinical resource by. It
// mirrors what fhir.Compartments reads, so a fixture cannot drift from the
// derivation the server actually performs.
func compartmentPath(resourceType string) (string, bool) {
	switch resourceType {
	case "AllergyIntolerance", "BodyStructure", "Claim", "ClaimResponse", "Consent",
		"CoverageEligibilityRequest", "CoverageEligibilityResponse", "DetectedIssue",
		"EpisodeOfCare", "ExplanationOfBenefit", "FamilyMemberHistory", "Immunization",
		"ImmunizationEvaluation", "ImmunizationRecommendation", "MolecularSequence",
		"NutritionOrder", "RelatedPerson", "SupplyDelivery", "VisionPrescription":
		return "patient", true
	case "AppointmentResponse":
		return "actor", true
	case "ResearchSubject":
		return "individual", true
	case "Coverage":
		return "beneficiary", true
	case "Binary":
		// R4 places a Binary in no compartment of its own. securityContext is
		// the element it added for exactly this: the resource that governs
		// access to the payload.
		return "securityContext", true
	case "Task":
		return "for", true
	default:
		if authz.CarriesClinicalData(storage.ResourceType(resourceType)) {
			return "subject", true
		}

		return "", false
	}
}
