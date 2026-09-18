package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/Ilavrita/Ilavrita/packages/subscription"
	"github.com/coder/websocket"
	"github.com/pocketbase/pocketbase/core"
)

// websocketPath is where a subscriber connects to be told about its own
// subscriptions.
const websocketPath = "/ws"

// bearerProtocol is the WebSocket subprotocol a session token is carried in.
//
// A browser cannot set headers on a WebSocket, and a token in the URL ends up
// in every access log between here and there. The subprotocol is the one place
// left that is neither, and it carries the same session token every other route
// takes — so revoking a session closes this too.
const bearerProtocol = "ilavrita.session"

// What one connection is held to. A socket that is neither read from nor
// written to is a file descriptor nobody is using.
const (
	bindDeadline      = 30 * time.Second
	pingDeadline      = 5 * time.Second
	maxBindMessage    = 512
	connectionMaxIdle = 10 * time.Minute

	// sessionCheckInterval is how often a bound socket asks whether the session
	// it bound under is still there. Every other route proves a token on each
	// request; this one is authorized once and then held, so the check that
	// route makes for free has to be made on a clock instead. It bounds how long
	// a logout elsewhere leaves this socket open.
	sessionCheckInterval = 30 * time.Second
)

// The messages R4 defines for this channel. They are plain text, deliberately:
// the notification is a signal, and what it is about is something the
// subscriber reads for itself under its own authorization.
const (
	bindCommand = "bind"
	boundReply  = "bound"
	pingNotice  = "ping"
)

var (
	// errNotBound reports a socket that asked for something other than a bind.
	errNotBound = errors.New("ilavrita: a subscriber binds before it is told anything")

	// errNotYours reports a bind to somebody else's subscription. Knowing when
	// one fires is knowing something about the data behind it.
	errNotYours = errors.New("ilavrita: a subscriber binds only to its own subscription")
)

// socketDuty is everything one socket needs, taken once when it is accepted.
//
// A socket outlives the request that accepted it — that is the whole point of
// one — so nothing below may reach back into this package's own wiring. What
// `serving` points at is replaced when a process reconfigures and unset when a
// test finishes, and a goroutine still reading it then is reading a variable
// somebody else is writing.
//
// Every field is taken in the handler, while the wiring is known good, and
// carried from there.
type socketDuty struct {
	sockets  *hub
	owners   subscription.Queue
	sessions sessionResolver
	now      func() time.Time
	interval time.Duration
}

// onDuty takes what a socket needs from the wiring, once.
func onDuty(held *backend) socketDuty {
	duty := socketDuty{
		sockets:  held.sockets,
		owners:   held.notifications,
		sessions: held.sessions,
		now:      held.clock,
		interval: sessionCheckInterval,
	}

	if held.sockets != nil {
		duty.interval = held.sockets.interval
	}

	return duty
}

// alive answers whether one session is still one this server may serve as.
//
// A fault answers that it is. A socket already authenticated is not one an
// unreachable database should disconnect, and the write that follows is
// authorized on its own terms anyway — the subscriber reads the resource under
// its own standing, which a dead database refuses for itself.
func (d socketDuty) alive(ctx context.Context, session project.Session) bool {
	if d.sessions == nil {
		return false
	}

	live, err := d.sessions.Live(ctx, session.Project(), session.ID(), d.now())
	if err != nil {
		return true
	}

	return live
}

// listener is one bound socket, held under the session that bound it.
type listener struct {
	socket *websocket.Conn

	// alive answers whether that session is still one this server may serve as.
	// It is a function rather than a store so the hub reaches no database: what
	// a bound socket needs is this one question, asked about itself.
	alive func(context.Context) bool
}

// hub holds the sockets bound in this process and tells them when their
// subscription fires.
//
// The sockets are in this process and nowhere else. A deployment running
// several replicas has a subscriber connected to one of them, and a
// notification worked out on another has nobody here to tell — which is
// recorded as never delivered rather than retried, because retrying would not
// move it to the replica holding the socket. rest-hook is the channel that
// survives that; this one is for a subscriber that is connected now.
type hub struct {
	mutex sync.RWMutex
	bound map[boundKey][]*listener

	// interval is how often a bound socket re-asks after its session. It is a
	// field rather than a constant read in place so a test can watch a session
	// end without waiting out the real one.
	interval time.Duration
}

