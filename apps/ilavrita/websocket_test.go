package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/fhir"
	"github.com/Ilavrita/Ilavrita/packages/subscription"
	"github.com/coder/websocket"
)

// connected opens one subscriber socket against the real routes.
func connected(t *testing.T, server *httptest.Server, token string) *websocket.Conn {
	t.Helper()

	socket, answer, err := websocket.Dial(context.Background(),
		"ws"+strings.TrimPrefix(server.URL, "http")+fhir.BasePath+websocketPath,
		&websocket.DialOptions{Subprotocols: []string{bearerProtocol + "." + token}})
	if answer != nil && answer.Body != nil {
		_ = answer.Body.Close()
	}

	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	t.Cleanup(func() { _ = socket.CloseNow() })

	return socket
}

// says sends one line and reads the answer, or reports that none came.
func says(t *testing.T, socket *websocket.Conn, line string) (string, bool) {
	t.Helper()

	ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()

	if err := socket.Write(ctx, websocket.MessageText, []byte(line)); err != nil {
		return "", false
	}

	_, answer, err := socket.Read(ctx)
	if err != nil {
		return "", false
	}

	return string(answer), true
}

// hears waits for one unprompted message, or reports that none came.
func hears(t *testing.T, socket *websocket.Conn, within time.Duration) (string, bool) {
	t.Helper()

	ctx, stop := context.WithTimeout(context.Background(), within)
	defer stop()

	_, message, err := socket.Read(ctx)
	if err != nil {
		return "", false
	}

	return string(message), true
}

// TestASubscriberIsPingedWhenItsSubscriptionFires.
func TestASubscriberIsPingedWhenItsSubscriptionFires(t *testing.T) {
	routes, _, _, worker := watchingServer(t)

	worker.channels = map[subscription.Channel]deliverer{subscription.ChannelWebSocket: serving.sockets}

	server := httptest.NewServer(routes)
	defer server.Close()

	id := subscribeOver(t, routes, "Observation?status=final", "websocket", "")

	socket := connected(t, server, conformanceTokenValue)

	if answer, ok := says(t, socket, "bind "+id); !ok || answer != "bound "+id {
		t.Fatalf("bind answered %q (ok=%v)", answer, ok)
	}

	observe(t, routes, "final")
	worker.pass(context.Background())

	notice, heard := hears(t, socket, 2*time.Second)
	if !heard || notice != "ping "+id {
		t.Errorf("heard %q (ok=%v), want a ping", notice, heard)
	}
}

// TestASubscriberBindsOnlyToItsOwnSubscription. Knowing when somebody else's
// fires is knowing something about the data behind it — and the notification is
// authorized as its owner, not as whoever happens to be listening.
func TestASubscriberBindsOnlyToItsOwnSubscription(t *testing.T) {
	routes, db, _, _ := watchingServer(t)

	server := httptest.NewServer(routes)
	defer server.Close()

	id := subscribeOver(t, routes, "Observation?status=final", "websocket", "")

	// Somebody else's standing, and the subscription moved onto it.
	if _, err := db.ExecContext(context.Background(),
		"INSERT INTO users (id, scope, email_normalized, email_display, state, created_at, updated_at)"+
			" VALUES ('usr_other', 'server', 'other@example.test', 'other@example.test', 'active', 0, 0)"); err != nil {
		t.Fatalf("seed the other identity: %v", err)
	}

	if _, err := db.ExecContext(context.Background(),
		"INSERT INTO project_memberships"+
			" (project_id, id, project_kind, user_id, state, invitation_source,"+
			" created_at, updated_at, activated_at)"+
			" VALUES (?, 'pm_other', 'standard', 'usr_other', 'active', 'api', 0, 0, 0)",
		string(homeProject)); err != nil {
		t.Fatalf("seed the other standing: %v", err)
	}

	if _, err := db.ExecContext(context.Background(),
		"UPDATE subscription_owners SET membership_id = 'pm_other'"); err != nil {
		t.Fatalf("move the subscription: %v", err)
	}

	socket := connected(t, server, conformanceTokenValue)

	if answer, ok := says(t, socket, "bind "+id); ok {
		t.Errorf("a bind to somebody else's subscription answered %q", answer)
	}
}

