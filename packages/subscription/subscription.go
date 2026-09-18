package subscription

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/search"
	"github.com/Ilavrita/Ilavrita/packages/storage"
)

var (
	// ErrMalformedCriteria reports a criteria this server cannot read, or one
	// naming a search it does not implement. It is refused when the
	// Subscription is written rather than when it would first fire: a
	// subscription that silently matches nothing is worse than one refused,
	// because somebody is relying on it.
	ErrMalformedCriteria = errors.New("subscription: the criteria names a search this server cannot run")

	// ErrUnknownChannel reports a channel type this build does not deliver on.
	// Accepting one would be promising to notify somebody this server has no
	// way of reaching.
	ErrUnknownChannel = errors.New("subscription: this build does not deliver on that channel")

	// ErrMalformedEndpoint reports an endpoint a channel cannot be delivered to.
	ErrMalformedEndpoint = errors.New("subscription: the endpoint is not one this channel can reach")

	// ErrUnknownStatus reports a status outside the enum.
	ErrUnknownStatus = errors.New("subscription: unknown status")
)

// Channel is how a subscriber is told.
type Channel string

// The channels this build delivers on.
const (
	// ChannelRestHook posts the notification to a URL the subscriber
	// registered. It needs no long-lived connection, which is what lets a
	// delivery survive a restart and a subscriber survive a deploy.
	ChannelRestHook Channel = "rest-hook"

	// ChannelWebSocket holds the notification for a subscriber that is
	// connected now. A subscriber that is not connected misses it: a socket is
	// not a queue, and R4's websocket channel carries no body anyway.
	ChannelWebSocket Channel = "websocket"
)

var knownChannels = []Channel{ChannelRestHook, ChannelWebSocket}

// Status is where a Subscription is in its life.
type Status string

// The statuses R4 defines, all of which this build stores.
const (
	// StatusRequested is asked for and not yet in service.
	StatusRequested Status = "requested"

	// StatusActive is in service: a matching write is delivered.
	StatusActive Status = "active"

	// StatusError has failed in a way that needs somebody to look.
	StatusError Status = "error"

	// StatusOff is switched off and delivers nothing.
	StatusOff Status = "off"
)

var knownStatuses = []Status{StatusRequested, StatusActive, StatusError, StatusOff}

// Subscription is one standing request to be told about writes.
type Subscription struct {
	id       storage.LogicalID
	watching storage.ResourceType
	criteria search.Query
	stated   string
	channel  Channel
	endpoint string
	payload  string
	status   Status
}

// ID returns the Subscription's own logical id.
func (s Subscription) ID() storage.LogicalID { return s.id }

// Watching returns the resource type the criteria names.
func (s Subscription) Watching() storage.ResourceType { return s.watching }

// Criteria returns the search a written resource is matched against.
func (s Subscription) Criteria() search.Query { return s.criteria }

// Stated returns the criteria as it was written, for rebuilding the search the
// match runs.
func (s Subscription) Stated() string { return s.stated }

// Channel returns how the subscriber is told.
func (s Subscription) Channel() Channel { return s.channel }

// Endpoint returns where, empty on a channel that needs no address.
func (s Subscription) Endpoint() string { return s.endpoint }

// Status returns where it is in its life.
func (s Subscription) Status() Status { return s.status }

// SendsPayload reports whether a notification carries the resource.
//
// R4 sends an empty body unless the subscription states a payload type, because
// the notification is a signal and the resource behind it is something the
// subscriber reads for themselves — under their own authorization, at their own
// time. Asking for no payload is the safer subscription, and it is the default.
func (s Subscription) SendsPayload() bool { return s.payload != "" }

// Delivers reports whether a matching write is told to this subscriber. Only an
// active Subscription delivers: the other three are states somebody has to move
// it out of.
func (s Subscription) Delivers() bool { return s.status == StatusActive }

// resource is the shape this reads out of a Subscription's content. Only the
// members that decide behaviour are named; the rest is the client's to carry.
type resource struct {
	Criteria string `json:"criteria"`
	Status   string `json:"status"`
	Channel  struct {
		Type     string `json:"type"`
		Endpoint string `json:"endpoint"`
		Payload  string `json:"payload"`
	} `json:"channel"`
}