// boundKey names one Project's one subscription.
type boundKey struct {
	project storage.ProjectID
	held    storage.LogicalID
}

func newHub() *hub {
	return &hub{bound: map[boundKey][]*listener{}, interval: sessionCheckInterval}
}

// bind registers one socket against one subscription and returns how to undo it.
func (h *hub) bind(key boundKey, held *listener) func() {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	h.bound[key] = append(h.bound[key], held)

	return func() { h.release(key, held) }
}

func (h *hub) release(key boundKey, held *listener) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	kept := h.bound[key][:0]

	for _, candidate := range h.bound[key] {
		if candidate != held {
			kept = append(kept, candidate)
		}
	}

	if len(kept) == 0 {
		delete(h.bound, key)

		return
	}

	h.bound[key] = kept
}

// listeners returns the sockets bound to one subscription right now.
func (h *hub) listeners(key boundKey) []*listener {
	h.mutex.RLock()
	defer h.mutex.RUnlock()

	return append([]*listener(nil), h.bound[key]...)
}

// Deliver tells whoever is bound. It carries no body: R4's websocket channel is
// a signal, and the resource behind it is something the subscriber reads for
// itself, under its own authorization, at its own time.
func (h *hub) Deliver(
	ctx context.Context,
	_ subscription.Delivery,
	held subscription.Subscription,
	record storage.ResourceRecord,
) error {
	bound := h.listeners(boundKey{project: record.Key.Project, held: held.ID()})
	if len(bound) == 0 {
		return subscription.ErrNobodyListening
	}

	notice := pingNotice + " " + string(held.ID())
	told := 0

	for _, socket := range bound {
		// The session is asked about again here rather than trusted from the
		// connect. A notification is something the subscriber is told about its
		// own data, and somebody who logged out is no longer that subscriber.
		if !socket.alive(ctx) {
			_ = socket.socket.Close(websocket.StatusPolicyViolation, sessionEndedReason)

			continue
		}

		sending, stop := context.WithTimeout(ctx, pingDeadline)

		err := socket.socket.Write(sending, websocket.MessageText, []byte(notice))

		stop()

		if err != nil {
			// A socket that cannot be written to is gone; closing it is what
			// releases it, and the reader is what notices.
			_ = socket.socket.CloseNow()

			continue
		}

		told++
	}

	if told == 0 {
		return subscription.ErrNobodyListening
	}

	return nil
}

// subscribeOverWebSocket accepts one subscriber's socket.
func subscribeOverWebSocket(request *core.RequestEvent) error {
	if serving == nil || serving.sockets == nil {
		return refuse(request, errSubscriptionsUnavailable)
	}

	// Taken before the socket is accepted, and the only place the wiring is
	// read: everything after this point runs for as long as the connection
	// does.
	duty := onDuty(serving)

	session, err := websocketCaller(request, duty)
	if err != nil {
		return refuse(request, err)
	}

	socket, err := websocket.Accept(request.Response, request.Request, &websocket.AcceptOptions{
		Subprotocols: []string{bearerProtocol},

		// The subscriber is not a page. Nothing here is reachable from a
		// browsing context on another origin, so nothing is opted out of.
		InsecureSkipVerify: false,
	})
	if err != nil {
		return nil
	}

	defer func() { _ = socket.CloseNow() }()

	return serveSocket(request.Request.Context(), socket, session, duty)
}

