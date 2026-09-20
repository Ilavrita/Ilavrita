package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/pocketbase/pocketbase/core"
)

// The members of a resource body this server reads or writes. Everything else
// passes through untouched: FHIR owns the rest of the resource, not Ilavrita.
const (
	resourceTypeField = "resourceType"
	idField           = "id"
	metaField         = "meta"
	versionIDField    = "versionId"
	lastUpdatedField  = "lastUpdated"
)

// serverMetaFields are the meta members the store assigns. A client's claim on
// them is dropped on write, so a row can never contradict the version beside it.
var serverMetaFields = []string{versionIDField, lastUpdatedField}

const (
	etagField         = "ETag"
	locationField     = "Location"
	lastModifiedField = "Last-Modified"
	ifMatchField      = "If-Match"
	ifNoneExistField  = "If-None-Exist"
	authenticateField = "WWW-Authenticate"
	weakPrefix        = "W/"
)

// authenticateChallenge names the scheme this server will accept. RFC 9110
// requires one on every 401, and a bearer token is what a FHIR client sends.
const authenticateChallenge = `Bearer realm="Ilavrita FHIR"`

// fhirInstant is R4's instant format, kept to milliseconds because that is the
// resolution the stored timestamp has.
const fhirInstant = "2006-01-02T15:04:05.000Z07:00"

// maximumBodyBytes bounds what one request may submit. A body larger than this
// is refused as too costly rather than buffered whole.
const maximumBodyBytes = 4 << 20

// submittedResource reads the body as a FHIR resource and refuses one that does
// not name the type the URL does.
func submittedResource(request *core.RequestEvent, want storage.ResourceType) (map[string]json.RawMessage, error) {
	body, err := io.ReadAll(http.MaxBytesReader(request.Response, request.Request.Body, maximumBodyBytes))
	if err != nil {
		return nil, readFailure(err)
	}

	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, unreadableBody
	}

	var declared string
	if err := json.Unmarshal(fields[resourceTypeField], &declared); err != nil || declared != string(want) {
		return nil, mismatchedType
	}

	return fields, nil
}

// readFailure separates a body this server declined to buffer from one it could
// not read. A client that sent too much is told so, rather than being told its
// body was malformed when it was merely large.
func readFailure(err error) error {
	var oversized *http.MaxBytesError
	if errors.As(err, &oversized) {
		return oversizedBody
	}

	return unreadableBody
}

// checkSubmittedID refuses a body naming a different id than the URL does. The
// disagreement is the client's to resolve; silently preferring one would store
// something nobody asked for.
func checkSubmittedID(fields map[string]json.RawMessage, id storage.LogicalID) error {
	raw, carried := fields[idField]
	if !carried {
		return nil
	}

	var declared string
	if err := json.Unmarshal(raw, &declared); err != nil || declared != string(id) {
		return mismatchedID
	}

	return nil
}

// storedResource is what the row carries: the submitted body under the logical
// id this route settled on, without the meta members the store owns.
func storedResource(fields map[string]json.RawMessage, id storage.LogicalID) (json.RawMessage, error) {
	if err := stamp(fields, idField, string(id)); err != nil {
		return nil, err
	}

	meta, err := submittedMeta(fields)
	if err != nil {
		return nil, err
	}

	for _, name := range serverMetaFields {
		delete(meta, name)
	}

	if err := replaceMeta(fields, meta); err != nil {
		return nil, err
	}

	return encode(fields)
}

// renderedResource stamps the stored body with the identity and version the row
// actually holds, so meta can never disagree with the ETag beside it.
func renderedResource(record storage.ResourceRecord) (json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(record.Content, &fields); err != nil {
		return nil, fmt.Errorf("ilavrita: decode a stored resource: %w", err)
	}

	meta, err := submittedMeta(fields)
	if err != nil {
		return nil, err
	}

	if err := stamp(fields, idField, string(record.Key.ID)); err != nil {
		return nil, err
	}

	if err := stamp(meta, versionIDField, string(record.Version)); err != nil {
		return nil, err
	}

	if err := stamp(meta, lastUpdatedField, record.LastUpdated.UTC().Format(fhirInstant)); err != nil {
		return nil, err
	}

	if err := replaceMeta(fields, meta); err != nil {
		return nil, err
	}

	return encode(fields)
}

// submittedMeta reads whatever meta a body already carries, so the members this
// server owns can be set without discarding the ones a client set.
func submittedMeta(fields map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	meta := map[string]json.RawMessage{}

	raw, carried := fields[metaField]
	if !carried {
		return meta, nil
	}

	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, unreadableBody
	}

	return meta, nil
}

