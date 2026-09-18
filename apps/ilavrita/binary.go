package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/Ilavrita/Ilavrita/packages/files"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/pocketbase/pocketbase/core"
)

// binaryType is the one resource type whose content is bytes rather than
// structure. Everything below is keyed on it by name rather than by a general
// rule, because nothing else this build serves behaves this way.
const binaryType storage.ResourceType = "Binary"

// The members of a Binary this server owns the meaning of.
const (
	contentTypeMember     = "contentType"
	dataMember            = "data"
	securityContextMember = "securityContext"
)

// maximumPayloadBytes bounds one payload. It is larger than a JSON body's limit
// because a scanned document is legitimately larger than a resource, and
// bounded for the same reason: an unbounded upload is one request that can fill
// a disk.
const maximumPayloadBytes = 64 << 20

var (
	// errPayloadUnavailable reports a deployment with nowhere to keep payloads.
	errPayloadUnavailable = errors.New("ilavrita: this deployment cannot hold payloads")

	// errMissingContentType reports a payload submitted without saying what it
	// is. What a client called it is what this server hands back, so there is
	// nothing to hand back without it.
	errMissingContentType = errors.New("ilavrita: a payload states its content type")

	// errUnreadablePayload reports base64 in a Binary's data member that is not.
	errUnreadablePayload = errors.New("ilavrita: the payload could not be read")

	// errPayloadMissing reports a row whose bytes are not there. The two stores
	// are ordered so a crash cannot produce this, so one that turns up is a
	// store somebody edited or a backup restored in halves — which is worth
	// saying rather than answering a Binary that quietly carries no document.
	errPayloadMissing = errors.New("ilavrita: the row exists and its payload does not")
)

// carriesRawPayload reports whether this request submits bytes rather than a
// resource: a Binary whose body is anything but FHIR JSON.
//
// It is asked before the body is negotiated, because negotiating it as a
// resource is exactly what must not happen to a PDF.
func carriesRawPayload(request *core.RequestEvent) bool {
	if storage.ResourceType(request.Request.PathValue(resourceTypeParameter)) != binaryType {
		return false
	}

	media := mediaType(request.Request.Header.Get(contentTypeField))

	return media != "" && !slices.Contains(bodyMediaTypes, media)
}

// createPayload answers a raw Binary submission. It authorizes as a write like
// every other create, and only then reads the body: a payload this caller may
// not store is one this server should not have spooled to disk first.
func createPayload(request *core.RequestEvent) error {
	held, err := beginReading(request, writeActions, rawPayloadBodies)
	if err != nil {
		return refuse(request, err)
	}

	key, err := held.mintedKey()
	if err != nil {
		return refuse(request, err)
	}

	if err := submitBinary(request, held, key); err != nil {
		return refuse(request, err)
	}

	return nil
}

// submitBinary turns a raw payload into the resource that describes it and
// hands both to the ordinary create path.
//
// What is stored in the row is what the payload is — its media type, and what
// governs access to it — and the bytes go to the payload store. Putting
// megabytes in the row would make every read of the metadata pay for them, and
// every backup of the database carry them.
func submitBinary(request *core.RequestEvent, held granted, key storage.ResourceKey) error {
	// Non-empty by construction: carriesRawPayload is what routed the request
	// here, and it only does so for a body that said what it is.
	media := mediaType(request.Request.Header.Get(contentTypeField))

	raw, err := io.ReadAll(io.LimitReader(request.Request.Body, maximumPayloadBytes+1))
	if err != nil {
		return errUnreadablePayload
	}

	if int64(len(raw)) > maximumPayloadBytes {
		return files.ErrTooLarge
	}

	fields := map[string]json.RawMessage{
		resourceTypeField: json.RawMessage(`"` + string(binaryType) + `"`),
	}

	if err := stamp(fields, contentTypeMember, media); err != nil {
		return err
	}

	if err := stamp(fields, dataMember, base64.StdEncoding.EncodeToString(raw)); err != nil {
		return err
	}

	// A raw submission names what governs access to it in a header, because a
	// PDF has nowhere else to say it. Without one the Binary lands in no
	// compartment, and a confined caller cannot write it — which is the answer
	// an unattributed document should get.
	if governs := strings.TrimSpace(request.Request.Header.Get(securityContextField)); governs != "" {
		fields[securityContextMember] = json.RawMessage(`{"reference":` + quoted(governs) + `}`)
	}

	content, err := storedResource(fields, key.ID)
	if err != nil {
		return err
	}

	return createResourceAt(request, held, key, content)
}

