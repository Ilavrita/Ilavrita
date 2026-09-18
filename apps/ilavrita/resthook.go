package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/Ilavrita/Ilavrita/packages/subscription"
)

// allowPrivateHooksVariable lets a development deployment post notifications to
// its own network. It is off by default: a subscriber chooses the address, so
// without this the notifier is a way to reach whatever this host can.
const allowPrivateHooksVariable = "ILAVRITA_ALLOW_PRIVATE_HOOKS"

// How long one notification may take and how much of an answer is read. A
// subscriber that is slow must not hold a worker, and a subscriber that answers
// with a gigabyte must not be read into memory.
const (
	hookTimeout      = 10 * time.Second
	hookResponseRead = 4 << 10
)

// errPrivateEndpoint reports a notification that would have been sent inside
// this host's own network.
var errPrivateEndpoint = errors.New("ilavrita: a notification endpoint may not name a private address")

// restHook posts a notification to the URL a subscriber registered.
//
// The address is theirs and the request is this server's, which is the whole
// risk: without a guard, registering a subscription would be a way to make this
// server reach anything it can reach. The guard is on the dial rather than on
// the URL, so a name that resolves to a private address — or resolves again to
// one between the check and the connection — is refused at the moment it would
// be connected to.
type restHook struct {
	client *http.Client
}

// newRestHook builds the deliverer.
func newRestHook() *restHook {
	return &restHook{client: &http.Client{
		Timeout: hookTimeout,
		Transport: &http.Transport{
			DialContext: guardedDial(privateHooksAllowed()),

			// A subscriber is not a page to browse: one connection, used and
			// closed, rather than a pool held open to an address they chose.
			DisableKeepAlives: true,
		},

		// A redirect is a second address the subscriber did not register. The
		// dial guard would catch a private one, but following at all means
		// delivering somewhere other than where they said.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// privateHooksAllowed reports whether this deployment opted into posting to its
// own network.
func privateHooksAllowed() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(allowPrivateHooksVariable)), "true")
}

// guardedDial refuses a connection to an address that is not public.
func guardedDial(allowPrivate bool) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{
		Timeout: hookTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			if allowPrivate {
				return nil
			}

			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("%w: %s", errPrivateEndpoint, address)
			}

			if reachesOwnNetwork(net.ParseIP(host)) {
				return fmt.Errorf("%w: %s", errPrivateEndpoint, host)
			}

			return nil
		},
	}

	return dialer.DialContext
}

// reachesOwnNetwork reports whether an address is one this host should not be
// made to reach on a stranger's say-so: its own interfaces, its own network,
// and the link-local range cloud metadata services sit on.
func reachesOwnNetwork(address net.IP) bool {
	if address == nil {
		return true
	}

	return address.IsLoopback() || address.IsPrivate() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() ||
		address.IsUnspecified() || address.IsInterfaceLocalMulticast()
}

// Deliver posts one notification.
//
// What it carries is what the Subscription asked for: R4 sends an empty body
// unless the subscription states a payload type, because the notification is a
// signal and the resource behind it is something the subscriber reads for
// themselves — under their own authorization, at their own time.
func (h *restHook) Deliver(
	ctx context.Context,
	delivery subscription.Delivery,
	held subscription.Subscription,
	record storage.ResourceRecord,
) error {
	body, media := notificationBody(held, record)

	sent, err := http.NewRequestWithContext(ctx, http.MethodPost, held.Endpoint(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ilavrita: build a notification: %w", err)
	}

	if media != "" {
		sent.Header.Set(contentTypeField, media)
	}

	// What the notification is about, for a subscriber that asked for no body.
	sent.Header.Set(subscriptionField, string(held.ID()))
	sent.Header.Set(resourceField, string(record.Key.Type)+"/"+string(record.Key.ID))

	// And which notification it is. Delivery is at-least-once: a worker that
	// died between posting this and recording that it had posted it leaves the
	// delivery to be made again. A subscriber that must act once per write acts
	// once per identifier.
	sent.Header.Set(deliveryField, string(delivery.ID))

	answer, err := h.client.Do(sent)
	if err != nil {
		return fmt.Errorf("ilavrita: send a notification: %w", err)
	}

	defer func() { _ = answer.Body.Close() }()

	// Read and discard a bounded amount, so the connection can be released
	// without reading whatever the subscriber decided to answer with.
	_, _ = io.CopyN(io.Discard, answer.Body, hookResponseRead)

	if answer.StatusCode < 200 || answer.StatusCode > 299 {
		return fmt.Errorf("ilavrita: the subscriber answered %d", answer.StatusCode)
	}

	return nil
}

// The headers a notification carries, so a subscriber that asked for no body
// still knows what it is about.
const (
	subscriptionField = "X-Subscription"
	resourceField     = "X-Resource"
	deliveryField     = "X-Delivery"
)

// notificationBody is what the subscription asked to be sent.
func notificationBody(
	held subscription.Subscription, record storage.ResourceRecord,
) ([]byte, string) {
	if !held.SendsPayload() {
		return nil, ""
	}

	return record.Content, fhir.ContentType
}