func replaceMeta(fields, meta map[string]json.RawMessage) error {
	if len(meta) == 0 {
		delete(fields, metaField)

		return nil
	}

	encoded, err := encode(meta)
	if err != nil {
		return err
	}

	fields[metaField] = encoded

	return nil
}

// stamp writes one string member, which is how a field this server owns reaches
// a body it otherwise passes through unchanged.
func stamp(fields map[string]json.RawMessage, name, value string) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("ilavrita: encode %s: %w", name, err)
	}

	fields[name] = encoded

	return nil
}

func encode(fields map[string]json.RawMessage) (json.RawMessage, error) {
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("ilavrita: encode a resource: %w", err)
	}

	return encoded, nil
}

// mintLogicalID assigns the id this server publishes. It is random rather than
// sequential, so one id reveals neither another nor how many a Project holds.
func mintLogicalID() (storage.LogicalID, error) {
	var raw [16]byte

	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("ilavrita: mint a logical id: %w", err)
	}

	return storage.LogicalID(hex.EncodeToString(raw[:])), nil
}

// preconditionKind is what an If-Match header claims about the resource.
type preconditionKind uint8

const (
	// noPrecondition is an absent If-Match: the write is unconditional.
	noPrecondition preconditionKind = iota

	// anyVersion is "*": the resource must exist, at whatever version.
	anyVersion

	// namedVersion is the one version the resource must currently hold.
	namedVersion
)

// precondition is the claim one If-Match header makes. RFC 9110 13.1.1 gives
// "*" its own meaning: it is a claim of existence, not the absence of one.
type precondition struct {
	kind    preconditionKind
	version storage.VersionID
}

// stated reports whether the client claimed anything at all, which is what
// separates an update that may create from one that may only replace.
func (p precondition) stated() bool {
	return p.kind != noPrecondition
}

// disagrees reports whether the version a resource actually holds refutes the
// claim. "*" is refuted only by absence, which the caller answers on its own.
func (p precondition) disagrees(version storage.VersionID) bool {
	return p.kind == namedVersion && p.version != version
}

// expected is the version a write must still hold when it lands. Only a named
// version is one: an unconditional write is last-write-wins, and "*" claims
// existence rather than any particular version.
func (p precondition) expected() storage.VersionID {
	if p.kind == namedVersion {
		return p.version
	}

	return ""
}

// claimedPrecondition reads what If-Match claims. The weak marker is optional
// on input, and a list, or anything unquoted, is refused rather than guessed at.
func claimedPrecondition(request *core.RequestEvent) (precondition, error) {
	header := strings.TrimSpace(request.Request.Header.Get(ifMatchField))

	switch header {
	case "":
		return precondition{kind: noPrecondition}, nil
	case "*":
		return precondition{kind: anyVersion}, nil
	}

	quoted := strings.TrimPrefix(header, weakPrefix)
	if len(quoted) < 2 || !strings.HasPrefix(quoted, `"`) || !strings.HasSuffix(quoted, `"`) {
		return precondition{}, malformedIfMatch
	}

	token := quoted[1 : len(quoted)-1]
	if token == "" || strings.ContainsAny(token, `",`) {
		return precondition{}, malformedIfMatch
	}

	return precondition{kind: namedVersion, version: storage.VersionID(token)}, nil
}

// respondResource writes one resource with the headers R4 requires beside it:
// the version as a weak ETag, and the instant the row was last written.
func respondResource(request *core.RequestEvent, status int, record storage.ResourceRecord) error {
	body, err := renderedResource(record)
	if err != nil {
		return refuse(request, err)
	}

	request.Response.Header().Set(etagField, weakETag(record.Version))
	request.Response.Header().Set(lastModifiedField, httpDate(record.LastUpdated))

	return respondAsPreferred(request, status, body)
}

// respondAsPreferred answers a write with what the client said it wanted.
//
// The headers are already set either way: a client that asked for nothing back
// still needs the version it just wrote, and Location already names it.
func respondAsPreferred(request *core.RequestEvent, status int, body []byte) error {
	// Only a write is asked about. A read answers the resource because the
	// resource is what was asked for.
	if !carriesBody(request.Request.Method) && request.Request.Method != http.MethodDelete {
		return respondBody(request, status, body)
	}

	switch preferredReturn(request) {
	case returnMinimal:
		request.Response.WriteHeader(status)

		return nil

	case returnOutcome:
		return request.JSON(status, fhir.NewOperationOutcome(
			fhir.SeverityInformation, fhir.CodeInformational,
			"The resource was written."))

	default:
		return respondBody(request, status, body)
	}
}

// respondCreated answers a write that brought a resource into existence, naming
// the version it wrote in Location.
func respondCreated(request *core.RequestEvent, record storage.ResourceRecord) error {
	base, err := baseURL(request)
	if err != nil {
		return refuse(request, err)
	}

	request.Response.Header().Set(locationField, versionedLocation(base, record.Key, record.Version))

	return respondResource(request, http.StatusCreated, record)
}

