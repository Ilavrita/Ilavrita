package main

import (
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/files"
	"github.com/Ilavrita/Ilavrita/packages/search"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/Ilavrita/Ilavrita/packages/subscription"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

// The FHIR routes, beneath the base path. The wildcard is last because it is
// the least specific: the router matches on specificity, so every named route
// above outranks it whatever order they were registered in.
const (
	metadataPath        = "/metadata"
	typePath            = "/{resourceType}"
	typeHistoryPath     = "/{resourceType}/_history"
	typeSearchPath      = "/{resourceType}/_search"
	systemHistoryPath   = "/_history"
	systemSearchPath    = "/_search"
	instancePath        = "/{resourceType}/{id}"
	instanceHistoryPath = "/{resourceType}/{id}/_history"
	instanceVersionPath = "/{resourceType}/{id}/_history/{vid}"
	everythingElse      = "/{path...}"
)

// The path parameters the routes above bind.
const (
	resourceTypeParameter = "resourceType"
	idParameter           = "id"
	versionParameter      = "vid"
)

const (
	contentTypeField = "Content-Type"

	// formContentType is the one media type R4 names for a posted search.
	formContentType = "application/x-www-form-urlencoded"
	acceptField     = "Accept"
	formatParameter = "_format"
)

// servedInteraction is one interaction this build implements: the route that
// dispatches it and the code it is advertised under. One table registers the
// routes and fills the CapabilityStatement, so neither can drift from the other.
type servedInteraction struct {
	code    fhir.Interaction
	method  string
	path    string
	handler func(*core.RequestEvent) error
}

// servedInteractions is the whole implemented FHIR surface. Adding a row
// registers a route and advertises it; deleting one withdraws both. Anything
// absent here is unimplemented, and the wildcard answers it.
var servedInteractions = []servedInteraction{
	{fhir.InteractionCreate, http.MethodPost, typePath, createResource},
	{fhir.InteractionRead, http.MethodGet, instancePath, readResource},
	{fhir.InteractionUpdate, http.MethodPut, instancePath, updateResource},
	{fhir.InteractionDelete, http.MethodDelete, instancePath, deleteResource},
	{fhir.InteractionInstanceHistory, http.MethodGet, instanceHistoryPath, listResourceHistory},
	{fhir.InteractionVersionRead, http.MethodGet, instanceVersionPath, readResourceVersion},
	{fhir.InteractionSearchType, http.MethodGet, typePath, searchResources},
	{fhir.InteractionSearchType, http.MethodPost, typeSearchPath, searchResourcesByPost},
}

// The FHIR surface is owned by Ilavrita. PocketBase collections, admin routes
// and error shapes must never appear beneath this base path (FR-007, FR-029).
func registerFHIRRoutes(routes *router.Router[*core.RequestEvent]) {
	base := routes.Group(fhir.BasePath)

	// The runtime allows every origin by default. No browser on another origin
	// may read patient data, so the FHIR surface answers no preflight and
	// carries no cross-origin headers at all.
	base.Unbind(apis.DefaultCorsMiddlewareId)

	base.GET(metadataPath, describeCapabilities)

	// Reserved segments, not logical ids: every method an instance route answers
	// is refused on them, so none of them reads one as an id. _search keeps the
	// method it actually serves, which is registered below.
	for _, method := range instanceMethods() {
		base.Route(method, typeHistoryPath, rejectUnimplemented)

		if method != http.MethodPost {
			base.Route(method, typeSearchPath, rejectUnimplemented)
		}
	}

	for _, served := range servedInteractions {
		base.Route(served.method, served.path, audited(served.code, served.handler))
	}

	// Whole-system interactions. Each names an interaction rather than a
	// resource type, so it answers "not supported" rather than "no such type" —
	// which is what the bare /{resourceType} routes would otherwise make of it.
	// They are registered per method rather than for any, because a literal path
	// answering more methods than the pattern beside it is a routing conflict.
	for _, method := range methodsOn(typePath) {
		base.Route(method, systemHistoryPath, rejectUnimplemented)
		base.Route(method, systemSearchPath, rejectUnimplemented)
	}

	base.Any(everythingElse, rejectUnimplemented)
}

// advertisedSearchParameters is what one type may actually be searched by. It
// reads the same registry the query parser reads, so a statement cannot name a
// parameter a search would refuse.
func advertisedSearchParameters(resourceType string) []fhir.SearchParamCapability {
	supported := search.Supported(storage.ResourceType(resourceType))
	declared := make([]fhir.SearchParamCapability, 0, len(supported))

	for _, parameter := range supported {
		declared = append(declared, fhir.SearchParamCapability{
			Name: parameter.Name(), Type: string(parameter.Kind()),
		})
	}

	return declared
}

// instanceMethods is every method registered on the instance path, which is
// exactly what could otherwise match a reserved segment as a logical id.
func instanceMethods() []string {
	return methodsOn(instancePath)
}

// methodsOn lists the methods one path pattern answers.
func methodsOn(path string) []string {
	methods := make([]string, 0, len(servedInteractions))

	for _, served := range servedInteractions {
		if served.path == path {
			methods = append(methods, served.method)
		}
	}

	return methods
}

// describeCapabilities publishes what this build actually supports. It names no
// resource and touches no storage, so it answers before a client authenticates.
func describeCapabilities(request *core.RequestEvent) error {
	base, err := baseURL(request)
	if err != nil {
		return refuse(request, err)
	}

	statement := fhir.NewCapabilityStatement(fhir.CapabilityConfig{
		SoftwareVersion:  version,
		Published:        startedAt,
		BaseURL:          base,
		Interactions:     advertisedInteractions(),
		SearchParameters: advertisedSearchParameters,
	})

	return respondFHIR(request, http.StatusOK, statement)
}

// advertisedInteractions reads the codes off the table the routes were
// registered from, so the statement can name nothing this server does not serve.
func advertisedInteractions() []fhir.Interaction {
	codes := make([]fhir.Interaction, 0, len(servedInteractions))
	for _, served := range servedInteractions {
		codes = append(codes, served.code)
	}

	return codes
}

// rejectUnimplemented answers every FHIR route with no behaviour yet, as an
// OperationOutcome so clients only ever parse FHIR-shaped failures.
func rejectUnimplemented(request *core.RequestEvent) error {
	outcome := fhir.NewOperationOutcome(
		fhir.SeverityError,
		fhir.CodeNotSupported,
		"This interaction is not implemented. See the CapabilityStatement at "+fhir.BasePath+metadataPath+".",
	)

	return respondFHIR(request, http.StatusNotImplemented, outcome)
}

func respondFHIR(request *core.RequestEvent, status int, payload any) error {
	request.Response.Header().Set(contentTypeField, fhir.ContentType)

	return request.JSON(status, payload)
}

// baseURL is the FHIR base every published Location, fullUrl and
// CapabilityStatement URL is built from. The client's own Host reaches it only
// when this deployment recognises that host, so a forged one publishes nothing.
func baseURL(request *core.RequestEvent) (string, error) {
	origin, err := publishing.origin(request.Request)
	if err != nil {
		return "", err
	}

	return origin + fhir.BasePath, nil
}

// granted is one authorized interaction: the stores it may use, the Scope
// bounding them, and the Project and type that Scope was decided for. The Scope
// travels with the stores, so no handler holds one without the other.
type granted struct {
	resources storage.ResourceRepository
	payloads  files.Store

	// notifications is what a write owes whoever is watching, and standing is
	// who a Subscription written here would deliver as.
	notifications subscription.Queue
	standing      subscription.Owner

	searches     search.Repository
	versions     storage.VersionStore
	transactions storage.Transactor
	scope        storage.Scope
	project      storage.ProjectID
	resourceType storage.ResourceType
}

// begin settles everything an interaction needs before it may touch storage:
// what this server speaks, whether the type is an endpoint here at all, who is
// asking, and the Scope for each action the interaction will perform.
func begin(request *core.RequestEvent, actions []storage.Action) (granted, error) {
	return beginReading(request, actions, bodyMediaTypes)
}

// beginReading is begin for a route whose body is not a FHIR resource.
func beginReading(
	request *core.RequestEvent, actions []storage.Action, accepted []string,
) (granted, error) {
	if err := negotiate(request, accepted); err != nil {
		return granted{}, err
	}

	// Before the caller is resolved, so an unrecognised type answers the same
	// whether or not this process authenticates anyone (REST-2).
	resourceType := request.Request.PathValue(resourceTypeParameter)
	if !fhir.ServesResourceType(resourceType) {
		return granted{}, unknownResourceType
	}

	// Settled before anything is written: a write this server could not then
	// name would leave behind a row its own client can never address.
	if _, err := baseURL(request); err != nil {
		return granted{}, err
	}

	return permit(request, storage.ResourceType(resourceType), actions)
}

// permit builds one Scope per action the interaction performs, and refuses the
// moment one of them authorizes nothing. Merging cannot widen: each Grant was
// decided on its own, and storage matches a Grant to the action it names.
func permit(request *core.RequestEvent, resourceType storage.ResourceType, actions []storage.Action) (granted, error) {
	held := granted{resourceType: resourceType}

	var grants []storage.Grant

	for _, action := range actions {
		allowed, err := authorize(request, decision{Kind: storage.KindFHIR, Type: resourceType, Action: action})
		if err != nil {
			return granted{}, err
		}

		if allowed.Scope.IsEmpty() {
			return granted{}, notAuthorized
		}

		held.resources, held.versions = allowed.Resources, allowed.Versions
		held.searches, held.transactions = allowed.Searches, allowed.Transactions
		held.payloads, held.project = allowed.Payloads, allowed.Project
		held.notifications = allowed.Notifications
		grants = append(grants, allowed.Scope.Grants()...)
	}

	held.scope = storage.NewScope(grants...)

	// Who a Subscription written here would deliver as. It is read from the
	// session rather than from the body, because a client that could state it
	// could subscribe as somebody else — and what a subscriber may be told is
	// exactly what that somebody may read.
	if session, found, err := serving.session(request); err == nil && found {
		held.standing = subscription.Owner{
			Membership: session.Membership(), Principal: session.Principal(),
		}
	}

	// Whether a caller can read back what it is about to write is decided in
	// storage, against the compartments the submitted resource declares. This
	// once refused every confined write here, because a new row had no
	// compartment to check; it has one now, and the only place that knows it is
	// the one holding the record.
	return held, nil
}

// addressed names the resource the URL points at, inside the Project the Scope
// was built for. The Project is never read from the request: a caller cannot
// name one, so no request can reach outside the one it was authenticated into.
func (g granted) addressed(request *core.RequestEvent) (storage.ResourceKey, error) {
	id := request.Request.PathValue(idParameter)
	if !logicalIDPattern.MatchString(id) {
		return storage.ResourceKey{}, unknownResource
	}

	return storage.NewResourceKey(g.project, g.resourceType, storage.LogicalID(id))
}

// logicalIDPattern is R4's own id syntax. An id outside it names no stored row,
// so it is answered as missing without a query, and is never minted either.
var logicalIDPattern = regexp.MustCompile(`^[A-Za-z0-9.-]{1,64}$`)

// versionPattern mirrors how a version id is built: the version counter cast to
// text. Anything else names no stored version.
var versionPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

// addressedVersion reads the version the URL names, for the one route that
// names one.
func addressedVersion(request *core.RequestEvent) (storage.VersionID, error) {
	raw := request.Request.PathValue(versionParameter)
	if !versionPattern.MatchString(raw) {
		return "", unknownResource
	}

	return storage.VersionID(raw), nil
}

// The representations this server reads and writes. It serves one, so a client
// asking for another is refused rather than sent JSON it did not ask for.
var (
	acceptedMediaRanges = []string{"*/*", "application/*", "application/json", fhir.ContentType}
	bodyMediaTypes      = []string{"application/json", fhir.ContentType}

	// A posted search states a query, not a resource, so it is the one route
	// that reads a form.
	searchBodyMediaTypes = []string{formContentType}

	// rawPayloadBodies is nil, which negotiate reads as "any media type". A
	// Binary's whole point is that this server does not decide what a document
	// is, so the one route that stores bytes accepts whatever they were called.
	rawPayloadBodies []string
	acceptedFormats  = []string{"", "json", "application/json", fhir.ContentType}
)

// negotiate refuses a request this server cannot answer in the representation
// asked for, before anything else looks at it.
//
// accepted is what a body on this route may be sent as. It differs by route
// because a posted search carries a form and every other body carries a
// resource, and a route that accepted both would accept a resource submitted as
// a form.
func negotiate(request *core.RequestEvent, accepted []string) error {
	// A Binary read may ask for the document rather than the resource, and what
	// a document's media type is only the payload knows. That one route settles
	// its own Accept, after it has read what the payload is.
	binary := storage.ResourceType(request.Request.PathValue(resourceTypeParameter)) == binaryType

	if !binary && !acceptsJSON(request.Request.Header.Get(acceptField)) {
		return unsupportedAccept
	}

	if !slices.Contains(acceptedFormats, strings.TrimSpace(request.Request.URL.Query().Get(formatParameter))) {
		return unsupportedAccept
	}

	// A nil list accepts any media type, which is what the payload route needs:
	// nothing else may use it, and nothing else does.
	if accepted != nil && carriesBody(request.Request.Method) &&
		!slices.Contains(accepted, mediaType(request.Request.Header.Get(contentTypeField))) {
		return unsupportedBody
	}

	return nil
}

// acceptsJSON reports whether any media range the client listed covers what
// this server sends. No Accept header at all accepts everything.
func acceptsJSON(header string) bool {
	if strings.TrimSpace(header) == "" {
		return true
	}

	for _, candidate := range strings.Split(header, ",") {
		if slices.Contains(acceptedMediaRanges, mediaType(candidate)) {
			return true
		}
	}

	return false
}

// mediaType is one media range without its parameters, lowercased.
func mediaType(value string) string {
	name, _, _ := strings.Cut(value, ";")

	return strings.ToLower(strings.TrimSpace(name))
}

func carriesBody(method string) bool {
	return method == http.MethodPost || method == http.MethodPut
}