// TestABindToNothingIsRefused.
func TestABindToNothingIsRefused(t *testing.T) {
	routes, _, _, _ := watchingServer(t)

	server := httptest.NewServer(routes)
	defer server.Close()

	for _, line := range []string{"bind", "bind ", "bind not-a-subscription", "hello", "", "ping sub-1"} {
		socket := connected(t, server, conformanceTokenValue)

		if answer, ok := says(t, socket, line); ok {
			t.Errorf("%q answered %q, want a refusal", line, answer)
		}
	}
}

// TestASocketWithNoSessionIsRefused, as every other route refuses one.
func TestASocketWithNoSessionIsRefused(t *testing.T) {
	routes, _, _, _ := watchingServer(t)

	server := httptest.NewServer(routes)
	defer server.Close()

	// The suite's session resolver accepts any token it can parse, so what is
	// asserted here is that a socket carrying none is refused — not what a
	// wrong one does, which is the resolver's own business and is tested where
	// it lives.
	for name, protocols := range map[string][]string{
		"no subprotocol at all": nil,
		"the bare protocol":     {bearerProtocol},
		"an empty token":        {bearerProtocol + "."},
	} {
		socket, answer, err := websocket.Dial(context.Background(),
			"ws"+strings.TrimPrefix(server.URL, "http")+fhir.BasePath+websocketPath,
			&websocket.DialOptions{Subprotocols: protocols})
		if answer != nil && answer.Body != nil {
			_ = answer.Body.Close()
		}

		if err == nil {
			_ = socket.CloseNow()

			t.Errorf("%s connected", name)
		}
	}
}

// TestANotificationWithNobodyListeningIsNotRetried. A socket is not a queue: a
// subscriber that may simply be offline must not be retried at for half an
// hour, and the delivery is recorded as never made rather than as made.
func TestANotificationWithNobodyListeningIsNotRetried(t *testing.T) {
	routes, db, _, worker := watchingServer(t)

	worker.channels = map[subscription.Channel]deliverer{subscription.ChannelWebSocket: serving.sockets}

	subscribeOver(t, routes, "Observation?status=final", "websocket", "")
	observe(t, routes, "final")

	worker.pass(context.Background())

	held := deliveryStates(t, db)
	if len(held) != 1 || held[0] != string(subscription.Abandoned) {
		t.Errorf("the delivery is %v, want it recorded as never made", held)
	}
}

// TestAWebSocketSubscriptionIsNotDeliveredByTheOtherChannel, so a subscription
// can never be reached by a channel it did not ask for.
func TestAWebSocketSubscriptionIsNotDeliveredByTheOtherChannel(t *testing.T) {
	routes, _, deliveries, worker := watchingServer(t)

	subscribeOver(t, routes, "Observation?status=final", "websocket", "")
	observe(t, routes, "final")

	worker.pass(context.Background())

	if sent := deliveries.delivered(); len(sent) != 0 {
		t.Errorf("a websocket subscription was posted to a hook: %v", sent)
	}
}

// subscribeOver creates a Subscription on one channel and returns its id.
func subscribeOver(t *testing.T, routes http.Handler, criteria, channel, payload string) string {
	t.Helper()

	held := `{"type":"` + channel + `"`

	if channel == "rest-hook" {
		held += `,"endpoint":"https://example.test/hook"`
	}

	if payload != "" {
		held += `,"payload":"` + payload + `"`
	}

	body := valid("Subscription", map[string]string{
		"criteria": `"` + criteria + `"`,
		"status":   `"active"`,
		"channel":  held + `}`,
	})

	answer := call{
		method: http.MethodPost, path: fhir.BasePath + "/Subscription", body: body,
	}.send(t, routes)

	assertStatus(t, answer, http.StatusCreated)

	return resourceID(t, answer)
}

