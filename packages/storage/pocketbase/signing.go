package pocketbase

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Ilavrita/Ilavrita/packages/project"
)

// ErrSigningKeyUnsealable reports a stored signing key this deployment cannot
// open. It is an error rather than an absence: a deployment answering "no key"
// would mint a second one beside a key it simply could not read, and every
// identity token it had already issued would stop verifying.
var ErrSigningKeyUnsealable = errors.New("pocketbase: a stored signing key cannot be opened")

// ErrNoSealingKey reports a deployment asked to mint a signing key with nothing
// to seal it under. Writing the private half in the clear would make a database
// read enough to assert anybody's identity.
var ErrNoSealingKey = errors.New("pocketbase: a signing key needs a sealing key to be stored under")

// signingKeyIDBytes is how much randomness a kid carries. It names a key rather
// than protecting anything, so this is about collision and not about guessing.
const signingKeyIDBytes = 8

// SigningKeyStore keeps the keys this install signs identity tokens with.
//
// The sealing key never reaches the database. What is stored is material this
// store cannot read without it, which is what makes a leaked file less than the
// ability to assert anybody's identity.
type SigningKeyStore struct {
	db  *sql.DB
	key project.SealingKey
}

// NewSigningKeyStore binds the store to a database and the key that seals what
// it holds.
func NewSigningKeyStore(db *sql.DB, key project.SealingKey) *SigningKeyStore {
	return &SigningKeyStore{db: db, key: key}
}

// Available reports whether this deployment can hold a signing key at all.
//
// One without a sealing key cannot: OpenID Connect is off on such a deployment,
// and it says so through discovery rather than by signing with material it
// wrote in the clear.
func (s *SigningKeyStore) Available() bool { return s != nil && !s.key.IsZero() }

const activeSigningKey = "SELECT id, private_key FROM identity_signing_keys WHERE state = 'active'"

// Active returns the key this server is signing with, if it has one.
func (s *SigningKeyStore) Active(ctx context.Context) (project.SigningKey, bool, error) {
	if !s.Available() {
		return project.SigningKey{}, false, nil
	}

	var id, sealed string

	switch err := conn(ctx, s.db).QueryRowContext(ctx, activeSigningKey).Scan(&id, &sealed); {
	case errors.Is(err, sql.ErrNoRows):
		return project.SigningKey{}, false, nil
	case err != nil:
		return project.SigningKey{}, false, fmt.Errorf("pocketbase: read the signing key: %w", err)
	}

	key, err := s.open(id, sealed)
	if err != nil {
		return project.SigningKey{}, false, err
	}

	return key, true, nil
}

// open decrypts one stored key and rebuilds it.
func (s *SigningKeyStore) open(id, sealed string) (project.SigningKey, error) {
	encoded, err := s.key.Open(sealed)
	if err != nil {
		return project.SigningKey{}, fmt.Errorf("%w: %s: %w", ErrSigningKeyUnsealable, id, err)
	}

	key, err := project.ParseSigningKey(id, encoded)
	if err != nil {
		return project.SigningKey{}, fmt.Errorf("%w: %s: %w", ErrSigningKeyUnsealable, id, err)
	}

	return key, nil
}

// insertSigningKey writes a minted key, losing the race rather than winning it.
//
// The conflict target is the partial index that allows one active key. Two
// processes starting at once both mint, one row lands, and the other reads the
// winner — so a start sequence cannot end with two keys each believing it is
// the one a token came from.
const insertSigningKey = "INSERT INTO identity_signing_keys" +
	" (id, private_key, state, created_at, retired_at)" +
	" VALUES (?, ?, 'active', ?, NULL)" +
	" ON CONFLICT (state) WHERE state = 'active' DO NOTHING"

// EnsureActive returns the key this server signs with, minting one if this
// install has never had one.
//
// It is idempotent and safe to call from every process at start: whoever gets
// there first is the key everybody then reads.
func (s *SigningKeyStore) EnsureActive(
	ctx context.Context, random io.Reader,
) (project.SigningKey, error) {
	if !s.Available() {
		return project.SigningKey{}, ErrNoSealingKey
	}

	if held, found, err := s.Active(ctx); err != nil || found {
		return held, err
	}

	minted, err := s.mint(random)
	if err != nil {
		return project.SigningKey{}, err
	}

	encoded, err := minted.Marshal()
	if err != nil {
		return project.SigningKey{}, fmt.Errorf("pocketbase: store the signing key: %w", err)
	}

	sealed, err := s.key.Seal(encoded)
	if err != nil {
		return project.SigningKey{}, fmt.Errorf("pocketbase: seal the signing key: %w", err)
	}

	if _, err := conn(ctx, s.db).ExecContext(ctx, insertSigningKey,
		minted.ID(), sealed, time.Now().UTC().UnixMilli()); err != nil {
		return project.SigningKey{}, fmt.Errorf("pocketbase: store the signing key: %w", err)
	}

	// Re-read rather than returning what was minted: on a lost race the row that
	// landed is somebody else's, and signing with a key no published set names
	// would produce tokens nothing can verify.
	held, found, err := s.Active(ctx)
	if err != nil {
		return project.SigningKey{}, err
	}

	if !found {
		return project.SigningKey{}, errors.New(
			"pocketbase: the signing key vanished after it was stored")
	}

	return held, nil
}

// mint draws a new key and the identifier it will be known by.
func (s *SigningKeyStore) mint(random io.Reader) (project.SigningKey, error) {
	// The name is drawn from the process's own randomness rather than from
	// random, which is the caller's source for key material. A test supplying a
	// deterministic reader wants a predictable key, not two keys named the same.
	name := make([]byte, signingKeyIDBytes)
	if _, err := io.ReadFull(rand.Reader, name); err != nil {
		return project.SigningKey{}, fmt.Errorf("pocketbase: name a signing key: %w", err)
	}

	key, err := project.NewSigningKey(hex.EncodeToString(name), random)
	if err != nil {
		return project.SigningKey{}, fmt.Errorf("pocketbase: mint a signing key: %w", err)
	}

	return key, nil
}