// quoted renders one string as JSON, so a header value cannot change the shape
// of the document it lands in.
func quoted(raw string) string {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return `""`
	}

	return string(encoded)
}

// carried is the bytes a Binary write submits, absent for every other type.
type carried struct {
	media string
	body  []byte
	held  bool
}

// splitPayload separates a Binary's bytes from the resource describing them, so
// one write stores the description and the other stores the document. Every
// other type passes through untouched.
func splitPayload(key storage.ResourceKey, content json.RawMessage) (json.RawMessage, carried, error) {
	if key.Type != binaryType {
		return content, carried{}, nil
	}

	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(content, &fields); err != nil {
		return nil, carried{}, unreadableBody
	}

	media := mediaOf(fields)
	if media == "" {
		return nil, carried{}, errMissingContentType
	}

	var encoded string
	if raw, present := fields[dataMember]; present {
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return nil, carried{}, errUnreadablePayload
		}
	}

	body, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, carried{}, errUnreadablePayload
	}

	delete(fields, dataMember)

	row, err := encode(fields)
	if err != nil {
		return nil, carried{}, err
	}

	return row, carried{media: media, body: body, held: true}, nil
}

// storing is what a write does once the row exists: put the bytes under the
// version the row settled on. It returns nil for a write that carries none, so
// every other type's write is unchanged.
func (g granted) storing(
	key storage.ResourceKey, payload carried,
) func(context.Context, storage.ResourceRecord) error {
	if !payload.held {
		return nil
	}

	return func(ctx context.Context, record storage.ResourceRecord) error {
		if g.payloads == nil {
			return errPayloadUnavailable
		}

		placing := files.Key{
			Project: key.Project, Type: key.Type, ID: key.ID, Version: record.Version,
		}

		if _, err := g.payloads.Put(
			ctx, placing, payload.media, bytes.NewReader(payload.body), maximumPayloadBytes,
		); err != nil {
			return err
		}

		recordPlacement(ctx, placing)

		return nil
	}
}

// placedKey is where a write reports the payloads it put on the disk, so a
// commit boundary that does not commit can take them back.
//
// The disk has no rollback of its own. The bytes are placed inside the
// transaction that writes the row — which is what keeps a committed row from
// ever naming bytes that are not there — so a transaction that rolls away
// leaves a document on the disk the client was told was not stored.
type placedKey struct{}

// placedPayloads is the slot a write fills in. One request is one goroutine, so
// nothing guards it.
type placedPayloads struct{ keys []files.Key }

// recordPlacement notes a payload this boundary placed, if something is
// listening.
func recordPlacement(ctx context.Context, key files.Key) {
	if slot, listening := ctx.Value(placedKey{}).(*placedPayloads); listening {
		slot.keys = append(slot.keys, key)
	}
}

// discardPlaced takes back every payload a boundary placed and did not commit,
// and forgets them so a boundary outside this one does not try again.
//
// Best effort: the row is gone either way, so a file that cannot be removed is
// wasted space rather than a wrong answer.
func discardPlaced(ctx context.Context, store files.Store, slot *placedPayloads) {
	if store == nil {
		return
	}

	for _, key := range slot.keys {
		_ = store.Discard(ctx, key)
	}

	slot.keys = nil
}