// TestOnlyABindBinds. A socket that says something else is saying something
// this server does not answer, and reading it as a bind would let a subscriber
// reach a state it never asked for.
func TestOnlyABindBinds(t *testing.T) {
	routes, _, _, _ := watchingServer(t)

	server := httptest.NewServer(routes)
	defer server.Close()

	id := subscribeOver(t, routes, "Observation?status=final", "websocket", "")

	// Every one of these names a subscription this caller does own, so what is
	// refused is the command rather than the subscription.
	for _, line := range []string{
		"ping " + id, "bound " + id, "BIND " + id, " " + id, "bindx " + id,
	} {
		socket := connected(t, server, conformanceTokenValue)

		if answer, ok := says(t, socket, line); ok {
			t.Errorf("%q answered %q, want a refusal", line, answer)
		}
	}

	// And the bind itself still works, so the refusals above are the command.
	socket := connected(t, server, conformanceTokenValue)

	if answer, ok := says(t, socket, "bind "+id); !ok || answer != "bound "+id {
		t.Errorf("bind answered %q (ok=%v)", answer, ok)
	}
}

// TestASocketIsClosedWhenItsSessionEnds. Every other route proves a token on
// each request, so a logout takes effect on the next one. A socket is
// authorized once and then held, so without a watch it would outlive the
// session it bound under for as long as the connection lasted.
func TestASocketIsClosedWhenItsSessionEnds(t *testing.T) {
	routes, _, _, _ := watchingServer(t)

	serving.sockets.interval = 20 * time.Millisecond

	server := httptest.NewServer(routes)
	defer server.Close()

	id := subscribeOver(t, routes, "Observation?status=final", "websocket", "")

	socket := connected(t, server, conformanceTokenValue)

	if answer, ok := says(t, socket, "bind "+id); !ok || answer != "bound "+id {
		t.Fatalf("bind answered %q (ok=%v)", answer, ok)
	}

	endTheSession(t)

	ctx, stop := context.WithTimeout(context.Background(), 2*time.Second)
	defer stop()

	_, _, err := socket.Read(ctx)
	if err == nil {
		t.Fatal("a socket whose session ended is still being served")
	}

	if reason := websocket.CloseStatus(err); reason != websocket.StatusPolicyViolation {
		t.Errorf("the socket closed with %v, want a policy violation", reason)
	}
}

// TestASubscriberWhoseSessionEndedIsNotPinged. The watch bounds how long a
// closed session leaves a socket open; this is what makes it never told
// anything in between, which is the part that matters — a ping says a resource
// this subscriber may read has changed, and somebody who logged out is no
// longer that subscriber.
func TestASubscriberWhoseSessionEndedIsNotPinged(t *testing.T) {
	routes, db, _, worker := watchingServer(t)

	worker.channels = map[subscription.Channel]deliverer{subscription.ChannelWebSocket: serving.sockets}

	// Long enough that the watch cannot be what closed the socket: what is
	// asserted here is the check the delivery itself makes.
	serving.sockets.interval = time.Hour

	server := httptest.NewServer(routes)
	defer server.Close()

	id := subscribeOver(t, routes, "Observation?status=final", "websocket", "")

	socket := connected(t, server, conformanceTokenValue)

	if answer, ok := says(t, socket, "bind "+id); !ok || answer != "bound "+id {
		t.Fatalf("bind answered %q (ok=%v)", answer, ok)
	}

	endTheSession(t)
	observe(t, routes, "final")
	worker.pass(context.Background())

	if notice, heard := hears(t, socket, 2*time.Second); heard && notice == "ping "+id {
		t.Error("a subscriber whose session ended was told its subscription fired")
	}

	held := deliveryStates(t, db)
	if len(held) != 1 || held[0] != string(subscription.Abandoned) {
		t.Errorf("the delivery is %v, want it recorded as never made", held)
	}
}

// TestASocketOutlivesNeitherItsSessionNorTheIdleBound. A session shorter than
// the idle bound is what actually ends the connection, and asking the store
// again is not how that is noticed — the deadline is known at the connect.
func TestASocketOutlivesNeitherItsSessionNorTheIdleBound(t *testing.T) {
	now := time.Date(2024, time.March, 1, 12, 0, 0, 0, time.UTC)

	shorter := earlier(now.Add(connectionMaxIdle), now.Add(time.Minute))
	if !shorter.Equal(now.Add(time.Minute)) {
		t.Errorf("a session ending in a minute is held for %v", shorter.Sub(now))
	}

	longer := earlier(now.Add(connectionMaxIdle), now.Add(12*time.Hour))
	if !longer.Equal(now.Add(connectionMaxIdle)) {
		t.Errorf("a long session is held for %v, want the idle bound", longer.Sub(now))
	}
}
