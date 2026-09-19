package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ilavrita/Ilavrita/packages/storage"
	"github.com/Ilavrita/Ilavrita/packages/subscription"
)

// hooked builds a Subscription pointing at one endpoint.
func hooked(t *testing.T, endpoint, payload string) subscription.Subscription {
	t.Helper()

	body := `{"resourceType":"Subscription","criteria":"Observation?status=final",` +
		`"status":"active","channel":{"type":"rest-hook","endpoint":"` + endpoint + `"`
	if payload != "" {
		body += `,"payload":"` + payload + `"`
	}

	held, err := subscription.Read(nil, "sub-1", []byte(body+`}}`))
	if err != nil {
		t.Fatalf("read the subscription: %v", err)
	}

	return held
}

func anObservation() storage.ResourceRecord {
	return storage.ResourceRecord{
		Key:     storage.ResourceKey{Project: "prj_a", Type: "Observation", ID: "obs-1"},
		Version: "1",
		Content: []byte(`{"resourceType":"Observation","status":"final"}`),
	}
}

// TestANotificationNeverReachesThisHostsOwnNetwork. A subscriber chooses the
// address, so without this, registering a subscription would be a way to make
// this server reach anything it can reach — a cloud metadata service included.
func TestANotificationNeverReachesThisHostsOwnNetwork(t *testing.T) {
	// A real listener, so what is refused is the connection rather than the
	// absence of anything to connect to.
	reached := make(chan struct{}, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	err := newRestHook().Deliver(context.Background(), aDelivery(), hooked(t, server.URL, ""), anObservation())
	if err == nil {
		t.Fatal("a notification reached a loopback address")
	}

	select {
	case <-reached:
		t.Error("the request arrived before it was refused")
	default:
	}
}

// TestAnAddressThisHostShouldNotBeMadeToReach names the ranges the guard is
// about, so the rule is stated rather than left to one example.
func TestAnAddressThisHostShouldNotBeMadeToReach(t *testing.T) {
	refused := map[string]string{
		"loopback":        "127.0.0.1",
		"loopback v6":     "::1",
		"private class a": "10.0.0.1",
		"private class b": "172.16.0.1",
		"private class c": "192.168.1.1",
		"link local":      "169.254.169.254",
		"link local v6":   "fe80::1",
		"unspecified":     "0.0.0.0",
		"unique local v6": "fc00::1",
		"not an address":  "",
	}

	for name, address := range refused {
		if !reachesOwnNetwork(net.ParseIP(address)) {
			t.Errorf("%s (%s) is treated as public", name, address)
		}
	}

	public := map[string]string{
		"a public v4": "93.184.216.34",
		"a public v6": "2606:2800:220:1:248:1893:25c8:1946",
	}

	for name, address := range public {
		if reachesOwnNetwork(net.ParseIP(address)) {
			t.Errorf("%s (%s) is treated as this host's own network", name, address)
		}
	}
}

// TestADeploymentMayOptIntoItsOwnNetwork, which is what a development one does
// when the subscriber is on the same machine.
func TestADeploymentMayOptIntoItsOwnNetwork(t *testing.T) {
	arrived := make(chan *http.Request, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, sent *http.Request) {
		arrived <- sent
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv(allowPrivateHooksVariable, "true")

	if err := newRestHook().Deliver(context.Background(), aDelivery(),
		hooked(t, server.URL, ""), anObservation()); err != nil {
		t.Fatalf("an opted-in deployment could not deliver: %v", err)
	}

	select {
	case sent := <-arrived:
		if sent.Method != http.MethodPost {
			t.Errorf("the notification arrived as %s", sent.Method)
		}

		if sent.Header.Get(subscriptionField) != "sub-1" {
			t.Errorf("it does not say which subscription: %q", sent.Header.Get(subscriptionField))
		}

		if sent.Header.Get(resourceField) != "Observation/obs-1" {
			t.Errorf("it does not say what about: %q", sent.Header.Get(resourceField))
		}

		// Delivery is at-least-once, so a subscriber that must act once per
		// write needs to be able to tell a repeat from a second write.
		if sent.Header.Get(deliveryField) != "dlv_one" {
			t.Errorf("it does not name itself: %q", sent.Header.Get(deliveryField))
		}
	default:
		t.Fatal("nothing arrived")
	}
}

// TestANotificationCarriesNoBodyUnlessItWasAskedFor. The notification is a
// signal; the resource behind it is something the subscriber reads for
// themselves, under their own authorization, at their own time.
func TestANotificationCarriesNoBodyUnlessItWasAskedFor(t *testing.T) {
	bodies := make(chan string, 2)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, sent *http.Request) {
		held := make([]byte, 1024)
		read, _ := sent.Body.Read(held)
		bodies <- string(held[:read])
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv(allowPrivateHooksVariable, "true")

	hook := newRestHook()

	if err := hook.Deliver(context.Background(), aDelivery(), hooked(t, server.URL, ""), anObservation()); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	if body := <-bodies; body != "" {
		t.Errorf("a subscription that asked for no payload was sent %q", body)
	}

	if err := hook.Deliver(context.Background(), aDelivery(),
		hooked(t, server.URL, "application/fhir+json"), anObservation()); err != nil {
		t.Fatalf("deliver with a payload: %v", err)
	}

	if body := <-bodies; !strings.Contains(body, `"resourceType":"Observation"`) {
		t.Errorf("a subscription that asked for the resource was sent %q", body)
	}
}

// TestASubscriberThatRefusesIsAFailedDelivery, so it is tried again rather than
// counted as told.
func TestASubscriberThatRefusesIsAFailedDelivery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	t.Setenv(allowPrivateHooksVariable, "true")

	if err := newRestHook().Deliver(context.Background(), aDelivery(),
		hooked(t, server.URL, ""), anObservation()); err == nil {
		t.Error("a subscriber answering 500 was counted as told")
	}
}

// TestARedirectIsNotFollowed. A redirect is a second address the subscriber did
// not register, and delivering there is delivering somewhere other than where
// they said.
func TestARedirectIsNotFollowed(t *testing.T) {
	elsewhere := make(chan struct{}, 1)

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		elsewhere <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, sent *http.Request) {
		http.Redirect(w, sent, target.URL, http.StatusFound)
	}))
	defer redirecting.Close()

	t.Setenv(allowPrivateHooksVariable, "true")

	err := newRestHook().Deliver(context.Background(), aDelivery(), hooked(t, redirecting.URL, ""), anObservation())
	if err == nil {
		t.Error("a redirect was followed and counted as delivered")
	}

	select {
	case <-elsewhere:
		t.Error("the notification was delivered to an address nobody registered")
	default:
	}
}

// aDelivery is what a notification names itself, so a subscriber can recognise
// the same one arriving twice.
func aDelivery() subscription.Delivery {
	return subscription.Delivery{
		Project: homeProject, ID: "dlv_one", Subscription: "sub-1",
	}
}