// mediaOf reads the content type a Binary states.
func mediaOf(fields map[string]json.RawMessage) string {
	var media string
	_ = json.Unmarshal(fields[contentTypeMember], &media)

	return media
}

// securityContextField is the header a raw submission names its access context
// in, because a PDF has nowhere else to say it.
const securityContextField = "X-Security-Context"

// servePayload answers a Binary read with its bytes, when that is what the
// caller asked for.
//
// It is served only when the Accept names the payload's own media type. This
// server answers JSON for */* everywhere else, and a route that inferred
// "native" from a wildcard would hand a browser a document where every other
// route hands it a resource.
func servePayload(
	request *core.RequestEvent, held granted, record storage.ResourceRecord,
) (bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(record.Content, &fields); err != nil {
		return false, err
	}

	asked := request.Request.Header.Get(acceptField)

	media := mediaOf(fields)
	if media == "" || !acceptsExactly(asked, media) {
		// The Accept named neither the document nor a representation this
		// server has. Answering the resource anyway would answer something the
		// caller said they could not read.
		if !acceptsJSON(asked) {
			return false, unsupportedAccept
		}

		return false, nil
	}

	if held.payloads == nil {
		return false, errPayloadUnavailable
	}

	body, stored, err := held.payloads.Open(request.Request.Context(), files.Key{
		Project: record.Key.Project, Type: record.Key.Type,
		ID: record.Key.ID, Version: record.Version,
	})
	if errors.Is(err, files.ErrNotFound) {
		// Same as the JSON read: the row was reached, so its bytes being gone is
		// this server's own inconsistency rather than a resource nobody has.
		return false, errPayloadMissing
	}

	if err != nil {
		return false, err
	}
	defer func() { _ = body.Close() }()

	request.Response.Header().Set(contentTypeField, stored.Media)
	request.Response.Header().Set(etagField, weakETag(record.Version))
	request.Response.Header().Set(lastModifiedField, httpDate(record.LastUpdated))
	request.Response.WriteHeader(http.StatusOK)

	_, err = io.Copy(request.Response, body)

	return true, err
}

// acceptsExactly reports whether an Accept names one media type, by name or by
// its family. A wildcard does not count: this server answers JSON for */*.
func acceptsExactly(header, media string) bool {
	family, _, _ := strings.Cut(media, "/")

	for _, offered := range strings.Split(header, ",") {
		name, _, _ := strings.Cut(strings.TrimSpace(offered), ";")

		switch strings.TrimSpace(name) {
		case media, family + "/*":
			return true
		}
	}

	return false
}

// embedPayload puts a Binary's bytes back into the resource, which is what a
// FHIR read of one returns.
func embedPayload(
	ctx context.Context, held granted, record storage.ResourceRecord,
) (storage.ResourceRecord, error) {
	if record.Key.Type != binaryType || held.payloads == nil {
		return record, nil
	}

	body, _, err := held.payloads.Open(ctx, files.Key{
		Project: record.Key.Project, Type: record.Key.Type,
		ID: record.Key.ID, Version: record.Version,
	})
	if errors.Is(err, files.ErrNotFound) {
		// The row was already read under this caller's Scope, so this is not an
		// authorization answer dressed as a missing one. Returning the resource
		// without its data would hand back a Binary that says it is a PDF and
		// carries nothing.
		return storage.ResourceRecord{}, errPayloadMissing
	}

	if err != nil {
		return storage.ResourceRecord{}, err
	}
	defer func() { _ = body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(body, maximumPayloadBytes+1))
	if err != nil {
		return storage.ResourceRecord{}, err
	}

	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(record.Content, &fields); err != nil {
		return storage.ResourceRecord{}, err
	}

	if err := stamp(fields, dataMember, base64.StdEncoding.EncodeToString(raw)); err != nil {
		return storage.ResourceRecord{}, err
	}

	embedded, err := encode(fields)
	if err != nil {
		return storage.ResourceRecord{}, err
	}

	record.Content = embedded

	return record, nil
}