// Read turns a submitted Subscription into what this server will act on, and
// refuses one it could not honour.
//
// Everything is checked when the Subscription is written rather than when it
// would first fire. A subscription accepted and then silently never matching is
// worse than one refused: somebody is relying on it, and nothing will tell them.
func Read(id storage.LogicalID, content []byte) (Subscription, error) {
	var held resource
	if err := json.Unmarshal(content, &held); err != nil {
		return Subscription{}, fmt.Errorf("%w: %w", ErrMalformedCriteria, err)
	}

	watching, criteria, err := readCriteria(held.Criteria)
	if err != nil {
		return Subscription{}, err
	}

	channel := Channel(held.Channel.Type)
	if !slices.Contains(knownChannels, channel) {
		return Subscription{}, fmt.Errorf("%w: %q", ErrUnknownChannel, held.Channel.Type)
	}

	endpoint, err := readEndpoint(channel, held.Channel.Endpoint)
	if err != nil {
		return Subscription{}, err
	}

	status := Status(held.Status)
	if !slices.Contains(knownStatuses, status) {
		return Subscription{}, fmt.Errorf("%w: %q", ErrUnknownStatus, held.Status)
	}

	payload, err := readPayload(held.Channel.Payload)
	if err != nil {
		return Subscription{}, err
	}

	return Subscription{
		id: id, watching: watching, criteria: criteria, stated: held.Criteria,
		channel: channel, endpoint: endpoint, payload: payload, status: status,
	}, nil
}

// readCriteria splits "Type?query" and checks the query against the search this
// build actually implements.
func readCriteria(stated string) (storage.ResourceType, search.Query, error) {
	if stated == "" {
		return "", search.Query{}, fmt.Errorf("%w: it names nothing", ErrMalformedCriteria)
	}

	name, query, _ := strings.Cut(stated, "?")

	watching := storage.ResourceType(strings.TrimSpace(name))
	if watching == "" {
		return "", search.Query{}, fmt.Errorf("%w: %q names no resource type", ErrMalformedCriteria, stated)
	}

	// A type this server does not serve can never be written, so a criteria
	// naming one is a subscription that could not fire. Naming a parameter
	// would catch most of these on its own; a bare type would not.
	if !fhir.ServesResourceType(string(watching)) {
		return "", search.Query{}, fmt.Errorf("%w: %s is not a resource type this server serves",
			ErrMalformedCriteria, watching)
	}

	values, err := url.ParseQuery(query)
	if err != nil {
		return "", search.Query{}, fmt.Errorf("%w: %w", ErrMalformedCriteria, err)
	}

	// A criteria describing a page is a criteria nobody meant: a match is about
	// one resource, and how many would come back is not a question here.
	for _, paging := range []string{search.CountParameter, search.CursorParameter, search.TotalParameter} {
		if values.Has(paging) {
			return "", search.Query{}, fmt.Errorf("%w: %s describes a page, not a match",
				ErrMalformedCriteria, paging)
		}
	}

	plan, err := search.Parse(watching, values)
	if err != nil {
		return "", search.Query{}, fmt.Errorf("%w: %w", ErrMalformedCriteria, err)
	}

	return watching, plan, nil
}

// readPayload checks what a notification would carry. This server serves one
// representation, so a subscription asking for another is asking for something
// it would not get.
func readPayload(stated string) (string, error) {
	switch strings.TrimSpace(stated) {
	case "":
		return "", nil
	case "application/fhir+json", "application/json":
		return "application/fhir+json", nil
	default:
		return "", fmt.Errorf("%w: this server does not send %q", ErrUnknownChannel, stated)
	}
}

// readEndpoint checks where a channel would deliver to.
func readEndpoint(channel Channel, stated string) (string, error) {
	if channel == ChannelWebSocket {
		// R4's websocket channel carries no endpoint: the subscriber connects
		// and names the Subscription, rather than the server reaching out.
		return "", nil
	}

	held, err := url.Parse(stated)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrMalformedEndpoint, err)
	}

	// A notification is a request this server makes on a subscriber's say-so, so
	// where it may be sent is this server's business. http and https only: a
	// file:// or a gopher:// endpoint would make the notifier a way to reach
	// whatever the host can.
	if held.Scheme != "https" && held.Scheme != "http" {
		return "", fmt.Errorf("%w: %q is not http or https", ErrMalformedEndpoint, stated)
	}

	if held.Host == "" {
		return "", fmt.Errorf("%w: %q names no host", ErrMalformedEndpoint, stated)
	}

	return held.String(), nil
}
