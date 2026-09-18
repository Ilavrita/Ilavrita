// Package subscription reads what a FHIR Subscription asks to be told about,
// and refuses one this server could not honour.
//
// Nothing here delivers anything. A Subscription states a criteria, a channel
// and an endpoint; what turns a written resource into a notification is the
// server, and what decides whether a subscriber may be told is the Scope the
// subscription was created under — never the subscription itself.
package subscription
