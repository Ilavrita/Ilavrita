package subscription_test

import (
	"errors"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/subscription"
)

// sound is the smallest Subscription this server will act on.
func sound(criteria, channel, endpoint, status string) string {
	return `{"resourceType":"Subscription","criteria":"` + criteria +
		`","status":"` + status + `","channel":{"type":"` + channel +
		`","endpoint":"` + endpoint + `"}}`
}

func read(t *testing.T, body string) subscription.Subscription {
	t.Helper()

	held, err := subscription.Read(nil, "sub-1", []byte(body))
	if err != nil {
		t.Fatalf("read %s: %v", body, err)
	}

	return held
}

// TestACriteriaIsCheckedWhenItIsWritten. A subscription accepted and then
// silently never matching is worse than one refused: somebody is relying on it,
// and nothing will tell them.
func TestACriteriaIsCheckedWhenItIsWritten(t *testing.T) {
	held := read(t, sound("Observation?status=final", "rest-hook", "https://example.test/hook", "active"))

	if held.Watching() != "Observation" {
		t.Errorf("watching %q", held.Watching())
	}

	if len(held.Criteria().Criteria()) != 1 {
		t.Errorf("the criteria read as %d criteria", len(held.Criteria().Criteria()))
	}

	if !held.Delivers() {
		t.Error("an active subscription does not deliver")
	}
}

// TestACriteriaThisServerCannotRunIsRefused, for the reason an unimplemented
// search parameter is refused: accepting it would promise something.
func TestACriteriaThisServerCannotRunIsRefused(t *testing.T) {
	refused := map[string]string{
		"nothing at all":              "",
		"no resource type":            "?status=final",
		"a type nobody declared":      "NotAResource?status=final",
		"a parameter nobody declared": "Observation?colour=blue",
		"a parameter of another type": "Observation?gender=female",
		"a modifier":                  "Patient?family:above=Smith",
		"a chain":                     "Observation?subject.name=Ada",
		"a page size":                 "Observation?status=final&_count=5",
		"a cursor":                    "Observation?status=final&_cursor=abc",
		"a total":                     "Observation?status=final&_total=accurate",
		"a malformed query":           "Observation?%zz=1",
	}

	for name, criteria := range refused {
		_, err := subscription.Read(nil, "sub-1",
			[]byte(sound(criteria, "rest-hook", "https://example.test/hook", "active")))
		if !errors.Is(err, subscription.ErrMalformedCriteria) {
			t.Errorf("%s (%q): err = %v, want %v", name, criteria, err, subscription.ErrMalformedCriteria)
		}
	}
}

// TestACriteriaNamingOnlyATypeMatchesEveryOne, which is a subscription somebody
// may legitimately want: tell me about every Observation.
func TestACriteriaNamingOnlyATypeMatchesEveryOne(t *testing.T) {
	held := read(t, sound("Observation", "rest-hook", "https://example.test/hook", "active"))

	if held.Watching() != "Observation" || len(held.Criteria().Criteria()) != 0 {
		t.Errorf("a bare type read as %q with %d criteria",
			held.Watching(), len(held.Criteria().Criteria()))
	}
}

// TestAChannelThisBuildCannotDeliverOnIsRefused. Accepting one would promise to
// notify somebody this server has no way of reaching.
func TestAChannelThisBuildCannotDeliverOnIsRefused(t *testing.T) {
	for _, channel := range []string{"email", "sms", "message", "", "webhook"} {
		_, err := subscription.Read(nil, "sub-1",
			[]byte(sound("Observation", channel, "https://example.test/hook", "active")))
		if !errors.Is(err, subscription.ErrUnknownChannel) {
			t.Errorf("%q: err = %v, want %v", channel, err, subscription.ErrUnknownChannel)
		}
	}
}

// TestAnEndpointThisServerWouldNotCallIsRefused. A notification is a request
// this server makes on a subscriber's say-so, so where it may be sent is this
// server's business: a file:// endpoint would make the notifier a way to reach
// whatever the host can.
func TestAnEndpointThisServerWouldNotCallIsRefused(t *testing.T) {
	for name, endpoint := range map[string]string{
		"a file url":     "file:///etc/passwd",
		"a gopher url":   "gopher://example.test/",
		"no scheme":      "example.test/hook",
		"no host":        "https://",
		"nothing at all": "",
		"not a url":      "://",
	} {
		_, err := subscription.Read(nil, "sub-1",
			[]byte(sound("Observation", "rest-hook", endpoint, "active")))
		if !errors.Is(err, subscription.ErrMalformedEndpoint) {
			t.Errorf("%s (%q): err = %v, want %v", name, endpoint, err, subscription.ErrMalformedEndpoint)
		}
	}
}

// TestAWebSocketSubscriptionNeedsNoEndpoint, because the subscriber connects
// and names it rather than the server reaching out.
func TestAWebSocketSubscriptionNeedsNoEndpoint(t *testing.T) {
	held := read(t, sound("Observation", "websocket", "", "active"))

	if held.Channel() != subscription.ChannelWebSocket || held.Endpoint() != "" {
		t.Errorf("a websocket subscription read as %q to %q", held.Channel(), held.Endpoint())
	}
}

// TestOnlyAnActiveSubscriptionDelivers. The other three are states somebody has
// to move it out of, and delivering from them would be delivering from a
// subscription nobody turned on.
func TestOnlyAnActiveSubscriptionDelivers(t *testing.T) {
	for _, status := range []string{"requested", "error", "off"} {
		held := read(t, sound("Observation", "rest-hook", "https://example.test/hook", status))

		if held.Delivers() {
			t.Errorf("a %s subscription delivers", status)
		}
	}

	if !read(t, sound("Observation", "rest-hook", "https://example.test/hook", "active")).Delivers() {
		t.Error("an active subscription does not deliver")
	}
}

// TestAStatusOutsideTheEnumIsRefused rather than read as the nearest one.
func TestAStatusOutsideTheEnumIsRefused(t *testing.T) {
	for _, status := range []string{"", "on", "enabled", "ACTIVE"} {
		_, err := subscription.Read(nil, "sub-1",
			[]byte(sound("Observation", "rest-hook", "https://example.test/hook", status)))
		if !errors.Is(err, subscription.ErrUnknownStatus) {
			t.Errorf("%q: err = %v, want %v", status, err, subscription.ErrUnknownStatus)
		}
	}
}