// websocketCaller reads the session a socket presents.
//
// It is the ordinary session token, carried in the subprotocol because a
// browser can set nothing else on a WebSocket. A narrower credential — one
// token per subscription, minted over HTTP — would be better, and is what R5's
// binding token is for; this reuses the session so that revoking it closes the
// socket's authority too.
func websocketCaller(request *core.RequestEvent, duty socketDuty) (project.Session, error) {
	offered := request.Request.Header.Get("Sec-WebSocket-Protocol")

	var token string

	for _, part := range strings.Split(offered, ",") {
		held := strings.TrimSpace(part)
		if after, found := strings.CutPrefix(held, bearerProtocol+"."); found {
			token = after
		}
	}

	// An empty token is refused by the parser, which is the one place that
	// decides what a token is.
	parsed, err := project.ParseSessionToken(token)
	if err != nil {
		return project.Session{}, errNoPrincipal
	}

	if duty.sessions == nil {
		return project.Session{}, errNoPrincipal
	}

	session, found, err := duty.sessions.Resolve(
		request.Request.Context(), parsed, duty.now())
	if err != nil {
		return project.Session{}, err
	}

	if !found {
		return project.Session{}, errNoPrincipal
	}

	return session, nil
}

// serveSocket binds one socket and holds it until either side is done.
func serveSocket(
	ctx context.Context, socket *websocket.Conn, session project.Session, duty socketDuty,
) error {
	socket.SetReadLimit(maxBindMessage)

	binding, stop := context.WithTimeout(ctx, bindDeadline)
	defer stop()

	kind, message, err := socket.Read(binding)
	if err != nil {
		return nil
	}

	if kind != websocket.MessageText {
		_ = socket.Close(websocket.StatusUnsupportedData, "a bind is text")

		return nil
	}

	held, err := boundSubscription(ctx, session, string(message), duty)
	if err != nil {
		_ = socket.Close(websocket.StatusPolicyViolation, "the bind was refused")

		return nil
	}

	listening := &listener{
		socket: socket,
		alive:  func(ctx context.Context) bool { return duty.alive(ctx, session) },
	}

	release := duty.sockets.bind(boundKey{project: session.Project(), held: held}, listening)

	defer release()

	if err := socket.Write(ctx, websocket.MessageText,
		[]byte(boundReply+" "+string(held))); err != nil {
		return nil
	}

	// Held until the subscriber goes away, stops saying anything, or loses the
	// session it bound under. Nothing else is read from it: a bound socket is
	// written to, not talked over.
	idle, stopIdle := context.WithDeadline(
		ctx, earlier(duty.now().Add(connectionMaxIdle), session.ExpiresAt()))
	defer stopIdle()

	watching, stopWatching := context.WithCancel(idle)
	defer stopWatching()

	go watchSession(watching, listening, duty.interval)

	_, _, _ = socket.Read(idle)

	return nil
}

// sessionEndedReason is what a socket is told when the session behind it is gone.
// It says nothing about which of revocation and expiry it was: the subscriber
// authenticates again either way.
const sessionEndedReason = "the session ended"

// earlier is the first of two deadlines to arrive.
func earlier(first, second time.Time) time.Time {
	if second.Before(first) {
		return second
	}

	return first
}

// watchSession closes a bound socket once the session behind it ends.
//
// A socket is authorized at the connect and then held, so without this a logout
// would leave one being told about writes for as long as the connection lasts.
// The interval is what bounds that; the deliver path checks as well, so nothing
// is actually told in between.
func watchSession(ctx context.Context, held *listener, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !held.alive(ctx) {
				_ = held.socket.Close(websocket.StatusPolicyViolation, sessionEndedReason)

				return
			}
		}
	}
}

// boundSubscription reads which subscription a socket asked for, and refuses
// one that is not this caller's own.
//
// Binding to somebody else's would tell a caller when it fires, which is
// knowing something about the data behind it — and the notification itself is
// authorized as its owner, not as whoever happens to be listening.
func boundSubscription(
	ctx context.Context, session project.Session, message string, duty socketDuty,
) (storage.LogicalID, error) {
	command, named, found := strings.Cut(strings.TrimSpace(message), " ")
	if !found || command != bindCommand {
		return "", errNotBound
	}

	held := storage.LogicalID(strings.TrimSpace(named))
	if held == "" {
		return "", errNotBound
	}

	if duty.owners == nil {
		return "", errSubscriptionsUnavailable
	}

	owner, owned, err := duty.owners.Owner(ctx, session.Project(), held)
	if err != nil {
		return "", err
	}

	if !owned || owner.Membership != session.Membership() {
		return "", errNotYours
	}

	return held, nil
}