// respondBody writes an already-serialised FHIR body verbatim, so what a client
// reads is what the row holds rather than a second encoding of it.
func respondBody(request *core.RequestEvent, status int, body []byte) error {
	return request.Blob(status, fhir.ContentType, body)
}

// weakETag is the only ETag form this server sends. It is weak because the JSON
// serialisation is not guaranteed byte-stable across requests even when the
// content has not changed.
func weakETag(version storage.VersionID) string {
	return weakPrefix + `"` + string(version) + `"`
}

func httpDate(instant time.Time) string {
	return instant.UTC().Format(http.TimeFormat)
}

// instantOf writes a moment the way R4's instant datatype spells one, which is
// not how an HTTP header spells it.
//
// Bundle.entry.response.lastModified is an instant; the Last-Modified header
// beside it is an HTTP-date. They name the same moment in different alphabets,
// and a client holding the JSON to its own datatype is right to refuse an
// HTTP-date there.
func instantOf(moment time.Time) string {
	return moment.UTC().Format(fhirInstant)
}

// versionedLocation always names the version, never the bare resource. Ilavrita
// versions every record, so the unversioned form R4 allows never applies.
func versionedLocation(base string, key storage.ResourceKey, version storage.VersionID) string {
	return resourceURL(base, key) + "/_history/" + string(version)
}

func resourceURL(base string, key storage.ResourceKey) string {
	return base + "/" + string(key.Type) + "/" + string(key.ID)
}

// historyBundle collects one resource's versions. ListVersions already returns
// them newest first and that order is the answer, so nothing here re-sorts.
func historyBundle(
	base string, page storage.VersionPage, self, next string,
) (json.RawMessage, error) {
	entries := make([]fhir.BundleEntry, 0, len(page.Records))

	for index, record := range page.Records {
		// Which interaction produced a version is read from where it sits in the
		// history, so it is only knowable on the page that holds the oldest one:
		// anywhere else, the version below this page is the one that would say.
		entry, err := historyEntry(base, record,
			versionVerb(record, index, len(page.Records), page.More))
		if err != nil {
			return nil, err
		}

		entries = append(entries, entry)
	}

	total, counted := page.Total()

	bundle, err := json.Marshal(fhir.NewHistoryBundle(fhir.HistoryConfig{
		Entries: entries, SelfURL: self, NextURL: next, Total: total, Counted: counted,
	}))
	if err != nil {
		return nil, fmt.Errorf("ilavrita: encode a history bundle: %w", err)
	}

	return bundle, nil
}

// versionVerb reconstructs which interaction wrote a version. Nothing stores it,
// so it is derived: the oldest version of an identity is the create, a tombstone
// is the delete, and every other version replaced the one before it.
func versionVerb(record storage.ResourceRecord, index, held int, more bool) fhir.HTTPVerb {
	switch {
	case record.Deleted:
		return fhir.VerbDelete

	// The oldest version is the one that created the resource, and it is the
	// last entry only on the last page. With a page still to come, the entry
	// below this one is somewhere the client has not read yet.
	case index == held-1 && !more:
		return fhir.VerbPost

	default:
		return fhir.VerbPut
	}
}

// historyEntry describes one version. A deletion carries no body, which is the
// same fact as the row holding no content.
func historyEntry(base string, record storage.ResourceRecord, verb fhir.HTTPVerb) (fhir.BundleEntry, error) {
	entry := fhir.BundleEntry{
		FullURL: resourceURL(base, record.Key),
		Request: &fhir.EntryRequest{Method: verb, URL: entryURL(record.Key, verb)},
		Response: &fhir.EntryResponse{
			Status:       entryStatus(verb),
			ETag:         weakETag(record.Version),
			LastModified: instantOf(record.LastUpdated),
		},
	}

	if record.Content == nil {
		return entry, nil
	}

	body, err := renderedResource(record)
	if err != nil {
		return fhir.BundleEntry{}, err
	}

	entry.Resource = body

	return entry, nil
}

// entryURL is the URL the recorded interaction was sent to: a create names the
// type it minted an id under, every other verb names the resource.
func entryURL(key storage.ResourceKey, verb fhir.HTTPVerb) string {
	if verb == fhir.VerbPost {
		return string(key.Type)
	}

	return string(key.Type) + "/" + string(key.ID)
}

func entryStatus(verb fhir.HTTPVerb) string {
	switch verb {
	case fhir.VerbPost:
		return "201"
	case fhir.VerbDelete:
		return "204"
	default:
		return "200"
	}
}
