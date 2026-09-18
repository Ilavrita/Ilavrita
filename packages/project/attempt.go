package project

import (
	"crypto/hmac"
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
// The digest is keyed. An unkeyed one would not hide anything: an email address
// and an IPv4 address are both drawn from a space small enough to walk, so
// anybody holding the table could hash their way back to every entry in it. The
// key is what turns "this is a digest" into a claim that survives the table
// being read.
//
// The kind is mixed in, so an identity can never collide with an address and
// one person's failures can never be counted against a host.
type AttemptKey string

// The kinds an attempt is counted under.
const (
	identityAttempt = "identity"
	addressAttempt  = "address"
)

// attemptPurpose names what this key is for, so it is independent of every
// other key the same secret derives.
const attemptPurpose = "attempt-key"

// AttemptKeys names attempts under one deployment's key.
//
// It is a value rather than a package function because the key has to come from
// somewhere, and the one place it can come from is what the deployment
// configured. Two installs holding the same failures still hold different
// tables.
type AttemptKeys struct {
	key []byte
}

// NewAttemptKeys derives the naming key from the deployment's sealing key.
//
// A deployment that configured none names attempts under a key of zeroes, which
// is an unkeyed digest wearing a keyed digest's clothes: the throttle still
// works and the table is still enumerable. That is the honest consequence of
// configuring no secret, and it is stated where the key is read rather than
// discovered later.
func NewAttemptKeys(sealing SealingKey) AttemptKeys {
	return AttemptKeys{key: sealing.Derive(attemptPurpose)}
}

// Keyed reports whether these names are worth anything against somebody holding
// the table.
func (k AttemptKeys) Keyed() bool {
	for _, b := range k.key {
		if b != 0 {
			return true
		}
	}

	return false
}

// Identity names one identity in one Project.
//
// The address is folded rather than normalised: normalising can fail, and an
// address this server cannot read is still an attempt worth counting. Folding
// is what makes two spellings of the same mailbox one key, which is all the
// throttle asks of it.
func (k AttemptKeys) Identity(slug, email string) AttemptKey {
	return k.name(identityAttempt, strings.ToLower(strings.TrimSpace(slug)),
		strings.ToLower(strings.TrimSpace(email)))
}

// Address names one client address.
func (k AttemptKeys) Address(address string) AttemptKey {
	return k.name(addressAttempt, strings.TrimSpace(address))
}

// name keys the parts with a separator between them, so no two different sets of
// parts can be spelled into the same key.
func (k AttemptKeys) name(kind string, parts ...string) AttemptKey {
	digest := hmac.New(sha256.New, k.key)

	// hash.Hash never reports an error, and this one is fed a byte slice.
	_, _ = digest.Write([]byte(kind + "\x00" + strings.Join(parts, "\x00")))

	return AttemptKey(hex.EncodeToString(digest.Sum(nil)))
}
