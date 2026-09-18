package project

import (
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/argon2"
)

// The argon2id cost this server hashes at. Unlike a client secret, a password is
// chosen by a person and carries far less entropy, so the work factor is what
// stands between a stolen hash and the password behind it.
const (
	passwordMemory      uint32 = 64 * 1024
	passwordIterations  uint32 = 3
	passwordParallelism uint8  = 4
	passwordSaltBytes          = 16
	passwordKeyBytes    uint32 = 32
)

// passwordScheme names the algorithm a stored hash was produced by. It is stored
// rather than assumed, so the cost can be raised later without making every
// existing hash unreadable.
const passwordScheme = "argon2id"

// passwordFields is how many $-separated fields an encoded hash carries:
// scheme, version, parameters, salt and key.
const passwordFields = 5

// HashPassword derives a stored hash from a plaintext password. The salt is
// drawn per password, so two people choosing the same one do not collide, and a
// short or failed read hashes nothing rather than falling back to a fixed salt.
func HashPassword(plaintext string, random io.Reader) (PasswordHash, error) {
	if plaintext == "" {
		return "", fmt.Errorf("%w: a password is required", ErrInvalidCredentialState)
	}

	salt := make([]byte, passwordSaltBytes)
	if _, err := io.ReadFull(random, salt); err != nil {
		return "", fmt.Errorf("project: cannot draw a password salt: %w", err)
	}

	key := argon2.IDKey([]byte(plaintext), salt,
		passwordIterations, passwordMemory, passwordParallelism, passwordKeyBytes)

	encoded := fmt.Sprintf("%s$v=%d$m=%d,t=%d,p=%d$%s$%s",
		passwordScheme, argon2.Version, passwordMemory, passwordIterations, passwordParallelism,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))

	return PasswordHash(encoded), nil
}

// Matches reports whether the plaintext is the password this hash stands for,
// compared in constant time so a wrong guess cannot be narrowed by how long the
// answer took. A hash this server cannot read matches nothing, which denies
// rather than falling back to a weaker comparison.
func (h PasswordHash) Matches(plaintext string) bool {
	if h == "" || plaintext == "" {
		return false
	}

	salt, key, cost, err := h.decode()
	if err != nil {
		return false
	}

	candidate := argon2.IDKey([]byte(plaintext), salt,
		cost.iterations, cost.memory, cost.parallelism, passwordKeyBytes)

	return subtle.ConstantTimeCompare(key, candidate) == 1
}

// NeedsRehash reports whether a stored hash was produced at a lower cost than
// this server now uses, so a correct password can be re-hashed on the way past
// rather than everyone being forced to reset. A hash it cannot read needs one.
func (h PasswordHash) NeedsRehash() bool {
	_, _, cost, err := h.decode()
	if err != nil {
		return true
	}

	return cost.memory < passwordMemory ||
		cost.iterations < passwordIterations ||
		cost.parallelism < passwordParallelism
}

// passwordCost is the work a stored hash was produced at, read back from the
// hash itself so a cost raised later does not invalidate existing passwords.
type passwordCost struct {
	memory      uint32
	iterations  uint32
	parallelism uint8
}

// decode reads an encoded hash back into the parts a comparison needs. Anything
// it cannot read whole is an error, never a partially trusted set of parameters.
func (h PasswordHash) decode() ([]byte, []byte, passwordCost, error) {
	var cost passwordCost

	fields := strings.Split(string(h), "$")
	if len(fields) != passwordFields || fields[0] != passwordScheme {
		return nil, nil, cost, fmt.Errorf("%w: unreadable password hash", ErrInvalidCredentialState)
	}

	var version int
	if _, err := fmt.Sscanf(fields[1], "v=%d", &version); err != nil || version != argon2.Version {
		return nil, nil, cost, fmt.Errorf("%w: unsupported password hash version", ErrInvalidCredentialState)
	}

	cost, err := parseCost(fields[2])
	if err != nil {
		return nil, nil, cost, err
	}

	salt, err := base64.RawStdEncoding.DecodeString(fields[3])
	if err != nil {
		return nil, nil, cost, fmt.Errorf("%w: unreadable password salt", ErrInvalidCredentialState)
	}

	key, err := base64.RawStdEncoding.DecodeString(fields[4])
	if err != nil {
		return nil, nil, cost, fmt.Errorf("%w: unreadable password key", ErrInvalidCredentialState)
	}

	if len(salt) == 0 || len(key) != int(passwordKeyBytes) {
		return nil, nil, cost, fmt.Errorf("%w: password salt or key is the wrong size", ErrInvalidCredentialState)
	}

	return salt, key, cost, nil
}

// parseCost reads the m, t and p a hash was produced at, refusing any value
// argon2 could not have been called with.
func parseCost(field string) (passwordCost, error) {
	var (
		cost                         passwordCost
		memory, iterations, parallel uint64
	)

	if _, err := fmt.Sscanf(field, "m=%d,t=%d,p=%d", &memory, &iterations, &parallel); err != nil {
		return cost, fmt.Errorf("%w: unreadable password parameters", ErrInvalidCredentialState)
	}

	if memory == 0 || iterations == 0 || parallel == 0 {
		return cost, fmt.Errorf("%w: password parameters name no work", ErrInvalidCredentialState)
	}

	if memory > uint64(^uint32(0)) || iterations > uint64(^uint32(0)) || parallel > uint64(^uint8(0)) {
		return cost, fmt.Errorf("%w: password parameters exceed what argon2 accepts", ErrInvalidCredentialState)
	}

	cost.memory, cost.iterations, cost.parallelism = uint32(memory), uint32(iterations), uint8(parallel)

	return cost, nil
}
