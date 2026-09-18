package project

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// AttemptKey is what a login throttle counts against.
//
// It is a digest rather than the identity it was derived from. A table of who
// tried to log in and failed is a list of this install's users and where they
// were, kept in a place nobody thinks to look; the throttle only ever needs to
// know whether two attempts are the same one, never what either of them was.
//
// The kind is mixed in, so an identity can never collide with an address and
// one person's failures can never be counted against a host.
type AttemptKey string

// The kinds an attempt is counted under.
const (
	identityAttempt = "identity"
	addressAttempt  = "address"
)

// IdentityAttemptKey names one identity in one Project.
//
// The address is folded rather than normalised: normalising can fail, and an
// address this server cannot read is still an attempt worth counting. Folding
// is what makes two spellings of the same mailbox one key, which is all the
// throttle asks of it.
func IdentityAttemptKey(slug, email string) AttemptKey {
	return attemptKey(identityAttempt, strings.ToLower(strings.TrimSpace(slug)),
		strings.ToLower(strings.TrimSpace(email)))
}

// AddressAttemptKey names one client address.
func AddressAttemptKey(address string) AttemptKey {
	return attemptKey(addressAttempt, strings.TrimSpace(address))
}

// attemptKey digests the parts with a separator between them, so no two
// different sets of parts can be spelled into the same key.
func attemptKey(kind string, parts ...string) AttemptKey {
	digest := sha256.Sum256([]byte(kind + "\x00" + strings.Join(parts, "\x00")))

	return AttemptKey(hex.EncodeToString(digest[:]))
}
